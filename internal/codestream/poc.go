// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package codestream

import (
	"encoding/binary"
	"errors"
)

// POCEntry is one progression order change.  ResEnd, CompEnd and LayerEnd
// are exclusive bounds.
type POCEntry struct {
	ResStart, CompStart, LayerEnd, ResEnd, CompEnd int
	Progression                                    Progression
}

func parsePOC(p []byte, csiz int) ([]POCEntry, error) {
	size := 7
	if csiz >= 257 {
		size = 9
	}
	if len(p) == 0 || len(p)%size != 0 {
		return nil, errors.New("POC segment has wrong length")
	}
	var res []POCEntry
	for ; len(p) > 0; p = p[size:] {
		var e POCEntry
		e.ResStart = int(p[0])
		q := p[1:]
		if csiz < 257 {
			e.CompStart, q = int(q[0]), q[1:]
		} else {
			e.CompStart, q = int(binary.BigEndian.Uint16(q)), q[2:]
		}
		e.LayerEnd = int(binary.BigEndian.Uint16(q))
		e.ResEnd = int(q[2])
		q = q[3:]
		if csiz < 257 {
			e.CompEnd, q = int(q[0]), q[1:]
		} else {
			e.CompEnd, q = int(binary.BigEndian.Uint16(q)), q[2:]
		}
		if csiz < 257 && e.CompEnd == 0 {
			e.CompEnd = 256 // only the 8-bit form maps zero to 256
		}
		e.Progression = Progression(q[0])
		if err := e.validate(csiz); err != nil {
			return nil, err
		}
		res = append(res, e)
	}
	return res, nil
}

// validate checks the ranges of the entry.  Upper bounds beyond the
// actual number of components or resolution levels are not an error:
// codestreams in circulation use the maximum field value to mean "all",
// and the decoder clamps the ranges.
func (e POCEntry) validate(csiz int) error {
	limit := 256
	if csiz >= 257 {
		limit = 16384
	}
	switch {
	case e.ResStart < 0 || e.ResStart > 32 || e.ResEnd <= e.ResStart || e.ResEnd > 33:
		return errors.New("invalid POC resolution range")
	case e.CompStart < 0 || e.CompEnd <= e.CompStart || e.CompEnd > limit:
		return errors.New("invalid POC component range")
	case e.LayerEnd < 1 || e.LayerEnd > 65535:
		return errors.New("invalid POC layer bound")
	case e.Progression > CPRL:
		return errors.New("invalid POC progression order")
	}
	return nil
}

func encodePOC(es []POCEntry, csiz int) ([]byte, error) {
	if len(es) == 0 {
		return nil, errors.New("empty POC")
	}
	var p []byte
	for _, e := range es {
		if err := e.validate(csiz); err != nil {
			return nil, err
		}
		p = append(p, byte(e.ResStart))
		if csiz < 257 {
			p = append(p, byte(e.CompStart))
		} else {
			p = append(p, byte(e.CompStart>>8), byte(e.CompStart))
		}
		p = append(p, byte(e.LayerEnd>>8), byte(e.LayerEnd), byte(e.ResEnd))
		if csiz < 257 {
			p = append(p, byte(e.CompEnd)) // 256 is written as 0
		} else {
			p = append(p, byte(e.CompEnd>>8), byte(e.CompEnd))
		}
		p = append(p, byte(e.Progression))
	}
	return p, nil
}
