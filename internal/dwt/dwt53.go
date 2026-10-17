// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package dwt

var kernel53 = kernel[int32]{ext: 2, fwd: fwd53, inv: inv53}

// Forward53 applies levels levels of the reversible 5/3 transform in place.
// The rectangle r describes the full-resolution signal held in buf.
func Forward53(buf []int32, r Rect, levels int) { forward2D(buf, r, levels, kernel53) }

// Inverse53 undoes Forward53 exactly.
func Inverse53(buf []int32, r Rect, levels int) { inverse2D(buf, r, levels, kernel53) }

// fwd53 transforms the padded line s (data at s[2:2+n]) in place.
func fwd53(s []int32, n, par int) {
	if n == 1 {
		if par == 1 {
			s[2] *= 2
		}
		return
	}
	// high-pass at odd coordinates, then low-pass at even coordinates
	for b := 1; b < n+3; b++ {
		if (par+b)&1 == 1 {
			s[b] -= (s[b-1] + s[b+1]) >> 1
		}
	}
	for b := 2; b < n+2; b++ {
		if (par+b)&1 == 0 {
			s[b] += (s[b-1] + s[b+1] + 2) >> 2
		}
	}
}

func inv53(s []int32, n, par int) {
	if n == 1 {
		if par == 1 {
			s[2] /= 2
		}
		return
	}
	for b := 1; b < n+3; b++ {
		if (par+b)&1 == 0 {
			s[b] -= (s[b-1] + s[b+1] + 2) >> 2
		}
	}
	for b := 2; b < n+2; b++ {
		if (par+b)&1 == 1 {
			s[b] += (s[b-1] + s[b+1]) >> 1
		}
	}
}
