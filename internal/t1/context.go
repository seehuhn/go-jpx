// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package t1

import "seehuhn.de/go/jpx/internal/mq"

// Band identifies the orientation of the sub-band a code-block belongs
// to.  The orientation selects the context formation rules for
// significance coding.
type Band uint8

// The four sub-band orientations.
const (
	BandLL Band = iota
	BandHL
	BandLH
	BandHH
)

// Style holds the code-block coding style flags from the COD or COC
// marker segment.
type Style struct {
	Bypass     bool // selective arithmetic coding bypass
	Reset      bool // reset context probabilities after each pass
	Terminate  bool // terminate the coder after each pass
	Causal     bool // vertically causal context formation
	SegSymbols bool // segmentation symbol after each cleanup pass

	// Predictable requests predictable termination.  The encoder always
	// terminates raw passes predictably and uses the standard flush for
	// arithmetic passes; the decoder is unaffected by the flag.
	Predictable bool
}

// Pass types within a bit-plane.
const (
	passSig   = iota // significance propagation
	passRef          // magnitude refinement
	passClean        // cleanup
)

// passType returns the type of pass number i.  Pass 0 is the cleanup
// pass of the first bit-plane.
func passType(i int) int {
	if i == 0 {
		return passClean
	}
	return (i - 1) % 3
}

// firstRawPass is the number of the first pass coded without the
// arithmetic coder when bypass is selected: the significance pass of the
// fifth bit-plane.
const firstRawPass = 10

// IsRaw reports whether pass i is coded without the arithmetic coder.
func IsRaw(i int, s Style) bool {
	return s.Bypass && i >= firstRawPass && passType(i) != passClean
}

// IsTerminated reports whether the codeword segment ends after pass i.
// The final pass of a block is always terminated, which the caller has to
// account for separately.
func IsTerminated(i int, s Style) bool {
	if s.Terminate {
		return true
	}
	if s.Bypass && i >= firstRawPass-1 {
		return passType(i) != passSig
	}
	return false
}

// Context labels.  The numbering is internal; only the grouping matters.
const (
	cxZC   = 0  // 9 significance contexts
	cxSC   = 9  // 5 sign contexts
	cxMR   = 14 // 3 magnitude refinement contexts
	cxRL   = 17 // run-length context
	cxUNI  = 18 // uniform context
	numCtx = 19
)

type contexts [numCtx]mq.Context

// reset puts all contexts into their initial states.
func (c *contexts) reset() {
	for i := range c {
		c[i].Reset(0, 0)
	}
	c[cxZC].Reset(4, 0)
	c[cxRL].Reset(3, 0)
	c[cxUNI].Reset(46, 0)
}

// Neighbourhood summary used for context formation.  h, v and d count
// the significant horizontal, vertical and diagonal neighbours.
func zcIndex(h, v, d int) int { return h + 3*v + 9*d }

// zcTable maps the neighbourhood summary to a significance context for
// each band orientation.  Index: zcTable[band][zcIndex(h, v, d)].
var zcTable [4][45]uint8

func init() {
	for h := 0; h <= 2; h++ {
		for v := 0; v <= 2; v++ {
			for d := 0; d <= 4; d++ {
				i := zcIndex(h, v, d)
				zcTable[BandLL][i] = zcContextLL(h, v, d)
				zcTable[BandLH][i] = zcContextLL(h, v, d)
				zcTable[BandHL][i] = zcContextLL(v, h, d)
				zcTable[BandHH][i] = zcContextHH(h+v, d)
			}
		}
	}
}

// zcContextLL gives the significance context for the LL and LH bands,
// where horizontal neighbours matter most.
func zcContextLL(h, v, d int) uint8 {
	switch {
	case h == 2:
		return 8
	case h == 1:
		switch {
		case v >= 1:
			return 7
		case d >= 1:
			return 6
		default:
			return 5
		}
	case v == 2:
		return 4
	case v == 1:
		return 3
	case d >= 2:
		return 2
	case d == 1:
		return 1
	default:
		return 0
	}
}

// zcContextHH gives the significance context for the HH band, where the
// diagonal neighbours matter most.
func zcContextHH(hv, d int) uint8 {
	switch {
	case d >= 3:
		return 8
	case d == 2:
		if hv >= 1 {
			return 7
		}
		return 6
	case d == 1:
		switch {
		case hv >= 2:
			return 5
		case hv == 1:
			return 4
		default:
			return 3
		}
	default:
		switch {
		case hv >= 2:
			return 2
		case hv == 1:
			return 1
		default:
			return 0
		}
	}
}

// Sign coding: the horizontal and vertical contributions are each in
// {-1, 0, 1}; the table gives the context offset within the sign contexts
// and the bit the decoded symbol is XORed with.
var scTable [3][3]struct {
	ctx uint8
	xor uint8
}

func init() {
	for hc := -1; hc <= 1; hc++ {
		for vc := -1; vc <= 1; vc++ {
			h, v, xor := hc, vc, uint8(0)
			if h < 0 || (h == 0 && v < 0) {
				h, v, xor = -h, -v, 1
			}
			// After normalisation h is 0 or 1 and, for h == 0, v is 0 or 1.
			// The five contexts are (h,v) = (0,0), (0,1), (1,-1), (1,0), (1,1).
			var ctx uint8
			if h == 1 {
				ctx = uint8(3 + v)
			} else {
				ctx = uint8(v)
			}
			scTable[hc+1][vc+1].ctx = ctx
			scTable[hc+1][vc+1].xor = xor
		}
	}
}

// contribution reduces the states of two opposite neighbours to the sign
// contribution -1, 0 or 1.  Each argument is 0 for an insignificant
// neighbour, +1 for a significant positive one and -1 for a significant
// negative one.
func contribution(a, b int) int {
	return max(-1, min(1, a+b))
}
