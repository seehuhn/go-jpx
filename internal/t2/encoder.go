// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package t2

import (
	"math/bits"

	"seehuhn.de/go/jpx/internal/t1"
)

// BlockData is the encoder-side state of one code-block: the coded data
// and the pass boundaries, plus the layer assignment.
type BlockData struct {
	Data   []byte
	Passes []t1.PassInfo

	// LayerEnd[l] is the number of passes included up to and including
	// layer l.  It is non-decreasing; a layer with no new passes repeats
	// the previous value.
	LayerEnd []int

	// decoding-side mirror of the state a decoder maintains
	included bool
	lblock   int
	sent     int // passes already written
}

// Encoded holds the per-block encoder data of a precinct-band in the
// same order as PrecinctBand.Blocks.
type Encoded struct {
	Blocks []BlockData
}

// WritePacket encodes the packet header and body for layer l of the
// precinct, appending both to out.  enc[i] holds the encoder data for
// prec.Bands[i].  SOP and EPH markers are written when the tile uses
// them.  The returned slice is out with the packet appended.
func (t *Tile) WritePacket(out []byte, tc *TileComp, prec *Precinct, enc []*Encoded, layer int, seq int) []byte {
	style := t1.Style{
		Bypass:    tc.Style.EntropyBypass,
		Terminate: tc.Style.Terminate,
	}
	if t.SOP {
		out = append(out, 0xFF, 0x91, 0, 4, byte(seq>>8), byte(seq))
	}

	var bw BitWriter
	var body []byte
	empty := true
	for bi, pb := range prec.Bands {
		for i := range pb.Blocks {
			bd := &enc[bi].Blocks[i]
			if bd.LayerEnd[layer] > bd.sent {
				empty = false
			}
		}
	}
	if !empty {
		bw.WriteBit(1)
		for bi, pb := range prec.Bands {
			for j := range pb.CBH {
				for i := range pb.CBW {
					bd := &enc[bi].Blocks[j*pb.CBW+i]
					body = t.writeBlockHeader(&bw, pb, bd, i, j, layer, style, body)
				}
			}
		}
	} else {
		bw.WriteBit(0)
	}
	out = append(out, bw.Flush()...)
	if t.EPH {
		out = append(out, 0xFF, 0x92)
	}
	return append(out, body...)
}

// writeBlockHeader writes the header fields of one code-block for the
// given layer and appends the block's contribution to body.
func (t *Tile) writeBlockHeader(bw *BitWriter, pb *PrecinctBand, bd *BlockData, i, j, layer int, style t1.Style, body []byte) []byte {
	end := bd.LayerEnd[layer]
	newPasses := end - bd.sent
	if bd.included {
		if newPasses == 0 {
			bw.WriteBit(0)
			return body
		}
		bw.WriteBit(1)
	} else {
		// first inclusion is coded through the tag tree, whose value is
		// the first layer with a contribution
		pb.Incl.Encode(bw, i, j, layer+1)
		if newPasses == 0 {
			return body
		}
		bd.included = true
		pb.ZeroBP.EncodeValue(bw, i, j)
	}
	writePassCount(bw, newPasses)

	// segment lengths: one per terminated pass in the range, plus the
	// final pass of the contribution
	start := bd.sent
	type seg struct{ length, passes int }
	var segs []seg
	segStart := start
	for p := start; p < end; p++ {
		if t1.IsTerminated(p, style) || p == end-1 {
			from := 0
			if segStart > 0 {
				from = bd.Passes[segStart-1].Len
			}
			segs = append(segs, seg{bd.Passes[p].Len - from, p - segStart + 1})
			segStart = p + 1
		}
	}
	// grow lblock until every length fits
	for _, s := range segs {
		need := bits.Len(uint(s.length)) - (bits.Len(uint(s.passes)) - 1)
		for bd.lblock < need {
			bw.WriteBit(1)
			bd.lblock++
		}
	}
	bw.WriteBit(0)
	for _, s := range segs {
		n := bd.lblock + bits.Len(uint(s.passes)) - 1
		bw.WriteBits(uint32(s.length), n)
	}

	from := 0
	if start > 0 {
		from = bd.Passes[start-1].Len
	}
	body = append(body, bd.Data[from:bd.Passes[end-1].Len]...)
	bd.sent = end
	return body
}

// writePassCount encodes the number of coding passes codeword.
func writePassCount(bw *BitWriter, n int) {
	switch {
	case n == 1:
		bw.WriteBit(0)
	case n == 2:
		bw.WriteBits(2, 2)
	case n <= 5:
		bw.WriteBits(0xC|uint32(n-3), 4)
	case n <= 36:
		bw.WriteBits(0xF, 4)
		bw.WriteBits(uint32(n-6), 5)
	default:
		bw.WriteBits(0x1FF, 9)
		bw.WriteBits(uint32(n-37), 7)
	}
}

// InitEncoder prepares the tag trees of a precinct-band for encoding
// from the layer assignment of its blocks: the inclusion tree holds the
// first layer with a contribution, the zero bit-plane tree the number of
// missing planes.
func (pb *PrecinctBand) InitEncoder(enc *Encoded, zeroPlanes func(i int) int, layers int) {
	for j := range pb.CBH {
		for i := range pb.CBW {
			k := j*pb.CBW + i
			bd := &enc.Blocks[k]
			bd.lblock = 3
			first := layers // never included
			for l, e := range bd.LayerEnd {
				if e > 0 {
					first = l
					break
				}
			}
			pb.Incl.SetValue(i, j, first)
			pb.ZeroBP.SetValue(i, j, zeroPlanes(k))
		}
	}
}
