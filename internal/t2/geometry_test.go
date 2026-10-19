// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package t2

import (
	"testing"

	"seehuhn.de/go/jpx/internal/codestream"
	"seehuhn.de/go/jpx/internal/t1"
)

// Quantization entries short of 3*levels+1 are assigned to the finest
// levels; the coarser bands reuse the first entry.
func TestNewBandFewQuantEntries(t *testing.T) {
	const levels = 32
	for _, style := range []codestream.QuantStyle{codestream.QuantNone, codestream.QuantExpounded} {
		for _, n := range []int{0, 1, 4, 3*levels + 1} {
			q := codestream.Quantization{Style: style, GuardBits: 2}
			for i := range n {
				q.Exponents = append(q.Exponents, 1+i%30)
				if style == codestream.QuantExpounded {
					q.Mantissas = append(q.Mantissas, i)
				}
			}
			tc := &TileComp{
				Bounds: Rect{X1: 1 << 20, Y1: 1 << 20},
				Style:  codestream.CodingStyle{Levels: levels},
				Quant:  q,
			}
			for r := range levels + 1 {
				orients := []t1.Band{t1.BandHL, t1.BandLH, t1.BandHH}
				if r == 0 {
					orients = []t1.Band{t1.BandLL}
				}
				for _, o := range orients {
					b, err := tc.newBand(r, o, 6, 6)
					if n == 0 {
						if err == nil {
							t.Errorf("style=%d n=0: no error", style)
						}
						continue
					}
					if err != nil {
						t.Fatalf("style=%d n=%d r=%d: %v", style, n, r, err)
					}
					qi := 0
					if r > 0 {
						qi = 3*(r-1) + 1 + int(o-t1.BandHL)
					}
					want := q.Exponents[max(qi-(3*levels+1-n), 0)]
					if b.Epsilon != want {
						t.Errorf("style=%d n=%d r=%d o=%d: epsilon %d, want %d", style, n, r, o, b.Epsilon, want)
					}
				}
			}
		}
	}
}
