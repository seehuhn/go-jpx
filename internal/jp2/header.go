// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jp2

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Header is everything the JP2/JPX wrapper says about the image.
type Header struct {
	HasWrapper         bool               // false for a bare codestream
	Brand              string             // brand from the file type box, e.g. "jp2 "
	Compat             []string           // compatibility list from the file type box
	Image              *ImageHeader       // image header box, nil if absent
	BitDepths          []BitDepth         // bits per component box, nil if absent
	Colors             []ColorSpec        // every colour specification box, in file order
	Palette            *Palette           // palette box, nil if absent
	ComponentMap       []ComponentMapping // component mapping entries, nil if absent
	Channels           []ChannelDef       // channel definition entries, nil if absent
	ReaderRequirements []byte             // raw reader requirements payload, not interpreted
}

// ImageHeader holds the fields of the image header box.
type ImageHeader struct {
	Height, Width uint32
	NC            uint16   // number of components
	BPC           BitDepth // common bit depth; Depth 0 means the depths vary
	C             uint8    // compression type
	UnkC          bool     // colourspace unknown
	IPR           bool     // file contains intellectual property rights box
}

// BitDepth is a bit depth together with a signedness flag.  A Depth of 0
// stands for the "depths vary, see the bits per component box" marker.
type BitDepth struct {
	Depth  int
	Signed bool
}

// decodeBitDepth decodes one bit depth byte.  It reports false for depths
// above 38 bits, which are invalid.
func decodeBitDepth(b byte) (BitDepth, bool) {
	if b == 0xFF {
		return BitDepth{}, true
	}
	d := BitDepth{Depth: int(b&0x7F) + 1, Signed: b&0x80 != 0}
	return d, d.Depth <= 38
}

func (d BitDepth) encode() (byte, error) {
	if d.Depth == 0 {
		if d.Signed {
			return 0, errors.New("jp2: signedness given for unspecified bit depth")
		}
		return 0xFF, nil
	}
	if d.Depth < 1 || d.Depth > 38 {
		return 0, fmt.Errorf("jp2: invalid bit depth %d", d.Depth)
	}
	b := byte(d.Depth - 1)
	if d.Signed {
		b |= 0x80
	}
	return b, nil
}

// ColorSpec is one colour specification box.
type ColorSpec struct {
	Method     uint8  // 1 enumerated, 2 restricted ICC, 3 any ICC, 4 vendor
	Precedence int8   // precedence among several boxes (JPX)
	Approx     uint8  // approximation quality (JPX)
	EnumCS     uint32 // enumerated colourspace, for Method 1
	EnumParams []byte // parameters following EnumCS, for Method 1
	ICC        []byte // ICC profile, for Methods 2 and 3
	Vendor     []byte // raw method data for Method 4 and unknown methods
}

// Palette is a palette box.
type Palette struct {
	Entries int
	Depths  []BitDepth // one per column
	Values  [][]int32  // Values[entry][column], sign-extended if signed
}

// ComponentMapping is one entry of the component mapping box.
type ComponentMapping struct {
	Component     uint16
	MappingType   uint8
	PaletteColumn uint8
}

// ChannelDef is one entry of the channel definition box.
type ChannelDef struct {
	Channel uint16
	Type    uint16
	Assoc   uint16
}

var errMalformed = errors.New("jp2: malformed header box")
var errBrand = errors.New("jp2: brand and compatibility entries must be 4 bytes")

// parseFtyp parses a file type box payload into h.
func (h *Header) parseFtyp(data []byte, charge func(int) error) error {
	if len(data) < 8 {
		return errMalformed
	}
	if err := charge(16 + 20*(len(data)/4)); err != nil {
		return err
	}
	h.Brand = string(data[:4])
	h.Compat = nil
	if n := (len(data) - 8) / 4; n > 0 {
		h.Compat = make([]string, 0, n)
	}
	for p := data[8:]; len(p) >= 4; p = p[4:] {
		h.Compat = append(h.Compat, string(p[:4]))
	}
	return nil
}

