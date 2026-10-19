// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jpx

import (
	"errors"
	"io"

	"seehuhn.de/go/jpx/internal/codestream"
	"seehuhn.de/go/jpx/internal/jp2"
	"seehuhn.de/go/membudget"
)

// ReadHeader parses the file structure and the main codestream header of
// a JPEG 2000 image.  It reads only as far as needed and keeps only the
// header information; the sample data is not decoded.  Unlike Decode, it
// also reports images with components deeper than MaxDepth bits, so that
// a caller can tell why Decode rejects them.
//
// Memory use is bounded by the size of the header boxes and marker
// segments actually read, and charged to budget, which may be nil.  JPX
// files whose codestream is given by a fragment table need an r that
// implements io.ReaderAt.
func ReadHeader(r io.Reader, budget *membudget.Budget) (*Header, error) {
	charge := chargeFunc(budget)
	wrapper, cs, err := jp2.Read(r, charge)
	if err != nil {
		return nil, err
	}
	main, err := codestream.ReadMainHeaderFrom(cs, charge)
	if err != nil {
		return nil, err
	}
	return buildHeader(wrapper, main, charge)
}

// buildHeader combines the JP2 wrapper information and the codestream
// main header into the public Header.
func buildHeader(w *jp2.Header, m *codestream.MainHeader, charge func(int) error) (*Header, error) {
	siz := &m.SIZ
	h := &Header{
		XOffset: int(siz.XOsiz),
		YOffset: int(siz.YOsiz),
		Width:   int(siz.Xsiz - siz.XOsiz),
		Height:  int(siz.Ysiz - siz.YOsiz),
		Wrapped: w.HasWrapper,
	}
	nx, ny, ok := siz.NumTiles()
	if !ok {
		return nil, errors.New("jpx: invalid tile geometry")
	}
	h.Tiles = nx * ny

	// the header's slices are sized by the boxes and marker segments
	// read, and charged like them
	numChannels := len(siz.Components)
	if len(w.ComponentMap) > 0 {
		numChannels = max(numChannels, len(w.ComponentMap))
	}
	if err := chargeOpt(charge, 64*len(siz.Components)+48*numChannels+128*len(w.Colors)+256); err != nil {
		return nil, err
	}

	h.Components = make([]Component, len(siz.Components))
	for i, c := range siz.Components {
		h.Components[i] = Component{
			Depth:      c.Depth,
			Signed:     c.Signed,
			Truncation: depthTruncation(c.Depth),
			DX:         c.XRsiz,
			DY:         c.YRsiz,
			Width:      ceilDiv(int(siz.Xsiz), c.XRsiz) - ceilDiv(int(siz.XOsiz), c.XRsiz),
			Height:     ceilDiv(int(siz.Ysiz), c.YRsiz) - ceilDiv(int(siz.YOsiz), c.YRsiz),
		}
	}

	h.Resolutions = m.COD.Levels + 1
	for c := range siz.Components {
		if coc, ok := m.COC[c]; ok {
			h.Resolutions = min(h.Resolutions, coc.Levels+1)
		}
	}

	if len(w.Colors) > 0 {
		h.ColorSpecs = make([]ColorSpec, 0, len(w.Colors))
	}
	for _, cs := range w.Colors {
		h.ColorSpecs = append(h.ColorSpecs, ColorSpec{
			Method:     int(cs.Method),
			Precedence: int(cs.Precedence),
			Approx:     int(cs.Approx),
			Enumerated: int(cs.EnumCS),
			EnumParams: cs.EnumParams,
			ICC:        cs.ICC,
			Vendor:     cs.Vendor,
		})
	}

	// a palette needs a mapping box with valid references; otherwise the
	// pair is ignored and the components are the channels
	if w.Palette != nil && validMapping(w, len(h.Components)) {
		h.Palette = &Palette{Entries: w.Palette.Values}
		for _, d := range w.Palette.Depths {
			h.Palette.Columns = append(h.Palette.Columns, PaletteColumn{Depth: d.Depth, Signed: d.Signed, Truncation: jp2.PaletteTruncation(d.Depth)})
		}
	}

	h.Channels = buildChannels(w, h)
	return h, nil
}

// validMapping reports whether the component mapping box exists and
// refers only to existing components and palette columns.
func validMapping(w *jp2.Header, numComps int) bool {
	if len(w.ComponentMap) == 0 {
		return false
	}
	for _, cm := range w.ComponentMap {
		if int(cm.Component) >= numComps {
			return false
		}
		if cm.MappingType == 1 && int(cm.PaletteColumn) >= len(w.Palette.Depths) {
			return false
		}
	}
	return true
}

// buildChannels derives the channel list from the component mapping and
// channel definition boxes, synthesising defaults where they are absent.
func buildChannels(w *jp2.Header, h *Header) []Channel {
	channels := make([]Channel, 0, len(h.Components))
	if h.Palette != nil {
		channels = make([]Channel, 0, len(w.ComponentMap))
		for _, cm := range w.ComponentMap {
			ch := Channel{Component: int(cm.Component), PaletteColumn: -1}
			if cm.MappingType == 1 {
				ch.PaletteColumn = int(cm.PaletteColumn)
			}
			channels = append(channels, ch)
		}
	} else {
		for i := range h.Components {
			channels = append(channels, Channel{Component: i, PaletteColumn: -1})
		}
	}
	for i := range channels {
		channels[i].Kind = ChannelColor
		channels[i].Assoc = i + 1
	}
	for _, cd := range w.Channels {
		i := int(cd.Channel)
		if i >= len(channels) {
			continue
		}
		channels[i].Type = int(cd.Type)
		switch cd.Type {
		case 0:
			channels[i].Kind = ChannelColor
		case 1:
			channels[i].Kind = ChannelOpacity
		case 2:
			channels[i].Kind = ChannelPremultipliedOpacity
		default:
			channels[i].Kind = ChannelUnknown
		}
		channels[i].Assoc = int(cd.Assoc)
	}
	return channels
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }
