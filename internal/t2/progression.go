// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package t2

import (
	"errors"
	"iter"
	"math"
	"slices"

	"seehuhn.de/go/jpx/internal/codestream"
)

// volume is one progression order volume: the packet ranges covered and
// the order in which they are visited.
type volume struct {
	resStart, resEnd   int // resolution levels [resStart, resEnd)
	compStart, compEnd int // components [compStart, compEnd)
	layerEnd           int // layers [0, layerEnd)
	order              codestream.Progression
}

// Packets returns the packets of the tile in codestream order.  With
// progression order changes, packets already emitted by an earlier volume
// are skipped, as the standard requires.
//
// The work done by the iterator is proportional to the number of packets
// it produces (plus, with progression order changes, the number of
// precincts per volume): resolution levels without precincts are skipped
// outright, and layers already emitted are not revisited.
func (t *Tile) Packets(charge Charge) (iter.Seq[Packet], error) {
	maxRes := 0
	for _, tc := range t.Comps {
		maxRes = max(maxRes, tc.Style.Levels+1)
	}
	// the position-driven orders keep lists of precinct start positions,
	// bounded by the number of precinct rows and columns
	rowsCols := 0
	for _, tc := range t.Comps {
		for _, res := range tc.Res {
			rowsCols += res.PW + res.PH
		}
	}
	if err := charge.charge(rowsCols * 16); err != nil {
		return nil, err
	}

	var vols []volume
	if len(t.POC) == 0 {
		vols = []volume{{0, maxRes, 0, len(t.Comps), t.Layers, t.Default}}
	} else {
		for _, e := range t.POC {
			vols = append(vols, volume{
				resStart:  e.ResStart,
				resEnd:    min(e.ResEnd, maxRes),
				compStart: e.CompStart,
				compEnd:   min(e.CompEnd, len(t.Comps)),
				layerEnd:  min(e.LayerEnd, t.Layers),
				order:     e.Progression,
			})
		}
		// track which layers have been emitted for every precinct
		for _, tc := range t.Comps {
			for _, res := range tc.Res {
				n, err := res.numPrecincts()
				if err != nil {
					return nil, err
				}
				if err := charge.charge(n * 4); err != nil {
					return nil, err
				}
				res.nextLayer = make([]int32, n)
				res.minLayer, res.minCount = 0, n
			}
		}
	}
	return func(yield func(Packet) bool) {
		for _, v := range vols {
			for pk := range t.volumePackets(v) {
				if len(t.POC) > 0 {
					if int(pk.Res.nextLayer[pk.Precinct]) > pk.Layer {
						continue
					}
					pk.Res.advance(pk.Precinct, pk.Layer)
				}
				if !yield(pk) {
					return
				}
			}
		}
	}, nil
}

// numPrecincts returns the number of precincts of the resolution level,
// guarding against overflow.
func (res *Resolution) numPrecincts() (int, error) {
	if res.PW < 0 || res.PH < 0 || (res.PW > 0 && res.PH > math.MaxInt/8/res.PW) {
		return 0, errors.New("t2: too many precincts")
	}
	return res.PW * res.PH, nil
}

// pair is a (component, resolution) combination with at least one
// precinct, together with the first layer that may still have packets.
type pair struct {
	c, r  int
	first int
}

// pairs lists the (component, resolution) combinations of the volume that
// have precincts, in component-major order.
func (t *Tile) pairs(v volume) []pair {
	var ps []pair
	for c := v.compStart; c < v.compEnd; c++ {
		tc := t.Comps[c]
		for r := v.resStart; r < min(v.resEnd, len(tc.Res)); r++ {
			res := tc.Res[r]
			if res.PW == 0 || res.PH == 0 {
				continue
			}
			first := 0
			if res.nextLayer != nil {
				first = int(res.minLayer)
			}
			if first >= v.layerEnd {
				continue
			}
			ps = append(ps, pair{c, r, first})
		}
	}
	return ps
}

// volumePackets enumerates the packets of one progression volume in its
// progression order.  Packets of precincts that do not exist (empty
// resolution levels) are not produced.
func (t *Tile) volumePackets(v volume) iter.Seq[Packet] {
	return func(yield func(Packet) bool) {
		ps := t.pairs(v)
		if len(ps) == 0 {
			return
		}
		switch v.order {
		case codestream.LRCP:
			first := v.layerEnd
			for _, p := range ps {
				first = min(first, p.first)
			}
			for l := first; l < v.layerEnd; l++ {
				for r := v.resStart; r < v.resEnd; r++ {
					for _, p := range ps {
						if p.r == r && l >= p.first && !t.yieldPrecincts(yield, p.c, r, l) {
							return
						}
					}
				}
			}
		case codestream.RLCP:
			for r := v.resStart; r < v.resEnd; r++ {
				first := v.layerEnd
				for _, p := range ps {
					if p.r == r {
						first = min(first, p.first)
					}
				}
				for l := first; l < v.layerEnd; l++ {
					for _, p := range ps {
						if p.r == r && l >= p.first && !t.yieldPrecincts(yield, p.c, r, l) {
							return
						}
					}
				}
			}
		case codestream.RPCL:
			for r := v.resStart; r < v.resEnd; r++ {
				var sub []pair
				for _, p := range ps {
					if p.r == r {
						sub = append(sub, p)
					}
				}
				if !t.yieldPositions(yield, v, sub) {
					return
				}
			}
		case codestream.PCRL:
			if !t.yieldPositions(yield, v, ps) {
				return
			}
		case codestream.CPRL:
			for c := v.compStart; c < v.compEnd; c++ {
				var sub []pair
				for _, p := range ps {
					if p.c == c {
						sub = append(sub, p)
					}
				}
				if !t.yieldPositions(yield, v, sub) {
					return
				}
			}
		}
	}
}

