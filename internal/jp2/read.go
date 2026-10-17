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

var signature = []byte{0, 0, 0, 12, 'j', 'P', ' ', ' ', 0x0D, 0x0A, 0x87, 0x0A}

// Read parses the wrapper from r.  It stops as soon as the first
// codestream is found and returns the header together with a reader for
// the codestream bytes.  The reader is bounded by the box length, or runs
// to the end of r when the box length is 0.
//
// If the input starts with a codestream instead of a signature box, the
// returned header has HasWrapper false and the reader yields the whole
// input.  If a fragment table precedes any contiguous codestream box and
// r implements [io.ReaderAt] (with offsets counted from the start of the
// file), the fragments are concatenated; otherwise an error is returned.
// A fragment table is resolved only when the input ends without a
// contiguous codestream box, so boxes after it are still parsed.
//
// Malformed ancillary boxes inside the header superbox, including a
// palette with columns deeper than 31 bits, are skipped and leave the
// corresponding Header field unset.  An error is returned for a
// bad signature, box framing that cannot be followed, truncated input
// before the codestream, an unusable fragment table when it is the
// only source of the codestream, or when charge fails.
//
// charge, if not nil, is called with the size of every allocation that
// depends on the input before the allocation is made; a non-nil result
// aborts Read and is returned unchanged.
func Read(r io.Reader, charge func(n int) error) (*Header, io.Reader, error) {
	if charge == nil {
		charge = func(int) error { return nil }
	}
	first := make([]byte, 12)
	n, err := io.ReadFull(r, first)
	first = first[:n]
	if n >= 4 && first[0] == 0xFF && first[1] == 0x4F && first[2] == 0xFF && first[3] == 0x51 {
		// a bare codestream; hand back the input from where it started,
		// rewinding when possible so that callers can recognise their own
		// reader
		if s, ok := r.(io.Seeker); ok {
			if _, err := s.Seek(-int64(n), io.SeekCurrent); err == nil {
				return &Header{}, r, nil
			}
		}
		return &Header{}, io.MultiReader(bytes.NewReader(first), r), nil
	}
	if err != nil || !bytes.Equal(first, signature) {
		return nil, nil, errors.New("jp2: not a JPEG 2000 file")
	}

	h := &Header{HasWrapper: true}
	seenJp2h := false
	var ftbl []byte
	haveFtbl := false
	// finish is called when no contiguous codestream box exists
	finish := func() (*Header, io.Reader, error) {
		if !haveFtbl {
			return nil, nil, errors.New("jp2: no codestream found")
		}
		cs, err := fragmentReader(r, ftbl, charge)
		if err != nil {
			return nil, nil, err
		}
		return h, cs, nil
	}
	for {
		bh, err := readBoxHeader(r, -1)
		if err == io.EOF {
			return finish()
		}
		if err != nil {
			return nil, nil, err
		}
		switch bh.Type {
		case typeJp2c:
			if bh.Len < 0 {
				return h, r, nil
			}
			return h, io.LimitReader(r, bh.Len), nil
		case typeFtyp, typeJp2h, typeRreq, typeFtbl:
			if bh.Len < 0 {
				return nil, nil, fmt.Errorf("jp2: %q box extends to end of file", bh.Type)
			}
			data, err := readPayload(r, bh.Len, charge)
			if err != nil {
				return nil, nil, err
			}
			switch bh.Type {
			case typeFtyp:
				err = tolerate(h.parseFtyp(data, charge))
			case typeJp2h:
				if !seenJp2h {
					seenJp2h = true
					err = h.parseJp2h(data, charge)
				}
			case typeRreq:
				if h.ReaderRequirements == nil {
					h.ReaderRequirements = data
				}
			case typeFtbl:
				if !haveFtbl {
					haveFtbl = true
					ftbl = data
				}
			}
			if err != nil {
				return nil, nil, err
			}
		default:
			if bh.Len < 0 {
				return finish()
			}
			if err := skip(r, bh.Len); err != nil {
				return nil, nil, err
			}
		}
	}
}

// tolerate turns a malformed-box error into nil.
func tolerate(err error) error {
	if errors.Is(err, errMalformed) {
		return nil
	}
	return err
}

// inputSize determines the total size of the input behind r, through a
// Size method or by seeking to the end and back.
func inputSize(r io.Reader) (int64, error) {
	if sr, ok := r.(interface{ Size() int64 }); ok {
		return sr.Size(), nil
	}
	if s, ok := r.(io.Seeker); ok {
		cur, err := s.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0, err
		}
		end, err := s.Seek(0, io.SeekEnd)
		if err != nil {
			return 0, err
		}
		if _, err := s.Seek(cur, io.SeekStart); err != nil {
			return 0, err
		}
		return end, nil
	}
	return 0, errors.New("jp2: fragment table requires an input of known size")
}

// fragmentReader builds a reader that concatenates the fragments listed
// in a fragment table box.  Fragment offsets are absolute positions in
// r's [io.ReaderAt] view, so the caller must pass a reader whose origin is
// the start of the file.  A table without fragments or with a truncated
// list is an error.
func fragmentReader(r io.Reader, ftbl []byte, charge func(int) error) (io.Reader, error) {
	ra, ok := r.(io.ReaderAt)
	if !ok {
		return nil, errors.New("jp2: fragment table requires a seekable input")
	}
	boxes, err := splitBoxes(ftbl, charge)
	if err != nil {
		return nil, err
	}
	for _, b := range boxes {
		if b.Type != typeFlst {
			continue
		}
		d := b.Data
		if len(d) < 2 {
			return nil, errMalformed
		}
		nf := int(binary.BigEndian.Uint16(d))
		d = d[2:]
		if nf == 0 {
			return nil, errors.New("jp2: empty fragment list")
		}
		if len(d) < 14*nf {
			return nil, errors.New("jp2: truncated fragment list")
		}
		if err := charge(nf * 64); err != nil {
			return nil, err
		}
		// Every fragment must lie inside the input and the fragments
		// together must not exceed it, so that the codestream cannot be
		// larger than the file; without a way to learn the size of the
		// input the table cannot be checked and is rejected.
		size, err := inputSize(r)
		if err != nil {
			return nil, err
		}
		var parts []io.Reader
		var covered int64
		for range nf {
			off := binary.BigEndian.Uint64(d)
			length := binary.BigEndian.Uint32(d[8:])
			dr := binary.BigEndian.Uint16(d[12:])
			d = d[14:]
			if dr != 0 {
				return nil, errors.New("jp2: unsupported fragment in external file")
			}
			if off > math.MaxInt64-math.MaxUint32 {
				return nil, errors.New("jp2: invalid fragment offset")
			}
			if int64(off)+int64(length) > size {
				return nil, errors.New("jp2: fragment beyond the end of the file")
			}
			covered += int64(length)
			if covered > size {
				return nil, errors.New("jp2: fragments larger than the file")
			}
			parts = append(parts, io.NewSectionReader(ra, int64(off), int64(length)))
		}
		return io.MultiReader(parts...), nil
	}
	return nil, errors.New("jp2: fragment table without fragment list")
}
