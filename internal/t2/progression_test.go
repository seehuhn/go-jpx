// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package t2

import (
	"math/rand/v2"
	"testing"

	"github.com/google/go-cmp/cmp"

	"seehuhn.de/go/jpx/internal/codestream"
)

// pkKey identifies a packet independently of the tile's pointers.
type pkKey struct {
	Layer, Comp, Res, Precinct int
}

// testComp describes one component of a test tile.
type testComp struct {
	dx, dy int
	levels int
	pp     []codestream.PrecinctSize // one entry per resolution level
}

// newTestTile builds a tile with the given geometry, without reading a
// codestream.
func newTestTile(t *testing.T, bounds Rect, layers int, order codestream.Progression, poc []codestream.POCEntry, comps []testComp) *Tile {
	t.Helper()
	tile := &Tile{Bounds: bounds, Layers: layers, Default: order, POC: poc}
	for c, tcs := range comps {
		tc := &TileComp{
			Index: c,
			Style: codestream.CodingStyle{
				Levels:        tcs.levels,
				CodeBlockW:    2,
				CodeBlockH:    2,
				PrecinctSizes: tcs.pp,
			},
			Quant: codestream.Quantization{
				Style:     codestream.QuantDerived,
				GuardBits: 2,
				Exponents: []int{16},
				Mantissas: []int{0},
			},
			Depth: 8,
			DX:    tcs.dx,
			DY:    tcs.dy,
		}
		tc.Bounds = Rect{
			X0: ceilDiv(bounds.X0, tcs.dx),
			Y0: ceilDiv(bounds.Y0, tcs.dy),
			X1: ceilDiv(bounds.X1, tcs.dx),
			Y1: ceilDiv(bounds.Y1, tcs.dy),
		}
		if err := tc.buildResolutions(nil); err != nil {
			t.Fatal(err)
		}
		tile.Comps = append(tile.Comps, tc)
	}
	return tile
}

func collectPackets(t *testing.T, tile *Tile) []pkKey {
	t.Helper()
	seq, err := tile.Packets(nil)
	if err != nil {
		t.Fatal(err)
	}
	var out []pkKey
	for pk := range seq {
		out = append(out, pkKey{pk.Layer, pk.Comp.Index, pk.Res.Level, pk.Precinct})
	}
	return out
}

// referencePackets lists the packets of a tile by scanning every position
// of the reference grid, following the definition of the progression
// orders directly.  The tile must not have been iterated before.
func referencePackets(tile *Tile) []pkKey {
	maxRes := 0
	for _, tc := range tile.Comps {
		maxRes = max(maxRes, tc.Style.Levels+1)
	}
	vols := []volume{{0, maxRes, 0, len(tile.Comps), tile.Layers, tile.Default}}
	if len(tile.POC) > 0 {
		vols = vols[:0]
		for _, e := range tile.POC {
			vols = append(vols, volume{e.ResStart, min(e.ResEnd, maxRes),
				e.CompStart, min(e.CompEnd, len(tile.Comps)),
				min(e.LayerEnd, tile.Layers), e.Progression})
		}
	}

	seen := make(map[pkKey]bool)
	var out []pkKey
	emit := func(pk Packet) bool {
		k := pkKey{pk.Layer, pk.Comp.Index, pk.Res.Level, pk.Precinct}
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
		return true
	}
	// precinct at the reference grid position, all layers of the volume
	at := func(v volume, c, r, x, y int) {
		if c >= v.compStart && c < v.compEnd && r >= v.resStart && r < v.resEnd {
			tile.yieldPosition(emit, v, c, r, x, y)
		}
	}
	// all precincts of (c, r) in layer l
	all := func(v volume, c, r, l int) {
		tc := tile.Comps[c]
		if c < v.compStart || c >= v.compEnd || r < v.resStart || r >= v.resEnd || r >= len(tc.Res) || l >= v.layerEnd {
			return
		}
		res := tc.Res[r]
		for k := range res.PW * res.PH {
			emit(Packet{Layer: l, Comp: tc, Res: res, Precinct: k})
		}
	}
	b := tile.Bounds
	nc := len(tile.Comps)
	for _, v := range vols {
		switch v.order {
		case codestream.LRCP:
			for l := range tile.Layers {
				for r := range maxRes {
					for c := range nc {
						all(v, c, r, l)
					}
				}
			}
		case codestream.RLCP:
			for r := range maxRes {
				for l := range tile.Layers {
					for c := range nc {
						all(v, c, r, l)
					}
				}
			}
		case codestream.RPCL:
			for r := range maxRes {
				for y := b.Y0; y < b.Y1; y++ {
					for x := b.X0; x < b.X1; x++ {
						for c := range nc {
							at(v, c, r, x, y)
						}
					}
				}
			}
		case codestream.PCRL:
			for y := b.Y0; y < b.Y1; y++ {
				for x := b.X0; x < b.X1; x++ {
					for c := range nc {
						for r := range maxRes {
							at(v, c, r, x, y)
						}
					}
				}
			}
		case codestream.CPRL:
			for c := range nc {
				for y := b.Y0; y < b.Y1; y++ {
					for x := b.X0; x < b.X1; x++ {
						for r := range maxRes {
							at(v, c, r, x, y)
						}
					}
				}
			}
		}
	}
	return out
}

