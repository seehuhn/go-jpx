// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jpx

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/rand/v2"
	"testing"

	"github.com/google/go-cmp/cmp"
	"seehuhn.de/go/membudget"
)

func TestApplyPalette(t *testing.T) {
	img := &Image{
		Width: 2, Height: 2,
		Components: []Component{{Depth: 2, DX: 1, DY: 1, Width: 2, Height: 2}},
		Palette: &Palette{
			Entries: [][]int32{{10, 100}, {20, 200}, {30, 300}},
			Columns: []PaletteColumn{{Depth: 8}, {Depth: 9}},
		},
		Channels: []Channel{
			{Component: 0, PaletteColumn: 0, Kind: ChannelColor, Assoc: 1},
			{Component: 0, PaletteColumn: 1, Kind: ChannelColor, Assoc: 2},
			{Component: 0, PaletteColumn: -1, Kind: ChannelOpacity, Assoc: 0},
		},
		Planes: []Plane{{Width: 2, Height: 2, Pix: []int32{0, 1, 2, 3}}},
	}
	got, err := ApplyPalette(img, newTestBudget(1<<20))
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

	// values of palette columns deeper than MaxPrecision bits lose their
	// lowest bits
	deep := *img
	deep.Palette = &Palette{
		Entries: [][]int32{{-1 << 30}, {1<<30 - 1}},
		Columns: []PaletteColumn{{Depth: 36, Signed: true, Truncation: 5}},
	}
	deep.Channels = []Channel{{Component: 0, PaletteColumn: 0}}
	got, err = ApplyPalette(&deep, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantDeep := []Component{{Depth: 36, Signed: true, Truncation: 12, DX: 1, DY: 1, Width: 2, Height: 2}}
	if diff := cmp.Diff(wantDeep, got.Components); diff != "" {
		t.Errorf("deep palette components (-want +got):\n%s", diff)
	}
	wantDeepPix := []int32{-1 << 23, 1<<23 - 1, 1<<23 - 1, 1<<23 - 1}
	if diff := cmp.Diff(wantDeepPix, got.Planes[0].Pix); diff != "" {
		t.Errorf("deep palette samples (-want +got):\n%s", diff)
	}

	// an image without a palette is returned unchanged
	plain := testImage(4, 4, gray(8), 1)
	if same, _ := ApplyPalette(plain, nil); same != plain {
		t.Error("image without palette was copied")
	}

	// the new planes are charged to the budget
	if _, err := ApplyPalette(img, newTestBudget(8)); !errors.Is(err, membudget.ErrExceeded) {
		t.Errorf("tiny budget: got %v, want budget error", err)
	}
	// inconsistent images are rejected, not mishandled
	bad := *img
	bad.Channels = []Channel{{Component: 5, PaletteColumn: 0}}
	if _, err := ApplyPalette(&bad, nil); err == nil {
		t.Error("channel with missing component accepted")
	}
	bad.Channels = []Channel{{Component: 0, PaletteColumn: 7}}
	if _, err := ApplyPalette(&bad, nil); err == nil {
		t.Error("channel with missing palette column accepted")
	}
}

func TestSYCCToRGB(t *testing.T) {
	img := &Image{
		Components: rgb(8),
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
	short := &Image{Components: gray(8), Planes: img.Planes}
	if err := SYCCToRGB(short); err == nil {
		t.Error("three planes with one component accepted")
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

// seqPlane returns a plane whose samples are 1, 2, 3, ... in row-major
// order.
func seqPlane(w, h int) Plane {
	p := Plane{Width: w, Height: h, Pix: make([]int32, w*h)}
	for i := range p.Pix {
		p.Pix[i] = int32(i + 1)
	}
	return p
}

func TestUpsample(t *testing.T) {
	cases := []struct {
		name   string
		x0, y0 int
		w, h   int
		reduce int
		dx, dy int
		src    Plane
		want   Plane
	}{
		{
			// grid columns 1..5 hold component columns 1,2; column 1 lies
			// before the first sample; grid rows 1..6 hold component rows 1,2
			name: "odd offset", x0: 1, y0: 1, w: 5, h: 6, dx: 2, dy: 3,
			src: seqPlane(2, 2),
			want: Plane{Width: 5, Height: 6, Pix: []int32{
				1, 1, 1, 2, 2,
				1, 1, 1, 2, 2,
				1, 1, 1, 2, 2,
				1, 1, 1, 2, 2,
				1, 1, 1, 2, 2,
				3, 3, 3, 4, 4,
			}},
		},
		{
			// grid columns 2..6 hold component columns 1..3, grid rows
			// 3..7 hold component rows 1,2
			name: "even offset", x0: 2, y0: 3, w: 5, h: 5, dx: 2, dy: 3,
			src: seqPlane(3, 2),
			want: Plane{Width: 5, Height: 5, Pix: []int32{
				1, 1, 2, 2, 3,
				1, 1, 2, 2, 3,
				1, 1, 2, 2, 3,
				4, 4, 5, 5, 6,
				4, 4, 5, 5, 6,
			}},
		},
		{
			name: "no offset", x0: 0, y0: 0, w: 3, h: 4, dx: 2, dy: 3,
			src: seqPlane(2, 2),
			want: Plane{Width: 3, Height: 4, Pix: []int32{
				1, 1, 2,
				1, 1, 2,
				1, 1, 2,
				3, 3, 4,
			}},
		},
		{
			// reduced grid columns 2..5, component columns 1,2; reduced
			// grid rows 1..6, component rows 1,2
			name: "reduced", x0: 3, y0: 1, w: 8, h: 12, reduce: 1, dx: 2, dy: 3,
			src: seqPlane(2, 2),
			want: Plane{Width: 4, Height: 6, Pix: []int32{
				1, 1, 2, 2,
				1, 1, 2, 2,
				1, 1, 2, 2,
				1, 1, 2, 2,
				1, 1, 2, 2,
				3, 3, 4, 4,
			}},
		},
		{
			// reduced grid columns 1..5 and component columns 1,2: grid
			// column 1 lies before the first sample
			name: "reduced odd", x0: 1, y0: 0, w: 10, h: 2, reduce: 1, dx: 2, dy: 1,
			src: seqPlane(2, 1),
			want: Plane{Width: 5, Height: 1, Pix: []int32{
				1, 1, 1, 2, 2,
			}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			full := Plane{Width: ceilShift(c.x0+c.w, c.reduce) - ceilShift(c.x0, c.reduce),
				Height: ceilShift(c.y0+c.h, c.reduce) - ceilShift(c.y0, c.reduce)}
			full.Pix = make([]int32, full.Width*full.Height)
			cw := ceilDiv(c.x0+c.w, c.dx) - ceilDiv(c.x0, c.dx)
			ch := ceilDiv(c.y0+c.h, c.dy) - ceilDiv(c.y0, c.dy)
			img := &Image{
				XOffset: c.x0, YOffset: c.y0, Width: c.w, Height: c.h,
				Components: []Component{
					{Depth: 8, DX: 1, DY: 1, Width: c.w, Height: c.h},
					{Depth: 8, DX: c.dx, DY: c.dy, Width: cw, Height: ch},
				},
				Channels: defaultChannels(2),
				Reduce:   c.reduce,
				Planes:   []Plane{full, c.src},
			}
			saved := *img
			saved.Components = append([]Component(nil), img.Components...)
			got, err := Upsample(img, newTestBudget(1<<20))
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(c.want, got.Planes[1]); diff != "" {
				t.Errorf("upsampled plane (-want +got):\n%s", diff)
			}
			if &got.Planes[0].Pix[0] != &full.Pix[0] {
				t.Error("full-size plane was copied")
			}
			wantComps := []Component{
				{Depth: 8, DX: 1, DY: 1, Width: c.w, Height: c.h},
				{Depth: 8, DX: 1, DY: 1, Width: c.w, Height: c.h},
			}
			if diff := cmp.Diff(wantComps, got.Components); diff != "" {
				t.Errorf("components (-want +got):\n%s", diff)
			}
			if got.Width != c.w || got.Height != c.h || got.XOffset != c.x0 || got.YOffset != c.y0 || got.Reduce != c.reduce {
				t.Errorf("geometry changed: %+v", got.Header)
			}
			if diff := cmp.Diff(saved.Header, img.Header); diff != "" {
				t.Errorf("input modified (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUpsampleUnchanged(t *testing.T) {
	img := testImage(5, 4, rgb(8), 1)
	got, err := Upsample(img, newTestBudget(0))
	if err != nil {
		t.Fatal(err)
	}
	for i := range img.Planes {
		if &got.Planes[i].Pix[0] != &img.Planes[i].Pix[0] {
			t.Errorf("plane %d was copied", i)
		}
	}
	if diff := cmp.Diff(img.Header, got.Header); diff != "" {
		t.Errorf("header changed (-want +got):\n%s", diff)
	}
}

// charged returns the number of bytes fn charges to a budget, including
// the budget's per-allocation overhead.
func charged(t *testing.T, fn func(b *membudget.Budget) error) int64 {
	t.Helper()
	b := newTestBudget(1 << 20)
	before := b.Available()
	if err := fn(b); err != nil {
		t.Fatal(err)
	}
	return before - b.Available()
}

func TestUpsampleErrors(t *testing.T) {
	sub := []Component{{Depth: 8, DX: 1, DY: 1}, {Depth: 8, DX: 2, DY: 2}}
	img := testImageAt(1, 1, 9, 7, sub, 1)
	if _, err := Upsample(img, newTestBudget(4*9*7-1)); !errors.Is(err, membudget.ErrExceeded) {
		t.Errorf("tiny budget: got %v, want budget error", err)
	}
	if n := charged(t, func(b *membudget.Budget) error { _, err := Upsample(img, b); return err }); n < 4*9*7 {
		t.Errorf("charged %d bytes, want at least 4 per new sample", n)
	}
	// shared planes are not charged
	full := testImageAt(1, 1, 9, 7, []Component{{Depth: 8, DX: 1, DY: 1}}, 1)
	if n := charged(t, func(b *membudget.Budget) error { _, err := Upsample(full, b); return err }); n != 0 {
		t.Errorf("charged %d bytes for shared planes", n)
	}

	bad := *img
	bad.Planes = img.Planes[:1]
	if _, err := Upsample(&bad, nil); err == nil {
		t.Error("missing plane accepted")
	}
	bad = *img
	bad.Planes = []Plane{img.Planes[0], {Width: 2, Height: 2, Pix: make([]int32, 4)}}
	if _, err := Upsample(&bad, nil); err == nil {
		t.Error("plane of wrong size accepted")
	}
	bad = *img
	bad.Components = []Component{sub[0], {Depth: 8, DX: 0, DY: 2}}
	if _, err := Upsample(&bad, nil); err == nil {
		t.Error("zero sub-sampling factor accepted")
	}
	// a component without samples cannot be upsampled
	empty := testImageAt(1, 0, 1, 2, []Component{{Depth: 8, DX: 2, DY: 1}}, 1)
	if _, err := Upsample(empty, nil); err == nil {
		t.Error("component without samples accepted")
	}
	// image rows 45..46 keep one row at Reduce 1, the DY=3 component
	// (rows 15..15) keeps none
	reduced := &Image{XOffset: 66, YOffset: 45, Width: 40, Height: 2, Reduce: 1}
	reduced.Components = []Component{{Depth: 8, DX: 1, DY: 3, Width: 40, Height: 1}}
	reduced.Planes = []Plane{{Width: 20, Height: 0}}
	if _, err := Upsample(reduced, nil); err == nil {
		t.Error("reduced component without samples accepted")
	}
}

// refUpsample computes the value of a full-resolution grid point
// directly from the sample-position rule.
func refUpsample(img *Image, c int, x, y int) int32 {
	comp := img.Components[c]
	pl := img.Planes[c]
	cx0 := ceilDiv(img.XOffset, comp.DX)
	cy0 := ceilDiv(img.YOffset, comp.DY)
	xc := min(max(x/comp.DX, cx0), cx0+pl.Width-1)
	yc := min(max(y/comp.DY, cy0), cy0+pl.Height-1)
	return pl.At(xc-cx0, yc-cy0)
}

func TestUpsampleEndToEnd(t *testing.T) {
	sub := []Component{{Depth: 8, DX: 1, DY: 1}, {Depth: 8, DX: 2, DY: 2}, {Depth: 8, DX: 3, DY: 2}}
	for _, off := range [][2]int{{0, 0}, {1, 1}, {4, 3}, {5, 2}} {
		src := testImageAt(off[0], off[1], 23, 17, sub, 2)
		var buf bytes.Buffer
		if err := Encode(&buf, src, nil); err != nil {
			t.Fatal(err)
		}
		dec, err := Decode(&buf, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Upsample(dec, nil)
		if err != nil {
			t.Fatal(err)
		}
		for c := range sub {
			want := Plane{Width: 23, Height: 17, Pix: make([]int32, 23*17)}
			for y := range 17 {
				for x := range 23 {
					want.Pix[y*23+x] = refUpsample(src, c, x+off[0], y+off[1])
				}
			}
			if diff := cmp.Diff(want, got.Planes[c]); diff != "" {
				t.Errorf("offset %v, component %d (-want +got):\n%s", off, c, diff)
			}
		}
	}
}

func TestToUnsigned(t *testing.T) {
	cases := []struct {
		name  string
		comp  Component
		depth int
		in    []int32
		want  []int32
	}{
		{"signed same depth", Component{Depth: 8, Signed: true}, 8,
			[]int32{-128, -1, 0, 127}, []int32{0, 127, 128, 255}},
		{"8 to 16", Component{Depth: 8}, 16,
			[]int32{0, 1, 128, 255}, []int32{0, 257, 128 * 257, 65535}},
		// 32767·255/65535 = 127.498 and 32768·255/65535 = 127.502
		{"16 to 8", Component{Depth: 16}, 8,
			[]int32{0, 128, 129, 32767, 32768, 65535}, []int32{0, 0, 1, 127, 128, 255}},
		{"1 to 8", Component{Depth: 1}, 8, []int32{0, 1}, []int32{0, 255}},
		{"signed 1 to 8", Component{Depth: 1, Signed: true}, 8, []int32{-1, 0}, []int32{0, 255}},
		// v·65535/4095 = 16v + v/273
		{"12 to 16", Component{Depth: 12}, 16,
			[]int32{0, 1, 136, 137, 2048, 4095}, []int32{0, 16, 2176, 2193, 32776, 65535}},
		{"8 to 1", Component{Depth: 8}, 1, []int32{0, 127, 128, 255}, []int32{0, 0, 1, 1}},
		{"signed 24", Component{Depth: 24, Signed: true}, 24,
			[]int32{-1 << 23, -1, 0, 1<<23 - 1}, []int32{0, 1<<23 - 1, 1 << 23, 1<<24 - 1}},
		{"31 to 8", Component{Depth: 31}, 8,
			[]int32{0, 1 << 30, math.MaxInt32}, []int32{0, 128, 255}},
		// (2^24-1)/255 = 65793
		{"8 to 24", Component{Depth: 8}, 24,
			[]int32{0, 1, 255}, []int32{0, 65793, 1<<24 - 1}},
		{"clamped", Component{Depth: 4}, 8, []int32{-5, 16, 100}, []int32{0, 255, 255}},
		{"signed clamped", Component{Depth: 4, Signed: true}, 4, []int32{-9, 8, 100}, []int32{0, 15, 15}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.comp.DX, c.comp.DY = 1, 1
			c.comp.Width, c.comp.Height = len(c.in), 1
			in := append([]int32(nil), c.in...)
			img := &Image{
				Width: len(in), Height: 1, Components: []Component{c.comp}, Channels: defaultChannels(1),
				Planes: []Plane{{Width: len(in), Height: 1, Pix: in}},
			}
			got, err := ToUnsigned(img, c.depth, newTestBudget(1<<20))
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(c.want, got.Planes[0].Pix); diff != "" {
				t.Errorf("samples (-want +got):\n%s", diff)
			}
			wantComp := c.comp
			wantComp.Depth, wantComp.Signed = c.depth, false
			if diff := cmp.Diff([]Component{wantComp}, got.Components); diff != "" {
				t.Errorf("components (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(c.in, in); diff != "" {
				t.Errorf("input modified (-want +got):\n%s", diff)
			}
			if img.Components[0] != c.comp {
				t.Error("input components modified")
			}
		})
	}
}

func TestToUnsignedShared(t *testing.T) {
	comps := []Component{{Depth: 8, DX: 1, DY: 1}, {Depth: 8, Signed: true, DX: 1, DY: 1}, {Depth: 12, DX: 1, DY: 1}}
	img := testImage(4, 3, comps, 1)
	got, err := ToUnsigned(img, 8, nil)
	if err != nil {
		t.Fatal(err)
	}
	if &got.Planes[0].Pix[0] != &img.Planes[0].Pix[0] {
		t.Error("unsigned plane at the target depth was copied")
	}
	for i := 1; i < 3; i++ {
		if &got.Planes[i].Pix[0] == &img.Planes[i].Pix[0] {
			t.Errorf("plane %d was not converted", i)
		}
	}
	if img.Components[1].Depth != 8 || !img.Components[1].Signed || img.Components[2].Depth != 12 {
		t.Error("input components modified")
	}
}

func TestToUnsignedErrors(t *testing.T) {
	img := testImage(4, 3, []Component{{Depth: 8, DX: 1, DY: 1}, {Depth: 12, DX: 1, DY: 1}}, 1)
	for _, depth := range []int{-1, 0, MaxPrecision + 1, 64} {
		if _, err := ToUnsigned(img, depth, nil); err == nil {
			t.Errorf("depth %d accepted", depth)
		}
	}
	// only the 12-bit plane is converted and charged
	if _, err := ToUnsigned(img, 8, newTestBudget(4*12-1)); !errors.Is(err, membudget.ErrExceeded) {
		t.Errorf("tiny budget: got %v, want budget error", err)
	}
	if n := charged(t, func(b *membudget.Budget) error { _, err := ToUnsigned(img, 8, b); return err }); n < 4*12 {
		t.Errorf("charged %d bytes, want at least 4 per new sample", n)
	}
	// shared planes are not charged
	if n := charged(t, func(b *membudget.Budget) error { _, err := ToUnsigned(img, 12, b); return err }); n < 4*12 {
		t.Errorf("charged %d bytes, want at least 4 per new sample", n)
	}
	eight := testImage(4, 3, []Component{{Depth: 8, DX: 1, DY: 1}}, 1)
	if n := charged(t, func(b *membudget.Budget) error { _, err := ToUnsigned(eight, 8, b); return err }); n != 0 {
		t.Errorf("charged %d bytes for a shared plane", n)
	}
	bad := *img
	bad.Planes = img.Planes[:1]
	if _, err := ToUnsigned(&bad, 8, nil); err == nil {
		t.Error("missing plane accepted")
	}
	bad = *img
	bad.Components = []Component{{Depth: 8, DX: 1, DY: 1}, {Depth: 32, DX: 1, DY: 1}}
	if _, err := ToUnsigned(&bad, 8, nil); err == nil {
		t.Error("component with more than 31 bits accepted")
	}
}

// TestToUnsignedProperty compares ToUnsigned with exact rational
// arithmetic, and with float64 where float64 is precise enough.
func TestToUnsignedProperty(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for range 2000 {
		d := 1 + rng.IntN(maxPlaneBits)
		n := 1 + rng.IntN(MaxPrecision)
		signed := rng.IntN(2) == 1
		lo, hi := sampleRange(Component{Depth: d, Signed: signed})
		pix := make([]int32, 16)
		for i := range pix {
			pix[i] = int32(int64(lo) + rng.Int64N(int64(hi)-int64(lo)+1))
		}
		pix[0], pix[1] = lo, hi
		img := &Image{
			Width: len(pix), Height: 1, Components: []Component{{Depth: d, Signed: signed, DX: 1, DY: 1, Width: len(pix), Height: 1}},
			Planes: []Plane{{Width: len(pix), Height: 1, Pix: pix}},
		}
		got, err := ToUnsigned(img, n, nil)
		if err != nil {
			t.Fatal(err)
		}
		mIn := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(d)), big.NewInt(1))
		mOut := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(n)), big.NewInt(1))
		for i, v := range pix {
			u := int64(v) - int64(lo)
			// exact: floor((2·u·mOut + mIn) / (2·mIn))
			num := new(big.Int).Mul(big.NewInt(2*u), mOut)
			num.Add(num, mIn)
			want := num.Div(num, new(big.Int).Lsh(mIn, 1)).Int64()
			if int64(got.Planes[0].Pix[i]) != want {
				t.Fatalf("d=%d n=%d signed=%t v=%d: got %d, want %d", d, n, signed, v, got.Planes[0].Pix[i], want)
			}
			ref := float64(u) * (math.Ldexp(1, n) - 1) / (math.Ldexp(1, d) - 1)
			if math.Abs(float64(want)-ref) > 0.5+1e-6 {
				t.Fatalf("d=%d n=%d v=%d: float reference %g differs from %d", d, n, v, ref, want)
			}
		}
	}
}
