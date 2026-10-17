// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jpx

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/google/go-cmp/cmp"
	"seehuhn.de/go/membudget"
)

// testImage builds a synthetic image with smooth gradients, an edge and
// noise, which exercises all sub-bands.  Sample values span the full
// range of each component.
func testImage(w, h int, comps []Component, seed uint64) *Image {
	rng := rand.New(rand.NewPCG(seed, 99))
	img := &Image{Header: Header{Width: w, Height: h, Components: comps}}
	for ci, c := range comps {
		pw, ph := ceilDiv(w, c.DX), ceilDiv(h, c.DY)
		img.Components[ci].Width, img.Components[ci].Height = pw, ph
		lo, hi := 0.0, float64(uint64(1)<<uint(c.Depth)-1)
		if c.Signed {
			lo, hi = -float64(uint64(1)<<uint(c.Depth-1)), float64(uint64(1)<<uint(c.Depth-1)-1)
		}
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
	img := testImage(8, 8, gray(8), 9)
	bad := []struct {
		name string
		mod  func(img *Image, p *EncodeParams)
	}{
		{"wrong plane size", func(img *Image, p *EncodeParams) { img.Planes[0].Width = 7 }},
		{"missing plane", func(img *Image, p *EncodeParams) { img.Planes = nil }},
		{"depth too large", func(img *Image, p *EncodeParams) { img.Components[0].Depth = 30 }},
		{"bad block size", func(img *Image, p *EncodeParams) { p.CodeBlockWidth = 100 }},
		{"block too large", func(img *Image, p *EncodeParams) { p.CodeBlockWidth, p.CodeBlockHeight = 1024, 1024 }},
		{"bad progression", func(img *Image, p *EncodeParams) { p.Progression = 7 }},
		{"bad tile", func(img *Image, p *EncodeParams) { p.TileWidth = 10 }},
		{"bad levels", func(img *Image, p *EncodeParams) { p.Levels = 40 }},
		{"empty roi", func(img *Image, p *EncodeParams) { p.ROI = &Rectangle{3, 3, 3, 5} }},
		{"reduced image", func(img *Image, p *EncodeParams) { img.Reduce = 1 }},
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
	_ = img
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
		// re-encode losslessly and decode again; the two decodes must agree
		var buf bytes.Buffer
		if err := Encode(&buf, img, &EncodeParams{}); err != nil {
			return
		}
		again, err := Decode(bytes.NewReader(buf.Bytes()), &Options{Budget: newTestBudget(64 << 20)})
		if err != nil {
			t.Fatalf("decoding the re-encoded image: %v", err)
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
		Header: Header{
			Width: 4, Height: 2,
			Components: []Component{{Depth: 8, DX: 1, DY: 1, Width: 4, Height: 2}},
		},
		Planes: []Plane{{Width: 4, Height: 2, Pix: []int32{0, 64, 128, 255, 255, 128, 64, 0}}},
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
