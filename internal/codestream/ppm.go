// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package codestream

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"unsafe"
)

// PPMSegment is one PPM marker segment of the main header.  A codestream
// must use either PPM or PPT segments, never both.
type PPMSegment struct {
	Z    int // Zppm, the index of the segment
	Data []byte
}

// PPTSegment is one PPT marker segment of a tile-part header.
type PPTSegment struct {
	Z    int // Zppt, the index of the segment
	Data []byte
}

func parseZ(p []byte) (int, []byte, error) {
	if len(p) < 1 {
		return 0, nil, errors.New("packed header segment too short")
	}
	return int(p[0]), p[1:], nil
}

// sortedSegs returns segs ordered by Z.  The input is returned as is when it
// is already ordered; otherwise a sorted copy is made, charged through charge.
func sortedSegs[S any](segs []S, z func(S) int, charge func(int) error) ([]S, error) {
	if slices.IsSortedFunc(segs, func(a, b S) int { return z(a) - z(b) }) {
		return segs, nil
	}
	if err := chargeN(charge, len(segs)*int(unsafe.Sizeof(segs[0]))); err != nil {
		return nil, err
	}
	segs = slices.Clone(segs)
	slices.SortStableFunc(segs, func(a, b S) int { return z(a) - z(b) })
	return segs, nil
}

// PackedHeaders returns the packet headers of each tile-part, in codestream
// order.  It returns nil if there are no PPM segments.  If the data is
// truncated, the complete chunks found so far are returned with an error.
// If charge is not nil, it is called with the size of every allocation
// before the allocation is made.
func (h *MainHeader) PackedHeaders(charge func(int) error) ([][]byte, error) {
	if len(h.PPM) == 0 {
		return nil, nil
	}
	segs, err := sortedSegs(h.PPM, func(s PPMSegment) int { return s.Z }, charge)
	if err != nil {
		return nil, err
	}
	total := 0
	for i, s := range segs {
		if s.Z != i {
			return nil, fmt.Errorf("PPM segment %d missing or duplicated", i)
		}
		total += len(s.Data)
	}
	if err := chargeN(charge, total); err != nil {
		return nil, err
	}
	all := make([]byte, 0, total)
	for _, s := range segs {
		all = append(all, s.Data...)
	}
	// every chunk consumes at least four bytes of the input
	if err := chargeN(charge, total/4*int(unsafe.Sizeof([]byte(nil)))); err != nil {
		return nil, err
	}
	var res [][]byte
	for len(all) > 0 {
		if len(all) < 4 {
			return res, errors.New("truncated PPM length")
		}
		n := binary.BigEndian.Uint32(all)
		all = all[4:]
		if uint64(n) > uint64(len(all)) {
			return res, errors.New("PPM chunk exceeds available data")
		}
		res = append(res, all[:n:n])
		all = all[n:]
	}
	return res, nil
}

// PackedHeaders returns the concatenated PPT data of the tile-part in Z
// order, or nil if there is none.  Indices need not start at zero, since
// encoders number the segments either per tile-part or across the
// tile-parts of a tile; a duplicated index is an error.  If charge is not nil, it is called with the size of every
// allocation before the allocation is made.
func (t *TilePart) PackedHeaders(charge func(int) error) ([]byte, error) {
	if len(t.PPT) == 0 {
		return nil, nil
	}
	segs, err := sortedSegs(t.PPT, func(s PPTSegment) int { return s.Z }, charge)
	if err != nil {
		return nil, err
	}
	total := 0
	// Encoders either restart the index in every tile-part or continue
	// counting across the tile-parts of a tile; only duplicates within one
	// tile-part header are rejected.
	for i, s := range segs {
		if i > 0 && s.Z == segs[i-1].Z {
			return nil, fmt.Errorf("duplicate PPT segment %d", s.Z)
		}
		total += len(s.Data)
	}
	if err := chargeN(charge, total); err != nil {
		return nil, err
	}
	all := make([]byte, 0, total)
	for _, s := range segs {
		all = append(all, s.Data...)
	}
	return all, nil
}
