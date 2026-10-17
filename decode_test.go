// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jpx

import (
	"bytes"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"seehuhn.de/go/jpx/internal/codestream"
)

// testBudget is a Budget that records the total charged.
type testBudget struct {
	left  int64
	total int64
}

func newTestBudget(limit int64) *testBudget { return &testBudget{left: limit} }

func (b *testBudget) Charge(n int) error {
	if int64(n) > b.left {
		return ErrBudget
	}
	b.left -= int64(n)
	b.total += int64(n)
	return nil
}

// minimalParams describes the hand-built codestreams used by the tests
// below: a single tile with a single empty packet per resolution level
// and component.
type minimalParams struct {
	width, height int
	comps         []codestream.ComponentSIZ
	levels        int
	tileSize      int // 0 = one tile
	mct           bool
}

// minimalCodestream builds a syntactically complete codestream whose
// packets are all empty, so that every sample decodes to the mid-point of
// the component's range (or zero for signed components).
func minimalCodestream(p minimalParams) []byte {
	var buf bytes.Buffer
	w := codestream.NewWriter(&buf)
	w.SOC()
	siz := &codestream.SIZ{
		Xsiz: uint32(p.width), Ysiz: uint32(p.height),
		XTsiz: uint32(p.width), YTsiz: uint32(p.height),
		Components: p.comps,
	}
	if p.tileSize > 0 {
		siz.XTsiz, siz.YTsiz = uint32(p.tileSize), uint32(p.tileSize)
	}
	w.SIZ(siz)
	cod := &codestream.COD{
		CodingStyle: codestream.CodingStyle{
			Levels:        p.levels,
			CodeBlockW:    6,
			CodeBlockH:    6,
			Reversible:    true,
			PrecinctSizes: codestream.DefaultPrecincts(p.levels + 1),
		},
		Layers: 1,
		MCT:    p.mct,
	}
	w.COD(cod)
	q := &codestream.Quantization{Style: codestream.QuantNone, GuardBits: 2}
	for range 3*p.levels + 1 {
		q.Exponents = append(q.Exponents, 10)
	}
	w.QCD(q)
	nx, ny, _ := siz.NumTiles()
	// hostile geometries get a single tile-part only
	for tile := range min(nx*ny, 64) {
		sot := buf.Len() // the packet bytes below bypass the writer's offset
		w.SOT(tile, 0, 1)
		w.SOD()
		// one empty packet (a single zero bit) per resolution and component
		for range (p.levels + 1) * len(p.comps) {
			buf.WriteByte(0)
		}
		codestream.PatchPsot(buf.Bytes(), sot, uint32(buf.Len()-sot))
	}
	w.EOC()
	if w.Err() != nil {
		panic(w.Err())
	}
	return buf.Bytes()
}

func TestDecodeMinimal(t *testing.T) {
	comps := []codestream.ComponentSIZ{
		{Depth: 8, XRsiz: 1, YRsiz: 1},
		{Depth: 4, Signed: true, XRsiz: 2, YRsiz: 1},
		{Depth: 12, XRsiz: 1, YRsiz: 3},
	}
	data := minimalCodestream(minimalParams{width: 17, height: 11, comps: comps, levels: 2})
	img, err := Decode(bytes.NewReader(data), &Options{Budget: newTestBudget(1 << 20)})
	if err != nil {
		t.Fatal(err)
	}
	if img.Width != 17 || img.Height != 11 || len(img.Planes) != 3 {
		t.Fatalf("got %dx%d with %d planes", img.Width, img.Height, len(img.Planes))
	}
	wantSize := [][2]int{{17, 11}, {9, 11}, {17, 4}}
	wantVal := []int32{128, 0, 2048}
	for c, p := range img.Planes {
		if p.Width != wantSize[c][0] || p.Height != wantSize[c][1] {
			t.Errorf("plane %d is %dx%d, want %dx%d", c, p.Width, p.Height, wantSize[c][0], wantSize[c][1])
		}
		for _, v := range p.Pix {
			if v != wantVal[c] {
				t.Errorf("plane %d has sample %d, want %d", c, v, wantVal[c])
				break
			}
		}
	}
	if img.Resolutions != 3 || img.Tiles != 1 || img.Wrapped {
		t.Errorf("header: resolutions %d, tiles %d, wrapped %v", img.Resolutions, img.Tiles, img.Wrapped)
	}
}

func TestDecodeReduce(t *testing.T) {
	comps := []codestream.ComponentSIZ{{Depth: 8, XRsiz: 1, YRsiz: 1}}
	data := minimalCodestream(minimalParams{width: 100, height: 70, comps: comps, levels: 3, tileSize: 32})
	for reduce, want := range [][2]int{{100, 70}, {50, 35}, {25, 18}, {13, 9}} {
		img, err := Decode(bytes.NewReader(data), &Options{Reduce: reduce})
		if err != nil {
			t.Fatalf("reduce %d: %v", reduce, err)
		}
		p := img.Planes[0]
		if p.Width != want[0] || p.Height != want[1] {
			t.Errorf("reduce %d: plane is %dx%d, want %dx%d", reduce, p.Width, p.Height, want[0], want[1])
		}
		for _, v := range p.Pix {
			if v != 128 {
				t.Errorf("reduce %d: sample %d, want 128", reduce, v)
				break
			}
		}
	}
	if _, err := Decode(bytes.NewReader(data), &Options{Reduce: 4}); err == nil {
		t.Error("reduction beyond the available levels was accepted")
	}
}

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

