// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jp2

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
)

// Write emits a JP2 (or JPX, depending on the brand) wrapper around the
// codestream.  Boxes are written in the standard order: signature, file
// type, reader requirements if present, the header superbox, and finally
// the codestream box.  Empty fields are omitted.  If h.Brand is empty, it
// defaults to "jp2 ", and so does the compatibility list if it is nil.
func Write(w io.Writer, h *Header, codestream []byte) error {
	if h == nil {
		return errors.New("jp2: nil header")
	}
	jp2h, err := h.encodeJp2h()
	if err != nil {
		return err
	}

	brand, compat := h.Brand, h.Compat
	if brand == "" {
		brand = "jp2 "
		if compat == nil {
			compat = []string{"jp2 "}
		}
	}
	if len(brand) != 4 {
		return errBrand
	}
	ftyp := append([]byte(brand), 0, 0, 0, 0)
	for _, c := range compat {
		if len(c) != 4 {
			return errBrand
		}
		ftyp = append(ftyp, c...)
	}

	buf := append([]byte(nil), signature...)
	buf = appendBox(buf, typeFtyp, ftyp)
	if h.ReaderRequirements != nil {
		buf = appendBox(buf, typeRreq, h.ReaderRequirements)
	}
	buf = append(buf, jp2h...)
	if _, err := w.Write(buf); err != nil {
		return err
	}
	hdr := appendBox(nil, typeJp2c, nil)
	if uint64(len(codestream))+8 > math.MaxUint32 {
		hdr = binary.BigEndian.AppendUint32(nil, 1)
		hdr = append(hdr, typeJp2c...)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(len(codestream))+16)
	} else {
		binary.BigEndian.PutUint32(hdr, uint32(len(codestream))+8)
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err = w.Write(codestream)
	return err
}
