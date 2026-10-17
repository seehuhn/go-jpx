// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package t1

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/google/go-cmp/cmp"
)

var allStyles = []Style{
	{},
	{Bypass: true},
	{Reset: true},
	{Terminate: true},
	{Causal: true},
	{Predictable: true},
	{SegSymbols: true},
	{Bypass: true, Terminate: true},
	{Bypass: true, Reset: true, Terminate: true, Causal: true, Predictable: true, SegSymbols: true},
}

func styleName(s Style) string {
	return fmt.Sprintf("bypass=%t,reset=%t,term=%t,causal=%t,pred=%t,seg=%t",
		s.Bypass, s.Reset, s.Terminate, s.Causal, s.Predictable, s.SegSymbols)
}

// randomBlock produces coefficients with a roughly Laplacian distribution
// so that all three passes see realistic data.
func randomBlock(rng *rand.Rand, w, h, planes int) []int32 {
	in := make([]int32, w*h)
	limit := int32(1)<<uint(planes) - 1
	for i := range in {
		var v int32
		switch rng.IntN(4) {
		case 0:
			v = 0
		case 1:
			v = int32(rng.IntN(4))
		case 2:
			v = int32(rng.IntN(1 << uint(max(1, planes/2))))
		default:
			v = int32(rng.Int64N(int64(limit) + 1))
		}
		v = min(v, limit)
		if rng.IntN(2) == 0 {
			v = -v
		}
		in[i] = v
	}
	return in
}

// segments splits the encoded data into codeword segments according to
// the pass information, taking the first n passes.
func segments(enc *EncodedBlock, n int) []Segment {
	var segs []Segment
	start, passes := 0, 0
	for i := 0; i < n; i++ {
		passes++
		if enc.Passes[i].Terminated || i == n-1 {
			end := enc.Passes[i].Len
			segs = append(segs, Segment{Data: enc.Data[start:end], Passes: passes})
			start, passes = end, 0
		}
	}
	return segs
}

// toInteger converts decoded fractional-bit values to integers.
func toInteger(v []int32) []int32 {
	out := make([]int32, len(v))
	for i, x := range v {
		if x < 0 {
			out[i] = -(-x >> 1)
		} else {
			out[i] = x >> 1
		}
	}
	return out
}

func TestRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	enc := NewEncoder()
	dec := NewDecoder()
	for _, style := range allStyles {
		t.Run(styleName(style), func(t *testing.T) {
			for iter := range 60 {
				w, h := 1+rng.IntN(64), 1+rng.IntN(64)
				planes := 1 + rng.IntN(12)
				band := Band(rng.IntN(4))
				in := randomBlock(rng, w, h, planes)
				if iter == 0 {
					clear(in)
				}

				eb, err := enc.Encode(in, w, h, band, style, planes)
				if err != nil {
					t.Fatal(err)
				}
				out := make([]int32, w*h)
				segs := segments(eb, len(eb.Passes))
				err = dec.Decode(out, w, h, band, style, planes-eb.ZeroPlanes, segs)
				if err != nil {
					t.Fatal(err)
				}
				if diff := cmp.Diff(in, toInteger(out)); diff != "" {
					t.Fatalf("round trip failed for %dx%d, %d planes, band %d (-want +got):\n%s",
						w, h, planes, band, diff)
				}
			}
		})
	}
}

// Decoding a prefix of the passes must give the mid-point reconstruction
// of the planes decoded so far: every coefficient must lie within the
// uncertainty interval implied by the number of planes it has seen.
func TestTruncatedPasses(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	enc := NewEncoder()
	dec := NewDecoder()
	for _, style := range allStyles {
		for iter := range 40 {
			w, h := 1+rng.IntN(40), 1+rng.IntN(40)
			planes := 2 + rng.IntN(10)
			band := Band(rng.IntN(4))
			in := randomBlock(rng, w, h, planes)
			eb, err := enc.Encode(in, w, h, band, style, planes)
			if err != nil {
				t.Fatal(err)
			}
			if len(eb.Passes) == 0 {
				continue
			}
			n := 1 + rng.IntN(len(eb.Passes))
			coded := planes - eb.ZeroPlanes
			out := make([]int32, w*h)
			err = dec.Decode(out, w, h, band, style, coded, segments(eb, n))
			if err != nil {
				t.Fatal(err)
			}
			// after n passes at least floor((n+2)/3) planes are complete
			fullPlanes := (n + 2) / 3
			step := int32(1) << uint(coded-fullPlanes)
			for i, v := range out {
				want := in[i]
				got := float64(v) / 2
				if want < 0 {
					want = -want
					got = -got
				}
				lo := float64(want/step*step) - 0.75*float64(step)
				hi := float64(want/step*step) + 1.75*float64(step)
				if got < lo || got > hi {
					t.Fatalf("iter %d style %s: %d passes of %d, coefficient %d: got %v for %d (step %d)",
						iter, styleName(style), n, len(eb.Passes), i, got, in[i], step)
				}
			}
		}
	}
}

