// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jpx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"strconv"
	"testing"

	"github.com/google/go-cmp/cmp"
	"seehuhn.de/go/jpx/internal/codestream"
	"seehuhn.de/go/jpx/internal/t2"
	"seehuhn.de/go/membudget"
)

// testImage builds a synthetic image with smooth gradients, an edge and
// noise, which exercises all sub-bands.  Sample values span the full
// range of each component.
func testImage(w, h int, comps []Component, seed uint64) *Image {
	return testImageAt(0, 0, w, h, comps, seed)
}

// testImageAt is like testImage, for an image area starting at (x0, y0)
// on the reference grid.
func testImageAt(x0, y0, w, h int, comps []Component, seed uint64) *Image {
	rng := rand.New(rand.NewPCG(seed, 99))
	img := &Image{XOffset: x0, YOffset: y0, Width: w, Height: h, Components: comps}
	for ci, c := range comps {
		pw := ceilDiv(x0+w, c.DX) - ceilDiv(x0, c.DX)
		ph := ceilDiv(y0+h, c.DY) - ceilDiv(y0, c.DY)
		img.Components[ci].Width, img.Components[ci].Height = pw, ph
		l, u := sampleRange(c)
		lo, hi := float64(l), float64(u)
		pix := make([]int32, pw*ph)
		for y := range ph {
			for x := range pw {
				fx, fy := float64(x)/float64(max(pw-1, 1)), float64(y)/float64(max(ph-1, 1))
				v := 0.3*fx + 0.3*fy + 0.2*math.Sin(7*fx+float64(ci))*math.Cos(5*fy)
				if fx > 0.6 && fy < 0.4 {
					v += 0.25
				}
				v += 0.05 * rng.Float64()
				v = min(max(v, 0), 1)
				pix[y*pw+x] = int32(math.Round(lo + v*(hi-lo)))
			}
		}
		img.Planes = append(img.Planes, Plane{Width: pw, Height: ph, Pix: pix})
	}
	img.Channels = defaultChannels(len(comps))
	return img
}

func defaultChannels(n int) []Channel {
	var ch []Channel
	for i := range n {
		ch = append(ch, Channel{Component: i, PaletteColumn: -1, Kind: ChannelColor, Assoc: i + 1})
	}
	return ch
}

func gray(depth int) []Component { return []Component{{Depth: depth, DX: 1, DY: 1}} }

func rgb(depth int) []Component {
	return []Component{{Depth: depth, DX: 1, DY: 1}, {Depth: depth, DX: 1, DY: 1}, {Depth: depth, DX: 1, DY: 1}}
}

