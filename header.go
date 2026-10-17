// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jpx

import (
	"errors"
	"fmt"
	"io"

	"seehuhn.de/go/jpx/internal/codestream"
	"seehuhn.de/go/jpx/internal/jp2"
)

// ReadHeader parses the file structure and the main codestream header of
// a JPEG 2000 image.  It reads only as far as needed and keeps only the
// header information; the sample data is not decoded.  Unlike Decode it
// also reports images whose precision exceeds what the decoder supports.
//
// Memory use is bounded by the size of the header boxes and marker
// segments actually read.  A nil budget means that only this implicit
// bound applies.  JPX files whose codestream is given by a fragment
// table need an r that implements io.ReaderAt.
func ReadHeader(r io.Reader, budget Budget) (*Header, error) {
	charge := chargeFunc(budget)
	wrapper, cs, err := jp2.Read(r, charge)
	if err != nil {
		return nil, err
	}
	main, err := codestream.ReadMainHeaderFrom(cs, charge)
	if err != nil {
		return nil, err
	}
	return buildHeader(wrapper, main)
}

// buildHeader combines the JP2 wrapper information and the codestream
// main header into the public Header.
func buildHeader(w *jp2.Header, m *codestream.MainHeader) (*Header, error) {
	siz := &m.SIZ
	h := &Header{
		Width:   int(siz.Xsiz - siz.XOsiz),
		Height:  int(siz.Ysiz - siz.YOsiz),
		Wrapped: w.HasWrapper,
	}
	nx, ny, ok := siz.NumTiles()
	if !ok {
		return nil, errors.New("jpx: invalid tile geometry")
	}
	h.Tiles = nx * ny

	h.Components = make([]Component, len(siz.Components))
	for i, c := range siz.Components {
		h.Components[i] = Component{
			Depth:  c.Depth,
			Signed: c.Signed,
			DX:     c.XRsiz,
			DY:     c.YRsiz,
			Width:  ceilDiv(int(siz.Xsiz), c.XRsiz) - ceilDiv(int(siz.XOsiz), c.XRsiz),
			Height: ceilDiv(int(siz.Ysiz), c.YRsiz) - ceilDiv(int(siz.YOsiz), c.YRsiz),
		}
	}

	h.Resolutions = m.COD.Levels + 1
	for c := range siz.Components {
		if coc, ok := m.COC[c]; ok {
			h.Resolutions = min(h.Resolutions, coc.Levels+1)
		}
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
			h.Palette.Columns = append(h.Palette.Columns, PaletteColumn{Depth: d.Depth, Signed: d.Signed})
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
	var channels []Channel
	if h.Palette != nil {
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

// checkSupported rejects codestreams that use features outside the
// supported subset.
func checkSupported(m *codestream.MainHeader) error {
	for i, c := range m.SIZ.Components {
		if c.Depth > MaxDepth {
			return unsupported(fmt.Sprintf("component %d has %d bits per sample", i, c.Depth))
		}
	}
	return nil
}