// Decoding must never panic on arbitrary segment data.
func TestGarbageInput(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	dec := NewDecoder()
	for range 300 {
		w, h := 1+rng.IntN(32), 1+rng.IntN(32)
		planes := 1 + rng.IntN(MaxPlanes)
		style := allStyles[rng.IntN(len(allStyles))]
		var segs []Segment
		for range 1 + rng.IntN(4) {
			data := make([]byte, rng.IntN(50))
			for i := range data {
				data[i] = byte(rng.IntN(256))
			}
			segs = append(segs, Segment{Data: data, Passes: 1 + rng.IntN(40)})
		}
		out := make([]int32, w*h)
		_ = dec.Decode(out, w, h, Band(rng.IntN(4)), style, planes, segs)
	}
}

// Decoding the first n passes from the first Passes[n-1].Len bytes must
// give exactly the same result as decoding n passes from all the data:
// the lengths reported for unterminated passes are sufficient.
func TestPassLengthsSufficient(t *testing.T) {
	rng := rand.New(rand.NewPCG(13, 14))
	enc := NewEncoder()
	dec := NewDecoder()
	for _, style := range allStyles {
		for range 40 {
			w, h := 1+rng.IntN(40), 1+rng.IntN(40)
			planes := 2 + rng.IntN(9)
			band := Band(rng.IntN(4))
			in := randomBlock(rng, w, h, planes)
			eb, err := enc.Encode(in, w, h, band, style, planes)
			if err != nil {
				t.Fatal(err)
			}
			coded := planes - eb.ZeroPlanes
			for n := 1; n <= len(eb.Passes); n++ {
				// n passes with the complete data of their segments
				all := segments(eb, len(eb.Passes))
				var whole []Segment
				for left := n; left > 0 && len(all) > 0; all = all[1:] {
					seg := all[0]
					seg.Passes = min(seg.Passes, left)
					left -= seg.Passes
					whole = append(whole, seg)
				}
				full := make([]int32, w*h)
				if err := dec.Decode(full, w, h, band, style, coded, whole); err != nil {
					t.Fatal(err)
				}
				// the same passes from copies of the segments cut at the
				// reported length, so that nothing beyond it can be read
				segs := segments(eb, n)
				for i := range segs {
					segs[i].Data = append([]byte(nil), segs[i].Data...)
				}
				trunc := make([]int32, w*h)
				if err := dec.Decode(trunc, w, h, band, style, coded, segs); err != nil {
					t.Fatal(err)
				}
				if diff := cmp.Diff(full, trunc); diff != "" {
					t.Fatalf("style %s, %d of %d passes (-full +cut):\n%s", styleName(style), n, len(eb.Passes), diff)
				}
			}
		}
	}
}

func TestInvalidGeometry(t *testing.T) {
	dec := NewDecoder()
	for _, g := range [][2]int{{0, 1}, {1, 0}, {2048, 1}, {65, 64}, {1 << 32, 1 << 32}} {
		if err := dec.Decode(make([]int32, 16), g[0], g[1], BandLL, Style{}, 1, nil); err == nil {
			t.Errorf("geometry %v was accepted", g)
		}
	}
}

