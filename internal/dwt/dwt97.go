// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package dwt

// lifting constants of the 9/7 filter bank
const (
	alpha = -1.586134342059924
	beta  = -0.052980118572961
	gamma = 0.882911075530934
	delta = 0.443506852043971
	kappa = 1.230174104914001
)

var kernel97 = kernel[float32]{ext: 4, fwd: fwd97, inv: inv97}

// Forward97 applies levels levels of the irreversible 9/7 transform in
// place.  The rectangle r describes the full-resolution signal held in buf.
func Forward97(buf []float32, r Rect, levels int) { forward2D(buf, r, levels, kernel97) }

// Inverse97 undoes Forward97 up to rounding error.
func Inverse97(buf []float32, r Rect, levels int) { inverse2D(buf, r, levels, kernel97) }

// lift adds c times the sum of both neighbours to every sample of parity q
// with scratch index in [lo,hi).
func lift(s []float32, lo, hi, par, q int, c float32) {
	for b := lo; b < hi; b++ {
		if (par+b)&1 == q {
			s[b] += c * (s[b-1] + s[b+1])
		}
	}
}

// fwd97 transforms the padded line s (data at s[4:4+n]) in place.
func fwd97(s []float32, n, par int) {
	if n == 1 {
		if par == 1 {
			s[4] *= 2
		}
		return
	}
	lift(s, 1, n+7, par, 1, alpha)
	lift(s, 2, n+6, par, 0, beta)
	lift(s, 3, n+5, par, 1, gamma)
	lift(s, 4, n+4, par, 0, delta)
	for b := 4; b < n+4; b++ {
		if (par+b)&1 == 1 {
			s[b] *= kappa
		} else {
			s[b] *= 1 / kappa
		}
	}
}

func inv97(s []float32, n, par int) {
	if n == 1 {
		if par == 1 {
			s[4] /= 2
		}
		return
	}
	for b := 0; b < n+8; b++ {
		if (par+b)&1 == 0 {
			s[b] *= kappa
		} else {
			s[b] *= 1 / kappa
		}
	}
	lift(s, 1, n+7, par, 0, -delta)
	lift(s, 2, n+6, par, 1, -gamma)
	lift(s, 3, n+5, par, 0, -beta)
	lift(s, 4, n+4, par, 1, -alpha)
}
