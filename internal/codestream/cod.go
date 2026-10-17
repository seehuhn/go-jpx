// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package codestream

import (
	"errors"
	"fmt"
)

// CodingStyle is the per-component coding style, the merged view of the
// component-specific parts of COD and COC.
type CodingStyle struct {
	// code-block style bits
	EntropyBypass          bool
	ResetContexts          bool
	Terminate              bool
	VerticallyCausal       bool
	PredictableTermination bool
	SegmentationSymbols    bool

	Levels        int  // number of decomposition levels, 0..32
	CodeBlockW    int  // log2 of the nominal code-block width, 2..10
	CodeBlockH    int  // log2 of the nominal code-block height, 2..10
	Reversible    bool // 5/3 filter when true, 9/7 when false
	PrecinctSizes []PrecinctSize
}

// PrecinctSize gives the log2 precinct size at one resolution level.
type PrecinctSize struct{ PPx, PPy int }

// COD is the tile-wide default coding style marker segment.
type COD struct {
	CodingStyle
	Progression Progression
	Layers      int
	MCT         bool
	SOP, EPH    bool
}

// DefaultPrecincts returns n precinct sizes of 2^15 by 2^15.
func DefaultPrecincts(n int) []PrecinctSize {
	ps := make([]PrecinctSize, n)
	for i := range ps {
		ps[i] = PrecinctSize{15, 15}
	}
	return ps
}

func (c *CodingStyle) validate() error {
	if c.Levels < 0 || c.Levels > 32 {
		return fmt.Errorf("invalid number of decomposition levels %d", c.Levels)
	}
	if c.CodeBlockW < 2 || c.CodeBlockW > 10 || c.CodeBlockH < 2 || c.CodeBlockH > 10 ||
		c.CodeBlockW+c.CodeBlockH > 12 {
		return errors.New("invalid code-block size")
	}
	if len(c.PrecinctSizes) != c.Levels+1 {
		return errors.New("wrong number of precinct sizes")
	}
	for r, p := range c.PrecinctSizes {
		if p.PPx < 0 || p.PPx > 15 || p.PPy < 0 || p.PPy > 15 {
			return errors.New("invalid precinct size")
		}
		if r > 0 && (p.PPx == 0 || p.PPy == 0) {
			return errors.New("zero precinct size above the lowest resolution")
		}
	}
	return nil
}

func (c *CodingStyle) defaultPrecincts() bool {
	for _, p := range c.PrecinctSizes {
		if p != (PrecinctSize{15, 15}) {
			return false
		}
	}
	return true
}

func (c *CodingStyle) styleByte() byte {
	var b byte
	for i, f := range []bool{c.EntropyBypass, c.ResetContexts, c.Terminate,
		c.VerticallyCausal, c.PredictableTermination, c.SegmentationSymbols} {
		if f {
			b |= 1 << i
		}
	}
	return b
}

// encodeSP returns the SPcod/SPcoc bytes and whether precincts are explicit.
func (c *CodingStyle) encodeSP() ([]byte, bool) {
	explicit := !c.defaultPrecincts()
	p := []byte{byte(c.Levels), byte(c.CodeBlockW - 2), byte(c.CodeBlockH - 2), c.styleByte(), 0}
	if c.Reversible {
		p[4] = 1
	}
	if explicit {
		for _, ps := range c.PrecinctSizes {
			p = append(p, byte(ps.PPx|ps.PPy<<4))
		}
	}
	return p, explicit
}

