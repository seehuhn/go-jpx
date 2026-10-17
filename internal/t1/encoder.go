// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package t1

import (
	"errors"
	"math/bits"

	"seehuhn.de/go/jpx/internal/mq"
)

// PassInfo describes one coding pass of an encoded block.
type PassInfo struct {
	// Len is a number of bytes from the start of the block's data which
	// is sufficient to decode this pass and all earlier ones.  For passes
	// ending a codeword segment this is exact; for the others it is a safe
	// upper bound.  Len is non-decreasing from pass to pass and never
	// points just past a 0xFF byte.
	Len int

	// Dist is the reduction in squared error, summed over the block's
	// coefficients in quantised units, obtained by decoding this pass.
	// It assumes mid-point reconstruction of undecoded planes and exact
	// reconstruction once all planes are decoded, which is what the
	// reversible path does; the irreversible path differs by at most a
	// quarter per significant coefficient on the last plane.  Individual
	// passes can make the reconstruction slightly worse, so the value may
	// be negative.
	Dist float64

	// Terminated reports whether a codeword segment ends with this pass.
	Terminated bool
}

// EncodedBlock is the result of encoding a code-block.
type EncodedBlock struct {
	ZeroPlanes int        // number of all-zero most significant planes
	Passes     []PassInfo // one entry per coding pass
	Data       []byte     // all codeword segments concatenated
}

// Encoder encodes code-blocks.  It keeps its working buffers between
// calls.
type Encoder struct {
	blockState
	mag []uint32 // coefficient magnitudes
	ctx contexts
	mq  *mq.Encoder
	raw rawWriter

	// per-call results
	data   []byte
	passes []PassInfo
	dist   float64
}

// NewEncoder returns an encoder with empty buffers.
func NewEncoder() *Encoder {
	return &Encoder{mq: mq.NewEncoder()}
}

// Encode codes the w×h block of quantised coefficients in, given in
// row-major order with stride w as ordinary signed integers.  planes is
// the number of magnitude bit-planes the block is allowed to have; every
// |in[k]| must be below 2^planes.  The returned structure is valid until
// the next call.
func (e *Encoder) Encode(in []int32, w, h int, band Band, style Style, planes int) (*EncodedBlock, error) {
	if !validGeometry(w, h) || len(in) < w*h {
		return nil, errors.New("t1: invalid code-block geometry")
	}
	if planes > MaxPlanes {
		return nil, errors.New("t1: too many bit-planes")
	}
	e.setup(w, h, band, style.Causal)
	if cap(e.mag) < w*h {
		e.mag = make([]uint32, w*h)
	} else {
		e.mag = e.mag[:w*h]
	}
	e.ctx.reset()

	var maxMag uint32
	for y := range h {
		row := e.st[(y+1)*e.stride+1:]
		for x := range w {
			v := in[y*w+x]
			if v < 0 {
				v = -v
				row[x] = stNeg
			}
			e.mag[y*w+x] = uint32(v)
			maxMag |= uint32(v)
		}
	}
	coded := bits.Len32(maxMag)
	if coded > planes {
		return nil, errors.New("t1: coefficient exceeds the number of bit-planes")
	}
	res := &EncodedBlock{ZeroPlanes: planes - coded}
	e.data = e.data[:0]
	e.passes = e.passes[:0]
	if coded == 0 {
		res.Data = e.data
		res.Passes = e.passes
		return res, nil
	}

	numPasses := 3*coded - 2
	e.mq.Reset()
	e.raw.reset()
	plane := coded - 1
	segStart := 0 // index into e.data where the current segment begins
	for pass := range numPasses {
		isRaw := IsRaw(pass, style)
		e.dist = 0
		switch passType(pass) {
		case passSig:
			if isRaw {
				e.sigPassRaw(plane)
			} else {
				e.sigPass(plane)
			}
		case passRef:
			if isRaw {
				e.refPassRaw(plane)
			} else {
				e.refPass(plane)
			}
		case passClean:
			e.cleanPass(plane)
			if style.SegSymbols {
				e.encodeSegSymbol()
			}
			plane--
		}
		if style.Reset {
			e.ctx.reset()
		}

		info := PassInfo{Dist: e.dist}
		last := pass == numPasses-1
		if IsTerminated(pass, style) || last {
			var seg []byte
			if isRaw {
				seg = e.raw.flush()
				e.raw.reset()
			} else {
				seg = e.mq.Flush()
				e.mq.Reset()
			}
			e.data = append(e.data, seg...)
			info.Len = len(e.data)
			info.Terminated = true
			segStart = len(e.data)
		} else {
			info.Len = segStart + e.pendingLen(isRaw)
		}
		e.passes = append(e.passes, info)
	}

	// Lengths of unterminated passes are estimates and may overshoot the
	// end of their segment; they must also never end right after a 0xFF
	// byte, and must not decrease.
	segEnd := 0
	for i := len(e.passes) - 1; i >= 0; i-- {
		p := &e.passes[i]
		if p.Terminated {
			segEnd = p.Len
			continue
		}
		p.Len = min(p.Len, segEnd)
		for p.Len < segEnd && e.data[p.Len-1] == 0xFF {
			p.Len++
		}
	}
	for i := 1; i < len(e.passes); i++ {
		e.passes[i].Len = max(e.passes[i].Len, e.passes[i-1].Len)
	}
	res.Passes = e.passes
	res.Data = e.data
	return res, nil
}