func randomTestTile(t *testing.T, rng *rand.Rand) *Tile {
	t.Helper()
	x0, y0 := rng.IntN(24), rng.IntN(24)
	bounds := Rect{X0: x0, Y0: y0, X1: x0 + 1 + rng.IntN(40), Y1: y0 + 1 + rng.IntN(40)}
	comps := make([]testComp, 1+rng.IntN(3))
	maxRes := 0
	for c := range comps {
		tc := testComp{dx: 1 + rng.IntN(3), dy: 1 + rng.IntN(3), levels: rng.IntN(4)}
		for r := range tc.levels + 1 {
			lo := 1
			if r == 0 {
				lo = 0
			}
			tc.pp = append(tc.pp, codestream.PrecinctSize{PPx: lo + rng.IntN(4), PPy: lo + rng.IntN(4)})
		}
		comps[c] = tc
		maxRes = max(maxRes, tc.levels+1)
	}
	layers := 1 + rng.IntN(3)
	order := codestream.Progression(rng.IntN(5))
	var poc []codestream.POCEntry
	if rng.IntN(2) == 0 {
		for range 1 + rng.IntN(4) {
			rs := rng.IntN(maxRes)
			cs := rng.IntN(len(comps))
			poc = append(poc, codestream.POCEntry{
				ResStart:    rs,
				ResEnd:      rs + 1 + rng.IntN(maxRes-rs+1),
				CompStart:   cs,
				CompEnd:     cs + 1 + rng.IntN(len(comps)-cs),
				LayerEnd:    1 + rng.IntN(layers+1),
				Progression: codestream.Progression(rng.IntN(5)),
			})
		}
	}
	return newTestTile(t, bounds, layers, order, poc, comps)
}

func TestProgressionOrderMatchesReference(t *testing.T) {
	for seed := range uint64(500) {
		rng := rand.New(rand.NewPCG(seed, 1))
		got := collectPackets(t, randomTestTile(t, rng))
		rng = rand.New(rand.NewPCG(seed, 1))
		want := referencePackets(randomTestTile(t, rng))
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("seed %d: packet order differs (-want +got):\n%s", seed, diff)
		}
	}
}

// TestProgressionRowWork checks that the row selection of the
// position-driven orders only touches the anchors which have a precinct
// on the row.  One pair has a fine precinct grid with many rows, many
// others have a single precinct row; scanning all anchors on every row
// would make the work rows × pairs.
func TestProgressionRowWork(t *testing.T) {
	const height = 1024
	comps := []testComp{{dx: 1, dy: 1, levels: 0, pp: []codestream.PrecinctSize{{PPx: 15, PPy: 1}}}}
	for range 500 {
		comps = append(comps, testComp{dx: 1, dy: 1, levels: 0, pp: []codestream.PrecinctSize{{PPx: 15, PPy: 15}}})
	}
	tile := newTestTile(t, Rect{X0: 0, Y0: 0, X1: 1, Y1: height}, 1, codestream.PCRL, nil, comps)

	v := volume{0, 1, 0, len(comps), 1, codestream.PCRL}
	ps := tile.pairs(v)
	anchors := make([]anchor, len(ps))
	for i, p := range ps {
		anchors[i] = tile.anchorOf(p)
	}

	total := 0
	rows := 0
	prev := int64(-1)
	for y, active := range activeRows(anchors, height) {
		if y <= prev {
			t.Fatalf("rows not increasing: %d after %d", y, prev)
		}
		prev = y
		rows++
		total += len(active)
		// exactly the anchors with a precinct row starting at y, in order
		var want []int
		for i, a := range anchors {
			if y == a.yFirst || (y >= a.ySecond && y%a.ystep == 0) {
				want = append(want, i)
			}
		}
		if diff := cmp.Diff(want, active); diff != "" {
			t.Fatalf("row %d: wrong anchors (-want +got):\n%s", y, diff)
		}
	}
	if rows != height/2 {
		t.Errorf("got %d rows, want %d", rows, height/2)
	}
	// one entry per precinct row of every pair
	if want := height/2 + len(comps) - 1; total != want {
		t.Errorf("anchors examined: got %d, want %d", total, want)
	}

	got := collectPackets(t, tile)
	if want := height/2 + len(comps) - 1; len(got) != want {
		t.Errorf("got %d packets, want %d", len(got), want)
	}
}
