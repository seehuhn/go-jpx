// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package t2

import (
	"container/heap"
	"errors"
	"iter"
	"math"

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
// precincts per volume), up to a logarithmic factor for the position-driven
// orders: resolution levels without precincts are skipped outright, and
// layers already emitted are not revisited.
func (t *Tile) Packets(charge Charge) (iter.Seq[Packet], error) {
	maxRes := 0
	for _, tc := range t.Comps {
		maxRes = max(maxRes, tc.Style.Levels+1)
	}
	// every volume lists its (component, resolution) pairs, and the
	// position-driven orders keep a precinct grid anchor and two heap
	// entries per pair
	nRes := 0
	for _, tc := range t.Comps {
		nRes += len(tc.Res)
	}
	if err := charge.charge(nRes * 128); err != nil {
		return nil, err
	}

	var vols []volume
	if len(t.POC) == 0 {
		vols = []volume{{0, maxRes, 0, len(t.Comps), t.Layers, t.Default}}
	} else {
		// every volume scans the (component, resolution) pairs of the tile
		// before producing packets; account for that work like memory so
		// that a flood of progression order changes is stopped by the
		// budget rather than by a fixed limit
		if err := charge.charge(len(t.POC) * (64 + len(t.Comps)*maxRes)); err != nil {
			return nil, err
		}
		vols = make([]volume, 0, len(t.POC))
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
			// ps is component-major, so each component is a contiguous run
			for start := 0; start < len(ps); {
				end := start
				for end < len(ps) && ps[end].c == ps[start].c {
					end++
				}
				if !t.yieldPositions(yield, v, ps[start:end]) {
					return
				}
				start = end
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
// Rows and columns are both found by merging the precinct grids of the
// pairs, so that only positions at which some pair has a precinct are
// visited, and at every position only the pairs anchored there.  The work
// is proportional to the number of precincts, up to a logarithmic factor.
// Pairs are visited in the order given, which is component-major as all
// position-driven progressions require.
func (t *Tile) yieldPositions(yield func(Packet) bool, v volume, ps []pair) bool {
	if len(ps) == 0 {
		return true
	}
	anchors := make([]anchor, len(ps))
	for i, p := range ps {
		anchors[i] = t.anchorOf(p)
	}
	h := &anchorHeap{cursor: make([]int64, len(ps))}
	var atX []int
	for y, active := range activeRows(anchors, int64(t.Bounds.Y1)) {
		h.idx = h.idx[:0]
		for _, i := range active {
			h.cursor[i] = anchors[i].xFirst
			h.idx = append(h.idx, i)
		}
		heap.Init(h)
		// visit the smallest next x; at that x the anchored pairs come out
		// of the heap in their given order
		for h.Len() > 0 {
			x := h.cursor[h.idx[0]]
			atX = atX[:0]
			for h.Len() > 0 && h.cursor[h.idx[0]] == x {
				atX = append(atX, heap.Pop(h).(int))
			}
			for _, i := range atX {
				if !t.yieldPosition(yield, v, ps[i].c, ps[i].r, int(x), int(y)) {
					return false
				}
				a := anchors[i]
				next := max(a.xSecond, (x/a.xstep+1)*a.xstep)
				if next < int64(t.Bounds.X1) {
					h.cursor[i] = next
					heap.Push(h, i)
				}
			}
		}
	}
	return true
}

// activeRows enumerates, in increasing order, the rows below limit at
// which any of the anchors has a precinct start, together with the
// indices of the anchors starting a precinct row there, in increasing
// order.  The slice of indices is reused and must not be modified.
// Every anchor is examined once per precinct row it has.
func activeRows(anchors []anchor, limit int64) iter.Seq2[int64, []int] {
	return func(yield func(int64, []int) bool) {
		h := &anchorHeap{
			cursor: make([]int64, len(anchors)),
			idx:    make([]int, len(anchors)),
		}
		for i, a := range anchors {
			h.cursor[i] = a.yFirst
			h.idx[i] = i
		}
		heap.Init(h)
		var active []int
		for h.Len() > 0 {
			y := h.cursor[h.idx[0]]
			active = active[:0]
			for h.Len() > 0 && h.cursor[h.idx[0]] == y {
				active = append(active, heap.Pop(h).(int))
			}
			if !yield(y, active) {
				return
			}
			for _, i := range active {
				a := anchors[i]
				next := a.ySecond
				if y != a.yFirst {
					next = y + a.ystep
				}
				if next < limit {
					h.cursor[i] = next
					heap.Push(h, i)
				}
			}
		}
	}
}

// anchorHeap orders pairs by their next anchor coordinate, then by pair
// index, for the merges in yieldPositions and activeRows.
type anchorHeap struct {
	cursor []int64
	idx    []int
}

func (h *anchorHeap) Len() int { return len(h.idx) }
func (h *anchorHeap) Less(a, b int) bool {
	i, j := h.idx[a], h.idx[b]
	return h.cursor[i] < h.cursor[j] || (h.cursor[i] == h.cursor[j] && i < j)
}
func (h *anchorHeap) Swap(a, b int) { h.idx[a], h.idx[b] = h.idx[b], h.idx[a] }
func (h *anchorHeap) Push(x any)    { h.idx = append(h.idx, x.(int)) }
func (h *anchorHeap) Pop() any {
	n := len(h.idx) - 1
	v := h.idx[n]
	h.idx = h.idx[:n]
	return v
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