// pendingLen returns a safe upper bound for the number of bytes of the
// current segment needed to decode everything coded so far.
func (e *Encoder) pendingLen(isRaw bool) int {
	if isRaw {
		// the current byte plus a possible stuffed byte
		return len(e.raw.buf) + 2
	}
	// The MQ encoder holds one byte in its buffer register and up to 27
	// bits in the code register.  Five more bytes cover all of them.
	return e.mq.Len() + 5
}

// bit returns bit p of the magnitude of coefficient k.
func (e *Encoder) bit(k, p int) int {
	return int(e.mag[k]>>uint(p)) & 1
}

// reconstruction is the value a decoder reconstructs for magnitude m
// once the planes down to p are known: the known bits plus half of the
// last known plane, except that the final plane is reconstructed exactly.
func reconstruction(m uint32, p int) float64 {
	known := float64(m >> uint(p) << uint(p))
	if p == 0 {
		return known
	}
	return known + 0.5*float64(uint32(1)<<uint(p))
}

// sigDist is the distortion reduction from coding coefficient k as
// significant at plane p.
func (e *Encoder) sigDist(k, p int) float64 {
	m := float64(e.mag[k])
	rec := reconstruction(e.mag[k], p)
	return m*m - (m-rec)*(m-rec)
}

// refDist is the distortion reduction from refining coefficient k at
// plane p.
func (e *Encoder) refDist(k, p int) float64 {
	m := e.mag[k]
	before := reconstruction(m, p+1)
	after := reconstruction(m, p)
	fm := float64(m)
	return (fm-before)*(fm-before) - (fm-after)*(fm-after)
}

// becomeSignificant codes the sign of the coefficient at state index i
// in row y and marks it significant at plane p.
func (e *Encoder) becomeSignificant(i, y, k, p int) {
	neg := e.st[i]&stNeg != 0
	ctx, xor := e.signContext(i, y)
	bit := 0
	if neg {
		bit = 1
	}
	e.mq.Encode(&e.ctx[cxSC+ctx], bit^int(xor))
	e.setSignificant(i, y, neg)
	e.dist += e.sigDist(k, p)
}

func (e *Encoder) sigPass(p int) {
	w, h, st, nb := e.w, e.h, e.st, e.nb
	for y0 := 0; y0 < h; y0 += 4 {
		y1 := min(y0+4, h)
		for x := range w {
			for y := y0; y < y1; y++ {
				i := e.index(x, y)
				if st[i]&stSig != 0 || nb[i] == 0 {
					continue
				}
				k := y*w + x
				b := e.bit(k, p)
				e.mq.Encode(&e.ctx[cxZC+e.lut[nb[i]]], b)
				if b != 0 {
					e.becomeSignificant(i, y, k, p)
				}
				st[i] |= stVisited
			}
		}
	}
}