// parseJp2h parses the children of a header superbox into h.  Unknown
// children are skipped and so are malformed ones, which leave the
// corresponding field unset.  For repeated singleton boxes the first
// valid one wins.  An error is returned only if the box framing cannot be
// followed, if a palette is unrepresentable, or if charge fails.
func (h *Header) parseJp2h(data []byte, charge func(int) error) error {
	boxes, err := splitBoxes(data, charge)
	if err != nil {
		return err
	}
	colors := 0
	for _, b := range boxes {
		if b.Type == typeColr {
			colors++
		}
	}
	if err := charge(colors * 96); err != nil {
		return err
	}
	if colors > 0 && h.Colors == nil {
		h.Colors = make([]ColorSpec, 0, colors)
	}
	defer func() {
		if len(h.Colors) == 0 {
			h.Colors = nil
		}
	}()
	// tolerate malformed boxes, but not other errors
	tol := func(err error) error {
		if errors.Is(err, errMalformed) {
			return nil
		}
		return err
	}
	for _, b := range boxes {
		switch b.Type {
		case typeIhdr:
			if h.Image == nil {
				if h.Image, err = parseIhdr(b.Data); err != nil {
					h.Image = nil
					if err = tol(err); err != nil {
						return err
					}
				}
			}
		case typeBpcc:
			if h.BitDepths == nil {
				if err := charge(16 * len(b.Data)); err != nil {
					return err
				}
				var depths []BitDepth
				for _, c := range b.Data {
					d, ok := decodeBitDepth(c)
					if !ok || d.Depth == 0 {
						depths = nil
						break
					}
					depths = append(depths, d)
				}
				h.BitDepths = depths
			}
		case typeColr:
			if err := charge(len(b.Data) + 64); err != nil {
				return err
			}
			cs, err := parseColr(b.Data)
			if err != nil {
				if err = tol(err); err != nil {
					return err
				}
				continue
			}
			h.Colors = append(h.Colors, cs)
		case typePclr:
			if h.Palette == nil {
				pal, err := parsePclr(b.Data, charge)
				if err != nil {
					if err = tol(err); err != nil {
						return err
					}
					continue
				}
				h.Palette = pal
			}
		case typeCmap:
			if h.ComponentMap == nil && len(b.Data)%4 == 0 {
				if err := charge(len(b.Data)); err != nil {
					return err
				}
				for p := b.Data; len(p) > 0; p = p[4:] {
					h.ComponentMap = append(h.ComponentMap, ComponentMapping{
						Component:     binary.BigEndian.Uint16(p),
						MappingType:   p[2],
						PaletteColumn: p[3],
					})
				}
			}
		case typeCdef:
			if h.Channels == nil && len(b.Data) >= 2 {
				n := int(binary.BigEndian.Uint16(b.Data))
				p := b.Data[2:]
				if len(p) < 6*n {
					continue
				}
				if err := charge(6 * n); err != nil {
					return err
				}
				for range n {
					h.Channels = append(h.Channels, ChannelDef{
						Channel: binary.BigEndian.Uint16(p),
						Type:    binary.BigEndian.Uint16(p[2:]),
						Assoc:   binary.BigEndian.Uint16(p[4:]),
					})
					p = p[6:]
				}
			}
		}
	}
	// a per-component depth list must match the declared component count
	if h.Image != nil && h.BitDepths != nil && len(h.BitDepths) != int(h.Image.NC) {
		h.BitDepths = nil
	}
	return nil
}

func parseIhdr(data []byte) (*ImageHeader, error) {
	if len(data) != 14 {
		return nil, errMalformed
	}
	bpc, ok := decodeBitDepth(data[10])
	if !ok {
		return nil, errMalformed
	}
	return &ImageHeader{
		Height: binary.BigEndian.Uint32(data),
		Width:  binary.BigEndian.Uint32(data[4:]),
		NC:     binary.BigEndian.Uint16(data[8:]),
		BPC:    bpc,
		C:      data[11],
		UnkC:   data[12] != 0,
		IPR:    data[13] != 0,
	}, nil
}

func parseColr(data []byte) (ColorSpec, error) {
	if len(data) < 3 {
		return ColorSpec{}, errMalformed
	}
	cs := ColorSpec{Method: data[0], Precedence: int8(data[1]), Approx: data[2]}
	rest := data[3:]
	switch cs.Method {
	case 1:
		if len(rest) < 4 {
			return cs, errMalformed
		}
		cs.EnumCS = binary.BigEndian.Uint32(rest)
		if len(rest) > 4 {
			cs.EnumParams = append([]byte{}, rest[4:]...)
		}
	case 2, 3:
		cs.ICC = append([]byte{}, rest...)
	default:
		cs.Vendor = append([]byte{}, rest...)
	}
	return cs, nil
}

func parsePclr(data []byte, charge func(int) error) (*Palette, error) {
	if len(data) < 3 {
		return nil, errMalformed
	}
	ne := int(binary.BigEndian.Uint16(data))
	npc := int(data[2])
	if ne < 1 || ne > 1024 || npc < 1 || len(data) < 3+npc {
		return nil, errMalformed
	}
	pal := &Palette{Entries: ne}
	size := 0
	for _, b := range data[3 : 3+npc] {
		d, ok := decodeBitDepth(b)
		if !ok || d.Depth < 1 {
			return nil, errMalformed
		}
		if d.Depth > 31 {
			// not representable in int32 samples
			return nil, errMalformed
		}
		if err := charge(16); err != nil {
			return nil, err
		}
		pal.Depths = append(pal.Depths, d)
		size += (d.Depth + 7) / 8
	}
	p := data[3+npc:]
	if len(p) < size*ne {
		return nil, errMalformed
	}
	if err := charge(ne * (4*npc + 24 + 16)); err != nil {
		return nil, err
	}
	for range ne {
		row := make([]int32, npc)
		for j, d := range pal.Depths {
			var v uint32
			for range (d.Depth + 7) / 8 {
				v = v<<8 | uint32(p[0])
				p = p[1:]
			}
			x := int64(v) & (1<<d.Depth - 1)
			if d.Signed && x >= 1<<(d.Depth-1) {
				x -= 1 << d.Depth
			}
			row[j] = int32(x)
		}
		pal.Values = append(pal.Values, row)
	}
	return pal, nil
}

