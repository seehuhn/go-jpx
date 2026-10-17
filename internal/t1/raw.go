// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package t1

// rawReader reads the bits of a bypass-coded pass.  Bits are packed MSB
// first; after a 0xFF byte the next byte carries only seven bits.  Past
// the end of the data the reader behaves as if 0xFF bytes followed, which
// is what the decoder is required to assume.
type rawReader struct {
	data []byte
	pos  int
	cur  byte
	left int
	prev byte
}

func (r *rawReader) init(data []byte) {
	r.data = data
	r.pos = 0
	r.left = 0
	r.prev = 0
}

func (r *rawReader) readBit() int {
	if r.left == 0 {
		b := byte(0xFF)
		if r.pos < len(r.data) {
			b = r.data[r.pos]
			r.pos++
		}
		r.left = 8
		if r.prev == 0xFF {
			r.left = 7
		}
		r.cur = b
		r.prev = b
	}
	r.left--
	return int(r.cur>>uint(r.left)) & 1
}

// rawWriter is the encoder counterpart of rawReader.
type rawWriter struct {
	buf  []byte
	cur  byte
	free int // bit positions still free in cur; 0 means cur is not started
}

func (w *rawWriter) reset() {
	w.buf = w.buf[:0]
	w.cur = 0
	w.free = 0
}

func (w *rawWriter) writeBit(b int) {
	if w.free == 0 {
		w.free = 8
		if n := len(w.buf); n > 0 && w.buf[n-1] == 0xFF {
			w.free = 7
		}
		w.cur = 0
	}
	w.free--
	if b != 0 {
		w.cur |= 1 << uint(w.free)
	}
	if w.free == 0 {
		w.buf = append(w.buf, w.cur)
	}
}

// flush terminates the raw segment.  The unused bits of the last byte are
// filled with an alternating 0,1,0,1… sequence, as predictable
// termination prescribes; this is also a valid termination otherwise.  A
// final 0xFF byte is dropped: the decoder supplies 0xFF bytes past the
// end of a segment anyway.
func (w *rawWriter) flush() []byte {
	if w.free > 0 {
		fill := 0
		for w.free > 0 {
			w.writeBit(fill)
			fill ^= 1
		}
	}
	if n := len(w.buf); n > 0 && w.buf[n-1] == 0xFF {
		w.buf = w.buf[:n-1]
	}
	return w.buf
}