// parseSP parses SPcod/SPcoc; it must consume exactly all of p.
func parseSP(p []byte, explicit bool) (CodingStyle, error) {
	var c CodingStyle
	if len(p) < 5 {
		return c, errors.New("coding style too short")
	}
	c.Levels = int(p[0])
	if c.Levels > 32 {
		return c, fmt.Errorf("invalid number of decomposition levels %d", c.Levels)
	}
	c.CodeBlockW = int(p[1]&0x0F) + 2
	c.CodeBlockH = int(p[2]&0x0F) + 2
	if p[1] > 8 || p[2] > 8 {
		return c, errors.New("invalid code-block size")
	}
	st := p[3]
	if st&^0x3F != 0 {
		return c, errors.New("unsupported code-block style")
	}
	c.EntropyBypass = st&1 != 0
	c.ResetContexts = st&2 != 0
	c.Terminate = st&4 != 0
	c.VerticallyCausal = st&8 != 0
	c.PredictableTermination = st&16 != 0
	c.SegmentationSymbols = st&32 != 0
	switch p[4] {
	case 0:
	case 1:
		c.Reversible = true
	default:
		return c, errors.New("unsupported wavelet transform")
	}
	rest := p[5:]
	if explicit {
		if len(rest) != c.Levels+1 {
			return c, errors.New("precinct size count does not match levels")
		}
		c.PrecinctSizes = make([]PrecinctSize, len(rest))
		for i, b := range rest {
			c.PrecinctSizes[i] = PrecinctSize{int(b & 15), int(b >> 4)}
		}
	} else {
		if len(rest) != 0 {
			return c, errors.New("trailing bytes in coding style")
		}
		c.PrecinctSizes = DefaultPrecincts(c.Levels + 1)
	}
	return c, c.validate()
}

func parseCOD(p []byte) (*COD, error) {
	if len(p) < 10 {
		return nil, errors.New("COD segment too short")
	}
	scod := p[0]
	d := &COD{
		SOP:         scod&2 != 0,
		EPH:         scod&4 != 0,
		Progression: Progression(p[1]),
		Layers:      int(p[2])<<8 | int(p[3]),
		MCT:         p[4] == 1,
	}
	if p[4] > 1 {
		return nil, errors.New("invalid multiple component transform")
	}
	if d.Progression > CPRL {
		return nil, errors.New("invalid progression order")
	}
	if d.Layers < 1 {
		return nil, errors.New("zero layers")
	}
	cs, err := parseSP(p[5:], scod&1 != 0)
	if err != nil {
		return nil, err
	}
	d.CodingStyle = cs
	return d, nil
}

func encodeCOD(d *COD) ([]byte, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	if d.Progression > CPRL || d.Layers < 1 || d.Layers > 65535 {
		return nil, errors.New("invalid progression or layer count")
	}
	sp, explicit := d.encodeSP()
	var scod byte
	if explicit {
		scod |= 1
	}
	if d.SOP {
		scod |= 2
	}
	if d.EPH {
		scod |= 4
	}
	var mct byte
	if d.MCT {
		mct = 1
	}
	p := []byte{scod, byte(d.Progression), byte(d.Layers >> 8), byte(d.Layers), mct}
	return append(p, sp...), nil
}

func parseCOC(p []byte, csiz int) (int, *CodingStyle, error) {
	c, p, err := readComponentIndex(p, csiz)
	if err != nil {
		return 0, nil, err
	}
	if len(p) < 1 {
		return 0, nil, errors.New("COC segment too short")
	}
	cs, err := parseSP(p[1:], p[0]&1 != 0)
	if err != nil {
		return 0, nil, err
	}
	return c, &cs, nil
}

func encodeCOC(c int, cs *CodingStyle, csiz int) ([]byte, error) {
	if c < 0 || c >= csiz {
		return nil, errors.New("COC component index out of range")
	}
	if err := cs.validate(); err != nil {
		return nil, err
	}
	sp, explicit := cs.encodeSP()
	p := appendComponentIndex(nil, c, csiz)
	var scoc byte
	if explicit {
		scoc = 1
	}
	p = append(p, scoc)
	return append(p, sp...), nil
}

// readComponentIndex reads an 8- or 16-bit component index depending on csiz.
func readComponentIndex(p []byte, csiz int) (int, []byte, error) {
	var c int
	if csiz < 257 {
		if len(p) < 1 {
			return 0, nil, errors.New("segment too short")
		}
		c, p = int(p[0]), p[1:]
	} else {
		if len(p) < 2 {
			return 0, nil, errors.New("segment too short")
		}
		c, p = int(p[0])<<8|int(p[1]), p[2:]
	}
	if c >= csiz {
		return 0, nil, fmt.Errorf("component index %d out of range", c)
	}
	return c, p, nil
}

func appendComponentIndex(p []byte, c, csiz int) []byte {
	if csiz < 257 {
		return append(p, byte(c))
	}
	return append(p, byte(c>>8), byte(c))
}