func TestEncodeErrors(t *testing.T) {
	enc := NewEncoder()
	if _, err := enc.Encode([]int32{8}, 1, 1, BandLL, Style{}, 3); err == nil {
		t.Error("coefficient above the plane count was accepted")
	}
	if _, err := enc.Encode([]int32{1}, 1, 1, BandLL, Style{}, MaxPlanes+1); err == nil {
		t.Error("too many planes were accepted")
	}
	if _, err := enc.Encode([]int32{1}, 0, 1, BandLL, Style{}, 1); err == nil {
		t.Error("empty block was accepted")
	}
	dec := NewDecoder()
	if err := dec.Decode(make([]int32, 1), 1, 1, BandLL, Style{}, MaxPlanes+1, nil); err == nil {
		t.Error("too many planes were accepted")
	}
}

func TestPassLengthsMonotone(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8))
	enc := NewEncoder()
	for _, style := range allStyles {
		for range 20 {
			w, h := 1+rng.IntN(64), 1+rng.IntN(64)
			in := randomBlock(rng, w, h, 10)
			eb, err := enc.Encode(in, w, h, BandHL, style, 10)
			if err != nil {
				t.Fatal(err)
			}
			prev := 0
			for i, p := range eb.Passes {
				if p.Len < prev || p.Len > len(eb.Data) {
					t.Fatalf("pass %d has length %d (previous %d, total %d)", i, p.Len, prev, len(eb.Data))
				}
				if p.Len > 0 && p.Len < len(eb.Data) && eb.Data[p.Len-1] == 0xFF {
					t.Fatalf("pass %d ends after a 0xFF byte", i)
				}
				prev = p.Len
			}
			if n := len(eb.Passes); n > 0 && eb.Passes[n-1].Len != len(eb.Data) {
				t.Fatalf("last pass length %d does not cover all %d bytes", eb.Passes[n-1].Len, len(eb.Data))
			}
		}
	}
}

func BenchmarkDecode(b *testing.B) {
	rng := rand.New(rand.NewPCG(9, 10))
	in := randomBlock(rng, 64, 64, 10)
	enc := NewEncoder()
	eb, err := enc.Encode(in, 64, 64, BandHL, Style{}, 10)
	if err != nil {
		b.Fatal(err)
	}
	segs := segments(eb, len(eb.Passes))
	dec := NewDecoder()
	out := make([]int32, 64*64)
	b.SetBytes(64 * 64)
	for b.Loop() {
		if err := dec.Decode(out, 64, 64, BandHL, Style{}, 10-eb.ZeroPlanes, segs); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncode(b *testing.B) {
	rng := rand.New(rand.NewPCG(9, 10))
	in := randomBlock(rng, 64, 64, 10)
	enc := NewEncoder()
	b.SetBytes(64 * 64)
	for b.Loop() {
		if _, err := enc.Encode(in, 64, 64, BandHL, Style{}, 10); err != nil {
			b.Fatal(err)
		}
	}
}

// The incrementally maintained neighbourhood counts must agree with a
// direct count over the eight neighbours, in both context formation
// modes.
func TestNeighbourhoodCounts(t *testing.T) {
	rng := rand.New(rand.NewPCG(15, 16))
	for _, causal := range []bool{false, true} {
		for range 50 {
			w, h := 1+rng.IntN(20), 1+rng.IntN(20)
			var s blockState
			s.setup(w, h, BandLL, causal)
			sig := make([]bool, w*h)
			for range w * h / 2 {
				x, y := rng.IntN(w), rng.IntN(h)
				if sig[y*w+x] {
					continue
				}
				sig[y*w+x] = true
				s.setSignificant(s.index(x, y), y, rng.IntN(2) == 0)
			}
			at := func(x, y, cy int) int {
				// neighbour (x, y) as seen from a coefficient in row cy
				if x < 0 || y < 0 || x >= w || y >= h || !sig[y*w+x] {
					return 0
				}
				if causal && y == cy+1 && cy&3 == 3 {
					return 0
				}
				return 1
			}
			for y := range h {
				for x := range w {
					hc := at(x-1, y, y) + at(x+1, y, y)
					vc := at(x, y-1, y) + at(x, y+1, y)
					dc := at(x-1, y-1, y) + at(x+1, y-1, y) + at(x-1, y+1, y) + at(x+1, y+1, y)
					want := uint8(hc | vc<<2 | dc<<4)
					if got := s.nb[s.index(x, y)]; got != want {
						t.Fatalf("causal=%v (%d,%d): counts %#x, want %#x", causal, x, y, got, want)
					}
				}
			}
		}
	}
}
