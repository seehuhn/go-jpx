// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package t2

import (
	"errors"
	"io"
)

// BitWriter packs bits MSB first into bytes.  After a 0xFF byte the next
// byte carries only seven bits, its top bit being a stuffed zero.
type BitWriter struct {
	buf  []byte
	cur  byte
	free int // unused bit positions in cur; 0 means cur is not started
}

func (w *BitWriter) capacity() int {
	if n := len(w.buf); n > 0 && w.buf[n-1] == 0xFF {
		return 7
	}
	return 8
}

// WriteBit appends a single bit (any non-zero value counts as 1).
func (w *BitWriter) WriteBit(b int) {
	if w.free == 0 {
		w.free = w.capacity()
		w.cur = 0
	}
	w.free--
	if b != 0 {
		w.cur |= 1 << w.free
	}
	if w.free == 0 {
		w.buf = append(w.buf, w.cur)
	}
}

// WriteBits appends the n low bits of v, most significant first.
func (w *BitWriter) WriteBits(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		w.WriteBit(int(v>>uint(i)) & 1)
	}
}

// Flush pads the current byte with zero bits and returns all bytes written
// so far.  If the last byte is 0xFF, an extra zero byte is appended so that
// the data never ends in 0xFF.  The returned slice aliases the writer's
// buffer and is valid until the next Reset; the writer must be Reset
// before it is used again.
func (w *BitWriter) Flush() []byte {
	if w.free > 0 {
		w.buf = append(w.buf, w.cur)
		w.free = 0
	}
	if n := len(w.buf); n > 0 && w.buf[n-1] == 0xFF {
		w.buf = append(w.buf, 0)
	}
	return w.buf
}

// Reset discards all written data.
func (w *BitWriter) Reset() {
	w.buf = w.buf[:0]
	w.cur = 0
	w.free = 0
}

// BitReader reads bits written by BitWriter.
type BitReader struct {
	data []byte
	pos  int // index of the next byte to load
	cur  byte
	left int // unread bits in cur
}

// NewBitReader returns a reader over data.
func NewBitReader(data []byte) *BitReader {
	return &BitReader{data: data}
}

// ReadBit returns the next bit, or io.ErrUnexpectedEOF at the end of data.
func (r *BitReader) ReadBit() (int, error) {
	if r.left == 0 {
		if r.pos >= len(r.data) {
			return 0, io.ErrUnexpectedEOF
		}
		r.left = 8
		if r.pos > 0 && r.data[r.pos-1] == 0xFF {
			r.left = 7
		}
		r.cur = r.data[r.pos]
		r.pos++
	}
	r.left--
	return int(r.cur>>uint(r.left)) & 1, nil
}

// ReadBits reads n bits (0 ≤ n ≤ 32), most significant first.
func (r *BitReader) ReadBits(n int) (uint32, error) {
	if n < 0 || n > 32 {
		return 0, errors.New("t2: bit count out of range")
	}
	var v uint32
	for range n {
		b, err := r.ReadBit()
		if err != nil {
			return 0, err
		}
		v = v<<1 | uint32(b)
	}
	return v, nil
}

// Align finishes the current byte, skips the stuffed byte following a
// trailing 0xFF, and returns the number of bytes consumed so far.  The
// reader does not check that the stuffed bit is actually zero; corrupt
// headers are tolerated as far as possible.
func (r *BitReader) Align() int {
	r.left = 0
	if r.pos > 0 && r.data[r.pos-1] == 0xFF && r.pos < len(r.data) {
		r.pos++
	}
	return r.pos
}
