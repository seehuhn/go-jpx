// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package dwt

// Rect is a half-open rectangle on the coordinate grid of a resolution level.
type Rect struct{ X0, Y0, X1, Y1 int }

// Split returns the bounds of the low-pass and high-pass halves of [a,b):
// low = [ceil(a/2), ceil(b/2)), high = [floor(a/2), floor(b/2)).
func Split(a, b int) (low0, low1, high0, high1 int) {
	return (a + 1) >> 1, (b + 1) >> 1, a >> 1, b >> 1
}

// Reduce returns the bounds of r after levels halvings
// (ceil division of each coordinate by 2^levels).
func Reduce(r Rect, levels int) Rect {
	for range levels {
		r = Rect{(r.X0 + 1) >> 1, (r.Y0 + 1) >> 1, (r.X1 + 1) >> 1, (r.Y1 + 1) >> 1}
	}
	return r
}

// kernel is a 1-D lifting filter working on a padded scratch line.  The
// line holds n samples starting at index ext; par is the parity of the
// coordinate of the first sample.  Because ext is even, the coordinate
// parity of scratch index b is (par+b)&1.
type kernel[T any] struct {
	ext      int
	fwd, inv func(s []T, n, par int)
}

// extend fills the padding around s[ext:ext+n] using periodic symmetric
// extension.  It requires n >= 2.
func extend[T any](s []T, ext, n int) {
	period := 2 * (n - 1)
	pse := func(m int) int {
		m %= period
		if m < 0 {
			m += period
		}
		if m >= n {
			m = period - m
		}
		return m
	}
	for k := 1; k <= ext; k++ {
		s[ext-k] = s[ext+pse(-k)]
		s[ext+n-1+k] = s[ext+pse(n-1+k)]
	}
}

// forward2D runs the forward transform over all levels.
func forward2D[T any](buf []T, r Rect, levels int, k kernel[T]) {
	stride := r.X1 - r.X0
	if stride <= 0 || r.Y1 <= r.Y0 {
		return
	}
	w, h := stride, r.Y1-r.Y0
	scratch := make([]T, max(w, h)+2*k.ext)
	u0, u1, v0, v1 := r.X0, r.X1, r.Y0, r.Y1
	for range levels {
		w, h = u1-u0, v1-v0
		if w == 0 || h == 0 {
			break
		}
		// vertical filtering, then horizontal
		nl := (v1+1)>>1 - (v0+1)>>1
		for x := range w {
			line := scratch[:h+2*k.ext]
			for y := range h {
				line[k.ext+y] = buf[y*stride+x]
			}
			filterLine(line, h, v0&1, k.ext, k.fwd)
			deinterleaveCol(line[k.ext:k.ext+h], buf, x, stride, nl, v0&1)
		}
		nl = (u1+1)>>1 - (u0+1)>>1
		for y := range h {
			row := buf[y*stride : y*stride+w]
			line := scratch[:w+2*k.ext]
			copy(line[k.ext:], row)
			filterLine(line, w, u0&1, k.ext, k.fwd)
			deinterleaveRow(line[k.ext:k.ext+w], row, nl, u0&1)
		}
		u0, u1, v0, v1 = (u0+1)>>1, (u1+1)>>1, (v0+1)>>1, (v1+1)>>1
	}
}

// inverse2D runs the inverse transform over all levels.
func inverse2D[T any](buf []T, r Rect, levels int, k kernel[T]) {
	stride := r.X1 - r.X0
	if stride <= 0 || r.Y1 <= r.Y0 {
		return
	}
	scratch := make([]T, max(stride, r.Y1-r.Y0)+2*k.ext)
	// bounds of the region at every level, from finest to coarsest
	rects := make([]Rect, 0, levels)
	cur := r
	for range levels {
		if cur.X1 == cur.X0 || cur.Y1 == cur.Y0 {
			break
		}
		rects = append(rects, cur)
		cur = Reduce(cur, 1)
	}
	for i := len(rects) - 1; i >= 0; i-- {
		c := rects[i]
		u0, u1, v0, v1 := c.X0, c.X1, c.Y0, c.Y1
		w, h := u1-u0, v1-v0
		// horizontal filtering, then vertical
		nl := (u1+1)>>1 - (u0+1)>>1
		for y := range h {
			row := buf[y*stride : y*stride+w]
			line := scratch[:w+2*k.ext]
			interleaveRow(row, line[k.ext:k.ext+w], nl, u0&1)
			filterLine(line, w, u0&1, k.ext, k.inv)
			copy(row, line[k.ext:k.ext+w])
		}
		nl = (v1+1)>>1 - (v0+1)>>1
		for x := range w {
			line := scratch[:h+2*k.ext]
			interleaveCol(buf, line[k.ext:k.ext+h], x, stride, nl, v0&1)
			filterLine(line, h, v0&1, k.ext, k.inv)
			for y := range h {
				buf[y*stride+x] = line[k.ext+y]
			}
		}
	}
}

// filterLine extends the line and applies f; the single-sample case is
// handled by f itself and needs no extension.
func filterLine[T any](line []T, n, par, ext int, f func(s []T, n, par int)) {
	if n >= 2 {
		extend(line, ext, n)
	}
	f(line, n, par)
}

// lowFirst reports the scratch position of the first low-pass sample.
// Low-pass samples sit at even coordinates.
func lowFirst(par int) int { return par }

// highFirst reports the scratch position of the first high-pass sample.
func highFirst(par int) int { return 1 - par }

// deinterleaveRow copies interleaved src into dst: low-pass first (nl
// samples), then high-pass.
func deinterleaveRow[T any](src, dst []T, nl, par int) {
	j := 0
	for i := lowFirst(par); i < len(src); i += 2 {
		dst[j] = src[i]
		j++
	}
	j = nl
	for i := highFirst(par); i < len(src); i += 2 {
		dst[j] = src[i]
		j++
	}
}

func deinterleaveCol[T any](src, dst []T, x, stride, nl, par int) {
	j := 0
	for i := lowFirst(par); i < len(src); i += 2 {
		dst[j*stride+x] = src[i]
		j++
	}
	j = nl
	for i := highFirst(par); i < len(src); i += 2 {
		dst[j*stride+x] = src[i]
		j++
	}
}

// interleaveRow is the inverse of deinterleaveRow.
func interleaveRow[T any](src, dst []T, nl, par int) {
	j := 0
	for i := lowFirst(par); i < len(dst); i += 2 {
		dst[i] = src[j]
		j++
	}
	j = nl
	for i := highFirst(par); i < len(dst); i += 2 {
		dst[i] = src[j]
		j++
	}
}

func interleaveCol[T any](src, dst []T, x, stride, nl, par int) {
	j := 0
	for i := lowFirst(par); i < len(dst); i += 2 {
		dst[i] = src[j*stride+x]
		j++
	}
	j = nl
	for i := highFirst(par); i < len(dst); i += 2 {
		dst[i] = src[j*stride+x]
		j++
	}
}
