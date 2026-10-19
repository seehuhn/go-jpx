// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package t2

import (
	"testing"
	"unsafe"
)

// counting returns a Charge which sums the bytes charged into *total.
func counting(total *int) Charge {
	return func(n int) error {
		*total += n
		return nil
	}
}

// The memory held by the segment and chunk lists of a code-block is
// charged.
func TestAddPieceCharge(t *testing.T) {
	const n = 1 << 16
	data := []byte{1, 2, 3}
	for _, closes := range []bool{false, true} {
		cb := &CodeBlock{}
		var charged int
		for range n {
			if err := cb.addPiece(data, 1, closes, counting(&charged)); err != nil {
				t.Fatal(err)
			}
		}
		held := cap(cb.Segments) * int(unsafe.Sizeof(Segment{}))
		for _, seg := range cb.Segments {
			held += cap(seg.Chunks) * int(unsafe.Sizeof(data))
		}
		if charged < held {
			t.Errorf("closes=%t: holds %d bytes, charged only %d", closes, held, charged)
		}
	}
}

// The memory held by a grown piece list is charged.
func TestGrowPiecesCharge(t *testing.T) {
	const n = 1 << 16
	var pieces []Piece
	var charged int
	for range n {
		if err := appendCharged(&pieces, Piece{length: 1}, counting(&charged)); err != nil {
			t.Fatal(err)
		}
	}
	if len(pieces) != n {
		t.Fatalf("got %d pieces, want %d", len(pieces), n)
	}
	if held := cap(pieces) * int(unsafe.Sizeof(Piece{})); charged < held {
		t.Errorf("holds %d bytes, charged only %d", held, charged)
	}
}
