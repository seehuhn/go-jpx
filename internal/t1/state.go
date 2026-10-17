// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package t1

// Per-coefficient state bits.
const (
	stSig     = 1 << iota // coefficient is significant
	stNeg                 // coefficient is negative
	stVisited             // coded in the significance pass of the current plane
	stRefined             // has been refined at least once
)

// Neighbourhood counts are kept per coefficient in one byte: the number
// of significant horizontal neighbours in bits 0-1, vertical in bits
// 2-3 and diagonal in bits 4-6.
const (
	nbH = 1
	nbV = 1 << 2
	nbD = 1 << 4
)

// zcLUT maps a neighbourhood byte to the significance context label for
// each band orientation.
var zcLUT [4][128]uint8

func init() {
	for band := range 4 {
		for n := range 128 {
			h, v, d := n&3, n>>2&3, n>>4
			if h > 2 || v > 2 || d > 4 {
				continue
			}
			zcLUT[band][n] = zcTable[band][zcIndex(h, v, d)]
		}
	}
}

// blockState is the coefficient state shared by the encoder and the
// decoder: significance and sign flags, and the neighbourhood counts
// from which the contexts are formed.  Both arrays have a one-coefficient
// border so that neighbours need no bounds checks.
type blockState struct {
	w, h   int
	stride int
	st     []uint8
	nb     []uint8
	band   Band
	causal bool
	lut    *[128]uint8
}

func (s *blockState) setup(w, h int, band Band, causal bool) {
	s.w, s.h, s.band, s.causal = w, h, band, causal
	s.stride = w + 2
	n := s.stride * (h + 2)
	if cap(s.st) < n {
		s.st = make([]uint8, n)
		s.nb = make([]uint8, n)
	} else {
		s.st = s.st[:n]
		s.nb = s.nb[:n]
		clear(s.st)
		clear(s.nb)
	}
	s.lut = &zcLUT[band]
}

// index returns the state index of coefficient (x, y).
func (s *blockState) index(x, y int) int { return (y+1)*s.stride + x + 1 }

// setSignificant marks the coefficient at state index i in row y as
// significant with the given sign bit and updates the neighbourhood
// counts of its neighbours.  In vertically causal mode a coefficient in
// the first row of a stripe is invisible to the stripe above.
func (s *blockState) setSignificant(i, y int, neg bool) {
	s.st[i] |= stSig
	if neg {
		s.st[i] |= stNeg
	}
	nb, stride := s.nb, s.stride
	nb[i-1] += nbH
	nb[i+1] += nbH
	nb[i+stride] += nbV
	nb[i+stride-1] += nbD
	nb[i+stride+1] += nbD
	if s.causal && y&3 == 0 {
		return
	}
	nb[i-stride] += nbV
	nb[i-stride-1] += nbD
	nb[i-stride+1] += nbD
}

// signContext returns the sign context offset and the XOR bit for the
// coefficient at state index i in row y.
func (s *blockState) signContext(i, y int) (ctx, xor uint8) {
	st := s.st
	hc := contribution(signState(st[i-1]), signState(st[i+1]))
	dn := 0
	if !s.causal || y&3 != 3 {
		dn = signState(st[i+s.stride])
	}
	vc := contribution(signState(st[i-s.stride]), dn)
	e := &scTable[hc+1][vc+1]
	return e.ctx, e.xor
}

// runLengthApplies reports whether the four coefficients of the column
// starting at state index i are all insignificant, unvisited and without
// significant neighbours, so that they are coded in run-length mode.
func (s *blockState) runLengthApplies(i int) bool {
	st, nb, stride := s.st, s.nb, s.stride
	return st[i]&(stSig|stVisited) == 0 && nb[i] == 0 &&
		st[i+stride]&(stSig|stVisited) == 0 && nb[i+stride] == 0 &&
		st[i+2*stride]&(stSig|stVisited) == 0 && nb[i+2*stride] == 0 &&
		st[i+3*stride]&(stSig|stVisited) == 0 && nb[i+3*stride] == 0
}

// signState converts a neighbour's state into -1, 0 or +1.
func signState(s uint8) int {
	if s&stSig == 0 {
		return 0
	}
	if s&stNeg != 0 {
		return -1
	}
	return 1
}
