// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package t2

import (
	"errors"
	"math/bits"

	"seehuhn.de/go/jpx/internal/t1"
)

// ErrTruncated reports that a packet body extended beyond the available
// data.  The packet's code-blocks have received whatever was present.
var ErrTruncated = errors.New("t2: truncated packet data")

// Stream is a byte slice with a read position.
type Stream struct {
	Data []byte
	Pos  int
}

// Remaining returns the number of unread bytes.
func (s *Stream) Remaining() int { return len(s.Data) - s.Pos }

// maxZeroPlanes bounds the zero bit-plane count signalled for a block.
// Valid values are below the number of bit-planes of the band, at most
// 37 plus the region-of-interest shift; the bound is generous.
const maxZeroPlanes = 512

// Piece is one codeword-segment contribution signalled in a packet header.
type Piece struct {
	cb     *CodeBlock
	length int
	passes int
	closes bool
}

// Packet identifies one packet of a tile.
type Packet struct {
	Layer    int
	Comp     *TileComp
	Res      *Resolution
	Precinct int
}

// ReadPacket decodes the packet header from hdr and attaches the packet
// body from body to the code-blocks of the precinct.  When headers are
// stored in-line, hdr and body are the same stream.  The pieces slice is
// scratch space that is reused between calls.
func (t *Tile) ReadPacket(pk Packet, hdr, body *Stream, pieces *[]Piece, charge Charge) error {
	tc, res := pk.Comp, pk.Res
	prec, err := tc.Precinct(res, pk.Precinct, charge)
	if err != nil {
		return err
	}
	if t.SOP {
		skipSOP(body)
	}

	br := NewBitReader(hdr.Data[hdr.Pos:])
	*pieces = (*pieces)[:0]
	present, err := br.ReadBit()
	if err != nil {
		return err
	}
	if present != 0 {
		for _, pb := range prec.Bands {
			if err := t.readBandHeader(br, pb, pk.Layer, tc, pieces); err != nil {
				return err
			}
		}
	}
	hdr.Pos += br.Align()
	if t.EPH {
		skipEPH(hdr)
	}

	// attach the body
	for _, p := range *pieces {
		n := p.length
		truncated := false
		if n > body.Remaining() {
			n = body.Remaining()
			truncated = true
		}
		data := body.Data[body.Pos : body.Pos+n]
		body.Pos += n
		if err := p.cb.addPiece(data, p.passes, p.closes, charge); err != nil {
			return err
		}
		if truncated {
			return ErrTruncated
		}
	}
	return nil
}

// readBandHeader decodes the header fields for the code-blocks of one
// precinct-band.
func (t *Tile) readBandHeader(br *BitReader, pb *PrecinctBand, layer int, tc *TileComp, pieces *[]Piece) error {
	style := t1.Style{
		Bypass:      tc.Style.EntropyBypass,
		Reset:       tc.Style.ResetContexts,
		Terminate:   tc.Style.Terminate,
		Causal:      tc.Style.VerticallyCausal,
		Predictable: tc.Style.PredictableTermination,
		SegSymbols:  tc.Style.SegmentationSymbols,
	}
	for j := range pb.CBH {
		for i := range pb.CBW {
			cb := &pb.Blocks[j*pb.CBW+i]
			var included bool
			if cb.Included {
				b, err := br.ReadBit()
				if err != nil {
					return err
				}
				included = b != 0
			} else {
				ok, err := pb.Incl.Decode(br, i, j, layer+1)
				if err != nil {
					return err
				}
				included = ok
			}
			if !included {
				continue
			}
			if !cb.Included {
				cb.Included = true
				// Encoders include all-zero blocks with a zero bit-plane count
				// at or above the number of planes; such blocks decode to
				// nothing.  The bound only keeps hostile input from looping.
				p, err := pb.ZeroBP.DecodeValue(br, i, j, maxZeroPlanes)
				if err != nil {
					return err
				}
				cb.ZeroPlanes = p
			}
			npasses, err := readPassCount(br)
			if err != nil {
				return err
			}
			// Encoders in circulation signal more passes than the bit-planes
			// of the band can hold; the surplus passes are ignored by the
			// block decoder, which stops at the last bit-plane.
			for {
				b, err := br.ReadBit()
				if err != nil {
					return err
				}
				if b == 0 {
					break
				}
				cb.LBlock++
				if cb.LBlock > 31 {
					return errors.New("t2: code-block length indicator too large")
				}
			}

			// one length per codeword segment ending in this packet
			start := 0
			for k := range npasses {
				pass := cb.Passes + k
				term := t1.IsTerminated(pass, style)
				if !term && k != npasses-1 {
					continue
				}
				n := k - start + 1
				nbits := cb.LBlock + bits.Len(uint(n)) - 1
				if nbits > 31 {
					return errors.New("t2: segment length too large")
				}
				length, err := br.ReadBits(nbits)
				if err != nil {
					return err
				}
				*pieces = append(*pieces, Piece{cb: cb, length: int(length), passes: n, closes: term})
				start = k + 1
			}
			cb.Passes += npasses
		}
	}
	return nil
}