func roundTrip(t *testing.T, img *Image, p *EncodeParams) *Image {
	t.Helper()
	var buf bytes.Buffer
	if err := Encode(&buf, img, p); err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := Decode(bytes.NewReader(buf.Bytes()), &Options{Budget: newTestBudget(256 << 20)})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

func TestRoundTripLossless(t *testing.T) {
	cmyk := []Component{{Depth: 8, DX: 1, DY: 1}, {Depth: 8, DX: 1, DY: 1}, {Depth: 8, DX: 1, DY: 1}, {Depth: 8, DX: 1, DY: 1}}
	mixed := []Component{{Depth: 8, DX: 1, DY: 1}, {Depth: 12, Signed: true, DX: 2, DY: 1}, {Depth: 3, DX: 1, DY: 2}, {Depth: 16, DX: 2, DY: 2}}
	cases := []struct {
		name  string
		w, h  int
		comps []Component
		p     EncodeParams
	}{
		{"gray8", 37, 29, gray(8), EncodeParams{}},
		{"bilevel", 64, 40, gray(1), EncodeParams{}},
		{"rgb8", 50, 33, rgb(8), EncodeParams{}},
		{"cmyk8", 31, 31, cmyk, EncodeParams{}},
		{"gray16", 33, 17, gray(16), EncodeParams{}},
		{"signed12", 20, 20, []Component{{Depth: 12, Signed: true, DX: 1, DY: 1}}, EncodeParams{}},
		{"mixed", 45, 38, mixed, EncodeParams{}},
		{"tiles", 70, 50, rgb(8), EncodeParams{TileWidth: 32, TileHeight: 24}},
		{"tiles odd", 65, 65, gray(8), EncodeParams{TileWidth: 17, TileHeight: 19, Levels: 2}},
		{"no levels", 20, 12, rgb(8), EncodeParams{Levels: NoLevels, CodeBlockWidth: 16, CodeBlockHeight: 16}},
		{"many levels", 40, 30, gray(8), EncodeParams{Levels: 8}},
		{"precincts", 64, 48, rgb(8), EncodeParams{Precincts: []PrecinctSize{{3, 3}, {4, 4}, {5, 4}}, CodeBlockWidth: 16, CodeBlockHeight: 8}},
		{"layers", 48, 48, rgb(8), EncodeParams{Layers: 4}},
		{"rates", 48, 48, rgb(8), EncodeParams{Rates: []float64{0.5, 2, 0}}},
		{"small blocks", 30, 30, gray(8), EncodeParams{CodeBlockWidth: 4, CodeBlockHeight: 4}},
		{"wide blocks", 30, 30, gray(8), EncodeParams{CodeBlockWidth: 1024, CodeBlockHeight: 4}},
		{"sop eph", 40, 30, rgb(8), EncodeParams{SOP: true, EPH: true, Layers: 2}},
		{"bypass", 60, 40, gray(8), EncodeParams{Bypass: true}},
		{"reset", 60, 40, gray(8), EncodeParams{ResetContexts: true}},
		{"terminate", 60, 40, gray(8), EncodeParams{Terminate: true}},
		{"causal", 60, 40, gray(8), EncodeParams{VerticallyCausal: true}},
		{"segsym", 60, 40, gray(8), EncodeParams{SegmentationSymbols: true}},
		{"all styles", 60, 40, rgb(8), EncodeParams{Bypass: true, ResetContexts: true, Terminate: true, VerticallyCausal: true, SegmentationSymbols: true, Layers: 3}},
		{"roi", 64, 64, rgb(8), EncodeParams{ROI: &Rectangle{10, 10, 40, 30}}},
		{"roi layers", 64, 64, gray(8), EncodeParams{ROI: &Rectangle{0, 0, 20, 64}, Layers: 3}},
		{"no mct", 20, 20, rgb(8), EncodeParams{NoMCT: true}},
		{"tile-parts", 50, 40, rgb(8), EncodeParams{TileParts: 3, Layers: 2, TileWidth: 30, TileHeight: 30}},
		{"ppt", 50, 40, rgb(8), EncodeParams{PackedHeaders: PackedInTile, TileWidth: 30, TileHeight: 30}},
		{"ppm", 50, 40, rgb(8), EncodeParams{PackedHeaders: PackedInMain, TileWidth: 30, TileHeight: 30, Layers: 2}},
		{"ppt tile-parts markers", 50, 40, rgb(8), EncodeParams{PackedHeaders: PackedInTile, TileParts: 4, SOP: true, EPH: true, Layers: 3, TileWidth: 30, TileHeight: 30}},
		{"ppm tile-parts markers", 50, 40, rgb(8), EncodeParams{PackedHeaders: PackedInMain, TileParts: 2, SOP: true, EPH: true, Layers: 3, TileWidth: 30, TileHeight: 30}},
		{"poc", 60, 40, rgb(8), EncodeParams{Layers: 3, ProgressionChanges: []ProgressionChange{
			{ResStart: 0, ResEnd: 2, CompStart: 0, CompEnd: 3, LayerEnd: 3, Progression: LRCP},
			{ResStart: 0, ResEnd: 6, CompStart: 0, CompEnd: 3, LayerEnd: 3, Progression: RPCL},
		}}},
		{"ppm poc", 60, 40, rgb(8), EncodeParams{PackedHeaders: PackedInMain, TileParts: 2, Layers: 2, ProgressionChanges: []ProgressionChange{
			{ResStart: 0, ResEnd: 6, CompStart: 0, CompEnd: 3, LayerEnd: 1, Progression: RPCL},
			{ResStart: 0, ResEnd: 6, CompStart: 0, CompEnd: 3, LayerEnd: 2, Progression: LRCP},
		}}},
		{"more parts than packets", 8, 8, gray(8), EncodeParams{TileParts: 255, Levels: 1}},
		{"poc layers", 60, 40, rgb(8), EncodeParams{Layers: 4, TileParts: 2, ProgressionChanges: []ProgressionChange{
			{ResStart: 0, ResEnd: 6, CompStart: 0, CompEnd: 3, LayerEnd: 1, Progression: CPRL},
			{ResStart: 0, ResEnd: 6, CompStart: 0, CompEnd: 1, LayerEnd: 4, Progression: RLCP},
			{ResStart: 0, ResEnd: 6, CompStart: 0, CompEnd: 3, LayerEnd: 4, Progression: PCRL},
		}}},
		{"wrapper", 20, 20, rgb(8), EncodeParams{Wrapper: true}},
		{"one pixel", 1, 1, rgb(8), EncodeParams{}},
		{"one row", 50, 1, gray(8), EncodeParams{}},
		{"one column", 1, 50, gray(8), EncodeParams{Levels: 3}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			img := testImage(c.w, c.h, c.comps, 1)
			got := roundTrip(t, img, &c.p)
			if diff := cmp.Diff(img.Planes, got.Planes); diff != "" {
				t.Errorf("round trip failed (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRoundTripProgressions(t *testing.T) {
	// sub-sampled components with several levels make the position-driven
	// progressions non-trivial
	comps := []Component{{Depth: 8, DX: 1, DY: 1}, {Depth: 8, DX: 2, DY: 1}, {Depth: 8, DX: 1, DY: 2}}
	img := testImage(75, 61, comps, 2)
	for prog := LRCP; prog <= CPRL; prog++ {
		for _, layers := range []int{1, 3} {
			p := &EncodeParams{
				Progression: prog, Layers: layers,
				Precincts: []PrecinctSize{{2, 2}, {3, 3}},
				TileWidth: 40, TileHeight: 50,
				CodeBlockWidth: 8, CodeBlockHeight: 8,
			}
			got := roundTrip(t, img, p)
			if diff := cmp.Diff(img.Planes, got.Planes); diff != "" {
				t.Errorf("progression %d, %d layers (-want +got):\n%s", prog, layers, diff)
			}
		}
	}
}

// psnr returns the peak signal-to-noise ratio in dB between two planes
// of the given depth.
func psnr(a, b Plane, depth int) float64 {
	var se float64
	for i := range a.Pix {
		d := float64(a.Pix[i] - b.Pix[i])
		se += d * d
	}
	if se == 0 {
		return math.Inf(1)
	}
	peak := float64(uint64(1)<<uint(depth) - 1)
	return 10 * math.Log10(peak*peak/(se/float64(len(a.Pix))))
}

func TestRoundTripLossy(t *testing.T) {
	cases := []struct {
		name    string
		comps   []Component
		p       EncodeParams
		minPSNR float64
	}{
		{"gray8 q75", gray(8), EncodeParams{Lossy: true}, 36},
		{"gray8 q95", gray(8), EncodeParams{Lossy: true, Quality: 95}, 44},
		{"gray8 q30", gray(8), EncodeParams{Lossy: true, Quality: 30}, 25},
		{"rgb8 q75", rgb(8), EncodeParams{Lossy: true, Quality: 75}, 36},
		{"rgb8 layers", rgb(8), EncodeParams{Lossy: true, Quality: 90, Layers: 5}, 36},
		{"gray16 q90", gray(16), EncodeParams{Lossy: true, Quality: 90}, 40},
		{"gray12 tiles", gray(12), EncodeParams{Lossy: true, Quality: 85, TileWidth: 30, TileHeight: 30}, 38},
		{"rgb8 bypass", rgb(8), EncodeParams{Lossy: true, Quality: 80, Bypass: true, Terminate: true}, 34},
		{"no levels", gray(8), EncodeParams{Lossy: true, Quality: 100, Levels: NoLevels}, 44},
		{"roi lossy", rgb(8), EncodeParams{Lossy: true, Quality: 80, ROI: &Rectangle{8, 8, 40, 40}}, 34},
		{"sop eph precincts", rgb(8), EncodeParams{Lossy: true, Quality: 80, SOP: true, EPH: true, Precincts: []PrecinctSize{{3, 3}, {4, 4}}, Layers: 3, Bypass: true}, 34},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			img := testImage(64, 48, c.comps, 3)
			got := roundTrip(t, img, &c.p)
			for i := range img.Planes {
				if p := psnr(img.Planes[i], got.Planes[i], c.comps[i].Depth); p < c.minPSNR {
					t.Errorf("plane %d: PSNR %.1f dB, want at least %.1f", i, p, c.minPSNR)
				}
			}
		})
	}
}

// Higher quality must not produce a worse image, and must cost more bytes.
func TestQualityMonotone(t *testing.T) {
	img := testImage(64, 64, rgb(8), 4)
	prevSize, prevPSNR := 0, 0.0
	for _, q := range []float64{20, 50, 75, 90, 100} {
		var buf bytes.Buffer
		if err := Encode(&buf, img, &EncodeParams{Lossy: true, Quality: q}); err != nil {
			t.Fatal(err)
		}
		got, err := Decode(bytes.NewReader(buf.Bytes()), nil)
		if err != nil {
			t.Fatal(err)
		}
		p := psnr(img.Planes[0], got.Planes[0], 8)
		if buf.Len() <= prevSize || p <= prevPSNR {
			t.Errorf("quality %v: %d bytes, %.1f dB; previous %d bytes, %.1f dB", q, buf.Len(), p, prevSize, prevPSNR)
		}
		prevSize, prevPSNR = buf.Len(), p
	}
}

// With several layers, a codestream cut after the first layers must still
// decode to an approximation of the image.
func TestLayersProgressive(t *testing.T) {
	img := testImage(64, 64, gray(8), 5)
	var buf bytes.Buffer
	if err := Encode(&buf, img, &EncodeParams{Layers: 4, Rates: []float64{0.25, 0.5, 1, 0}}); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	prev := 0.0
	for _, frac := range []float64{0.1, 0.25, 0.5, 1} {
		n := int(float64(len(data)) * frac)
		got, err := Decode(bytes.NewReader(data[:n]), nil)
		if err != nil {
			t.Fatalf("%d bytes: %v", n, err)
		}
		p := psnr(img.Planes[0], got.Planes[0], 8)
		if p < prev {
			t.Errorf("%d bytes: PSNR %.1f dB is worse than with fewer bytes (%.1f)", n, p, prev)
		}
		prev = p
	}
	if !math.IsInf(prev, 1) {
		t.Errorf("full codestream is not lossless: %.1f dB", prev)
	}
}

func TestReduceDecoding(t *testing.T) {
	img := testImage(97, 75, rgb(8), 6)
	var buf bytes.Buffer
	if err := Encode(&buf, img, &EncodeParams{Levels: 3, TileWidth: 40, TileHeight: 40}); err != nil {
		t.Fatal(err)
	}
	for reduce := range 4 {
		got, err := Decode(bytes.NewReader(buf.Bytes()), &Options{Reduce: reduce})
		if err != nil {
			t.Fatalf("reduce %d: %v", reduce, err)
		}
		wantW, wantH := ceilShift(97, reduce), ceilShift(75, reduce)
		for c, p := range got.Planes {
			if p.Width != wantW || p.Height != wantH {
				t.Fatalf("reduce %d: plane %d is %dx%d, want %dx%d", reduce, c, p.Width, p.Height, wantW, wantH)
			}
			// the reduced image is a low-pass version: its mean stays close
			mean := func(p Plane) float64 {
				s := 0.0
				for _, v := range p.Pix {
					s += float64(v)
				}
				return s / float64(len(p.Pix))
			}
			if d := math.Abs(mean(p) - mean(img.Planes[c])); d > 3 {
				t.Errorf("reduce %d: plane %d mean differs by %.1f", reduce, c, d)
			}
		}
	}
}

func TestWrapperMetadata(t *testing.T) {
	img := testImage(16, 16, rgb(8), 7)
	img.ColorSpecs = []ColorSpec{
		{Method: 1, Precedence: 1, Approx: 1, Enumerated: 16},
		{Method: 2, Precedence: 0, Approx: 2, ICC: []byte{1, 2, 3, 4, 5, 6, 7, 8}},
	}
	img.Channels = []Channel{
		{Component: 0, PaletteColumn: -1, Kind: ChannelColor, Assoc: 1},
		{Component: 1, PaletteColumn: -1, Kind: ChannelColor, Assoc: 2},
		{Component: 2, PaletteColumn: -1, Kind: ChannelOpacity, Type: 1, Assoc: 0},
	}
	var buf bytes.Buffer
	if err := Encode(&buf, img, &EncodeParams{Wrapper: true}); err != nil {
		t.Fatal(err)
	}
	h, err := ReadHeader(bytes.NewReader(buf.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := img.Header
	want.Resolutions = 6
	want.Wrapped = true
	want.Tiles = 1
	if diff := cmp.Diff(&want, h); diff != "" {
		t.Errorf("header mismatch (-want +got):\n%s", diff)
	}
}

func TestPaletteMetadata(t *testing.T) {
	img := testImage(8, 8, gray(4), 8)
	img.Palette = &Palette{
		Columns: []PaletteColumn{{Depth: 8}, {Depth: 8}, {Depth: 10, Signed: true}},
	}
	for i := range 16 {
		img.Palette.Entries = append(img.Palette.Entries, []int32{int32(i * 17), int32(255 - i*17), int32(i - 8)})
	}
	img.Channels = []Channel{
		{Component: 0, PaletteColumn: 0, Kind: ChannelColor, Assoc: 1},
		{Component: 0, PaletteColumn: 1, Kind: ChannelColor, Assoc: 2},
		{Component: 0, PaletteColumn: 2, Kind: ChannelColor, Assoc: 3},
	}
	img.ColorSpecs = []ColorSpec{{Method: 1, Enumerated: 16}}
	var buf bytes.Buffer
	if err := Encode(&buf, img, &EncodeParams{Wrapper: true}); err != nil {
		t.Fatal(err)
	}
	got, err := Decode(bytes.NewReader(buf.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(img.Palette, got.Palette); diff != "" {
		t.Errorf("palette mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(img.Channels, got.Channels); diff != "" {
		t.Errorf("channels mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(img.Planes, got.Planes); diff != "" {
		t.Errorf("samples mismatch (-want +got):\n%s", diff)
	}
}

func TestEncodeErrors(t *testing.T) {
	bad := []struct {
		name string
		mod  func(img *Image, p *EncodeParams)
	}{
		{"wrong plane size", func(img *Image, p *EncodeParams) { img.Planes[0].Width = 7 }},
		{"missing plane", func(img *Image, p *EncodeParams) { img.Planes = nil }},
		{"depth too large", func(img *Image, p *EncodeParams) {
			img.Components[0].Depth, img.Components[0].Truncation = MaxDepth+1, MaxDepth+1-MaxPrecision
		}},
		{"deep samples without truncation", func(img *Image, p *EncodeParams) { img.Components[0].Depth = 30 }},
		{"inconsistent truncation", func(img *Image, p *EncodeParams) {
			img.Components[0].Depth, img.Components[0].Truncation = 20, 3
		}},
		{"bad block size", func(img *Image, p *EncodeParams) { p.CodeBlockWidth = 100 }},
		{"block too large", func(img *Image, p *EncodeParams) { p.CodeBlockWidth, p.CodeBlockHeight = 1024, 1024 }},
		{"bad progression", func(img *Image, p *EncodeParams) { p.Progression = 7 }},
		{"bad tile", func(img *Image, p *EncodeParams) { p.TileWidth = 10 }},
		{"bad levels", func(img *Image, p *EncodeParams) { p.Levels = 40 }},
		{"empty roi", func(img *Image, p *EncodeParams) { p.ROI = &Rectangle{3, 3, 3, 5} }},
		{"reduced image", func(img *Image, p *EncodeParams) { img.Reduce = 1 }},
		{"truncated samples", func(img *Image, p *EncodeParams) { img.Components[0].Truncation = 1 }},
		{"truncated palette", func(img *Image, p *EncodeParams) {
			img.Palette = &Palette{Entries: [][]int32{{0}}, Columns: []PaletteColumn{{Depth: 8, Truncation: 1}}}
			img.Channels = []Channel{{Component: 0, PaletteColumn: 0}}
		}},
		{"deep palette without truncation", func(img *Image, p *EncodeParams) {
			img.Palette = &Palette{Entries: [][]int32{{0}}, Columns: []PaletteColumn{{Depth: 38}}}
			img.Channels = []Channel{{Component: 0, PaletteColumn: 0}}
		}},
		{"palette too deep", func(img *Image, p *EncodeParams) {
			img.Palette = &Palette{Entries: [][]int32{{0}}, Columns: []PaletteColumn{{Depth: 39, Truncation: 8}}}
			img.Channels = []Channel{{Component: 0, PaletteColumn: 0}}
		}},
		{"sample out of range", func(img *Image, p *EncodeParams) { img.Planes[0].Pix[3] = 256 }},
		{"negative sample", func(img *Image, p *EncodeParams) { img.Planes[0].Pix[3] = -1 }},
		{"quality NaN", func(img *Image, p *EncodeParams) { p.Lossy, p.Quality = true, math.NaN() }},
		{"negative rate", func(img *Image, p *EncodeParams) { p.Rates = []float64{-1, 0} }},
		{"roi too deep", func(img *Image, p *EncodeParams) {
			*img = *testImage(8, 8, gray(16), 9)
			p.ROI = &Rectangle{0, 0, 4, 4}
		}},
		{"bad channel", func(img *Image, p *EncodeParams) { img.Channels = []Channel{{Component: 3, PaletteColumn: -1}} }},
		{"too many tile-parts", func(img *Image, p *EncodeParams) { p.TileParts = 256 }},
		{"negative tile-parts", func(img *Image, p *EncodeParams) { p.TileParts = -1 }},
		{"bad header placement", func(img *Image, p *EncodeParams) { p.PackedHeaders = 3 }},
		{"poc out of range", func(img *Image, p *EncodeParams) {
			p.ProgressionChanges = []ProgressionChange{{ResEnd: 7, CompEnd: 1, LayerEnd: 1}}
		}},
		{"poc incomplete", func(img *Image, p *EncodeParams) {
			p.ProgressionChanges = []ProgressionChange{{ResEnd: 2, CompEnd: 1, LayerEnd: 1}}
		}},
	}
	for _, c := range bad {
		img := testImage(8, 8, gray(8), 9)
		p := &EncodeParams{}
		c.mod(img, p)
		if err := Encode(&bytes.Buffer{}, img, p); err == nil {
			t.Errorf("%s: no error", c.name)
		}
	}
	if err := Encode(&bytes.Buffer{}, nil, nil); err == nil {
		t.Error("nil image: no error")
	}
}

func TestRoundTripOffset(t *testing.T) {
	mixed := []Component{{Depth: 8, DX: 1, DY: 1}, {Depth: 12, Signed: true, DX: 2, DY: 3}, {Depth: 8, DX: 3, DY: 2}}
	cases := []struct {
		name   string
		x0, y0 int
		w, h   int
		comps  []Component
		p      EncodeParams
	}{
		{"even", 4, 6, 30, 20, mixed, EncodeParams{}},
		{"odd", 3, 5, 31, 21, mixed, EncodeParams{}},
		{"odd one sample", 1, 1, 2, 2, []Component{{Depth: 8, DX: 2, DY: 2}}, EncodeParams{}},
		{"component without samples", 1, 0, 1, 2, []Component{{Depth: 8, DX: 1, DY: 1}, {Depth: 8, DX: 2, DY: 1}}, EncodeParams{}},
		{"tiles", 7, 9, 50, 40, mixed, EncodeParams{TileWidth: 16, TileHeight: 16}},
		{"lossy", 5, 2, 40, 30, rgb(8), EncodeParams{Lossy: true, Quality: 100}},
		{"wrapper", 1, 0, 20, 20, rgb(8), EncodeParams{Wrapper: true}},
		{"roi", 3, 3, 40, 40, gray(8), EncodeParams{ROI: &Rectangle{5, 5, 20, 20}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			img := testImageAt(c.x0, c.y0, c.w, c.h, c.comps, 1)
			got := roundTrip(t, img, &c.p)
			if got.XOffset != c.x0 || got.YOffset != c.y0 || got.Width != c.w || got.Height != c.h {
				t.Errorf("geometry: got offset (%d,%d) size %dx%d, want (%d,%d) %dx%d",
					got.XOffset, got.YOffset, got.Width, got.Height, c.x0, c.y0, c.w, c.h)
			}
			if diff := cmp.Diff(img.Components, got.Components); diff != "" {
				t.Errorf("components (-want +got):\n%s", diff)
			}
			if c.p.Lossy {
				for i := range img.Planes {
					if q := psnr(img.Planes[i], got.Planes[i], img.Components[i].Depth); q < 40 {
						t.Errorf("plane %d: PSNR %.1f", i, q)
					}
				}
				return
			}
			if diff := cmp.Diff(img.Planes, got.Planes); diff != "" {
				t.Errorf("round trip failed (-want +got):\n%s", diff)
			}
		})
	}
}

func TestEncodeInvalidOffset(t *testing.T) {
	type offsetCase struct {
		name   string
		x0, y0 int
		w, h   int
	}
	cases := []offsetCase{
		{"negative x", -1, 0, 4, 4},
		{"negative y", 0, -1, 4, 4},
	}
	if strconv.IntSize == 64 {
		// offset plus size beyond 32 bits is not representable on 32-bit int
		limit := uint64(math.MaxUint32)
		cases = append(cases,
			offsetCase{"x overflow", int(limit - 3), 0, 4, 4},
			offsetCase{"y overflow", 0, int(limit - 2), 4, 4},
		)
	}
	for _, c := range cases {
		img := testImage(c.w, c.h, gray(8), 1)
		img.XOffset, img.YOffset = c.x0, c.y0
		if err := Encode(&bytes.Buffer{}, img, nil); err == nil {
			t.Errorf("%s: no error", c.name)
		}
	}
}

// Many levels or layers must not cost time or memory beyond the budget.
func TestEncodeHostileParams(t *testing.T) {
	img := testImage(16, 16, gray(8), 12)
	var buf bytes.Buffer
	if err := Encode(&buf, img, &EncodeParams{Levels: 32, Lossy: true}); err != nil {
		t.Errorf("32 levels: %v", err)
	}
	b := newTestBudget(1 << 20)
	err := Encode(&buf, img, &EncodeParams{Layers: 60000, Budget: b})
	if !errors.Is(err, membudget.ErrExceeded) {
		t.Errorf("60000 layers with a small budget: got %v, want budget error", err)
	}
}

// With explicit rates the layers must honour their targets roughly: each
// layer's data must stay below the target plus packet overhead, and the
// layer boundaries must be ordered.
func TestRatesHonoured(t *testing.T) {
	img := testImage(64, 64, gray(8), 13)
	rates := []float64{0.5, 1, 2}
	var buf bytes.Buffer
	if err := Encode(&buf, img, &EncodeParams{Rates: rates, Lossy: true, Quality: 100}); err != nil {
		t.Fatal(err)
	}
	total := buf.Len()
	// the final layer target is 2 bits per sample = 1024 bytes
	if total > 1024+600 {
		t.Errorf("codestream has %d bytes for a 2 bit/sample target", total)
	}
}

// Packed headers that do not fit one marker segment are split over
// several segments, each ending on a packet header boundary, and the
// result still decodes.
func TestPackedHeaderSegments(t *testing.T) {
	// tiny precincts and many layers give thousands of packets, whose
	// headers together exceed the 64 KiB a marker segment can hold
	img := testImage(256, 384, gray(8), 14)
	for _, mode := range []PackedHeaders{PackedInTile, PackedInMain} {
		var buf bytes.Buffer
		p := &EncodeParams{PackedHeaders: mode, Layers: 20, Precincts: []PrecinctSize{{2, 2}, {3, 3}}, CodeBlockWidth: 4, CodeBlockHeight: 4}
		if err := Encode(&buf, img, p); err != nil {
			t.Fatal(err)
		}
		data := buf.Bytes()
		marker := byte(0x61)
		if mode == PackedInMain {
			marker = 0x60
		}
		segments, total := 0, 0
		for i := 0; i+4 <= len(data); {
			if data[i] != 0xFF {
				i++
				continue
			}
			m := data[i+1]
			if m == 0x93 { // SOD: skip to the next tile-part is not needed; stop at the first body
				break
			}
			if m == 0x4F || m == 0xD9 || (m >= 0x30 && m <= 0x3F) {
				i += 2
				continue
			}
			n := int(data[i+2])<<8 | int(data[i+3])
			if m == marker {
				segments++
				total += n - 3
				if n > 65535 {
					t.Fatalf("segment of %d bytes", n)
				}
			}
			i += 2 + n
		}
		if segments < 2 || total <= 65532 {
			t.Errorf("mode %d: %d segments holding %d bytes; want several", mode, segments, total)
		}
		got, err := Decode(bytes.NewReader(data), nil)
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(img.Planes, got.Planes); diff != "" {
			t.Errorf("mode %d (-want +got):\n%s", mode, diff)
		}
	}
}

func FuzzRoundTrip(f *testing.F) {
	for _, s := range fuzzSeeds(f) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		b := newTestBudget(min(1<<20+1024*int64(len(data)), 4<<20))
		img, err := Decode(bytes.NewReader(data), &Options{Budget: b})
		if err != nil {
			return
		}
		// every decoded image is re-encoded losslessly, and the two
		// decodes must agree
		var buf bytes.Buffer
		if err := Encode(&buf, img, &EncodeParams{Wrapper: img.Wrapped}); err != nil {
			t.Fatalf("re-encoding the decoded image: %v", err)
		}
		again, err := Decode(bytes.NewReader(buf.Bytes()), &Options{Budget: newTestBudget(64 << 20)})
		if err != nil {
			t.Fatalf("decoding the re-encoded image: %v", err)
		}
		if diff := cmp.Diff(img.Components, again.Components); diff != "" {
			t.Fatalf("re-encoded components differ (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(img.Palette, again.Palette); diff != "" {
			t.Fatalf("re-encoded palette differs (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(img.Planes, again.Planes); diff != "" {
			t.Fatalf("re-encoded image differs (-want +got):\n%s", diff)
		}
	})
}

func BenchmarkEncodeLossless(b *testing.B) {
	img := testImage(512, 512, rgb(8), 11)
	b.SetBytes(512 * 512 * 3)
	for b.Loop() {
		if err := Encode(&bytes.Buffer{}, img, &EncodeParams{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeLossless(b *testing.B) {
	img := testImage(512, 512, rgb(8), 11)
	var buf bytes.Buffer
	if err := Encode(&buf, img, &EncodeParams{}); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(512 * 512 * 3)
	for b.Loop() {
		if _, err := Decode(bytes.NewReader(buf.Bytes()), nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeLossy(b *testing.B) {
	img := testImage(512, 512, rgb(8), 11)
	var buf bytes.Buffer
	if err := Encode(&buf, img, &EncodeParams{Lossy: true}); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(512 * 512 * 3)
	for b.Loop() {
		if _, err := Decode(bytes.NewReader(buf.Bytes()), nil); err != nil {
			b.Fatal(err)
		}
	}
}

func ExampleEncode() {
	img := &Image{
		Width: 4, Height: 2,
		Components: []Component{{Depth: 8, DX: 1, DY: 1, Width: 4, Height: 2}},
		Planes:     []Plane{{Width: 4, Height: 2, Pix: []int32{0, 64, 128, 255, 255, 128, 64, 0}}},
	}
	var buf bytes.Buffer
	if err := Encode(&buf, img, &EncodeParams{}); err != nil {
		panic(err)
	}
	got, err := Decode(&buf, nil)
	if err != nil {
		panic(err)
	}
	fmt.Println(got.Planes[0].Pix)
	// Output: [0 64 128 255 255 128 64 0]
}

func TestDecodeWorkers(t *testing.T) {
	comps := []Component{{Depth: 8, DX: 1, DY: 1}, {Depth: 8, DX: 2, DY: 1}, {Depth: 10, DX: 1, DY: 3}}
	img := testImage(130, 97, comps, 15)
	poc := []ProgressionChange{
		{ResStart: 0, ResEnd: 2, CompStart: 0, CompEnd: 3, LayerEnd: 2, Progression: LRCP},
		{ResStart: 0, ResEnd: 6, CompStart: 0, CompEnd: 3, LayerEnd: 2, Progression: CPRL},
	}
	for _, p := range []EncodeParams{
		{}, // a single tile: the workers share its code-blocks
		{Lossy: true, Layers: 3, CodeBlockWidth: 16, CodeBlockHeight: 16},
		{TileWidth: 32, TileHeight: 40, Layers: 2},
		{TileWidth: 32, TileHeight: 40, PackedHeaders: PackedInMain, TileParts: 2},
		{TileWidth: 32, TileHeight: 40, PackedHeaders: PackedInTile, TileParts: 3, Layers: 2, ProgressionChanges: poc},
		{Lossy: true, TileWidth: 50, TileHeight: 50, SOP: true, EPH: true, Bypass: true, Terminate: true},
		{TileWidth: 50, TileHeight: 50, ROI: &Rectangle{10, 10, 60, 60}, SegmentationSymbols: true},
	} {
		var buf bytes.Buffer
		if err := Encode(&buf, img, &p); err != nil {
			t.Fatal(err)
		}
		data := buf.Bytes()
		for _, reduce := range []int{0, 1, 2} {
			want, err := Decode(bytes.NewReader(data), &Options{Reduce: reduce})
			if err != nil {
				t.Fatal(err)
			}
			for _, workers := range []int{2, 3, 16} {
				got, err := Decode(bytes.NewReader(data), &Options{Workers: workers, Reduce: reduce})
				if err != nil {
					t.Fatalf("%d workers, reduce %d: %v", workers, reduce, err)
				}
				if diff := cmp.Diff(want.Planes, got.Planes); diff != "" {
					t.Errorf("%d workers, reduce %d (-want +got):\n%s", workers, reduce, diff)
				}
			}
		}

		// a budget that suffices sequentially also suffices concurrently,
		// and one that does not stops the concurrent decode as well
		seq := membudget.New(64 << 20)
		if _, err := Decode(bytes.NewReader(data), &Options{Budget: seq}); err != nil {
			t.Fatal(err)
		}
		used := 64<<20 - seq.Available()
		if _, err := Decode(bytes.NewReader(data), &Options{Workers: 4, Budget: membudget.New(2 * used)}); err != nil {
			t.Errorf("twice the sequential budget was not enough with workers: %v", err)
		}
		if _, err := Decode(bytes.NewReader(data), &Options{Workers: 4, Budget: membudget.New(used / 4)}); !errors.Is(err, membudget.ErrExceeded) {
			t.Errorf("a quarter of the sequential budget: got %v, want budget error", err)
		}

		// truncated input decodes to a partial image with any number of workers
		if _, err := Decode(bytes.NewReader(data[:len(data)*2/3]), &Options{Workers: 3}); err != nil {
			t.Errorf("truncated input with workers: %v", err)
		}
	}
}

// writeBack encodes img with p, decodes the result and checks that it
// equals img.
func writeBack(t *testing.T, img *Image, p *EncodeParams) {
	t.Helper()
	got := roundTrip(t, img, p)
	if diff := cmp.Diff(img, got); diff != "" {
		t.Errorf("write-back changed the image (-want +got):\n%s", diff)
	}
}

// patchMainHeader returns a copy of the codestream or JP2 file with
// every component of the codestream given the depth and the guard bits
// of every quantisation marker segment raised by raise, or to the
// largest value the field holds.  For a reversible codestream this
// places the coded coefficients that many planes higher.
func patchMainHeader(t testing.TB, data []byte, depth, raise int) []byte {
	t.Helper()
	data = append([]byte(nil), data...)
	// the codestream starts with SOC and SIZ, also inside a JP2 file
	pos := bytes.Index(data, []byte{0xFF, 0x4F, 0xFF, 0x51}) + 2
	for {
		marker := binary.BigEndian.Uint16(data[pos:])
		if marker == 0xFF90 {
			return data
		}
		n := int(binary.BigEndian.Uint16(data[pos+2:]))
		switch marker {
		case 0xFF51: // SIZ
			nc := int(binary.BigEndian.Uint16(data[pos+38:]))
			for c := range nc {
				i := pos + 40 + 3*c
				data[i] = data[i]&0x80 | byte(depth-1)
			}
		case 0xFF5C: // QCD
			raiseGuardBits(&data[pos+4], raise)
		case 0xFF5D: // QCC, fewer than 257 components
			raiseGuardBits(&data[pos+5], raise)
		}
		pos += 2 + n
	}
}

// raiseGuardBits raises the guard bits in the quantization style byte s
// by raise, at most to 7.
func raiseGuardBits(s *byte, raise int) {
	g := min(int(*s>>5)+raise, 7)
	*s = *s&0x1F | byte(g<<5)
}

// truncatedImage returns an image with components of the given depth
// and the truncation of decoded images, with planes spanning the full
// range of their values.
func truncatedImage(w, h, n, depth int, signed bool, seed uint64) *Image {
	comps := make([]Component, n)
	for i := range comps {
		comps[i] = Component{Depth: depth, Signed: signed, Truncation: max(0, depth-MaxPrecision), DX: 1, DY: 1}
	}
	img := testImage(w, h, comps, seed)
	// extreme values next to each other give the largest coefficients
	for c := range img.Planes {
		lo, hi := sampleRange(comps[c])
		pix := img.Planes[c].Pix
		for i := range min(len(pix), 2*w) {
			if (i+i/w+c)%2 == 0 {
				pix[i] = lo
			} else {
				pix[i] = hi
			}
		}
	}
	return img
}

// Every image decoded from a file is written back exactly, for all
// accepted depths.  The first file is written by the encoder, or built
// with deep samples whose lowest bits are not zero.
func TestRoundTripDeep(t *testing.T) {
	depths := []int{1, 8, 16, 24, 25, 28, 31, 32, 33, MaxDepth}
	configs := []struct {
		name string
		n    int
		p    EncodeParams
	}{
		{"grey", 1, EncodeParams{Wrapper: true}},
		{"rgb", 3, EncodeParams{Wrapper: true, Levels: 3}},
		{"grey tiled", 1, EncodeParams{TileWidth: 16, TileHeight: 8, Levels: 2}},
		{"rgb tiled", 3, EncodeParams{TileWidth: 8, TileHeight: 16, Levels: 6, CodeBlockWidth: 8, CodeBlockHeight: 4}},
	}
	decode := func(t *testing.T, data []byte) *Image {
		t.Helper()
		img, err := Decode(bytes.NewReader(data), &Options{Budget: newTestBudget(64 << 20)})
		if err != nil {
			t.Fatal(err)
		}
		return img
	}
	for _, depth := range depths {
		for _, signed := range []bool{false, true} {
			for _, c := range configs {
				t.Run(fmt.Sprintf("%s %d signed=%t", c.name, depth, signed), func(t *testing.T) {
					img := truncatedImage(40, 36, c.n, depth, signed, uint64(depth))
					var buf bytes.Buffer
					if err := Encode(&buf, img, &c.p); err != nil {
						t.Fatal(err)
					}
					first := decode(t, buf.Bytes())
					if diff := cmp.Diff(img.Components, first.Components); diff != "" {
						t.Errorf("wrong components (-want +got):\n%s", diff)
					}
					if diff := cmp.Diff(img.Planes, first.Planes); diff != "" {
						t.Errorf("wrong planes (-want +got):\n%s", diff)
					}
					writeBack(t, first, &c.p)

					// The same coefficients in a deeper codestream: the
					// samples have non-zero bits below the held ones.
					if depth <= 24 {
						return
					}
					trunc := depth - MaxPrecision
					raise := min(trunc-1, 5)
					if raise == 0 {
						return // no room below the held bits, see the synthetic case
					}
					deep := decode(t, patchMainHeader(t, buf.Bytes(), depth, raise))
					writeBack(t, deep, &c.p)
				})
			}
			if depth <= 24 {
				continue
			}
			t.Run(fmt.Sprintf("synthetic %d signed=%t", depth, signed), func(t *testing.T) {
				data, _ := deepImage(t, depth, signed, true)
				writeBack(t, decode(t, data), &EncodeParams{Levels: NoLevels})
			})
		}
	}
}

// The component transform is applied to matching components, unless
// the bit-plane it adds would leave a sub-band with more planes than a
// codestream can signal; the image is then coded without it.
func TestMCTFallback(t *testing.T) {
	// usesMCT reports whether the first tile is coded with the component
	// transform, after checking the round trip
	usesMCT := func(t *testing.T, img *Image) bool {
		t.Helper()
		var buf bytes.Buffer
		if err := Encode(&buf, img, &EncodeParams{Levels: 2}); err != nil {
			t.Fatal(err)
		}
		cr := codestream.NewReader(buf.Bytes(), nil)
		m, err := cr.ReadMainHeader()
		if err != nil {
			t.Fatal(err)
		}
		tp, err := cr.NextTilePart()
		if err != nil {
			t.Fatal(err)
		}
		tile, err := t2.NewTile(m, []*codestream.TilePart{tp}, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := roundTrip(t, img, &EncodeParams{Levels: 2})
		if diff := cmp.Diff(img.Planes, got.Planes); diff != "" {
			t.Errorf("round trip failed (-want +got):\n%s", diff)
		}
		return tile.MCT
	}
	if !usesMCT(t, testImage(48, 40, rgb(8), 3)) {
		t.Error("component transform not applied to an 8-bit image")
	}
	if !usesMCT(t, truncatedImage(48, 40, 3, 30, false, 3)) {
		t.Error("component transform not applied to a 30-bit image")
	}
	pick := func(x, y, c int) bool { return (x+y)%2 == 0 }
	if usesMCT(t, extremeImage(48, 40, 3, MaxDepth, false, pick)) {
		t.Errorf("component transform applied to a %d-bit image of extreme values", MaxDepth)
	}
}

// A palette column of the largest depth is written back exactly, also
// when its values have non-zero bits below the held ones.
func TestRoundTripDeepPalette(t *testing.T) {
	img := testImage(8, 8, gray(4), 3)
	img.Palette = &Palette{Columns: []PaletteColumn{{Depth: 38, Signed: true, Truncation: 7}, {Depth: 32, Truncation: 1}, {Depth: 8}}}
	for i := range 16 {
		img.Palette.Entries = append(img.Palette.Entries, []int32{int32(-1<<30 + i*0x7654321), int32(i * 0x7000001), int32(i)})
	}
	img.Palette.Entries[15][0] = 1<<30 - 1
	img.Channels = []Channel{
		{Component: 0, PaletteColumn: 0, Kind: ChannelColor, Assoc: 1},
		{Component: 0, PaletteColumn: 1, Kind: ChannelColor, Assoc: 2},
		{Component: 0, PaletteColumn: 2, Kind: ChannelColor, Assoc: 3},
	}
	p := &EncodeParams{Wrapper: true}
	var buf bytes.Buffer
	if err := Encode(&buf, img, p); err != nil {
		t.Fatal(err)
	}
	first, err := Decode(bytes.NewReader(buf.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(img.Palette, first.Palette); diff != "" {
		t.Errorf("wrong palette (-want +got):\n%s", diff)
	}
	writeBack(t, first, p)

	// set low-order bits of the deep columns in the file
	data := append([]byte(nil), buf.Bytes()...)
	i := bytes.Index(data, []byte("pclr")) + 4
	entries := i + 3 + 3
	for e := range 16 {
		row := data[entries+e*(5+4+1):]
		row[4] |= 0x5A
		row[5+3] |= 0x01
	}
	deep, err := Decode(bytes.NewReader(data), nil)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(img.Palette, deep.Palette); diff != "" {
		t.Errorf("low-order bits changed the held values (-want +got):\n%s", diff)
	}
	writeBack(t, deep, p)
}

// Components deeper than MaxDepth bits are rejected on reading.
func TestDecodeTooDeep(t *testing.T) {
	var buf bytes.Buffer
	if err := Encode(&buf, testImage(8, 8, gray(8), 1), nil); err != nil {
		t.Fatal(err)
	}
	ok := patchMainHeader(t, buf.Bytes(), MaxDepth, 0)
	if _, err := Decode(bytes.NewReader(ok), nil); err != nil {
		t.Errorf("depth %d rejected: %v", MaxDepth, err)
	}
	bad := patchMainHeader(t, buf.Bytes(), MaxDepth+1, 0)
	if _, err := Decode(bytes.NewReader(bad), nil); err == nil {
		t.Errorf("depth %d accepted by Decode", MaxDepth+1)
	}
	h, err := ReadHeader(bytes.NewReader(bad), nil)
	if err != nil {
		t.Fatalf("depth %d rejected by ReadHeader: %v", MaxDepth+1, err)
	}
	if h.Components[0].Depth != MaxDepth+1 {
		t.Errorf("ReadHeader reports depth %d, want %d", h.Components[0].Depth, MaxDepth+1)
	}
}

// Lossy encoding of truncated components quantises at the scale of the
// full samples, with the quality of the same image at 24 bits.
func TestRoundTripDeepLossy(t *testing.T) {
	var ref []float64
	for _, depth := range []int{24, 30, MaxDepth} {
		img := truncatedImage(48, 40, 3, depth, false, 5)
		got := roundTrip(t, img, &EncodeParams{Lossy: true, Quality: 90})
		if diff := cmp.Diff(img.Components, got.Components); diff != "" {
			t.Errorf("depth %d: wrong components (-want +got):\n%s", depth, diff)
		}
		for i := range img.Planes {
			p := psnr(img.Planes[i], got.Planes[i], MaxPrecision)
			if depth == 24 {
				ref = append(ref, p)
			}
			if p < 35 || math.Abs(p-ref[i]) > 0.5 {
				t.Errorf("depth %d: plane %d has PSNR %.1f, want about %.1f", depth, i, p, ref[i])
			}
		}
	}
}

// extremeImage returns an image of n components whose samples take only
// the smallest and the largest value of their range, the largest where
// pick reports true.
func extremeImage(w, h, n, depth int, signed bool, pick func(x, y, c int) bool) *Image {
	comps := make([]Component, n)
	for i := range comps {
		comps[i] = Component{Depth: depth, Signed: signed, Truncation: depthTruncation(depth), DX: 1, DY: 1, Width: w, Height: h}
	}
	img := &Image{Width: w, Height: h, Components: comps, Channels: defaultChannels(n)}
	for c := range comps {
		lo, hi := sampleRange(comps[c])
		pix := make([]int32, w*h)
		for y := range h {
			for x := range w {
				pix[y*w+x] = lo
				if pick(x, y, c) {
					pix[y*w+x] = hi
				}
			}
		}
		img.Planes = append(img.Planes, Plane{Width: w, Height: h, Pix: pix})
	}
	return img
}

// extremePatterns returns sample patterns which produce large wavelet
// coefficients: checkerboards, stripes of several periods and noise,
// each also with the second component inverted, which maximises the
// chroma of the component transform.
func extremePatterns(seed uint64) map[string]func(x, y, c int) bool {
	base := map[string]func(x, y int) bool{
		"checker": func(x, y int) bool { return (x+y)%2 == 0 },
	}
	for _, period := range []int{1, 2, 3, 8} {
		base[fmt.Sprintf("columns %d", period)] = func(x, y int) bool { return (x/period)%2 == 0 }
		base[fmt.Sprintf("rows %d", period)] = func(x, y int) bool { return (y/period)%2 == 0 }
	}
	rng := rand.New(rand.NewPCG(seed, 7))
	noise := make([]bool, 1<<16)
	for i := range noise {
		noise[i] = rng.IntN(2) == 0
	}
	base["noise"] = func(x, y int) bool { return noise[(y*251+x)%len(noise)] }

	patterns := make(map[string]func(x, y, c int) bool)
	for name, f := range base {
		patterns[name] = func(x, y, c int) bool { return f(x, y) }
		patterns[name+" inverted"] = func(x, y, c int) bool { return f(x, y) != (c == 1) }
	}
	return patterns
}

// Images of extreme values, which need the most magnitude bit-planes
// relative to their depth, are encoded losslessly for every number of
// decomposition levels, with and without the component transform.
func TestRoundTripExtreme(t *testing.T) {
	configs := []struct {
		depth  int
		sizes  [][2]int
		levels []int
	}{
		{1, [][2]int{{48, 138}, {17, 33}}, []int{1, 2, 3, 4, 5}},
		{2, [][2]int{{48, 138}, {17, 33}}, []int{1, 2, 3, 4, 5}},
		{MaxDepth, [][2]int{{48, 138}}, []int{2, 5}},
	}
	for _, cfg := range configs {
		depth := cfg.depth
		for _, signed := range []bool{false, true} {
			for name, pick := range extremePatterns(uint64(depth)) {
				for _, n := range []int{1, 3} {
					for _, sz := range cfg.sizes {
						img := extremeImage(sz[0], sz[1], n, depth, signed, pick)
						for _, levels := range cfg.levels {
							var buf bytes.Buffer
							if err := Encode(&buf, img, &EncodeParams{Levels: levels}); err != nil {
								t.Errorf("depth %d signed=%t %s n=%d %dx%d levels=%d: %v",
									depth, signed, name, n, sz[0], sz[1], levels, err)
								continue
							}
							got, err := Decode(bytes.NewReader(buf.Bytes()), nil)
							if err != nil {
								t.Fatal(err)
							}
							if reflect.DeepEqual(img.Planes, got.Planes) {
								continue
							}
							if diff := cmp.Diff(img.Planes, got.Planes); diff != "" {
								t.Errorf("depth %d signed=%t %s n=%d %dx%d levels=%d: round trip failed (-want +got):\n%s",
									depth, signed, name, n, sz[0], sz[1], levels, diff)
							}
						}
					}
				}
			}
		}
	}
}
