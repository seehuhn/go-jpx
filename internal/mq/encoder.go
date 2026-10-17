// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package mq

// Encoder produces one codeword segment from binary decisions.
type Encoder struct {
	// buf[0] is a dummy byte standing in for the byte before the segment;
	// the last element of buf is the byte addressed by BP.
	buf []byte
	c   uint32 // C: code register
	a   uint32 // A: interval register
	ct  uint32 // CT: number of shifts left before the next byte is output
}

// NewEncoder returns an encoder ready to code a new segment.
func NewEncoder() *Encoder {
	e := &Encoder{}
	e.Reset()
	return e
}

// Reset starts a new codeword segment and keeps the buffer capacity.
func (e *Encoder) Reset() {
	e.buf = append(e.buf[:0], 0)
	e.a = 0x8000
	e.c = 0
	e.ct = 12
}

// Len returns the number of bytes emitted so far, without flushing.
func (e *Encoder) Len() int {
	return len(e.buf) - 1
}

// Encode codes one binary decision in the given context and updates it.
// bit must be 0 or 1.
func (e *Encoder) Encode(cx *Context, bit int) {
	st := &table[cx.index]
	qe := st.qe
	e.a -= qe
	if uint8(bit) == cx.mps {
		// CODEMPS
		if e.a&0x8000 != 0 {
			e.c += qe
			return
		}
		if e.a < qe {
			e.a = qe
		} else {
			e.c += qe
		}
		cx.index = st.nmps
	} else {
		// CODELPS
		if e.a < qe {
			e.c += qe
		} else {
			e.a = qe
		}
		cx.mps ^= st.sw
		cx.index = st.nlps
	}
	// RENORME
	for {
		e.a <<= 1
		e.c <<= 1
		e.ct--
		if e.ct == 0 {
			e.byteOut()
		}
		if e.a&0x8000 != 0 {
			break
		}
	}
}

// byteOut moves a byte from C to the output, with carry handling and bit
// stuffing after 0xFF.
func (e *Encoder) byteOut() {
	last := len(e.buf) - 1
	if e.buf[last] == 0xFF {
		e.buf = append(e.buf, byte(e.c>>20))
		e.c &= 0xFFFFF
		e.ct = 7
		return
	}
	if e.c < 0x8000000 {
		e.buf = append(e.buf, byte(e.c>>19))
		e.c &= 0x7FFFF
		e.ct = 8
		return
	}
	e.buf[last]++
	if e.buf[last] == 0xFF {
		e.c &= 0x7FFFFFF
		e.buf = append(e.buf, byte(e.c>>20))
		e.c &= 0xFFFFF
		e.ct = 7
		return
	}
	e.buf = append(e.buf, byte(e.c>>19))
	e.c &= 0x7FFFF
	e.ct = 8
}

// Flush terminates the codeword with the standard FLUSH procedure and
// returns the bytes.  The returned slice is valid until the next Reset.
// After Flush the encoder must be Reset before further use.
func (e *Encoder) Flush() []byte {
	// SETBITS
	tempC := e.c + e.a
	e.c |= 0xFFFF
	if e.c >= tempC {
		e.c -= 0x8000
	}
	e.c <<= e.ct
	e.byteOut()
	e.c <<= e.ct
	e.byteOut()
	if e.buf[len(e.buf)-1] == 0xFF {
		e.buf = e.buf[:len(e.buf)-1]
	}
	return e.buf[1:]
}
