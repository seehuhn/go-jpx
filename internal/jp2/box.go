// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jp2

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// chunkSize is the largest piece of a box payload read in one step.
const chunkSize = 64 << 10

// Box types used by this package.
const (
	typeSignature = "jP  "
	typeFtyp      = "ftyp"
	typeJp2h      = "jp2h"
	typeIhdr      = "ihdr"
	typeBpcc      = "bpcc"
	typeColr      = "colr"
	typePclr      = "pclr"
	typeCmap      = "cmap"
	typeCdef      = "cdef"
	typeRreq      = "rreq"
	typeJp2c      = "jp2c"
	typeFtbl      = "ftbl"
	typeFlst      = "flst"
)

// boxHeader describes the framing of one box.
type boxHeader struct {
	Type   string
	HdrLen int   // size of the header in bytes (8 or 16)
	Len    int64 // payload length; -1 when the box extends to the end of input
}

var errTruncated = errors.New("jp2: truncated box")

// readBoxHeader reads one box header from r.  It returns io.EOF only if no
// byte at all could be read.  avail is the number of bytes available to
// the box including its header, or -1 if unknown; boxes that claim more
// are rejected.
func readBoxHeader(r io.Reader, avail int64) (boxHeader, error) {
	var buf [16]byte
	_, err := io.ReadFull(r, buf[:8])
	if err == io.EOF {
		return boxHeader{}, io.EOF
	}
	if err != nil {
		return boxHeader{}, errTruncated
	}
	lbox := binary.BigEndian.Uint32(buf[:4])
	h := boxHeader{Type: string(buf[4:8]), HdrLen: 8}
	switch lbox {
	case 0:
		if avail >= 0 {
			h.Len = avail - 8
		} else {
			h.Len = -1
		}
		return h, nil
	case 1:
		if _, err := io.ReadFull(r, buf[8:16]); err != nil {
			return boxHeader{}, errTruncated
		}
		xl := binary.BigEndian.Uint64(buf[8:16])
		if xl < 16 || xl > math.MaxInt64 {
			return boxHeader{}, fmt.Errorf("jp2: invalid extended box length %d", xl)
		}
		h.HdrLen = 16
		h.Len = int64(xl) - 16
	default:
		if lbox < 8 {
			return boxHeader{}, fmt.Errorf("jp2: invalid box length %d", lbox)
		}
		h.Len = int64(lbox) - 8
	}
	if avail >= 0 && h.Len+int64(h.HdrLen) > avail {
		return boxHeader{}, errors.New("jp2: box exceeds enclosing box")
	}
	return h, nil
}

// readPayload reads exactly n bytes.  The declared length n is not
// trusted: the data is read in chunks of at most chunkSize bytes, and each
// chunk is charged before it is allocated.  When more than one chunk is
// needed, the joined result is charged as well.
func readPayload(r io.Reader, n int64, charge func(int) error) ([]byte, error) {
	var chunks [][]byte
	total := 0
	for n > 0 {
		sz := int(min(n, chunkSize))
		if err := charge(sz); err != nil {
			return nil, err
		}
		buf := make([]byte, sz)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, errTruncated
		}
		chunks = append(chunks, buf)
		total += sz
		n -= int64(sz)
	}
	switch len(chunks) {
	case 0:
		return nil, nil
	case 1:
		return chunks[0], nil
	}
	if err := charge(total); err != nil {
		return nil, err
	}
	return bytes.Join(chunks, nil), nil
}

// skip discards n bytes from r.
func skip(r io.Reader, n int64) error {
	m, err := io.CopyN(io.Discard, r, n)
	if err != nil && m < n {
		return errTruncated
	}
	return nil
}

// subBox is one box found inside an in-memory superbox payload.
type subBox struct {
	Type string
	Data []byte
}

// splitBoxes splits the payload of a superbox into its child boxes.
// Trailing bytes too few for a box header are padding and are ignored.
// A child whose declared length exceeds the payload is an error.
func splitBoxes(data []byte, charge func(int) error) ([]subBox, error) {
	// count the boxes first so that the result is allocated once
	count := 0
	for d := data; len(d) >= 8; count++ {
		lbox := uint64(binary.BigEndian.Uint32(d))
		switch {
		case lbox == 0:
			lbox = uint64(len(d))
		case lbox == 1 && len(d) >= 16:
			lbox = binary.BigEndian.Uint64(d[8:])
		}
		if lbox < 8 || lbox > uint64(len(d)) {
			break
		}
		d = d[lbox:]
	}
	if err := charge(count * 48); err != nil {
		return nil, err
	}
	res := make([]subBox, 0, count)
	for len(data) >= 8 {
		lbox := uint64(binary.BigEndian.Uint32(data))
		typ := string(data[4:8])
		hdr := uint64(8)
		switch lbox {
		case 0:
			lbox = uint64(len(data))
		case 1:
			if len(data) < 16 {
				return res, nil
			}
			lbox = binary.BigEndian.Uint64(data[8:])
			hdr = 16
		}
		if lbox < hdr || lbox > uint64(len(data)) {
			return nil, errors.New("jp2: box exceeds enclosing box")
		}
		res = append(res, subBox{typ, data[hdr:lbox]})
		data = data[lbox:]
	}
	return res, nil
}

// appendBox appends a complete box to buf, using the extended length form
// only when needed.
func appendBox(buf []byte, typ string, payload []byte) []byte {
	if uint64(len(payload))+8 > math.MaxUint32 {
		buf = binary.BigEndian.AppendUint32(buf, 1)
		buf = append(buf, typ...)
		return append(binary.BigEndian.AppendUint64(buf, uint64(len(payload))+16), payload...)
	}
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(payload))+8)
	buf = append(buf, typ...)
	return append(buf, payload...)
}
