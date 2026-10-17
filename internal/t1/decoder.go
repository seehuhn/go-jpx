// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package t1

import (
	"errors"

	"seehuhn.de/go/jpx/internal/mq"
)

// MaxPlanes is the largest number of bit-planes a code-block may have.
// Together with the fractional bit this fills the 32-bit coefficient
// representation.
const MaxPlanes = 30

// MaxBlockSize is the largest code-block width or height, and
// MaxBlockArea the largest number of coefficients in a block.  Both
// follow from the limits on the code-block size exponents in the
// codestream syntax.
const (
	MaxBlockSize = 1 << 10
	MaxBlockArea = 1 << 12
)

// validGeometry reports whether w×h is an acceptable block size.  The
// working buffers of a coder are therefore bounded by a small constant
// and need no budget accounting.
func validGeometry(w, h int) bool {
	return w > 0 && h > 0 && w <= MaxBlockSize && h <= MaxBlockSize && w*h <= MaxBlockArea
}

// Segment is one codeword segment of a code-block: the coded bytes and
// the number of coding passes they contain.  Segments are terminated
// either by the coding style or because they are the last one received.
type Segment struct {
	Data   []byte
	Passes int
}

// Per-coefficient state bits.
const (
	stSig     = 1 << iota // coefficient is significant
	stNeg                 // coefficient is negative
	stVisited             // coded in the significance pass of the current plane
	stRefined             // has been refined at least once
)

// Decoder decodes code-blocks.  It keeps its working buffers between
// calls so that decoding many blocks does not allocate.
type Decoder struct {
	w, h   int
	stride int     // w + 2, the state array has a one-coefficient border
	state  []uint8 // (w+2)*(h+2)
	mag    []uint32
	ctx    contexts
	mq     mq.Decoder
	raw    rawReader
	band   Band
	causal bool
}

// NewDecoder returns a decoder with empty buffers.
func NewDecoder() *Decoder {
	return &Decoder{}
}

// Decode decodes one code-block of size w×h whose coefficients have
// planes magnitude bit-planes, using the passes contained in segs.  The
// result is written to out in row-major order with stride w, in the
// fractional-bit representation described in the package documentation.
// Fewer passes than needed for all planes leave the remaining planes
// undecoded; the fractional bit then marks the reconstruction mid-point.
func (d *Decoder) Decode(out []int32, w, h int, band Band, style Style, planes int, segs []Segment) error {
	if !validGeometry(w, h) || len(out) < w*h {
		return errors.New("t1: invalid code-block geometry")
	}
	if planes > MaxPlanes {
		return errors.New("t1: too many bit-planes")
	}
	d.setup(w, h, band, style.Causal)

	if planes <= 0 {
		clear(out[:w*h])
		return nil
	}
	maxPasses := 3*planes - 2
	plane := planes - 1
	pass := 0
	for _, seg := range segs {
		isRaw := IsRaw(pass, style)
		if isRaw {
			d.raw.init(seg.Data)
		} else {
			d.mq.Init(seg.Data)
		}
		for range seg.Passes {
			if pass >= maxPasses {
				break
			}
			if IsRaw(pass, style) != isRaw {
				// a segment must not mix raw and arithmetic passes
				return errors.New("t1: inconsistent codeword segment")
			}
			switch passType(pass) {
			case passSig:
				if isRaw {
					d.sigPassRaw(plane)
				} else {
					d.sigPass(plane)
				}
			case passRef:
				if isRaw {
					d.refPassRaw(plane)
				} else {
					d.refPass(plane)
				}
			case passClean:
				d.cleanPass(plane)
				if style.SegSymbols {
					d.decodeSegSymbol()
				}
				plane--
			}
			if style.Reset {
				d.ctx.reset()
			}
			pass++
		}
	}

	for y := range h {
		row := d.state[(y+1)*d.stride+1:]
		for x := range w {
			v := int32(d.mag[y*w+x])
			if row[x]&stNeg != 0 {
				v = -v
			}
			out[y*w+x] = v
		}
	}
	return nil
}

