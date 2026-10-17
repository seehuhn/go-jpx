// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package mq

// Decoder decodes binary decisions from one codeword segment.  Init or
// NewDecoder must be called before Decode.
type Decoder struct {
	data []byte
	bp   int    // BP: index of the current byte
	c    uint32 // C: Chigh in the upper 16 bits, Clow below
	a    uint32 // A: interval register
	ct   uint32 // CT: number of shifts left before the next byte is needed
}

// NewDecoder starts decoding the given codeword segment.  Reading past the
// end of data behaves as if the data were followed by 0xFF 0xFF, so a
// truncated segment decodes to well-defined bits.
func NewDecoder(data []byte) *Decoder {
	d := &Decoder{}
	d.Init(data)
	return d
}

// Init restarts the decoder on a new segment without allocating.
func (d *Decoder) Init(data []byte) {
	d.data = data
	d.bp = 0
	d.c = uint32(d.at(0)) << 16
	d.byteIn()
	d.c <<= 7
	d.ct -= 7
	d.a = 0x8000
}

// at returns the byte at index i, or 0xFF beyond the end of the data.
func (d *Decoder) at(i int) byte {
	if i < len(d.data) {
		return d.data[i]
	}
	return 0xFF
}

// byteIn loads the next byte into Clow, skipping stuffed bits and
// stopping at a marker.
func (d *Decoder) byteIn() {
	if d.at(d.bp) == 0xFF {
		if d.at(d.bp+1) > 0x8F {
			d.c += 0xFF00
			d.ct = 8
			return
		}
		d.bp++
		d.c += uint32(d.data[d.bp]) << 9
		d.ct = 7
		return
	}
	d.bp++
	d.c += uint32(d.at(d.bp)) << 8
	d.ct = 8
}

// Decode decodes one binary decision in the given context and updates it.
// The result is always 0 or 1.
func (d *Decoder) Decode(cx *Context) int {
	st := &table[cx.index]
	qe := st.qe
	d.a -= qe
	var bit uint8
	if d.c>>16 < qe {
		// LPS exchange
		if d.a < qe {
			bit = cx.mps
			cx.index = st.nmps
		} else {
			bit = 1 - cx.mps
			cx.mps ^= st.sw
			cx.index = st.nlps
		}
		d.a = qe
	} else {
		d.c -= qe << 16
		if d.a&0x8000 != 0 {
			return int(cx.mps)
		}
		// MPS exchange
		if d.a < qe {
			bit = 1 - cx.mps
			cx.mps ^= st.sw
			cx.index = st.nlps
		} else {
			bit = cx.mps
			cx.index = st.nmps
		}
	}
	// RENORMD
	for {
		if d.ct == 0 {
			d.byteIn()
		}
		d.a <<= 1
		d.c <<= 1
		d.ct--
		if d.a&0x8000 != 0 {
			break
		}
	}
	return int(bit)
}