// readPassCount decodes the number of coding passes codeword.
func readPassCount(br *BitReader) (int, error) {
	b, err := br.ReadBit()
	if err != nil {
		return 0, err
	}
	if b == 0 {
		return 1, nil
	}
	if b, err = br.ReadBit(); err != nil {
		return 0, err
	}
	if b == 0 {
		return 2, nil
	}
	v, err := br.ReadBits(2)
	if err != nil {
		return 0, err
	}
	if v < 3 {
		return 3 + int(v), nil
	}
	if v, err = br.ReadBits(5); err != nil {
		return 0, err
	}
	if v < 31 {
		return 6 + int(v), nil
	}
	if v, err = br.ReadBits(7); err != nil {
		return 0, err
	}
	return 37 + int(v), nil
}

// addPiece appends body data for passes coding passes to the code-block,
// starting a new codeword segment if the previous one was terminated.
func (cb *CodeBlock) addPiece(data []byte, passes int, closes bool, charge Charge) error {
	n := len(cb.Segments)
	if n == 0 || cb.Segments[n-1].Closed {
		if err := charge.charge(64); err != nil {
			return err
		}
		cb.Segments = append(cb.Segments, Segment{})
		n++
	}
	seg := &cb.Segments[n-1]
	if err := charge.charge(24); err != nil {
		return err
	}
	seg.Chunks = append(seg.Chunks, data)
	seg.Passes += passes
	seg.Closed = closes
	return nil
}

// skipSOP skips a start-of-packet marker segment if one is present.
func skipSOP(s *Stream) {
	d := s.Data[s.Pos:]
	if len(d) >= 6 && d[0] == 0xFF && d[1] == 0x91 && d[2] == 0 && d[3] == 4 {
		s.Pos += 6
	}
}

// skipEPH skips an end-of-packet-header marker if one is present.
func skipEPH(s *Stream) {
	d := s.Data[s.Pos:]
	if len(d) >= 2 && d[0] == 0xFF && d[1] == 0x92 {
		s.Pos += 2
	}
}

// DataLen returns the total number of bytes received for the code-block.
func (cb *CodeBlock) DataLen() int {
	n := 0
	for _, s := range cb.Segments {
		for _, c := range s.Chunks {
			n += len(c)
		}
	}
	return n
}

// Data assembles the codeword segments of a code-block for the tier-1
// decoder.  Segments consisting of several chunks are copied into buf,
// which is grown as needed; the possibly reallocated buf and the segment
// list are returned.  The copy is bounded by the code-block's own data.
func (cb *CodeBlock) Data(buf []byte, segs []t1.Segment) ([]byte, []t1.Segment) {
	total := 0
	for _, s := range cb.Segments {
		if len(s.Chunks) > 1 {
			for _, c := range s.Chunks {
				total += len(c)
			}
		}
	}
	if cap(buf) < total {
		buf = make([]byte, 0, total)
	}
	buf = buf[:0]
	segs = segs[:0]
	for _, s := range cb.Segments {
		var data []byte
		switch len(s.Chunks) {
		case 0:
		case 1:
			data = s.Chunks[0]
		default:
			start := len(buf)
			for _, c := range s.Chunks {
				buf = append(buf, c...)
			}
			data = buf[start:len(buf):len(buf)]
		}
		segs = append(segs, t1.Segment{Data: data, Passes: s.Passes})
	}
	return buf, segs
}