func (d *Decoder) setup(w, h int, band Band, causal bool) {
	d.w, d.h, d.band, d.causal = w, h, band, causal
	d.stride = w + 2
	n := d.stride * (h + 2)
	if cap(d.state) < n {
		d.state = make([]uint8, n)
	} else {
		d.state = d.state[:n]
		clear(d.state)
	}
	if cap(d.mag) < w*h {
		d.mag = make([]uint32, w*h)
	} else {
		d.mag = d.mag[:w*h]
		clear(d.mag)
	}
	d.ctx.reset()
}

// zcLabel computes the significance context label from the eight
// neighbours of state index i.
func zcLabel(st []uint8, i, stride int, band Band, below bool) uint8 {
	h := int(st[i-1]&stSig) + int(st[i+1]&stSig)
	up := i - stride
	v := int(st[up] & stSig)
	dg := int(st[up-1]&stSig) + int(st[up+1]&stSig)
	if below {
		dn := i + stride
		v += int(st[dn] & stSig)
		dg += int(st[dn-1]&stSig) + int(st[dn+1]&stSig)
	}
	return zcTable[band][zcIndex(h, v, dg)]
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

// decodeSign decodes the sign of the coefficient at state index i and
// returns the state bits to set (stSig, possibly with stNeg).
func (d *Decoder) decodeSign(i int, below bool) uint8 {
	st := d.state
	hc := contribution(signState(st[i-1]), signState(st[i+1]))
	dn := 0
	if below {
		dn = signState(st[i+d.stride])
	}
	vc := contribution(signState(st[i-d.stride]), dn)
	e := &scTable[hc+1][vc+1]
	bit := d.mq.Decode(&d.ctx[cxSC+e.ctx]) ^ int(e.xor)
	if bit != 0 {
		return stSig | stNeg
	}
	return stSig
}

// useBelow reports whether the row below row y takes part in context
// formation.
func (d *Decoder) useBelow(y int) bool {
	return !d.causal || y&3 != 3
}

// sigPass is the significance propagation pass for bit-plane p.
func (d *Decoder) sigPass(p int) {
	w, h, st := d.w, d.h, d.state
	for y0 := 0; y0 < h; y0 += 4 {
		y1 := min(y0+4, h)
		for x := range w {
			for y := y0; y < y1; y++ {
				i := (y+1)*d.stride + x + 1
				if st[i]&stSig != 0 {
					continue
				}
				below := d.useBelow(y)
				label := zcLabel(st, i, d.stride, d.band, below)
				if label == 0 {
					continue
				}
				if d.mq.Decode(&d.ctx[cxZC+label]) != 0 {
					st[i] |= d.decodeSign(i, below)
					d.mag[y*w+x] = 3 << uint(p)
				}
				st[i] |= stVisited
			}
		}
	}
}

// sigPassRaw is the significance propagation pass in bypass mode.
func (d *Decoder) sigPassRaw(p int) {
	w, h, st := d.w, d.h, d.state
	for y0 := 0; y0 < h; y0 += 4 {
		y1 := min(y0+4, h)
		for x := range w {
			for y := y0; y < y1; y++ {
				i := (y+1)*d.stride + x + 1
				if st[i]&stSig != 0 {
					continue
				}
				if zcLabel(st, i, d.stride, d.band, d.useBelow(y)) == 0 {
					continue
				}
				if d.raw.readBit() != 0 {
					st[i] |= stSig
					if d.raw.readBit() != 0 {
						st[i] |= stNeg
					}
					d.mag[y*w+x] = 3 << uint(p)
				}
				st[i] |= stVisited
			}
		}
	}
}

// refine updates the magnitude of coefficient k with refinement bit b at
// bit-plane p and moves the mid-point marker down one plane.
func (d *Decoder) refine(k, p, b int) {
	m := d.mag[k]
	m &^= 1 << uint(p+1)
	m |= uint32(b) << uint(p+1)
	m |= 1 << uint(p)
	d.mag[k] = m
}

// refPass is the magnitude refinement pass for bit-plane p.
func (d *Decoder) refPass(p int) {
	w, h, st := d.w, d.h, d.state
	for y0 := 0; y0 < h; y0 += 4 {
		y1 := min(y0+4, h)
		for x := range w {
			for y := y0; y < y1; y++ {
				i := (y+1)*d.stride + x + 1
				if st[i]&(stSig|stVisited) != stSig {
					continue
				}
				var cx *mq.Context
				if st[i]&stRefined != 0 {
					cx = &d.ctx[cxMR+2]
				} else if zcLabel(st, i, d.stride, d.band, d.useBelow(y)) != 0 {
					cx = &d.ctx[cxMR+1]
				} else {
					cx = &d.ctx[cxMR]
				}
				d.refine(y*w+x, p, d.mq.Decode(cx))
				st[i] |= stRefined
			}
		}
	}
}

// refPassRaw is the magnitude refinement pass in bypass mode.
func (d *Decoder) refPassRaw(p int) {
	w, h, st := d.w, d.h, d.state
	for y0 := 0; y0 < h; y0 += 4 {
		y1 := min(y0+4, h)
		for x := range w {
			for y := y0; y < y1; y++ {
				i := (y+1)*d.stride + x + 1
				if st[i]&(stSig|stVisited) != stSig {
					continue
				}
				d.refine(y*w+x, p, d.raw.readBit())
				st[i] |= stRefined
			}
		}
	}
}

// cleanPass is the cleanup pass for bit-plane p.  It also clears the
// visited marks left by the significance pass.
func (d *Decoder) cleanPass(p int) {
	w, h, st := d.w, d.h, d.state
	for y0 := 0; y0 < h; y0 += 4 {
		y1 := min(y0+4, h)
		for x := range w {
			y := y0
			if y1-y0 == 4 && d.runLengthApplies(x, y0) {
				if d.mq.Decode(&d.ctx[cxRL]) == 0 {
					continue
				}
				k := d.mq.Decode(&d.ctx[cxUNI]) << 1
				k |= d.mq.Decode(&d.ctx[cxUNI])
				y = y0 + k
				i := (y+1)*d.stride + x + 1
				st[i] |= d.decodeSign(i, d.useBelow(y))
				d.mag[y*w+x] = 3 << uint(p)
				y++
			}
			for ; y < y1; y++ {
				i := (y+1)*d.stride + x + 1
				if st[i]&stVisited != 0 {
					st[i] &^= stVisited
					continue
				}
				if st[i]&stSig != 0 {
					continue
				}
				below := d.useBelow(y)
				label := zcLabel(st, i, d.stride, d.band, below)
				if d.mq.Decode(&d.ctx[cxZC+label]) != 0 {
					st[i] |= d.decodeSign(i, below)
					d.mag[y*w+x] = 3 << uint(p)
				}
			}
		}
	}
}

// runLengthApplies reports whether the four coefficients of the column
// starting at (x, y0) are all insignificant, unvisited and without
// significant neighbours, so that they are coded in run-length mode.
func (d *Decoder) runLengthApplies(x, y0 int) bool {
	st := d.state
	for y := y0; y < y0+4; y++ {
		i := (y+1)*d.stride + x + 1
		if st[i]&(stSig|stVisited) != 0 {
			return false
		}
		if zcLabel(st, i, d.stride, d.band, d.useBelow(y)) != 0 {
			return false
		}
	}
	return true
}

// decodeSegSymbol consumes the four-bit segmentation symbol.  A wrong
// symbol indicates corrupt data, which the decoder tolerates.
func (d *Decoder) decodeSegSymbol() {
	for range 4 {
		d.mq.Decode(&d.ctx[cxUNI])
	}
}
