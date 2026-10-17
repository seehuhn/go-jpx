// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jpx

import (
	"bytes"
	"os"
	"testing"

	"seehuhn.de/go/membudget"
)

// Bytes of the main and tile-part headers of valid codestreams are
// overwritten with a selection of values; the decoder must not panic and
// must return consistent planes when it succeeds.  MUTATE_FULL=1 tries
// every byte and every value.
func TestMutateHeaders(t *testing.T) {
	full := os.Getenv("MUTATE_FULL") != ""
	seeds := fuzzSeeds(&testing.F{})
	img := testImage(40, 30, []Component{{Depth: 8, DX: 1, DY: 1}, {Depth: 12, Signed: true, DX: 2, DY: 1}}, 3)
	for _, p := range []EncodeParams{
		{TileWidth: 16, TileHeight: 16, Layers: 2, Precincts: []PrecinctSize{{2, 2}, {3, 3}}, CodeBlockWidth: 8, CodeBlockHeight: 8, Progression: RPCL},
		{Lossy: true, ROI: &Rectangle{2, 2, 20, 20}, SOP: true, EPH: true, Bypass: true, Terminate: true},
		{PackedHeaders: PackedInMain, TileParts: 2, TileWidth: 20, TileHeight: 20, ProgressionChanges: []ProgressionChange{{0, 0, 3, 2, 1, CPRL}, {0, 0, 6, 2, 1, PCRL}}},
	} {
		var buf bytes.Buffer
		if err := Encode(&buf, img, &p); err != nil {
			t.Fatal(err)
		}
		seeds = append(seeds, buf.Bytes())
	}
	values := []int{0, 1, 2, 0x0f, 0x7f, 0x80, 0xfe, 0xff}
	step := 1
	if full {
		values = make([]int, 256)
		for i := range values {
			values[i] = i
		}
	} else {
		step = 3
	}
	for si, seed := range seeds {
		// mutate the first 400 bytes (headers) and a sample of later ones
		positions := []int{}
		for i := 0; i < min(400, len(seed)); i += step {
			positions = append(positions, i)
		}
		for i := 400; i < len(seed); i += 397 {
			positions = append(positions, i)
		}
		for _, pos := range positions {
			data := append([]byte(nil), seed...)
			for _, v := range values {
				data[pos] = byte(v)
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("seed %d byte %d = %#x: panic: %v", si, pos, v, r)
						}
					}()
					b := membudget.New(32 << 20)
					img, err := Decode(bytes.NewReader(data), &Options{Budget: b})
					if err == nil {
						for _, pl := range img.Planes {
							if len(pl.Pix) != pl.Width*pl.Height {
								t.Fatalf("seed %d byte %d = %#x: inconsistent plane", si, pos, v)
							}
						}
					}
					_, _ = ReadHeader(bytes.NewReader(data), membudget.New(1<<20))
				}()
			}
		}
	}
}