// anchor describes the precinct grid of one pair on the reference grid:
// precincts start at multiples of step, except that the first one starts
// at the tile origin when the grid is not aligned with it.
type anchor struct {
	xstep, ystep     int64
	xFirst, yFirst   int64 // start of the first precinct
	xSecond, ySecond int64 // start of the second precinct column/row
}

func (t *Tile) anchorOf(p pair) anchor {
	tc := t.Comps[p.c]
	res := tc.Res[p.r]
	shift := tc.Style.Levels - p.r
	a := anchor{
		xstep: int64(tc.DX) << uint(res.PPx+shift),
		ystep: int64(tc.DY) << uint(res.PPy+shift),
	}
	a.xFirst, a.xSecond = int64(t.Bounds.X0), (int64(t.Bounds.X0)/a.xstep+1)*a.xstep
	a.yFirst, a.ySecond = int64(t.Bounds.Y0), (int64(t.Bounds.Y0)/a.ystep+1)*a.ystep
	return a
}

// yieldPositions emits, in raster order of the precinct start positions
// on the reference grid, the packets of all layers of the given pairs.
// Only positions at which some pair has a precinct are visited, so the
// work is proportional to the number of precincts.  Pairs are visited in
// the order given, which is component-major as all position-driven
// progressions require.
func (t *Tile) yieldPositions(yield func(Packet) bool, v volume, ps []pair) bool {
	if len(ps) == 0 {
		return true
	}
	anchors := make([]anchor, len(ps))
	for i, p := range ps {
		anchors[i] = t.anchorOf(p)
	}
	ys := t.anchorRows(anchors, int64(t.Bounds.Y1))
	var xs []int64
	var active []int
	for _, y := range ys {
		active = active[:0]
		for i, a := range anchors {
			if y == a.yFirst || (y >= a.ySecond && y%a.ystep == 0) {
				active = append(active, i)
			}
		}
		xs = xs[:0]
		for _, i := range active {
			a := anchors[i]
			xs = append(xs, a.xFirst)
			for x := a.xSecond; x < int64(t.Bounds.X1); x += a.xstep {
				xs = append(xs, x)
			}
		}
		slices.Sort(xs)
		xs = slices.Compact(xs)
		for _, x := range xs {
			for _, i := range active {
				if !t.yieldPosition(yield, v, ps[i].c, ps[i].r, int(x), int(y)) {
					return false
				}
			}
		}
	}
	return true
}

// anchorRows returns the sorted distinct rows at which any of the
// anchors has a precinct start, below limit.
func (t *Tile) anchorRows(anchors []anchor, limit int64) []int64 {
	var ys []int64
	for _, a := range anchors {
		ys = append(ys, a.yFirst)
		for y := a.ySecond; y < limit; y += a.ystep {
			ys = append(ys, y)
		}
	}
	slices.Sort(ys)
	return slices.Compact(ys)
}

// yieldPrecincts emits the packets of all precincts of (c, r) in layer l.
func (t *Tile) yieldPrecincts(yield func(Packet) bool, c, r, l int) bool {
	tc := t.Comps[c]
	res := tc.Res[r]
	for k := range res.PW * res.PH {
		if res.nextLayer != nil && int(res.nextLayer[k]) > l {
			continue
		}
		if !yield(Packet{Layer: l, Comp: tc, Res: res, Precinct: k}) {
			return false
		}
	}
	return true
}

// yieldPosition emits the packets of all layers for the precinct of
// (c, r) that starts at reference grid position (x, y), if there is one.
func (t *Tile) yieldPosition(yield func(Packet) bool, v volume, c, r, x, y int) bool {
	tc := t.Comps[c]
	if r >= len(tc.Res) {
		return true
	}
	res := tc.Res[r]
	if res.PW == 0 || res.PH == 0 {
		return true
	}
	shift := tc.Style.Levels - r
	// a precinct starts here if the position is a multiple of the
	// precinct size on the reference grid, or at the tile origin when
	// the first precinct is not aligned
	ystep := int64(tc.DY) << uint(res.PPy+shift)
	xstep := int64(tc.DX) << uint(res.PPx+shift)
	yAligned := (int64(res.Bounds.Y0)<<uint(shift))%(int64(1)<<uint(res.PPy+shift)) == 0
	xAligned := (int64(res.Bounds.X0)<<uint(shift))%(int64(1)<<uint(res.PPx+shift)) == 0
	if int64(y)%ystep != 0 && (y != t.Bounds.Y0 || yAligned) {
		return true
	}
	if int64(x)%xstep != 0 && (x != t.Bounds.X0 || xAligned) {
		return true
	}
	px := ceilShift(ceilDiv(x, tc.DX), shift)>>uint(res.PPx) - res.Bounds.X0>>uint(res.PPx)
	py := ceilShift(ceilDiv(y, tc.DY), shift)>>uint(res.PPy) - res.Bounds.Y0>>uint(res.PPy)
	if px < 0 || px >= res.PW || py < 0 || py >= res.PH {
		return true
	}
	k := py*res.PW + px
	first := 0
	if res.nextLayer != nil {
		first = int(res.nextLayer[k])
	}
	for l := first; l < v.layerEnd; l++ {
		if !yield(Packet{Layer: l, Comp: tc, Res: res, Precinct: k}) {
			return false
		}
	}
	return true
}
