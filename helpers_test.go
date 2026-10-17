// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jpx

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestApplyPalette(t *testing.T) {
	img := &Image{
		Header: Header{
			Width: 2, Height: 2,
			Components: []Component{{Depth: 2, DX: 1, DY: 1, Width: 2, Height: 2}},
			Palette: &Palette{
				Entries: [][]int32{{10, 100}, {20, 200}, {30, 300}},
				Columns: []Component{{Depth: 8}, {Depth: 9}},
			},
			Channels: []Channel{
				{Component: 0, PaletteColumn: 0, Kind: ChannelColor, Assoc: 1},
				{Component: 0, PaletteColumn: 1, Kind: ChannelColor, Assoc: 2},
				{Component: 0, PaletteColumn: -1, Kind: ChannelOpacity, Assoc: 0},
			},
		},
		Planes: []Plane{{Width: 2, Height: 2, Pix: []int32{0, 1, 2, 3}}},
	}
	got, err := ApplyPalette(img)
	if err != nil {
		t.Fatal(err)
	}
	wantPlanes := []Plane{
		{Width: 2, Height: 2, Pix: []int32{10, 20, 30, 30}}, // index 3 is clamped to the last entry
		{Width: 2, Height: 2, Pix: []int32{100, 200, 300, 300}},
		{Width: 2, Height: 2, Pix: []int32{0, 1, 2, 3}},
	}
	if diff := cmp.Diff(wantPlanes, got.Planes); diff != "" {
		t.Errorf("planes (-want +got):\n%s", diff)
	}
	wantComps := []Component{
		{Depth: 8, DX: 1, DY: 1, Width: 2, Height: 2},
		{Depth: 9, DX: 1, DY: 1, Width: 2, Height: 2},
		{Depth: 2, DX: 1, DY: 1, Width: 2, Height: 2},
	}
	if diff := cmp.Diff(wantComps, got.Components); diff != "" {
		t.Errorf("components (-want +got):\n%s", diff)
	}
	if got.Palette != nil || len(got.Channels) != 3 || got.Channels[1].Component != 1 || got.Channels[1].PaletteColumn != -1 {
		t.Errorf("unexpected header after palette expansion: %+v", got.Header)
	}

	// an image without a palette is returned unchanged
	plain := testImage(4, 4, gray(8), 1)
	if same, _ := ApplyPalette(plain); same != plain {
		t.Error("image without palette was copied")
	}
}

func TestSYCCToRGB(t *testing.T) {
	img := &Image{
		Header: Header{Components: rgb(8)},
		Planes: []Plane{
			{Width: 2, Height: 1, Pix: []int32{128, 255}},
			{Width: 2, Height: 1, Pix: []int32{128, 128}},
			{Width: 2, Height: 1, Pix: []int32{128, 0}},
		},
	}
	if err := SYCCToRGB(img); err != nil {
		t.Fatal(err)
	}
	// neutral chroma leaves grey unchanged; a strongly negative Cr
	// reduces red and increases green, clamped to the range
	want := []Plane{
		{Width: 2, Height: 1, Pix: []int32{128, 76}},
		{Width: 2, Height: 1, Pix: []int32{128, 255}},
		{Width: 2, Height: 1, Pix: []int32{128, 255}},
	}
	if diff := cmp.Diff(want, img.Planes); diff != "" {
		t.Errorf("conversion (-want +got):\n%s", diff)
	}
	if err := SYCCToRGB(testImage(2, 2, gray(8), 1)); err == nil {
		t.Error("single plane accepted")
	}
}

func ExampleReadHeader() {
	img := testImage(8, 8, rgb(8), 1)
	var buf bytes.Buffer
	if err := Encode(&buf, img, &EncodeParams{Wrapper: true}); err != nil {
		panic(err)
	}
	h, err := ReadHeader(&buf, nil)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%dx%d, %d components, %d colour channels, enumerated colour space %d\n",
		h.Width, h.Height, len(h.Components), countColorChannels(h), h.ColorSpecs[0].Enumerated)
	// Output: 8x8, 3 components, 3 colour channels, enumerated colour space 16
}