// encodeJp2h returns the jp2h box for h, or nil if there is nothing to write.
func (h *Header) encodeJp2h() ([]byte, error) {
	var in []byte
	if im := h.Image; im != nil {
		b, err := im.BPC.encode()
		if err != nil {
			return nil, err
		}
		p := binary.BigEndian.AppendUint32(nil, im.Height)
		p = binary.BigEndian.AppendUint32(p, im.Width)
		p = binary.BigEndian.AppendUint16(p, im.NC)
		p = append(p, b, im.C, boolByte(im.UnkC), boolByte(im.IPR))
		in = appendBox(in, typeIhdr, p)
	}
	if len(h.BitDepths) > 0 {
		var p []byte
		for _, d := range h.BitDepths {
			b, err := d.encode()
			if err == nil && d.Depth == 0 {
				err = errors.New("jp2: unspecified depth in per-component list")
			}
			if err != nil {
				return nil, err
			}
			p = append(p, b)
		}
		in = appendBox(in, typeBpcc, p)
	}
	for _, cs := range h.Colors {
		p := []byte{cs.Method, byte(cs.Precedence), cs.Approx}
		switch cs.Method {
		case 1:
			if len(cs.ICC) > 0 {
				return nil, errors.New("jp2: ICC profile given for enumerated colour space")
			}
			p = binary.BigEndian.AppendUint32(p, cs.EnumCS)
			p = append(p, cs.EnumParams...)
		case 2, 3:
			if cs.EnumCS != 0 || len(cs.EnumParams) > 0 {
				return nil, errors.New("jp2: enumerated fields given for ICC colour space")
			}
			p = append(p, cs.ICC...)
		default:
			p = append(p, cs.Vendor...)
		}
		in = appendBox(in, typeColr, p)
	}
	if pal := h.Palette; pal != nil {
		p, err := pal.encode()
		if err != nil {
			return nil, err
		}
		in = appendBox(in, typePclr, p)
	}
	if len(h.ComponentMap) > 0 {
		var p []byte
		for _, m := range h.ComponentMap {
			p = binary.BigEndian.AppendUint16(p, m.Component)
			p = append(p, m.MappingType, m.PaletteColumn)
		}
		in = appendBox(in, typeCmap, p)
	}
	if h.Channels != nil {
		if len(h.Channels) > 0xFFFF {
			return nil, errors.New("jp2: too many channel definitions")
		}
		p := binary.BigEndian.AppendUint16(nil, uint16(len(h.Channels)))
		for _, c := range h.Channels {
			p = binary.BigEndian.AppendUint16(p, c.Channel)
			p = binary.BigEndian.AppendUint16(p, c.Type)
			p = binary.BigEndian.AppendUint16(p, c.Assoc)
		}
		in = appendBox(in, typeCdef, p)
	}
	if in == nil {
		return nil, nil
	}
	return appendBox(nil, typeJp2h, in), nil
}

func (pal *Palette) encode() ([]byte, error) {
	npc := len(pal.Depths)
	if pal.Entries < 1 || pal.Entries > 1024 || npc < 1 || npc > 255 ||
		len(pal.Values) != pal.Entries {
		return nil, errors.New("jp2: invalid palette dimensions")
	}
	p := binary.BigEndian.AppendUint16(nil, uint16(pal.Entries))
	p = append(p, byte(npc))
	for _, d := range pal.Depths {
		if d.Depth < 1 || d.Depth > 31 {
			return nil, errors.New("jp2: unsupported palette depth")
		}
		b, _ := d.encode()
		p = append(p, b)
	}
	for _, row := range pal.Values {
		if len(row) != npc {
			return nil, errors.New("jp2: invalid palette dimensions")
		}
		for j, v := range row {
			d := pal.Depths[j]
			lo, hi := int64(0), int64(1)<<d.Depth-1
			if d.Signed {
				lo, hi = -(int64(1) << (d.Depth - 1)), int64(1)<<(d.Depth-1)-1
			}
			if int64(v) < lo || int64(v) > hi {
				return nil, errors.New("jp2: palette value out of range for its depth")
			}
			nb := (pal.Depths[j].Depth + 7) / 8
			for k := nb - 1; k >= 0; k-- {
				p = append(p, byte(uint32(v)>>(8*k)))
			}
		}
	}
	return p, nil
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}
