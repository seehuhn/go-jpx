// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jpx

import (
	"bytes"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"seehuhn.de/go/jpx/internal/codestream"
	"seehuhn.de/go/membudget"
)

func newTestBudget(limit int64) *membudget.Budget { return membudget.New(limit) }

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
		if !errors.Is(err, membudget.ErrExceeded) {
			t.Errorf("%dx%d: got %v, want budget error", p.width, p.height, err)
		}
		// the budget is what stopped the decode: a comfortable budget
		// still fails for the same declared geometry only when the planes
		// themselves do not fit, i.e. the stop happens before any large
		// allocation
		if _, err := Decode(bytes.NewReader(data), &Options{Budget: newTestBudget(1 << 30)}); !errors.Is(err, membudget.ErrExceeded) {
			t.Errorf("%dx%d with a 1 GiB budget: got %v, want budget error", p.width, p.height, err)
		}
	}
}

// A codestream box with length zero extends to the end of the file.
func TestDecodeCodestreamToEOF(t *testing.T) {
	img := testImage(10, 7, rgb(8), 16)
	var buf bytes.Buffer
	if err := Encode(&buf, img, &EncodeParams{Wrapper: true}); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	i := bytes.LastIndex(data, []byte("jp2c"))
	if i < 4 {
		t.Fatal("jp2c box not found")
	}
	copy(data[i-4:i], []byte{0, 0, 0, 0})
	got, err := Decode(bytes.NewReader(data), nil)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(img.Planes, got.Planes); diff != "" {
		t.Errorf("round trip failed (-want +got):\n%s", diff)
	}
	if _, err := ReadHeader(bytes.NewReader(data), nil); err != nil {
		t.Errorf("ReadHeader: %v", err)
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

// fuzzSeeds returns encoded images that reach the wrapper, palette,
// channel definition, tiling and packed header code paths.
func fuzzSeeds(f *testing.F) [][]byte {
	comps := []codestream.ComponentSIZ{{Depth: 8, XRsiz: 1, YRsiz: 1}, {Depth: 8, XRsiz: 1, YRsiz: 1}}
	seeds := [][]byte{
		minimalCodestream(minimalParams{width: 40, height: 30, comps: comps, levels: 2, tileSize: 16}),
		minimalCodestream(minimalParams{width: 7, height: 5, comps: comps[:1], levels: 0}),
	}
	pal := testImage(8, 8, gray(4), 8)
	pal.Palette = &Palette{Columns: []PaletteColumn{{Depth: 8}, {Depth: 8}, {Depth: 8}}}
	for i := range 16 {
		pal.Palette.Entries = append(pal.Palette.Entries, []int32{int32(i * 17), int32(255 - i*17), 7})
	}
	pal.Channels = []Channel{
		{Component: 0, PaletteColumn: 0, Kind: ChannelColor, Assoc: 1},
		{Component: 0, PaletteColumn: 1, Kind: ChannelColor, Assoc: 2},
		{Component: 0, PaletteColumn: 2, Kind: ChannelColor, Assoc: 3},
	}
	alpha := testImage(12, 10, rgb(8), 9)
	alpha.Channels[2] = Channel{Component: 2, PaletteColumn: -1, Kind: ChannelOpacity, Type: 1, Assoc: 0}
	alpha.ColorSpecs = []ColorSpec{{Method: 2, ICC: []byte{1, 2, 3, 4}}, {Method: 1, Enumerated: 16}}
	for _, c := range []struct {
		img *Image
		p   EncodeParams
	}{
		{pal, EncodeParams{Wrapper: true}},
		{alpha, EncodeParams{Wrapper: true, Lossy: true, Layers: 2}},
		{testImage(20, 14, rgb(8), 10), EncodeParams{TileWidth: 8, TileHeight: 8, PackedHeaders: PackedInMain, TileParts: 2, SOP: true, EPH: true}},
		{testImage(20, 14, gray(8), 10), EncodeParams{PackedHeaders: PackedInTile, Bypass: true, Terminate: true, Progression: RPCL}},
	} {
		var buf bytes.Buffer
		if err := Encode(&buf, c.img, &c.p); err != nil {
			f.Fatal(err)
		}
		seeds = append(seeds, buf.Bytes())
	}
	return seeds
}

func FuzzDecode(f *testing.F) {
	for _, s := range fuzzSeeds(f) {
		f.Add(s)
	}
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
		// the header's references must be usable without further checks
		for _, ch := range img.Channels {
			if ch.Component >= len(img.Components) || (ch.PaletteColumn >= 0 && (img.Palette == nil || ch.PaletteColumn >= len(img.Palette.Columns))) {
				t.Fatalf("channel %+v refers outside the image", ch)
			}
		}
		if _, err := ApplyPalette(img, b); err != nil && !errors.Is(err, membudget.ErrExceeded) {
			t.Fatalf("ApplyPalette: %v", err)
		}
	})
}

func FuzzReadHeader(f *testing.F) {
	for _, s := range fuzzSeeds(f) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		b := newTestBudget(min(1<<20+1024*int64(len(data)), 4<<20))
		_, _ = ReadHeader(bytes.NewReader(data), b)
	})
}

// A nil budget imposes no limit, so that a valid, highly compressed
// image is decoded whatever its ratio of decoded to encoded size.
func TestDecodeNilBudget(t *testing.T) {
	img := testImage(3000, 2000, rgb(8), 17)
	var buf bytes.Buffer
	if err := Encode(&buf, img, &EncodeParams{Lossy: true, Quality: 1, Rates: []float64{0.005}}); err != nil {
		t.Fatal(err)
	}
	t.Logf("encoded size %d", buf.Len())
	got, err := Decode(bytes.NewReader(buf.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Width != img.Width || got.Height != img.Height || len(got.Planes) != 3 {
		t.Errorf("got %dx%d with %d planes", got.Width, got.Height, len(got.Planes))
	}
}
