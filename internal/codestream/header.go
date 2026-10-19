// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package codestream

import (
	"errors"
	"fmt"
	"io"
)

// MainHeader holds the parsed main header.
//
// The raw payloads in TLM, PLM, Comments and PPM refer to the input when the
// header was parsed by a Reader over a byte slice, and must not be modified.
type MainHeader struct {
	SIZ SIZ
	COD *COD // required
	COC map[int]*CodingStyle
	QCD *Quantization // required
	QCC map[int]*Quantization
	RGN []RGN
	POC []POCEntry
	// PPM holds the packed packet header segments.  PPM and PPT must not
	// both be used in one codestream; the decoder rejects that case.
	PPM []PPMSegment
	// TLM and PLM hold the raw payloads (starting with the Z index) of the
	// segments in file order.  Segments with an empty payload carry no
	// information and are not recorded.
	TLM          [][]byte
	PLM          [][]byte
	CRG          *CRG
	Comments     []Comment
	Capabilities []byte // raw CAP payload, if present
}

// UsesPPM reports whether the packet headers are stored in PPM segments of
// the main header.
func (h *MainHeader) UsesPPM() bool { return len(h.PPM) > 0 }

// TilePart holds one tile-part header and body.
type TilePart struct {
	Index     int    // Isot
	Length    uint32 // Psot, 0 if the tile-part extends to the end
	PartIndex int    // TPsot
	PartCount int    // TNsot, 0 if unknown

	// COD, COC, QCD, QCC and RGN only occur in the first tile-part of a
	// tile (PartIndex 0).
	COD      *COD
	COC      map[int]*CodingStyle
	QCD      *Quantization
	QCC      map[int]*Quantization
	RGN      []RGN
	POC      []POCEntry
	PPT      []PPTSegment
	PLT      [][]byte
	Comments []Comment

	Data []byte // body after SOD

	// Truncated is set when Psot claims more bytes than the input holds,
	// so that Data was clamped to the available bytes.
	Truncated bool
}

// maxChunk is the largest piece read from a stream in one go, so that a
// declared segment length cannot drive a large allocation.
const maxChunk = 64 << 10

func chargeN(charge func(int) error, n int) error {
	if charge == nil {
		return nil
	}
	return charge(n)
}

// source supplies markers and marker segment payloads.
type source interface {
	marker() (uint16, error)
	// segment returns the payload after the length field.  The returned
	// slice is never modified by the source afterwards.
	segment() ([]byte, error)
}

