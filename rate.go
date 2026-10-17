// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jpx

import (
	"math"
	"slices"

	"seehuhn.de/go/jpx/internal/t2"
)

// truncation is a candidate truncation point of a code-block: the number
// of passes and the rate-distortion slope of the convex hull segment that
// ends there.
type truncation struct {
	passes int
	slope  float64
}

// hull computes the convex hull truncation points of a block: the pass
// counts at which the distortion decrease per byte is largest, with
// strictly decreasing slopes.
func hull(bd *t2.BlockData) []truncation {
	type point struct {
		passes, length int
		dist           float64
	}
	slope := func(a, b point) float64 {
		if b.length <= a.length {
			return math.Inf(1)
		}
		return (b.dist - a.dist) / float64(b.length-a.length)
	}
	pts := []point{{}}
	cum := 0.0
	for i, p := range bd.Passes {
		cum += p.Dist
		cand := point{i + 1, p.Len, cum}
		for len(pts) >= 2 {
			a, b := pts[len(pts)-2], pts[len(pts)-1]
			if slope(a, cand) >= slope(a, b) {
				pts = pts[:len(pts)-1]
			} else {
				break
			}
		}
		if last := pts[len(pts)-1]; cand.length == last.length {
			if cand.dist >= last.dist {
				pts[len(pts)-1] = cand
			}
			continue
		}
		pts = append(pts, cand)
	}
	res := make([]truncation, 0, len(pts)-1)
	for i := 1; i < len(pts); i++ {
		res = append(res, truncation{pts[i].passes, slope(pts[i-1], pts[i])})
	}
	return res
}

// assignLayers distributes the coding passes of all blocks over the
// quality layers.  With a single layer every pass is included.  With
// several, a common rate-distortion slope threshold per layer selects the
// passes, so that each layer adds the most valuable data first.
func (e *encoder) assignLayers(td *tileData) error {
	var blocks []*t2.BlockData
	for _, comp := range td.enc {
		for _, res := range comp {
			for _, prec := range res {
				for _, enc := range prec {
					for i := range enc.Blocks {
						blocks = append(blocks, &enc.Blocks[i])
					}
				}
			}
		}
	}
	if err := chargeOpt(e.charge, len(blocks)*e.layers*8); err != nil {
		return err
	}
	for _, bd := range blocks {
		bd.LayerEnd = make([]int, e.layers)
	}
	if e.layers == 1 {
		for _, bd := range blocks {
			bd.LayerEnd[0] = len(bd.Passes)
		}
		return nil
	}

	hulls := make([][]truncation, len(blocks))
	total := 0
	for i, bd := range blocks {
		hulls[i] = hull(bd)
		if n := len(bd.Passes); n > 0 {
			total += bd.Passes[n-1].Len
		}
	}

	// cumulative byte targets per layer
	targets := make([]int, e.layers)
	area := td.tile.Bounds.Dx() * td.tile.Bounds.Dy()
	for l := range e.layers {
		switch {
		case l == e.layers-1 && (e.rates == nil || e.rates[l] == 0):
			targets[l] = total
		case e.rates != nil:
			targets[l] = int(e.rates[l] * float64(area) / 8)
		default:
			targets[l] = total >> uint(e.layers-1-l)
		}
	}

	var slopes []float64
	for _, h := range hulls {
		for _, t := range h {
			slopes = append(slopes, t.slope)
		}
	}
	slices.Sort(slopes)

	for l := range e.layers {
		if targets[l] >= total {
			for _, bd := range blocks {
				bd.LayerEnd[l] = len(bd.Passes)
			}
			continue
		}
		lambda := thresholdFor(blocks, hulls, slopes, targets[l])
		for i, bd := range blocks {
			n := passesForSlope(hulls[i], lambda)
			if l > 0 {
				n = max(n, bd.LayerEnd[l-1])
			}
			bd.LayerEnd[l] = n
		}
	}
	return nil
}

// passesForSlope returns the number of passes of the hull with slope at
// least lambda.
func passesForSlope(h []truncation, lambda float64) int {
	n := 0
	for _, t := range h {
		if t.slope >= lambda {
			n = t.passes
		}
	}
	return n
}

// thresholdFor finds the slope threshold at which the included passes of
// all blocks amount to about target bytes.  slopes holds all hull slopes
// in increasing order.
func thresholdFor(blocks []*t2.BlockData, hulls [][]truncation, slopes []float64, target int) float64 {
	if len(slopes) == 0 {
		return math.Inf(1)
	}
	// binary search for the smallest slope whose total fits the target
	lo, hi := 0, len(slopes)-1
	for lo < hi {
		mid := (lo + hi) / 2
		if bytesForSlope(blocks, hulls, slopes[mid]) <= target {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return slopes[lo]
}

// bytesForSlope returns the total bytes included at slope threshold
// lambda.
func bytesForSlope(blocks []*t2.BlockData, hulls [][]truncation, lambda float64) int {
	n := 0
	for i, bd := range blocks {
		if p := passesForSlope(hulls[i], lambda); p > 0 {
			n += bd.Passes[p-1].Len
		}
	}
	return n
}
