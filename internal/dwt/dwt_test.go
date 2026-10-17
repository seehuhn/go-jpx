// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package dwt

import (
	"math"
	"math/rand"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func randRect(rng *rand.Rand) Rect {
	x0, y0 := rng.Intn(8), rng.Intn(8)
	return Rect{x0, y0, x0 + 1 + rng.Intn(40), y0 + 1 + rng.Intn(40)}
}

func area(r Rect) int { return (r.X1 - r.X0) * (r.Y1 - r.Y0) }

func TestRoundTrip53(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for range 500 {
		r := randRect(rng)
		levels := rng.Intn(7)
		want := make([]int32, area(r))
		for i := range want {
			want[i] = int32(rng.Intn(1<<21) - 1<<20)
		}
		buf := append([]int32(nil), want...)
		Forward53(buf, r, levels)
		Inverse53(buf, r, levels)
		if diff := cmp.Diff(want, buf); diff != "" {
			t.Fatalf("rect %v levels %d: round trip failed (-want +got):\n%s", r, levels, diff)
		}
	}
}

func TestRoundTrip97(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for range 500 {
		r := randRect(rng)
		levels := rng.Intn(7)
		want := make([]float32, area(r))
		for i := range want {
			want[i] = float32(rng.Float64()*2000 - 1000)
		}
		buf := append([]float32(nil), want...)
		Forward97(buf, r, levels)
		Inverse97(buf, r, levels)
		for i := range want {
			if d := math.Abs(float64(want[i] - buf[i])); d > 1e-3 {
				t.Fatalf("rect %v levels %d: error %g at %d", r, levels, d, i)
			}
		}
	}
}

func TestConstant(t *testing.T) {
	for x0 := range 2 {
		for y0 := range 2 {
			r := Rect{x0, y0, x0 + 13, y0 + 9}
			w, h := 13, 9
			b53 := make([]int32, w*h)
			b97 := make([]float32, w*h)
			for i := range b53 {
				b53[i] = 77
				b97[i] = 77
			}
			Forward53(b53, r, 1)
			Forward97(b97, r, 1)
			l := Reduce(r, 1)
			lw, lh := l.X1-l.X0, l.Y1-l.Y0
			for y := range h {
				for x := range w {
					low := x < lw && y < lh
					v53 := b53[y*w+x]
					v97 := float64(b97[y*w+x])
					if low && (v53 != 77 || math.Abs(v97-77) > 1e-3) {
						t.Errorf("origin (%d,%d): low-pass (%d,%d) = %d, %g", x0, y0, x, y, v53, v97)
					}
					if !low && (v53 != 0 || math.Abs(v97) > 1e-3) {
						t.Errorf("origin (%d,%d): high-pass (%d,%d) = %d, %g", x0, y0, x, y, v53, v97)
					}
				}
			}
		}
	}
}

func TestSingleSample(t *testing.T) {
	for _, y0 := range []int{0, 1} {
		buf := []int32{5}
		Forward53(buf, Rect{0, y0, 1, y0 + 1}, 3)
		want := int32(5)
		if y0 == 1 {
			want = 10
		}
		if buf[0] != want {
			t.Errorf("y0=%d: got %d, want %d", y0, buf[0], want)
		}
	}
}

func TestEmpty(t *testing.T) {
	Forward53(nil, Rect{3, 3, 3, 8}, 2)
	Inverse97(nil, Rect{3, 3, 8, 3}, 2)
}

func TestSplitReduce(t *testing.T) {
	l0, l1, h0, h1 := Split(3, 8)
	if l0 != 2 || l1 != 4 || h0 != 1 || h1 != 4 {
		t.Errorf("split(3,8) = %d %d %d %d", l0, l1, h0, h1)
	}
	got := Reduce(Rect{1, 3, 10, 12}, 2)
	if want := (Rect{1, 1, 3, 3}); got != want {
		t.Errorf("reduce: got %v, want %v", got, want)
	}
}

func BenchmarkInverse53(b *testing.B) {
	buf := make([]int32, 512*512)
	for b.Loop() {
		Inverse53(buf, Rect{0, 0, 512, 512}, 5)
	}
}

func BenchmarkInverse97(b *testing.B) {
	buf := make([]float32, 512*512)
	for b.Loop() {
		Inverse97(buf, Rect{0, 0, 512, 512}, 5)
	}
}

// A short signal with a hand-computed 5/3 decomposition pins down the
// lifting arithmetic, including the rounding and the origin parity.
func TestKnownVector53(t *testing.T) {
	// Signal at coordinates 1..6: 10 20 30 40 50 60 (odd origin).
	// High-pass samples sit at odd coordinates 1, 3, 5:
	//   h(1) = 10 - floor((20+20)/2) = -10   (left neighbour mirrored)
	//   h(3) = 30 - floor((20+40)/2) =   0
	//   h(5) = 50 - floor((40+60)/2) =   0
	// Low-pass samples at even coordinates 2, 4, 6:
	//   l(2) = 20 + floor((-10+0+2)/4) = 18
	//   l(4) = 40 + floor((0+0+2)/4)   = 40
	//   l(6) = 60 + floor((0+0+2)/4)   = 60   (right neighbour h(7) = h(5))
	buf := []int32{10, 20, 30, 40, 50, 60}
	Forward53(buf, Rect{1, 0, 7, 1}, 1)
	want := []int32{18, 40, 60, -10, 0, 0}
	if diff := cmp.Diff(want, buf); diff != "" {
		t.Errorf("forward 5/3 (-want +got):\n%s", diff)
	}
	Inverse53(buf, Rect{1, 0, 7, 1}, 1)
	if diff := cmp.Diff([]int32{10, 20, 30, 40, 50, 60}, buf); diff != "" {
		t.Errorf("inverse 5/3 (-want +got):\n%s", diff)
	}
}

// The 9/7 high-pass filter has a gain of two at the Nyquist frequency and
// the low-pass filter rejects it completely; an alternating signal thus
// pins down the scaling convention.
func TestNyquist97(t *testing.T) {
	for _, par := range []int{0, 1} {
		const n = 16
		buf := make([]float32, n)
		for i := range buf {
			if (i+par)%2 == 0 {
				buf[i] = 1
			} else {
				buf[i] = -1
			}
		}
		Forward97(buf, Rect{par, 0, par + n, 1}, 1)
		for i, v := range buf {
			want := float32(-2)
			if i < n/2 {
				want = 0
			}
			if math.Abs(float64(v-want)) > 1e-4 {
				t.Errorf("origin %d, coefficient %d: got %v, want %v", par, i, v, want)
			}
		}
	}
}

// A single sample at an odd coordinate is doubled by the forward
// transform and halved by the inverse; at an even coordinate it is
// unchanged.  This holds for both filters.
func TestSingleSample97(t *testing.T) {
	for _, par := range []int{0, 1} {
		buf := []float32{5}
		Forward97(buf, Rect{par, par, par + 1, par + 1}, 1)
		want := float32(5)
		if par == 1 {
			want = 20 // doubled by the row pass and again by the column pass
		}
		if buf[0] != want {
			t.Errorf("origin %d: forward gave %v, want %v", par, buf[0], want)
		}
		Inverse97(buf, Rect{par, par, par + 1, par + 1}, 1)
		if buf[0] != 5 {
			t.Errorf("origin %d: inverse gave %v, want 5", par, buf[0])
		}
	}
}