// Truncating a codestream must never cause a panic and, once the main
// header is complete, must yield an image.
func TestDecodeTruncated(t *testing.T) {
	comps := []codestream.ComponentSIZ{{Depth: 8, XRsiz: 1, YRsiz: 1}, {Depth: 8, XRsiz: 1, YRsiz: 1}}
	data := minimalCodestream(minimalParams{width: 40, height: 30, comps: comps, levels: 2, tileSize: 16})
	gotImage := false
	for n := range len(data) {
		img, err := Decode(bytes.NewReader(data[:n]), &Options{Budget: newTestBudget(1 << 20)})
		if err == nil {
			gotImage = true
			if img.Width != 40 || img.Height != 30 {
				t.Fatalf("prefix %d: image is %dx%d", n, img.Width, img.Height)
			}
		}
	}
	if !gotImage {
		t.Error("no truncated prefix decoded to an image")
	}
}

// A tiny file declaring an enormous image must fail because of the
// budget, before any large allocation.
func TestDecodeHostileSize(t *testing.T) {
	comps := []codestream.ComponentSIZ{{Depth: 8, XRsiz: 1, YRsiz: 1}}
	for _, p := range []minimalParams{
		{width: 60000, height: 60000, comps: comps, levels: 5},
		{width: 60000, height: 60000, comps: comps, levels: 0, tileSize: 256},
		{width: 1 << 20, height: 1 << 20, comps: comps, levels: 32},
	} {
		data := minimalCodestream(p)
		b := newTestBudget(1 << 20)
		_, err := Decode(bytes.NewReader(data), &Options{Budget: b})
		if !errors.Is(err, ErrBudget) {
			t.Errorf("%dx%d: got %v, want budget error", p.width, p.height, err)
		}
		if b.total > 1<<20 {
			t.Errorf("%dx%d: charged %d bytes, more than the budget", p.width, p.height, b.total)
		}
	}
}

func TestDecodeGarbage(t *testing.T) {
	for _, data := range [][]byte{
		nil,
		{0xFF},
		{0xFF, 0x4F},
		{0xFF, 0x4F, 0xFF, 0x51},
		[]byte("not a JPEG 2000 file at all, just some text"),
		{0, 0, 0, 12, 'j', 'P', ' ', ' ', 0x0D, 0x0A, 0x87, 0x0A},
	} {
		if _, err := Decode(bytes.NewReader(data), nil); err == nil {
			t.Errorf("%q was accepted", data)
		}
		if _, err := ReadHeader(bytes.NewReader(data), nil); err == nil {
			t.Errorf("%q was accepted by ReadHeader", data)
		}
	}
}

func TestDecodeDefaultBudget(t *testing.T) {
	// without an explicit budget, a small hostile file is still refused
	comps := []codestream.ComponentSIZ{{Depth: 8, XRsiz: 1, YRsiz: 1}}
	data := minimalCodestream(minimalParams{width: 1 << 20, height: 1 << 20, comps: comps, levels: 0})
	if _, err := Decode(bytes.NewReader(data), nil); !errors.Is(err, ErrBudget) {
		t.Errorf("got %v, want budget error", err)
	}
}

func FuzzDecode(f *testing.F) {
	comps := []codestream.ComponentSIZ{{Depth: 8, XRsiz: 1, YRsiz: 1}, {Depth: 8, XRsiz: 1, YRsiz: 1}}
	f.Add(minimalCodestream(minimalParams{width: 40, height: 30, comps: comps, levels: 2, tileSize: 16}))
	f.Add(minimalCodestream(minimalParams{width: 7, height: 5, comps: comps[:1], levels: 0}))
	f.Fuzz(func(t *testing.T, data []byte) {
		b := newTestBudget(min(1<<20+1024*int64(len(data)), 4<<20))
		img, err := Decode(bytes.NewReader(data), &Options{Budget: b})
		if err != nil {
			return
		}
		for c, p := range img.Planes {
			if len(p.Pix) != p.Width*p.Height {
				t.Fatalf("plane %d: %d samples for %dx%d", c, len(p.Pix), p.Width, p.Height)
			}
		}
	})
}

func FuzzReadHeader(f *testing.F) {
	comps := []codestream.ComponentSIZ{{Depth: 8, XRsiz: 1, YRsiz: 1}}
	f.Add(minimalCodestream(minimalParams{width: 40, height: 30, comps: comps, levels: 2}))
	f.Fuzz(func(t *testing.T, data []byte) {
		b := newTestBudget(min(1<<20+1024*int64(len(data)), 4<<20))
		_, _ = ReadHeader(bytes.NewReader(data), b)
	})
}
