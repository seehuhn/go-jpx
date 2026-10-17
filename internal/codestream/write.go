// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package codestream

import (
	"encoding/binary"
	"errors"
	"io"
)

// Writer emits codestream marker segments.  Errors are sticky and reported
// by every later call and by Err.
type Writer struct {
	w    io.Writer
	n    int64
	err  error
	csiz int
}

// NewWriter returns a Writer that writes to w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Err returns the first error encountered.
func (w *Writer) Err() error { return w.err }

// Offset returns the number of bytes written so far.
func (w *Writer) Offset() int64 { return w.n }

func (w *Writer) raw(b []byte) {
	if w.err != nil {
		return
	}
	n, err := w.w.Write(b)
	w.n += int64(n)
	w.err = err
}

func (w *Writer) marker(m uint16) error {
	w.raw([]byte{byte(m >> 8), byte(m)})
	return w.err
}

func (w *Writer) fail(err error) error {
	if w.err == nil {
		w.err = err
	}
	return w.err
}

func (w *Writer) segment(m uint16, p []byte, err error) error {
	if w.err != nil {
		return w.err
	}
	if err == nil && len(p)+2 > 0xFFFF {
		err = errors.New("marker segment too long")
	}
	if err != nil {
		w.err = err
		return err
	}
	b := make([]byte, 0, len(p)+4)
	b = append(b, byte(m>>8), byte(m))
	b = binary.BigEndian.AppendUint16(b, uint16(len(p)+2))
	w.raw(append(b, p...))
	return w.err
}

// SOC writes the start-of-codestream marker.
func (w *Writer) SOC() error { return w.marker(markerSOC) }

// EOC writes the end-of-codestream marker.
func (w *Writer) EOC() error { return w.marker(markerEOC) }

// SOD writes the start-of-data marker.
func (w *Writer) SOD() error { return w.marker(markerSOD) }

// SIZ writes the SIZ segment and records the component count for later
// segments.
func (w *Writer) SIZ(s *SIZ) error {
	p, err := encodeSIZ(s)
	if err == nil {
		w.csiz = len(s.Components)
	}
	return w.segment(markerSIZ, p, err)
}

// COD writes a COD segment.
func (w *Writer) COD(d *COD) error {
	p, err := encodeCOD(d)
	return w.segment(markerCOD, p, err)
}

// COC writes a COC segment for component c.
func (w *Writer) COC(c int, cs *CodingStyle) error {
	p, err := encodeCOC(c, cs, w.csiz)
	return w.segment(markerCOC, p, err)
}

// QCD writes a QCD segment.
func (w *Writer) QCD(q *Quantization) error {
	p, err := encodeQuant(q)
	return w.segment(markerQCD, p, err)
}

// QCC writes a QCC segment for component c.
func (w *Writer) QCC(c int, q *Quantization) error {
	p, err := encodeQCC(c, q, w.csiz)
	return w.segment(markerQCC, p, err)
}

// RGN writes an RGN segment.
func (w *Writer) RGN(r RGN) error {
	p, err := encodeRGN(r, w.csiz)
	return w.segment(markerRGN, p, err)
}

// POC writes a POC segment with the given entries.
func (w *Writer) POC(es []POCEntry) error {
	p, err := encodePOC(es, w.csiz)
	return w.segment(markerPOC, p, err)
}

// PPM writes a PPM segment with index z in 0..255.  PPM and PPT segments
// must not be mixed in one codestream.
func (w *Writer) PPM(z int, data []byte) error {
	if z < 0 || z > 255 {
		return w.fail(errors.New("PPM index out of range"))
	}
	return w.segment(markerPPM, append([]byte{byte(z)}, data...), nil)
}

// PPT writes a PPT segment with index z in 0..255.
func (w *Writer) PPT(z int, data []byte) error {
	if z < 0 || z > 255 {
		return w.fail(errors.New("PPT index out of range"))
	}
	return w.segment(markerPPT, append([]byte{byte(z)}, data...), nil)
}

// TLM writes a TLM segment from its raw payload (starting with Ztlm).
func (w *Writer) TLM(payload []byte) error { return w.segment(markerTLM, payload, nil) }

// PLM writes a PLM segment from its raw payload (starting with Zplm).
func (w *Writer) PLM(payload []byte) error { return w.segment(markerPLM, payload, nil) }

// PLT writes a PLT segment from its raw payload (starting with Zplt).
func (w *Writer) PLT(payload []byte) error { return w.segment(markerPLT, payload, nil) }

// CRG writes a CRG segment.
func (w *Writer) CRG(g *CRG) error {
	p, err := encodeCRG(g, w.csiz)
	return w.segment(markerCRG, p, err)
}

// COM writes a COM segment.
func (w *Writer) COM(c Comment) error {
	return w.segment(markerCOM, append(binary.BigEndian.AppendUint16(nil, c.Registration), c.Data...), nil)
}

// SOT writes a tile-part header with Psot set to zero and returns the offset
// of the SOT marker, to be passed to PatchPsot once the length is known.
func (w *Writer) SOT(tile, part, count int) (int64, error) {
	off := w.n
	if tile < 0 || tile > 0xFFFF || part < 0 || part > 255 || count < 0 || count > 255 {
		w.err = errors.New("invalid SOT parameters")
		return off, w.err
	}
	p := []byte{byte(tile >> 8), byte(tile), 0, 0, 0, 0, byte(part), byte(count)}
	return off, w.segment(markerSOT, p, nil)
}

// PatchPsot sets the Psot field of the SOT marker starting at offset sot in
// buf, which holds the output written so far.  The length is that of the
// whole tile-part, from the SOT marker to the end of its data.
func PatchPsot(buf []byte, sot int, length uint32) error {
	if sot < 0 || sot+sotLen > len(buf) || buf[sot] != 0xFF || buf[sot+1] != 0x90 {
		return errors.New("no SOT marker at offset")
	}
	binary.BigEndian.PutUint32(buf[sot+6:], length)
	return nil
}
