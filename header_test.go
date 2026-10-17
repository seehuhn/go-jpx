// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jpx

import (
	"bytes"
	"testing"

	"github.com/google/go-cmp/cmp"
	"seehuhn.de/go/jpx/internal/codestream"
)

func TestReadHeader(t *testing.T) {
	comps := []codestream.ComponentSIZ{
		{Depth: 8, XRsiz: 1, YRsiz: 1},
		{Depth: 16, Signed: true, XRsiz: 2, YRsiz: 2},
	}
	data := minimalCodestream(minimalParams{width: 33, height: 20, comps: comps, levels: 1, tileSize: 16})
	h, err := ReadHeader(bytes.NewReader(data), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := &Header{
		Width:  33,
		Height: 20,
		Components: []Component{
			{Depth: 8, DX: 1, DY: 1, Width: 33, Height: 20},
			{Depth: 16, Signed: true, DX: 2, DY: 2, Width: 17, Height: 10},
		},
		Channels: []Channel{
			{Component: 0, PaletteColumn: -1, Kind: ChannelColor, Assoc: 1},
			{Component: 1, PaletteColumn: -1, Kind: ChannelColor, Assoc: 2},
		},
		Resolutions: 2,
		Tiles:       6,
	}
	if diff := cmp.Diff(want, h); diff != "" {
		t.Errorf("header mismatch (-want +got):\n%s", diff)
	}

	// the header must be readable from a prefix that ends after the main header
	for n := len(data); n > 0; n-- {
		if data[n-1] == 0x90 && data[n-2] == 0xFF {
			if _, err := ReadHeader(bytes.NewReader(data[:n]), nil); err != nil {
				t.Errorf("header from prefix of %d bytes: %v", n, err)
			}
			break
		}
	}
}

// A component mapping box referring to components or palette columns
// that do not exist is ignored together with the palette, and channel
// definitions for channels that do not exist are dropped, so that the
// header's references are always valid.
func TestInvalidMappingIgnored(t *testing.T) {
	img := testImage(8, 8, gray(4), 8)
	img.Palette = &Palette{Columns: []PaletteColumn{{Depth: 8}}, Entries: [][]int32{{1}, {2}}}
	img.Channels = []Channel{{Component: 0, PaletteColumn: 0, Kind: ChannelColor, Assoc: 1}}
	var buf bytes.Buffer
	if err := Encode(&buf, img, &EncodeParams{Wrapper: true}); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	// patch the cmap entry to refer to component 9
	cmap := bytes.Index(data, []byte("cmap"))
	if cmap < 0 {
		t.Fatal("cmap box not found")
	}
	data[cmap+5] = 9 // CMP field of the first entry
	h, err := ReadHeader(bytes.NewReader(data), nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.Palette != nil || len(h.Channels) != 1 || h.Channels[0].Component != 0 || h.Channels[0].PaletteColumn != -1 {
		t.Errorf("invalid mapping not ignored: palette %v, channels %+v", h.Palette != nil, h.Channels)
	}
	for _, ch := range h.Channels {
		if ch.Component >= len(h.Components) {
			t.Errorf("channel refers to component %d of %d", ch.Component, len(h.Components))
		}
	}
}
