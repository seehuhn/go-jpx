// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package codestream

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// SIZ is the image and tile size marker segment.
type SIZ struct {
	Rsiz           uint16
	Xsiz, Ysiz     uint32 // reference grid size
	XOsiz, YOsiz   uint32 // image offset
	XTsiz, YTsiz   uint32 // tile size
	XTOsiz, YTOsiz uint32 // tile offset
	Components     []ComponentSIZ
}

// ComponentSIZ describes one image component.
type ComponentSIZ struct {
	Depth        int // bits per sample, 1..38
	Signed       bool
	XRsiz, YRsiz int // sub-sampling factors, 1..255
}

const maxTiles = 65535

// NumTiles returns the number of tiles in each direction.  It reports
// ok=false if a tile size is zero or the geometry is inconsistent.
func (s *SIZ) NumTiles() (nx, ny int, ok bool) {
	if s.XTsiz == 0 || s.YTsiz == 0 || s.XTOsiz > s.Xsiz || s.YTOsiz > s.Ysiz {
		return 0, 0, false
	}
	x := (uint64(s.Xsiz-s.XTOsiz) + uint64(s.XTsiz) - 1) / uint64(s.XTsiz)
	y := (uint64(s.Ysiz-s.YTOsiz) + uint64(s.YTsiz) - 1) / uint64(s.YTsiz)
	if x > math.MaxInt || y > math.MaxInt {
		return 0, 0, false
	}
	return int(x), int(y), true
}

// Validate checks the geometry and component parameters.
func (s *SIZ) Validate() error {
	if len(s.Components) < 1 || len(s.Components) > 16384 {
		return fmt.Errorf("invalid component count %d", len(s.Components))
	}
	if s.XTsiz == 0 || s.YTsiz == 0 {
		return errors.New("zero tile size")
	}
	if s.Xsiz <= s.XOsiz || s.Ysiz <= s.YOsiz {
		return errors.New("empty image area")
	}
	if s.XTOsiz > s.XOsiz || s.YTOsiz > s.YOsiz {
		return errors.New("tile offset exceeds image offset")
	}
	if uint64(s.XTOsiz)+uint64(s.XTsiz) <= uint64(s.XOsiz) ||
		uint64(s.YTOsiz)+uint64(s.YTsiz) <= uint64(s.YOsiz) {
		return errors.New("first tile does not intersect the image")
	}
	nx, ny, ok := s.NumTiles()
	if !ok || uint64(nx)*uint64(ny) > maxTiles {
		return errors.New("invalid number of tiles")
	}
	for i, c := range s.Components {
		if c.Depth < 1 || c.Depth > 38 {
			return fmt.Errorf("component %d: invalid bit depth %d", i, c.Depth)
		}
		if c.XRsiz < 1 || c.XRsiz > 255 || c.YRsiz < 1 || c.YRsiz > 255 {
			return fmt.Errorf("component %d: invalid sub-sampling", i)
		}
	}
	return nil
}

func parseSIZ(p []byte) (*SIZ, error) {
	if len(p) < 36 {
		return nil, errors.New("SIZ segment too short")
	}
	be := binary.BigEndian
	s := &SIZ{
		Rsiz:   be.Uint16(p),
		Xsiz:   be.Uint32(p[2:]),
		Ysiz:   be.Uint32(p[6:]),
		XOsiz:  be.Uint32(p[10:]),
		YOsiz:  be.Uint32(p[14:]),
		XTsiz:  be.Uint32(p[18:]),
		YTsiz:  be.Uint32(p[22:]),
		XTOsiz: be.Uint32(p[26:]),
		YTOsiz: be.Uint32(p[30:]),
	}
	csiz := int(be.Uint16(p[34:]))
	if csiz < 1 || csiz > 16384 {
		return nil, fmt.Errorf("invalid component count %d", csiz)
	}
	if len(p) != 36+3*csiz {
		return nil, errors.New("SIZ length does not match component count")
	}
	s.Components = make([]ComponentSIZ, csiz)
	for i := range s.Components {
		q := p[36+3*i:]
		s.Components[i] = ComponentSIZ{
			Depth:  int(q[0]&0x7F) + 1,
			Signed: q[0]&0x80 != 0,
			XRsiz:  int(q[1]),
			YRsiz:  int(q[2]),
		}
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return s, nil
}

func encodeSIZ(s *SIZ) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	p := make([]byte, 36, 36+3*len(s.Components))
	be := binary.BigEndian
	be.PutUint16(p, s.Rsiz)
	for i, v := range []uint32{s.Xsiz, s.Ysiz, s.XOsiz, s.YOsiz, s.XTsiz, s.YTsiz, s.XTOsiz, s.YTOsiz} {
		be.PutUint32(p[2+4*i:], v)
	}
	be.PutUint16(p[34:], uint16(len(s.Components)))
	for _, c := range s.Components {
		d := byte(c.Depth - 1)
		if c.Signed {
			d |= 0x80
		}
		p = append(p, d, byte(c.XRsiz), byte(c.YRsiz))
	}
	return p, nil
}
