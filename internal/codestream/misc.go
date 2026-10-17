// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package codestream

import (
	"encoding/binary"
	"errors"
)

// Comment is a COM marker segment.
type Comment struct {
	Registration uint16 // 0 for binary data, 1 for Latin-1 text
	Data         []byte
}

// CRG is the component registration marker segment.
type CRG struct {
	Xcrg, Ycrg []uint16
}

func parseCOM(p []byte) (Comment, error) {
	if len(p) < 2 {
		return Comment{}, errors.New("COM segment too short")
	}
	return Comment{binary.BigEndian.Uint16(p), p[2:]}, nil
}

func parseCRG(p []byte, csiz int) (*CRG, error) {
	if len(p) != 4*csiz {
		return nil, errors.New("CRG length does not match component count")
	}
	g := &CRG{make([]uint16, csiz), make([]uint16, csiz)}
	for i := range csiz {
		g.Xcrg[i] = binary.BigEndian.Uint16(p[4*i:])
		g.Ycrg[i] = binary.BigEndian.Uint16(p[4*i+2:])
	}
	return g, nil
}

func encodeCRG(g *CRG, csiz int) ([]byte, error) {
	if len(g.Xcrg) != csiz || len(g.Ycrg) != csiz {
		return nil, errors.New("CRG length does not match component count")
	}
	var p []byte
	for i := range csiz {
		p = binary.BigEndian.AppendUint16(p, g.Xcrg[i])
		p = binary.BigEndian.AppendUint16(p, g.Ycrg[i])
	}
	return p, nil
}
