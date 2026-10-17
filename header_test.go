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