func (e *Encoder) sigPassRaw(p int) {
	w, h, st, nb := e.w, e.h, e.st, e.nb
	for y0 := 0; y0 < h; y0 += 4 {
		y1 := min(y0+4, h)
		for x := range w {
			for y := y0; y < y1; y++ {
				i := e.index(x, y)
				if st[i]&stSig != 0 || nb[i] == 0 {
					continue
				}
				k := y*w + x
				b := e.bit(k, p)
				e.raw.writeBit(b)
				if b != 0 {
					neg := st[i]&stNeg != 0
					e.raw.writeBit(int(st[i]&stNeg) >> 1)
					e.setSignificant(i, y, neg)
					e.dist += e.sigDist(k, p)
				}
				st[i] |= stVisited
			}
		}
	}
}

func (e *Encoder) refPass(p int) {
	w, h, st, nb := e.w, e.h, e.st, e.nb
	for y0 := 0; y0 < h; y0 += 4 {
		y1 := min(y0+4, h)
		for x := range w {
			for y := y0; y < y1; y++ {
				i := e.index(x, y)
				if st[i]&(stSig|stVisited) != stSig {
					continue
				}
				var cx *mq.Context
				if st[i]&stRefined != 0 {
					cx = &e.ctx[cxMR+2]
				} else if nb[i] != 0 {
					cx = &e.ctx[cxMR+1]
				} else {
					cx = &e.ctx[cxMR]
				}
				k := y*w + x
				e.mq.Encode(cx, e.bit(k, p))
				e.dist += e.refDist(k, p)
				st[i] |= stRefined
			}
		}
	}
}

func (e *Encoder) refPassRaw(p int) {
	w, h, st := e.w, e.h, e.st
	for y0 := 0; y0 < h; y0 += 4 {
		y1 := min(y0+4, h)
		for x := range w {
			for y := y0; y < y1; y++ {
				i := e.index(x, y)
				if st[i]&(stSig|stVisited) != stSig {
					continue
				}
				k := y*w + x
				e.raw.writeBit(e.bit(k, p))
				e.dist += e.refDist(k, p)
				st[i] |= stRefined
			}
		}
	}
}

func (e *Encoder) cleanPass(p int) {
	w, h, st, nb := e.w, e.h, e.st, e.nb
	for y0 := 0; y0 < h; y0 += 4 {
		y1 := min(y0+4, h)
		for x := range w {
			y := y0
			i := e.index(x, y0)
			if y1-y0 == 4 && e.runLengthApplies(i) {
				// find the first coefficient of the column with bit p set
				k := 0
				for k < 4 && e.bit((y0+k)*w+x, p) == 0 {
					k++
				}
				if k == 4 {
					e.mq.Encode(&e.ctx[cxRL], 0)
					continue
				}
				e.mq.Encode(&e.ctx[cxRL], 1)
				e.mq.Encode(&e.ctx[cxUNI], k>>1)
				e.mq.Encode(&e.ctx[cxUNI], k&1)
				y = y0 + k
				i += k * e.stride
				e.becomeSignificant(i, y, y*w+x, p)
				y++
				i += e.stride
			}
			for ; y < y1; y, i = y+1, i+e.stride {
				if st[i]&stVisited != 0 {
					st[i] &^= stVisited
					continue
				}
				if st[i]&stSig != 0 {
					continue
				}
				k := y*w + x
				b := e.bit(k, p)
				e.mq.Encode(&e.ctx[cxZC+e.lut[nb[i]]], b)
				if b != 0 {
					e.becomeSignificant(i, y, k, p)
				}
			}
		}
	}
}

// encodeSegSymbol codes the segmentation symbol 1010.
func (e *Encoder) encodeSegSymbol() {
	for _, b := range [...]int{1, 0, 1, 0} {
		e.mq.Encode(&e.ctx[cxUNI], b)
	}
}
