// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/google/go-cmp/cmp"

	"seehuhn.de/go/jpx"
)

func TestPGXRoundTrip(t *testing.T) {
	for _, signed := range []bool{false, true} {
		for _, depth := range []int{1, 8, 12, 16} {
			c := jpx.Component{Depth: depth, Signed: signed, DX: 1, DY: 1, Width: 3, Height: 2}
			p := jpx.Plane{Width: 3, Height: 2}
			lo, hi := int32(0), int32(1)<<uint(depth)-1
			if signed {
				lo, hi = -(1 << uint(depth-1)), 1<<uint(depth-1)-1
			}
			p.Pix = []int32{lo, hi, (lo + hi) / 2, hi, lo, hi - hi/2}
			var buf bytes.Buffer
			if err := writePGX(&buf, c, &p); err != nil {
				t.Fatal(err)
			}
			c2, p2, err := readPGX(&buf)
			if err != nil {
				t.Fatal(err)
			}
			if d := cmp.Diff(c, c2); d != "" {
				t.Errorf("component mismatch (-want +got):\n%s", d)
			}
			if d := cmp.Diff(p, p2); d != "" {
				t.Errorf("plane mismatch (-want +got):\n%s", d)
			}
		}
	}
}

func TestPGXTruncated(t *testing.T) {
	if _, _, err := readPGX(bytes.NewReader([]byte("PG ML + 8 100000 100000\nab"))); err == nil {
		t.Error("truncated file accepted")
	}
}

func TestPNGToImage(t *testing.T) {
	pic := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	pic.SetNRGBA(0, 0, color.NRGBA{10, 20, 30, 255})
	pic.SetNRGBA(1, 0, color.NRGBA{40, 50, 60, 128})
	var buf bytes.Buffer
	if err := png.Encode(&buf, pic); err != nil {
		t.Fatal(err)
	}
	dec, err := png.Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	img := fromPNGImage(dec)

	wantC := jpx.Component{Depth: 8, DX: 1, DY: 1, Width: 2, Height: 1}
	wantComps := []jpx.Component{wantC, wantC, wantC, wantC}
	if d := cmp.Diff(wantComps, img.Components); d != "" {
		t.Errorf("components mismatch (-want +got):\n%s", d)
	}
	want := [][]int32{{10, 40}, {20, 50}, {30, 60}, {255, 128}}
	for i, pl := range img.Planes {
		if d := cmp.Diff(want[i], pl.Pix); d != "" {
			t.Errorf("plane %d mismatch (-want +got):\n%s", i, d)
		}
	}
	if len(img.Channels) != 4 || img.Channels[3].Kind != jpx.ChannelOpacity {
		t.Error("missing opacity channel")
	}

	back, err := toPNGImage(img)
	if err != nil {
		t.Fatal(err)
	}
	if d := cmp.Diff(image.Image(pic), back); d != "" {
		t.Errorf("round trip failed (-want +got):\n%s", d)
	}
}