func checkMarker(b0, b1 byte) (uint16, error) {
	if b0 != 0xFF || b1 < 0x01 || b1 == 0xFF {
		return 0, fmt.Errorf("invalid marker %02x%02x", b0, b1)
	}
	return uint16(b0)<<8 | uint16(b1), nil
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// byteSrc reads from memory and returns payloads as slices of the input.
type byteSrc struct {
	data []byte
	pos  int
}

func (s *byteSrc) marker() (uint16, error) {
	if len(s.data)-s.pos < 2 {
		return 0, io.ErrUnexpectedEOF
	}
	m, err := checkMarker(s.data[s.pos], s.data[s.pos+1])
	if err != nil {
		return 0, err
	}
	s.pos += 2
	return m, nil
}

func (s *byteSrc) segment() ([]byte, error) {
	if len(s.data)-s.pos < 2 {
		return nil, io.ErrUnexpectedEOF
	}
	l := int(s.data[s.pos])<<8 | int(s.data[s.pos+1])
	if l < 2 {
		return nil, errors.New("invalid marker segment length")
	}
	if len(s.data)-s.pos < l {
		return nil, io.ErrUnexpectedEOF
	}
	p := s.data[s.pos+2 : s.pos+l : s.pos+l]
	s.pos += l
	return p, nil
}

// streamSrc reads from an io.Reader and copies payloads into fresh buffers.
type streamSrc struct {
	r      io.Reader
	charge func(int) error
}

func (s *streamSrc) marker() (uint16, error) {
	var b [2]byte
	if _, err := io.ReadFull(s.r, b[:]); err != nil {
		return 0, unexpected(err)
	}
	return checkMarker(b[0], b[1])
}

func (s *streamSrc) segment() ([]byte, error) {
	var b [2]byte
	if _, err := io.ReadFull(s.r, b[:]); err != nil {
		return nil, unexpected(err)
	}
	l := int(b[0])<<8 | int(b[1])
	if l < 2 {
		return nil, errors.New("invalid marker segment length")
	}
	n := l - 2
	if n == 0 {
		return []byte{}, nil
	}
	// The first chunk is read before the full buffer is allocated, so the
	// allocation is backed by data that is actually present.
	k := min(n, maxChunk)
	if err := chargeN(s.charge, k); err != nil {
		return nil, err
	}
	first := make([]byte, k)
	if _, err := io.ReadFull(s.r, first); err != nil {
		return nil, unexpected(err)
	}
	if k == n {
		return first, nil
	}
	if err := chargeN(s.charge, n); err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	copy(buf, first)
	for pos := k; pos < n; {
		k := min(n-pos, maxChunk)
		if _, err := io.ReadFull(s.r, buf[pos:pos+k]); err != nil {
			return nil, unexpected(err)
		}
		pos += k
	}
	return buf, nil
}

// readMainHeader parses the main header and returns the marker (SOT or EOC)
// that terminated it; that marker has been consumed.
func readMainHeader(src source, charge func(int) error) (*MainHeader, uint16, error) {
	m, err := src.marker()
	if err != nil {
		return nil, 0, err
	}
	if m != markerSOC {
		return nil, 0, errors.New("codestream does not start with SOC")
	}
	if m, err = src.marker(); err != nil {
		return nil, 0, err
	}
	if m != markerSIZ {
		return nil, 0, errors.New("SIZ does not follow SOC")
	}
	p, err := src.segment()
	if err != nil {
		return nil, 0, err
	}
	if err := chargeN(charge, 16*len(p)+128); err != nil {
		return nil, 0, err
	}
	s, err := parseSIZ(p)
	if err != nil {
		return nil, 0, err
	}
	h := &MainHeader{SIZ: *s}
	csiz := len(s.Components)
	for {
		m, err := src.marker()
		if err != nil {
			return nil, 0, err
		}
		switch m {
		case markerSOT, markerEOC:
			if h.COD == nil || h.QCD == nil {
				return nil, 0, errors.New("main header lacks COD or QCD")
			}
			return h, m, nil
		}
		if isNoLength(m) {
			if m == markerSOC || m == markerSOD || m == markerEPH {
				return nil, 0, fmt.Errorf("unexpected marker %04x in main header", m)
			}
			continue // reserved marker without length
		}
		p, err := src.segment()
		if err != nil {
			return nil, 0, err
		}
		if err := h.add(m, p, csiz, charge); err != nil {
			return nil, 0, fmt.Errorf("marker %04x: %w", m, err)
		}
	}
}

func (h *MainHeader) add(m uint16, p []byte, csiz int, charge func(int) error) error {
	// every case charges an upper bound for what the parsed form retains
	switch m {
	case markerSIZ:
		return errors.New("duplicate SIZ")
	case markerCAP:
		h.Capabilities = p
	case markerCOD:
		if h.COD != nil {
			return errors.New("duplicate COD")
		}
		if err := chargeN(charge, 16*len(p)+128); err != nil {
			return err
		}
		d, err := parseCOD(p)
		h.COD = d
		return err
	case markerCOC:
		if err := chargeN(charge, 16*len(p)+128); err != nil {
			return err
		}
		c, cs, err := parseCOC(p, csiz)
		if err != nil {
			return err
		}
		if h.COC == nil {
			h.COC = map[int]*CodingStyle{}
		}
		if h.COC[c] != nil {
			return errors.New("duplicate COC")
		}
		h.COC[c] = cs
	case markerQCD:
		if h.QCD != nil {
			return errors.New("duplicate QCD")
		}
		if err := chargeN(charge, 8*len(p)+128); err != nil {
			return err
		}
		q, err := parseQuant(p)
		h.QCD = q
		return err
	case markerQCC:
		if err := chargeN(charge, 8*len(p)+128); err != nil {
			return err
		}
		c, q, err := parseQCC(p, csiz)
		if err != nil {
			return err
		}
		if h.QCC == nil {
			h.QCC = map[int]*Quantization{}
		}
		if h.QCC[c] != nil {
			return errors.New("duplicate QCC")
		}
		h.QCC[c] = q
	case markerRGN:
		if err := chargeN(charge, 128); err != nil {
			return err
		}
		r, err := parseRGN(p, csiz)
		if err != nil {
			return err
		}
		h.RGN = append(h.RGN, r)
	case markerPOC:
		if h.POC != nil {
			return errors.New("duplicate POC")
		}
		if err := chargeN(charge, 16*len(p)+64); err != nil {
			return err
		}
		es, err := parsePOC(p, csiz)
		if err != nil {
			return err
		}
		h.POC = es
	case markerPPM:
		if err := chargeN(charge, 128); err != nil {
			return err
		}
		z, d, err := parseZ(p)
		if err != nil {
			return err
		}
		h.PPM = append(h.PPM, PPMSegment{z, d})
	case markerTLM:
		if len(p) == 0 {
			return nil
		}
		if err := chargeN(charge, 48); err != nil {
			return err
		}
		h.TLM = append(h.TLM, p)
	case markerPLM:
		if len(p) == 0 {
			return nil
		}
		if err := chargeN(charge, 48); err != nil {
			return err
		}
		h.PLM = append(h.PLM, p)
	case markerCRG:
		if h.CRG != nil {
			return errors.New("duplicate CRG")
		}
		if err := chargeN(charge, len(p)+64); err != nil {
			return err
		}
		g, err := parseCRG(p, csiz)
		if err != nil {
			return err
		}
		h.CRG = g
	case markerCOM:
		if err := chargeN(charge, 64); err != nil {
			return err
		}
		c, err := parseCOM(p)
		if err != nil {
			return err
		}
		h.Comments = append(h.Comments, c)
	case markerPLT, markerPPT:
		return errors.New("tile-part marker in main header")
	}
	// PRF and unknown markers are skipped
	return nil
}

// readTilePartHeader parses the tile-part header after the SOT segment up to
// and including SOD.
func readTilePartHeader(src source, t *TilePart, csiz int, charge func(int) error) error {
	for {
		m, err := src.marker()
		if err != nil {
			return err
		}
		if m == markerSOD {
			return nil
		}
		if isNoLength(m) {
			if m == markerSOC || m == markerEOC || m == markerEPH {
				return fmt.Errorf("unexpected marker %04x in tile-part header", m)
			}
			continue
		}
		p, err := src.segment()
		if err != nil {
			return err
		}
		if err := t.add(m, p, csiz, charge); err != nil {
			return fmt.Errorf("marker %04x: %w", m, err)
		}
	}
}

func (t *TilePart) add(m uint16, p []byte, csiz int, charge func(int) error) error {
	switch m {
	case markerSIZ, markerSOT, markerTLM, markerPLM, markerPPM, markerCRG, markerCAP:
		return errors.New("marker not allowed in tile-part header")
	case markerCOD, markerCOC, markerQCD, markerQCC, markerRGN:
		if t.PartIndex > 0 {
			// These markers are only meaningful in the first tile-part of a
			// tile.  Writers in the wild repeat them, so ignore them rather
			// than lose the rest of the tile.
			return nil
		}
	}
	switch m {
	case markerCOD:
		if t.COD != nil {
			return errors.New("duplicate COD")
		}
		if err := chargeN(charge, 16*len(p)+128); err != nil {
			return err
		}
		d, err := parseCOD(p)
		t.COD = d
		return err
	case markerCOC:
		if err := chargeN(charge, 16*len(p)+128); err != nil {
			return err
		}
		c, cs, err := parseCOC(p, csiz)
		if err != nil {
			return err
		}
		if t.COC == nil {
			t.COC = map[int]*CodingStyle{}
		}
		if t.COC[c] != nil {
			return errors.New("duplicate COC")
		}
		t.COC[c] = cs
	case markerQCD:
		if t.QCD != nil {
			return errors.New("duplicate QCD")
		}
		if err := chargeN(charge, 8*len(p)+128); err != nil {
			return err
		}
		q, err := parseQuant(p)
		t.QCD = q
		return err
	case markerQCC:
		if err := chargeN(charge, 8*len(p)+128); err != nil {
			return err
		}
		c, q, err := parseQCC(p, csiz)
		if err != nil {
			return err
		}
		if t.QCC == nil {
			t.QCC = map[int]*Quantization{}
		}
		if t.QCC[c] != nil {
			return errors.New("duplicate QCC")
		}
		t.QCC[c] = q
	case markerRGN:
		if err := chargeN(charge, 128); err != nil {
			return err
		}
		r, err := parseRGN(p, csiz)
		if err != nil {
			return err
		}
		t.RGN = append(t.RGN, r)
	case markerPOC:
		if t.POC != nil {
			return errors.New("duplicate POC")
		}
		if err := chargeN(charge, 16*len(p)+64); err != nil {
			return err
		}
		es, err := parsePOC(p, csiz)
		if err != nil {
			return err
		}
		t.POC = es
	case markerPPT:
		if err := chargeN(charge, 128); err != nil {
			return err
		}
		z, d, err := parseZ(p)
		if err != nil {
			return err
		}
		t.PPT = append(t.PPT, PPTSegment{z, d})
	case markerPLT:
		if err := chargeN(charge, 48); err != nil {
			return err
		}
		t.PLT = append(t.PLT, p)
	case markerCOM:
		if err := chargeN(charge, 64); err != nil {
			return err
		}
		c, err := parseCOM(p)
		if err != nil {
			return err
		}
		t.Comments = append(t.Comments, c)
	}
	return nil
}

// ReadMainHeaderFrom parses just the main header from r, stopping right after
// the first SOT marker is seen (or at EOC).  It does not read further.
//
// Segment payloads are read in chunks of at most 64 KiB, so a declared length
// never causes an allocation larger than the data actually present.  If
// charge is not nil, it is called with the size of every allocation before
// the allocation is made; an error from it aborts parsing.
func ReadMainHeaderFrom(r io.Reader, charge func(n int) error) (*MainHeader, error) {
	h, _, err := readMainHeader(&streamSrc{r, charge}, charge)
	return h, err
}

// Reader walks a codestream held in memory.  Payloads in the parsed results
// are slices of the input and must not be modified.
type Reader struct {
	data   []byte
	pos    int
	main   *MainHeader
	charge func(int) error
}

// NewReader returns a Reader over data.  If charge is not nil, it is called
// with the size of every allocation the Reader makes for parsed structures
// before the allocation is made; an error from it aborts parsing.
func NewReader(data []byte, charge func(n int) error) *Reader {
	return &Reader{data: data, charge: charge}
}

// ReadMainHeader parses the main header, leaving the reader positioned at
// the first tile-part.
func (r *Reader) ReadMainHeader() (*MainHeader, error) {
	src := &byteSrc{data: r.data}
	h, m, err := readMainHeader(src, r.charge)
	if err != nil {
		return nil, err
	}
	r.pos = src.pos
	if m == markerSOT {
		r.pos -= 2
	}
	r.main = h
	return h, nil
}

// NextTilePart parses the next tile-part.  It returns (nil, nil) at EOC or at
// the end of the input.  A tile-part length that overruns the input is
// clamped and reported through TilePart.Truncated, and a length of zero
// extends the tile-part to the end of the input (excluding a final EOC
// marker).  If the tile-part header is truncated or malformed, the reader
// is moved to the end of the input before the error is returned, so
// repeated calls cannot loop.
func (r *Reader) NextTilePart() (*TilePart, error) {
	tp, err := r.nextTilePart()
	if err != nil {
		r.pos = len(r.data)
	}
	return tp, err
}

func (r *Reader) nextTilePart() (*TilePart, error) {
	if r.main == nil {
		return nil, errors.New("main header not read")
	}
	if len(r.data)-r.pos < 2 {
		r.pos = len(r.data)
		return nil, nil
	}
	start := r.pos
	src := &byteSrc{data: r.data, pos: start}
	m, err := src.marker()
	if err != nil {
		return nil, err
	}
	if m == markerEOC {
		r.pos = len(r.data)
		return nil, nil
	}
	if m != markerSOT {
		return nil, fmt.Errorf("unexpected marker %04x, want SOT", m)
	}
	p, err := src.segment()
	if err != nil {
		return nil, err
	}
	idx, psot, part, count, err := parseSOT(p)
	if err != nil {
		return nil, err
	}
	if nx, ny, ok := r.main.SIZ.NumTiles(); !ok || int64(idx) >= int64(nx)*int64(ny) {
		return nil, fmt.Errorf("tile index %d out of range", idx)
	}
	t := &TilePart{Index: idx, Length: psot, PartIndex: part, PartCount: count}
	if err := readTilePartHeader(src, t, len(r.main.SIZ.Components), r.charge); err != nil {
		return nil, err
	}
	hdrEnd := src.pos
	end := len(r.data)
	if psot == 0 {
		if end-2 >= hdrEnd && r.data[end-2] == 0xFF && r.data[end-1] == 0xD9 {
			end -= 2
		}
	} else {
		if int64(psot) < int64(hdrEnd-start) {
			return nil, errors.New("tile-part length too small")
		}
		if avail := int64(len(r.data) - start); int64(psot) < avail {
			end = start + int(psot)
		} else if int64(psot) > avail {
			t.Truncated = true
		}
	}
	t.Data = r.data[hdrEnd:end:end]
	r.pos = end
	return t, nil
}

// Resolve returns the coding style and quantization that apply to component c
// in a tile, given the main header and the tile's first tile-part (which may
// be nil).  Precedence is tile COC, tile COD, main COC, main COD, and
// likewise for QCC and QCD.
//
// The result is checked with [Quantization.Validate], and reversible coding
// must use QuantNone while irreversible coding must not.  The quantization
// therefore has at least one entry, with Mantissas parallel to Exponents
// except for QuantNone, but it may have fewer entries than subbands: callers
// must map subbands to entries without indexing past the end.
func Resolve(m *MainHeader, t *TilePart, c int) (CodingStyle, Quantization, error) {
	if m == nil || m.COD == nil || m.QCD == nil {
		return CodingStyle{}, Quantization{}, errors.New("incomplete main header")
	}
	if c < 0 || c >= len(m.SIZ.Components) {
		return CodingStyle{}, Quantization{}, fmt.Errorf("component index %d out of range", c)
	}
	var cs CodingStyle
	switch {
	case t != nil && t.COC[c] != nil:
		cs = *t.COC[c]
	case t != nil && t.COD != nil:
		cs = t.COD.CodingStyle
	case m.COC[c] != nil:
		cs = *m.COC[c]
	default:
		cs = m.COD.CodingStyle
	}
	var q Quantization
	switch {
	case t != nil && t.QCC[c] != nil:
		q = *t.QCC[c]
	case t != nil && t.QCD != nil:
		q = *t.QCD
	case m.QCC[c] != nil:
		q = *m.QCC[c]
	default:
		q = *m.QCD
	}
	if err := q.Validate(cs.Levels); err != nil {
		return cs, q, err
	}
	if cs.Reversible != (q.Style == QuantNone) {
		return cs, q, errors.New("quantization style does not match the wavelet transform")
	}
	return cs, q, nil
}
