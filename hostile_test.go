// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jpx

// Regression suite for hostile input.  Every test builds a malformed or
// extreme stream at run time and decodes it with a bounded budget.  The
// assertions are structural: nothing panics, the memory actually
// allocated stays within a small multiple of the memory charged to the
// budget, and successful results are internally consistent.  Wall-clock
// time is never asserted.
//
// Iteration counts of the randomised tests can be scaled up with the
// environment variable HOSTILE_SCALE (a positive integer, default 1);
// HOSTILE_SEED changes the random seed.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"math/rand/v2"
	"os"
	"runtime"
	"slices"
	"strconv"
	"testing"

	"github.com/google/go-cmp/cmp"

	"seehuhn.de/go/jpx/internal/codestream"
	"seehuhn.de/go/jpx/internal/t1"
	"seehuhn.de/go/jpx/internal/t2"
	"seehuhn.de/go/membudget"
)

// ---- measurement helpers ----

// hostileSlack is the constant allowance, in bytes, for allocations that
// are too small to be charged individually.
const hostileSlack = 1 << 20

// hostileScale returns the iteration multiplier from HOSTILE_SCALE.
func hostileScale() int {
	n, err := strconv.Atoi(os.Getenv("HOSTILE_SCALE"))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// hostileSeed returns the random seed, def unless HOSTILE_SEED is set.
func hostileSeed(def uint64) uint64 {
	if v, err := strconv.ParseUint(os.Getenv("HOSTILE_SEED"), 10, 64); err == nil {
		return v
	}
	return def
}

// hostileLimit returns a budget for an input of the given size: a fixed
// allowance plus a multiple of the input size, ample for valid images
// but small enough to stop hostile ones.
func hostileLimit(n int) int64 {
	return 64<<20 + 256*int64(n)
}

// hostileStats describes one measured operation.
type hostileStats struct {
	charged  int64  // bytes charged to the budget
	alloc    uint64 // bytes allocated according to runtime.MemStats
	panicked any
}

// hostileMeasure runs fn with a fresh budget of the given size and
// reports what it charged and allocated.  A panic in fn is captured.
func hostileMeasure(limit int64, fn func(b *membudget.Budget)) (s hostileStats) {
	b := membudget.New(limit)
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	func() {
		defer func() { s.panicked = recover() }()
		fn(b)
	}()
	runtime.ReadMemStats(&m1)
	s.alloc = m1.TotalAlloc - m0.TotalAlloc
	if avail := b.Available(); avail > 0 {
		s.charged = limit - avail - 32 // per-allocation overhead of the budget
	} else {
		s.charged = limit
	}
	return s
}

// check asserts that the operation did not panic and that the memory
// allocated is at most three times the memory charged, plus a constant.
func (s hostileStats) check(t testing.TB, name string) {
	t.Helper()
	s.checkFactor(t, name, 3)
}

// checkFactor is check with an explicit allowance factor.
func (s hostileStats) checkFactor(t testing.TB, name string, factor float64) {
	t.Helper()
	if s.panicked != nil {
		t.Errorf("%s: panic: %v", name, s.panicked)
		return
	}
	if want := uint64(factor*float64(max(s.charged, 0))) + hostileSlack; s.alloc > want {
		t.Errorf("%s: allocated %d bytes, charged only %d", name, s.alloc, s.charged)
	}
}

// hostileCheckImage asserts that a decoded image is internally consistent.
func hostileCheckImage(t testing.TB, name string, img *Image) {
	t.Helper()
	if img == nil {
		t.Errorf("%s: nil image without error", name)
		return
	}
	if len(img.Planes) != len(img.Components) {
		t.Errorf("%s: %d planes for %d components", name, len(img.Planes), len(img.Components))
	}
	for i, p := range img.Planes {
		if p.Width < 0 || p.Height < 0 || len(p.Pix) != p.Width*p.Height {
			t.Errorf("%s: plane %d is %dx%d with %d samples", name, i, p.Width, p.Height, len(p.Pix))
		}
	}
}

// hostileResult is the outcome of hostileDecode.
type hostileResult struct {
	hostileStats
	img *Image
	err error
}

// hostileDecode decodes data with a budget of limit bytes and asserts the
// common invariants: no panic, bounded allocation, consistent planes.
func hostileDecode(t testing.TB, name string, data []byte, limit int64, opt *Options) hostileResult {
	t.Helper()
	var r hostileResult
	r.hostileStats = hostileMeasure(limit, func(b *membudget.Budget) {
		o := Options{}
		if opt != nil {
			o = *opt
		}
		o.Budget = b
		r.img, r.err = Decode(bytes.NewReader(data), &o)
	})
	r.check(t, name)
	if r.panicked == nil && r.err == nil {
		hostileCheckImage(t, name, r.img)
	}
	return r
}

// hostileExceeded reports whether err is a budget failure.
func hostileExceeded(err error) bool { return errors.Is(err, membudget.ErrExceeded) }

// ---- codestream builder ----

// hostilePart describes one tile-part of a hand-built codestream.
type hostilePart struct {
	tile, part, count int
	cod               *codestream.COD
	poc               []codestream.POCEntry
	ppt               [][]byte
	body              []byte
	extra             func(w *codestream.Writer)
	hasCOC            bool // extra writes a COC/QCC pair
}

// hostileSpec describes a hand-built codestream.
type hostileSpec struct {
	siz   codestream.SIZ
	cod   codestream.COD
	coc   map[int]codestream.CodingStyle // main-header COC markers
	qcd   codestream.Quantization
	poc   []codestream.POCEntry
	ppm   [][]byte
	parts []hostilePart
	eoc   bool
}

// build assembles the codestream.  It returns nil when the marker
// writers refuse the parameters.
func (s *hostileSpec) build() []byte {
	var buf bytes.Buffer
	w := codestream.NewWriter(&buf)
	w.SOC()
	w.SIZ(&s.siz)
	w.COD(&s.cod)
	for c := range len(s.siz.Components) {
		if cs, ok := s.coc[c]; ok {
			w.COC(c, &cs)
		}
	}
	w.QCD(&s.qcd)
	if len(s.poc) > 0 {
		w.POC(s.poc)
	}
	for i, p := range s.ppm {
		w.PPM(i, p)
	}
	if w.Err() != nil {
		return nil
	}
	for _, p := range s.parts {
		off := int64(buf.Len())
		w.SOT(p.tile, p.part, p.count)
		if p.cod != nil {
			w.COD(p.cod)
		}
		if len(p.poc) > 0 {
			w.POC(p.poc)
		}
		if p.extra != nil {
			p.extra(w)
		}
		for i, d := range p.ppt {
			w.PPT(i, d)
		}
		w.SOD()
		buf.Write(p.body)
		b := buf.Bytes()
		if err := codestream.PatchPsot(b, int(off), uint32(len(b))-uint32(off)); err != nil {
			return nil
		}
	}
	if s.eoc {
		w.EOC()
	}
	if w.Err() != nil {
		return nil
	}
	return buf.Bytes()
}

// mustBuild is build, failing the test when the stream cannot be built.
func (s *hostileSpec) mustBuild(t testing.TB) []byte {
	t.Helper()
	data := s.build()
	if data == nil {
		t.Fatal("cannot build test codestream")
	}
	return data
}

// addPart appends a tile-part of tile 0 with the given body and POC.
func (s *hostileSpec) addPart(poc []codestream.POCEntry, body []byte) {
	s.parts = append(s.parts, hostilePart{part: len(s.parts), poc: poc, body: body})
}

// hostileCOD returns coding parameters with the given number of levels
// and the same precinct exponents at every resolution (resolutions above
// zero need exponents of at least one).
func hostileCOD(levels int, prog codestream.Progression, layers, ppx, ppy int) codestream.COD {
	c := codestream.COD{
		Progression: prog, Layers: layers,
		Levels: levels, CodeBlockW: 6, CodeBlockH: 6, Reversible: true,
		PrecinctSizes: make([]codestream.PrecinctSize, levels+1),
	}
	for i := range c.PrecinctSizes {
		c.PrecinctSizes[i] = codestream.PrecinctSize{PPx: ppx, PPy: ppy}
		if i > 0 {
			c.PrecinctSizes[i] = codestream.PrecinctSize{PPx: max(ppx, 1), PPy: max(ppy, 1)}
		}
	}
	return c
}

// hostileQCD returns reversible quantisation parameters for the given
// number of levels.
func hostileQCD(levels int) codestream.Quantization {
	q := codestream.Quantization{Style: codestream.QuantNone, GuardBits: 2}
	for range 3*levels + 1 {
		q.Exponents = append(q.Exponents, 10)
	}
	return q
}

// hostileQCDLossy returns expounded quantisation parameters.
func hostileQCDLossy(levels int) codestream.Quantization {
	q := codestream.Quantization{Style: codestream.QuantExpounded, GuardBits: 2}
	for range 3*levels + 1 {
		q.Exponents = append(q.Exponents, 10)
		q.Mantissas = append(q.Mantissas, 0)
	}
	return q
}

// hostileComps returns n 8-bit components with the sub-sampling given by f.
func hostileComps(n int, f func(i int) (dx, dy int)) []codestream.ComponentSIZ {
	cs := make([]codestream.ComponentSIZ, n)
	for i := range cs {
		dx, dy := f(i)
		cs[i] = codestream.ComponentSIZ{Depth: 8, XRsiz: dx, YRsiz: dy}
	}
	return cs
}

// hostileUnit gives every component the sampling factors (1, 1).
func hostileUnit(int) (int, int) { return 1, 1 }

// hostileSpecFor returns a spec for a single tile covering the image.
func hostileSpecFor(w, h uint32, comps []codestream.ComponentSIZ, cod codestream.COD, qcd codestream.Quantization) *hostileSpec {
	return &hostileSpec{
		siz: codestream.SIZ{Xsiz: w, Ysiz: h, XTsiz: w, YTsiz: h, Components: comps},
		cod: cod, qcd: qcd, eoc: true,
	}
}

// hostileBase returns a spec of ncomp equally sub-sampled components.
func hostileBase(w, h uint32, ncomp, dx, dy, levels int, prog codestream.Progression, pp int) *hostileSpec {
	comps := hostileComps(ncomp, func(int) (int, int) { return dx, dy })
	return hostileSpecFor(w, h, comps, hostileCOD(levels, prog, 1, pp, pp), hostileQCD(levels))
}

// hostileMakeLossy switches a spec to the irreversible path.
func (s *hostileSpec) makeLossy() {
	s.cod.Reversible = false
	s.qcd = hostileQCDLossy(s.cod.Levels)
}

// hostileTiles gives the spec 1x1 tiles and one tile-part with the given
// body for each of the first n tiles.
func (s *hostileSpec) fillTiles(n int, body []byte) {
	s.siz.XTsiz, s.siz.YTsiz = 1, 1
	for i := range n {
		s.parts = append(s.parts, hostilePart{tile: i, body: body})
	}
}

// ---- geometry extremes ----

func TestHostileExtremeSIZ(t *testing.T) {
	m := uint32(math.MaxUint32)
	cases := []struct {
		name    string
		mod     func(s *hostileSpec)
		wantErr bool // the declared planes cannot fit the budget
	}{
		{"max dims one tile", func(s *hostileSpec) {
			s.siz.Xsiz, s.siz.Ysiz, s.siz.XTsiz, s.siz.YTsiz = m, m, m, m
		}, true},
		{"max dims sub255", func(s *hostileSpec) {
			s.siz.Xsiz, s.siz.Ysiz, s.siz.XTsiz, s.siz.YTsiz = m, m, m, m
			s.siz.Components[0].XRsiz, s.siz.Components[0].YRsiz = 255, 255
		}, true},
		{"max dims offsets", func(s *hostileSpec) {
			s.siz.Xsiz, s.siz.Ysiz, s.siz.XTsiz, s.siz.YTsiz = m, m, m, m
			s.siz.XOsiz, s.siz.YOsiz = m-3, m-3
			s.siz.XTOsiz, s.siz.YTOsiz = m-4, m-4
		}, false},
		{"max dims tiny tiles", func(s *hostileSpec) {
			s.siz.Xsiz, s.siz.Ysiz = m, m
			s.siz.XTsiz, s.siz.YTsiz = m/255, m/255
		}, true},
		{"tile bigger than image, offset", func(s *hostileSpec) {
			s.siz.Xsiz, s.siz.Ysiz = 100, 100
			s.siz.XOsiz, s.siz.YOsiz = 50, 50
			s.siz.XTsiz, s.siz.YTsiz = m, m
			s.siz.XTOsiz, s.siz.YTOsiz = 50, 50
		}, false},
		{"tile offset max, tile size max", func(s *hostileSpec) {
			s.siz.Xsiz, s.siz.Ysiz = m, m
			s.siz.XOsiz, s.siz.YOsiz = m-1, m-1
			s.siz.XTOsiz, s.siz.YTOsiz = m-1, m-1
			s.siz.XTsiz, s.siz.YTsiz = m, m
		}, false},
		{"one pixel at max offset, 32 levels, lossy", func(s *hostileSpec) {
			s.siz.Xsiz, s.siz.Ysiz = m, m
			s.siz.XOsiz, s.siz.YOsiz = m-1, m-1
			s.siz.XTOsiz, s.siz.YTOsiz = m-1, m-1
			s.siz.XTsiz, s.siz.YTsiz = 1, 1
			s.cod = hostileCOD(32, codestream.RPCL, 1, 15, 15)
			s.qcd = hostileQCD(32)
		}, false},
		{"one pixel, 32 levels, PCRL, 1x1 precincts", func(s *hostileSpec) {
			s.siz.Xsiz, s.siz.Ysiz, s.siz.XTsiz, s.siz.YTsiz = 1, 1, 1, 1
			s.cod = hostileCOD(32, codestream.PCRL, 1, 1, 1)
			s.cod.PrecinctSizes[0] = codestream.PrecinctSize{}
			s.qcd = hostileQCD(32)
		}, false},
		{"65535 layers on tiny image", func(s *hostileSpec) {
			s.siz.Xsiz, s.siz.Ysiz, s.siz.XTsiz, s.siz.YTsiz = 3, 3, 3, 3
			s.cod.Layers = 65535
		}, false},
		{"16384 components, 32 levels", func(s *hostileSpec) {
			s.siz.Xsiz, s.siz.Ysiz, s.siz.XTsiz, s.siz.YTsiz = 4, 4, 4, 4
			s.siz.Components = hostileComps(16384, hostileUnit)
			s.cod = hostileCOD(32, codestream.LRCP, 1, 15, 15)
			s.qcd = hostileQCD(32)
		}, false},
	}
	for _, c := range cases {
		s := hostileBase(8, 8, 1, 1, 1, 2, codestream.LRCP, 15)
		c.mod(s)
		s.addPart(nil, make([]byte, 64))
		data := s.mustBuild(t)
		r := hostileDecode(t, c.name, data, 4<<20, nil)
		if c.wantErr && r.err == nil {
			t.Errorf("%s: decoded an image larger than the budget", c.name)
		}
		hs := hostileMeasure(1<<20, func(b *membudget.Budget) { ReadHeader(bytes.NewReader(data), b) })
		hs.check(t, c.name+" (header)")
	}
}

// Position-driven progressions build their row/column lists up front.
func TestHostileTallPositionProgression(t *testing.T) {
	for _, prog := range []codestream.Progression{codestream.RPCL, codestream.PCRL, codestream.CPRL, codestream.LRCP} {
		for _, lg := range []int{22, 24, 26} {
			s := hostileBase(1, 1<<lg, 1, 1, 1, 5, prog, 1)
			s.addPart(nil, []byte{0})
			name := fmt.Sprintf("%v h=2^%d", prog, lg)
			r := hostileDecode(t, name, s.mustBuild(t), 32<<20, nil)
			if lg >= 24 && !hostileExceeded(r.err) {
				t.Errorf("%s: plane larger than the budget, err = %v", name, r.err)
			}
		}
	}
}

// With Options.Reduce the output planes shrink but the position-driven
// progressions still enumerate precinct rows at full resolution.
func TestHostileReducePositionProgression(t *testing.T) {
	for _, prog := range []codestream.Progression{codestream.PCRL, codestream.CPRL} {
		for _, lg := range []int{22, 24, 26, 27} {
			s := hostileBase(1, 1<<lg, 1, 1, 1, 20, prog, 1)
			s.addPart(nil, []byte{0})
			name := fmt.Sprintf("%v h=2^%d", prog, lg)
			hostileDecode(t, name, s.mustBuild(t), 4<<20, &Options{Reduce: 20})
		}
	}
}

// The declared image size drives the work even when the input is tiny.
func TestHostileDeclaredSize(t *testing.T) {
	for _, c := range []struct {
		w, h   uint32
		comps  int
		levels int
		lossy  bool
	}{
		{2300, 2300, 1, 5, false}, {2500, 2500, 1, 32, true}, {1400, 1400, 3, 5, true},
		{1, 6_000_000, 1, 5, false}, {6_000_000, 1, 1, 5, true}, {200000, 30, 1, 5, true},
	} {
		s := hostileBase(c.w, c.h, c.comps, 1, 1, c.levels, codestream.LRCP, 15)
		if c.lossy {
			s.makeLossy()
		}
		s.addPart(nil, []byte{0})
		data := s.mustBuild(t)
		hostileDecode(t, fmt.Sprintf("%+v", c), data, hostileLimit(len(data)), nil)
	}
}

// ---- tile-parts and tiles ----

// Many tile-parts for one tile, with and without packed headers.
func TestHostileManyTileParts(t *testing.T) {
	for _, mode := range []string{"inline", "ppt", "ppt-bodyless", "ppm"} {
		s := hostileBase(64, 64, 3, 1, 1, 3, codestream.LRCP, 15)
		s.cod.Layers = 50
		for i := range 20000 {
			p := hostilePart{tile: 0, part: i & 255, body: []byte{0, 0, 0, 0}}
			switch mode {
			case "ppt":
				p.ppt = [][]byte{{0, 0}}
			case "ppt-bodyless":
				p.ppt = [][]byte{{0, 0}}
				p.body = nil
			}
			s.parts = append(s.parts, p)
		}
		if mode == "ppm" {
			for range 200 {
				chunk := make([]byte, 4+50)
				chunk[3] = 50
				s.ppm = append(s.ppm, chunk)
			}
		}
		data := s.mustBuild(t)
		hostileDecode(t, mode, data, hostileLimit(len(data)), nil)
	}
}

// With PPT the tile-part bodies are joined into one stream, which must
// be charged for the memory it occupies.
func TestHostilePPTBodies(t *testing.T) {
	s := hostileBase(64, 64, 1, 1, 1, 0, codestream.LRCP, 15)
	for i := range 16 {
		s.parts = append(s.parts, hostilePart{tile: 0, part: i, ppt: [][]byte{{0}}, body: make([]byte, 1<<20+i)})
	}
	data := s.mustBuild(t)
	r := hostileDecode(t, "ppt", data, hostileLimit(len(data)), nil)
	if r.err != nil {
		t.Fatal(r.err)
	}
	r.checkFactor(t, "ppt", 1.25)
}

// Many tiles, each with its own tile-part.
func TestHostileManyTiles(t *testing.T) {
	for _, levels := range []int{0, 5, 32} {
		s := hostileBase(255, 256, 3, 1, 1, levels, codestream.RPCL, 15)
		s.fillTiles(255*256, make([]byte, 8))
		data := s.mustBuild(t)
		hostileDecode(t, fmt.Sprintf("levels=%d", levels), data, hostileLimit(len(data)), nil)
	}
}

// Every tile validates the whole (up to 65532 entry) QCD.
func TestHostileHugeQCDManyTiles(t *testing.T) {
	for _, comps := range []int{1, 4} {
		s := hostileBase(64, 64, comps, 1, 1, 0, codestream.LRCP, 15)
		s.qcd = codestream.Quantization{Style: codestream.QuantNone, GuardBits: 2}
		for range 65000 {
			s.qcd.Exponents = append(s.qcd.Exponents, 10)
		}
		s.fillTiles(64*64, []byte{0})
		data := s.mustBuild(t)
		hostileDecode(t, fmt.Sprintf("comps=%d", comps), data, hostileLimit(len(data)), nil)
	}
}

// Every tile replays the main-header RGN markers.
func TestHostileManyRGNManyTiles(t *testing.T) {
	for _, nrgn := range []int{1000, 10000} {
		s := hostileBase(64, 64, 1, 1, 1, 0, codestream.LRCP, 15)
		s.fillTiles(64*64, []byte{0})
		data := s.mustBuild(t)
		// splice the markers in before the first SOT; the Writer has no
		// hook for repeated RGN markers
		rgn := []byte{0xFF, 0x5E, 0x00, 0x05, 0x00, 0x00, 0x00}
		i := bytes.Index(data, []byte{0xFF, 0x90, 0x00, 0x0A})
		if i < 0 {
			t.Fatal("no SOT marker")
		}
		data = slices.Concat(data[:i], bytes.Repeat(rgn, nrgn), data[i:])
		hostileDecode(t, fmt.Sprintf("rgn=%d", nrgn), data, hostileLimit(len(data)), nil)
	}
}

// An ROI shift beyond the coefficient range, balanced by as many extra
// zero bit-planes, leaves every coefficient in the background: the image
// must decode as it does without the shift.
func TestHostileROIShift(t *testing.T) {
	const side = 16
	q := codestream.Quantization{Style: codestream.QuantNone, GuardBits: 7, Exponents: []int{24}}
	cod := hostileCOD(0, codestream.LRCP, 1, 15, 15)
	cod.CodeBlockW, cod.CodeBlockH = 2, 2
	decode := func(shift, zero int) *Image {
		s := hostileSpecFor(side, side, hostileComps(1, hostileUnit), cod, q)
		s.addPart(nil, singleLayerPacketZ(side, side, 4, 7, 16, false, zero))
		if shift > 0 {
			s.parts[0].extra = func(w *codestream.Writer) { w.RGN(codestream.RGN{Component: 0, Shift: shift}) }
		}
		data := s.mustBuild(t)
		r := hostileDecode(t, fmt.Sprintf("shift=%d", shift), data, hostileLimit(len(data)), nil)
		if r.err != nil {
			t.Fatalf("shift=%d: %v", shift, r.err)
		}
		return r.img
	}
	want := decode(0, 25)
	if !slices.ContainsFunc(want.Planes[0].Pix, func(v int32) bool { return v != want.Planes[0].Pix[0] }) {
		t.Fatal("reference image is flat")
	}
	for _, shift := range []int{1, 5, 20, 26, 31, 32, 40, 255} {
		// background only: the shift adds as many zero bit-planes; the
		// 5 planes of the block are below 2^shift for shifts from 5
		if shift >= 5 {
			got := decode(shift, 25+shift)
			if d := cmp.Diff(want.Planes, got.Planes); d != "" {
				t.Errorf("shift=%d, background: image differs (-want +got):\n%s", shift, d)
			}
		}
		// region of interest only: the shift adds as many bit-planes at
		// the bottom, beyond what the tier-1 decoder reconstructs for
		// shifts above 25
		got := decode(shift, 25)
		if d := cmp.Diff(want.Planes, got.Planes); d != "" {
			t.Errorf("shift=%d, region of interest: image differs (-want +got):\n%s", shift, d)
		}
	}
}

// A tile that is empty at the decoded resolution skips the per-component
// buffer set-up, but the component transform must not run over buffers
// left behind by the previous tile, which used different transform
// settings.
func TestHostileStaleBuffers(t *testing.T) {
	irrQ := hostileQCDLossy(0)
	revQ := hostileQCD(0)
	for _, name := range []string{"reversible main, irreversible tile 0", "irreversible main, reversible tile 0"} {
		s := hostileBase(2, 1, 3, 2, 1, 0, codestream.LRCP, 15)
		s.siz.XTsiz, s.siz.YTsiz = 1, 1
		s.cod.MCT = true
		tileCOD := hostileCOD(0, codestream.LRCP, 1, 15, 15)
		var extra func(w *codestream.Writer)
		if name[0] == 'r' {
			tileCOD.Reversible = false
			rev := hostileCOD(0, codestream.LRCP, 1, 15, 15)
			extra = func(w *codestream.Writer) {
				w.QCD(&irrQ)
				w.COC(0, &rev.CodingStyle)
				w.QCC(0, &revQ)
			}
		} else {
			s.cod.Reversible = false
			s.qcd = irrQ
			tileCOD.MCT = false
			irr := hostileCOD(0, codestream.LRCP, 1, 15, 15)
			irr.Reversible = false
			extra = func(w *codestream.Writer) {
				w.QCD(&revQ)
				w.COC(0, &irr.CodingStyle)
				w.QCC(0, &irrQ)
			}
		}
		s.parts = []hostilePart{
			{tile: 0, body: []byte{0, 0, 0}, cod: &tileCOD, extra: extra},
			{tile: 1, body: []byte{0, 0, 0}},
		}
		data := s.mustBuild(t)
		for _, workers := range []int{0, 2} {
			hostileDecode(t, fmt.Sprintf("%s workers=%d", name, workers), data, 8<<20, &Options{Workers: workers})
		}
	}
}

// ---- progression iterators and POC ----

// A POC marker with thousands of entries makes the packet iterator walk
// all component/resolution pairs once per entry.
func TestHostilePOCVolumes(t *testing.T) {
	for _, c := range []struct{ comps, levels, entries int }{
		{255, 32, 9000}, {4000, 31, 7000}, {16000, 0, 7000}, {16000, 3, 7000},
	} {
		s := hostileBase(4, 4, c.comps, 1, 1, c.levels, codestream.LRCP, 15)
		entry := codestream.POCEntry{ResEnd: 33, CompEnd: c.comps, LayerEnd: 1, Progression: codestream.LRCP}
		s.poc = slices.Repeat([]codestream.POCEntry{entry}, min(c.entries, 7000))
		s.addPart(nil, make([]byte, c.comps*(c.levels+1)+16))
		data := s.mustBuild(t)
		hostileDecode(t, fmt.Sprintf("%+v", c), data, hostileLimit(len(data)), nil)
	}
}

// The main-header POC list is replayed for every tile.
func TestHostilePOCManyTiles(t *testing.T) {
	s := hostileBase(64, 64, 1, 1, 1, 0, codestream.LRCP, 15)
	entry := codestream.POCEntry{ResEnd: 1, CompEnd: 1, LayerEnd: 1, Progression: codestream.LRCP}
	s.poc = slices.Repeat([]codestream.POCEntry{entry}, 9000)
	s.fillTiles(64*64, []byte{0, 0})
	data := s.mustBuild(t)
	hostileDecode(t, "poc replay", data, hostileLimit(len(data)), nil)
}

// POC entries that emit nothing each cost a scan of all (component,
// resolution) pairs.
func TestHostilePOCEmpty(t *testing.T) {
	const comps, levels = 1000, 32
	empty := codestream.POCEntry{ResEnd: 33, CompEnd: comps, LayerEnd: 1, Progression: codestream.LRCP}
	last := empty
	last.LayerEnd = 2
	for _, n := range []int{10, 1000, 7200} {
		s := hostileSpecFor(1, 1, hostileComps(comps, hostileUnit),
			hostileCOD(levels, codestream.LRCP, 2, 15, 15), hostileQCD(levels))
		s.poc = slices.Concat([]codestream.POCEntry{empty}, slices.Repeat([]codestream.POCEntry{empty}, n), []codestream.POCEntry{last})
		s.addPart(nil, make([]byte, 2*comps*33))
		data := s.mustBuild(t)
		hostileDecode(t, fmt.Sprintf("entries=%d", n), data, hostileLimit(len(data)), nil)
	}
}

// Empty POC entries spread over several tile-parts.  The volumes are
// large, so this only runs with HOSTILE_SCALE > 1.
func TestHostilePOCManyTileParts(t *testing.T) {
	if hostileScale() == 1 {
		t.Skip("set HOSTILE_SCALE=2 to run")
	}
	const comps, levels = 2000, 32
	empty := codestream.POCEntry{ResEnd: 33, CompEnd: comps, LayerEnd: 1, Progression: codestream.LRCP}
	last := empty
	last.LayerEnd = 2
	for _, k := range []int{0, 1, 4} {
		s := hostileSpecFor(1, 1, hostileComps(comps, hostileUnit),
			hostileCOD(levels, codestream.LRCP, 2, 15, 15), hostileQCD(levels))
		s.addPart(slices.Repeat([]codestream.POCEntry{empty}, 7001), make([]byte, comps*33))
		for range k {
			s.addPart(slices.Repeat([]codestream.POCEntry{empty}, 7000), nil)
		}
		s.addPart([]codestream.POCEntry{last}, make([]byte, comps*33))
		data := s.mustBuild(t)
		hostileDecode(t, fmt.Sprintf("extra tile-parts=%d", k), data, hostileLimit(len(data)), nil)
	}
}

// CPRL filters the whole pair list once per component: quadratic in the
// component count.
func TestHostileCPRLManyComps(t *testing.T) {
	for _, c := range []struct{ comps, levels int }{
		{1024, 3}, {4000, 0}, {4000, 31}, {16384, 0}, {16384, 2}, {16384, 3}, {16384, 5},
	} {
		s := hostileSpecFor(1, 1, hostileComps(c.comps, hostileUnit),
			hostileCOD(c.levels, codestream.CPRL, 1, 15, 15), hostileQCD(c.levels))
		s.addPart(nil, make([]byte, c.comps*(c.levels+1)+16))
		data := s.mustBuild(t)
		hostileDecode(t, fmt.Sprintf("%+v", c), data, hostileLimit(len(data)), nil)
	}
}

// Position-driven progression with one fine component and many coarse
// ones: for every position of the fine component the iterator probes
// all active components.
func TestHostilePositionWaste(t *testing.T) {
	const size = 100000
	fineCoarse := func(i int) (int, int) {
		if i == 0 {
			return 1, 1
		}
		return 255, 255
	}
	for _, horizontal := range []bool{true, false} {
		for _, c := range []int{1, 500, 2000} {
			w, h := uint32(size), uint32(1)
			if !horizontal {
				w, h = h, w
			}
			s := hostileSpecFor(w, h, hostileComps(c, fineCoarse),
				hostileCOD(0, codestream.PCRL, 1, 0, 0), hostileQCD(0))
			packets := size + (c-1)*((size+254)/255)
			s.addPart(nil, make([]byte, packets+8))
			data := s.mustBuild(t)
			hostileDecode(t, fmt.Sprintf("horizontal=%v comps=%d", horizontal, c), data, hostileLimit(len(data)), nil)
		}
	}
}

// One component has size precincts in a row, the others have one precinct
// each (through COC markers).  Probing costs size*comps while only
// size+comps packets are read.
func TestHostilePositionWasteCOC(t *testing.T) {
	const size = 100000
	fineCoarse := func(i int) (int, int) {
		if i == 0 {
			return 1, 1
		}
		return 255, 255
	}
	for _, c := range []int{1, 1000, 7000} {
		s := hostileSpecFor(size, 1, hostileComps(c, fineCoarse),
			hostileCOD(0, codestream.PCRL, 1, 0, 0), hostileQCD(0))
		big := s.cod.CodingStyle
		big.PrecinctSizes = []codestream.PrecinctSize{{PPx: 15, PPy: 15}}
		s.coc = map[int]codestream.CodingStyle{}
		for i := 1; i < c; i++ {
			s.coc[i] = big
		}
		s.addPart(nil, make([]byte, size+c+8))
		data := s.mustBuild(t)
		hostileDecode(t, fmt.Sprintf("comps=%d", c), data, hostileLimit(len(data)), nil)
	}
}

// The scaled-up variant of TestHostilePositionWaste.
func TestHostilePositionWasteBig(t *testing.T) {
	if hostileScale() == 1 {
		t.Skip("set HOSTILE_SCALE=2 to run")
	}
	const size, c = 300000, 7000
	s := hostileSpecFor(size, 1, hostileComps(c, func(i int) (int, int) {
		if i == 0 {
			return 1, 1
		}
		return 255, 255
	}), hostileCOD(0, codestream.PCRL, 1, 0, 0), hostileQCD(0))
	s.addPart(nil, make([]byte, size+(c-1)*((size+254)/255)+8))
	data := s.mustBuild(t)
	hostileDecode(t, "big", data, hostileLimit(len(data)), nil)
}

// Every packet must be produced exactly once, whatever the geometry.
func TestHostileIteratorExtremes(t *testing.T) {
	rng := rand.New(rand.NewPCG(hostileSeed(11), 13))
	checked := 0
	for iter := range 3000 * hostileScale() {
		nc := 1 + rng.IntN(3)
		comps := hostileComps(nc, func(int) (int, int) {
			if rng.IntN(2) == 0 {
				return 1 + rng.IntN(255), 1 + rng.IntN(255)
			}
			return 1 + rng.IntN(3), 1 + rng.IntN(3)
		})
		w, h := uint32(1+rng.IntN(400)), uint32(1+rng.IntN(400))
		var x0, y0 uint32
		if rng.IntN(2) == 0 {
			x0, y0 = rng.Uint32N(math.MaxUint32-w), rng.Uint32N(math.MaxUint32-h)
		} else {
			x0, y0 = uint32(rng.IntN(1000)), uint32(rng.IntN(1000))
		}
		siz := codestream.SIZ{Xsiz: x0 + w, Ysiz: y0 + h, XOsiz: x0, YOsiz: y0,
			XTsiz: x0 + w, YTsiz: y0 + h, Components: comps}
		if rng.IntN(2) == 0 {
			// tile grid anchored below the image offset, several tiles
			siz.XTOsiz = x0 - uint32(rng.IntN(int(min(x0, 100))+1))
			siz.YTOsiz = y0 - uint32(rng.IntN(int(min(y0, 100))+1))
			siz.XTsiz = uint32(1 + rng.IntN(int(x0+w-siz.XTOsiz)))
			siz.YTsiz = uint32(1 + rng.IntN(int(y0+h-siz.YTOsiz)))
			if siz.XTOsiz+siz.XTsiz <= x0 {
				siz.XTsiz = x0 - siz.XTOsiz + 1
			}
			if siz.YTOsiz+siz.YTsiz <= y0 {
				siz.YTsiz = y0 - siz.YTOsiz + 1
			}
		}
		levels := rng.IntN(33)
		cod := hostileCOD(levels, codestream.Progression(rng.IntN(5)), 1+rng.IntN(3), 0, 0)
		for r := range cod.PrecinctSizes {
			lo := min(r, 1)
			cod.PrecinctSizes[r] = codestream.PrecinctSize{PPx: lo + rng.IntN(16-lo), PPy: lo + rng.IntN(16-lo)}
		}
		s := hostileSpecFor(0, 0, comps, cod, hostileQCD(levels))
		s.siz = siz
		if rng.IntN(3) == 0 {
			for range 1 + rng.IntN(3) {
				s.poc = append(s.poc, codestream.POCEntry{ResStart: rng.IntN(3), ResEnd: 3 + rng.IntN(31),
					CompEnd: 1 + rng.IntN(nc), LayerEnd: 1 + rng.IntN(4), Progression: codestream.Progression(rng.IntN(5))})
			}
		}
		s.addPart(nil, []byte{0})
		data := s.build()
		if data == nil {
			continue
		}
		cr := codestream.NewReader(data, nil)
		m, err := cr.ReadMainHeader()
		if err != nil {
			continue
		}
		nx, ny, ok := m.SIZ.NumTiles()
		if !ok || nx*ny > 4 {
			continue
		}
		tp, err := cr.NextTilePart()
		if err != nil || tp == nil {
			continue
		}
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("iter %d: panic: %v\nsiz=%+v\ncod=%+v\npoc=%+v", iter, p, siz, cod.CodingStyle, s.poc)
				}
			}()
			tile, err := t2.NewTile(m, []*codestream.TilePart{tp}, tp.Index, nil)
			if err != nil {
				return
			}
			total := 0
			for _, tc := range tile.Comps {
				for _, res := range tc.Res {
					total += res.PW * res.PH
					if total > 300000 {
						return
					}
				}
			}
			seq, err := tile.Packets(nil)
			if err != nil {
				return
			}
			type key struct{ c, r, p, l int }
			seen := map[key]bool{}
			for pk := range seq {
				k := key{pk.Comp.Index, pk.Res.Level, pk.Precinct, pk.Layer}
				if seen[k] {
					t.Errorf("iter %d: duplicate packet %+v (progression %v)", iter, k, cod.Progression)
					return
				}
				seen[k] = true
			}
			checked++
			if len(s.poc) == 0 && len(seen) != tile.NumPackets() {
				t.Errorf("iter %d: %d packets, want %d (progression %v)", iter, len(seen), tile.NumPackets(), cod.Progression)
			}
		}()
	}
	if checked == 0 {
		t.Error("no tile was checked")
	}
}

// ---- packet headers, precincts and tier-1 ----

// Precincts whose bands are empty are allocated without being charged.
func TestHostileEmptyBands(t *testing.T) {
	const n = 400000
	for _, levels := range []int{1, 8} {
		s := hostileSpecFor(1, n, hostileComps(1, hostileUnit),
			hostileCOD(levels, codestream.RLCP, 1, 1, 1), hostileQCD(levels))
		s.cod.PrecinctSizes[0] = codestream.PrecinctSize{PPx: 1, PPy: 1}
		total := 0
		for r := 0; r <= levels; r++ {
			total += (n>>(levels-r) + 3) / 2
		}
		s.addPart(nil, make([]byte, total+8))
		data := s.mustBuild(t)
		hostileDecode(t, fmt.Sprintf("levels=%d", levels), data, hostileLimit(len(data)), nil)
	}
}

// Every input byte creates a fresh tiny precinct.
func TestHostilePrecinctAlloc(t *testing.T) {
	for _, c := range []struct {
		name   string
		levels int
		pp     int
	}{{"1x1 precincts at r=0 only", 0, 0}, {"2x2 precincts, 3 levels", 3, 1}} {
		s := hostileBase(1024, 1024, 1, 1, 1, c.levels, codestream.LRCP, c.pp)
		s.cod.PrecinctSizes[0] = codestream.PrecinctSize{}
		s.addPart(nil, make([]byte, 1<<20))
		hostileDecode(t, c.name, s.mustBuild(t), 1<<30, nil)
	}
}

// singleLayerPacket builds the one packet of an image that fits a single
// precinct and a single LL band (zero levels): every code-block is
// included with zero zero-bit-planes, the given number of passes and
// dlen bytes of pseudo-random data.  With term set, each pass is a
// segment of its own and carries the length dlen/passes.
func singleLayerPacket(w, h, cb, passes, dlen int, term bool) []byte {
	return singleLayerPacketZ(w, h, cb, passes, dlen, term, 0)
}

// singleLayerPacketZ is singleLayerPacket with zeroPlanes zero bit-planes
// in every code-block.
func singleLayerPacketZ(w, h, cb, passes, dlen int, term bool, zeroPlanes int) []byte {
	cbw, cbh := (w+cb-1)/cb, (h+cb-1)/cb
	incl, zero := t2.NewTree(cbw, cbh), t2.NewTree(cbw, cbh)
	for j := range cbh {
		for i := range cbw {
			zero.SetValue(i, j, zeroPlanes)
		}
	}
	var bw t2.BitWriter
	bw.WriteBit(1)
	var body []byte
	seed := uint32(12345)
	random := func(n int) {
		for range n {
			seed = seed*1664525 + 1013904223
			body = append(body, byte(seed>>24))
		}
	}
	for j := range cbh {
		for i := range cbw {
			incl.Encode(&bw, i, j, 1)
			zero.EncodeValue(&bw, i, j)
			passCountCode(&bw, passes)
			bw.WriteBit(0) // no Lblock increment
			if term {
				for range passes {
					bw.WriteBits(uint32(dlen/passes), 3)
				}
				random(dlen / passes * passes)
				continue
			}
			bw.WriteBits(uint32(dlen), 3+bits.Len(uint(passes))-1)
			random(dlen)
		}
	}
	return append(bw.Flush(), body...)
}

// Tier-1 work for a tiny input: every block claims 164 passes.
func TestHostileTier1Work(t *testing.T) {
	for _, side := range []int{512, 1024} {
		q := codestream.Quantization{Style: codestream.QuantNone, GuardBits: 7, Exponents: []int{24}}
		s := hostileSpecFor(uint32(side), uint32(side), hostileComps(1, hostileUnit),
			hostileCOD(0, codestream.LRCP, 1, 15, 15), q)
		s.addPart(nil, singleLayerPacket(side, side, 64, 164, 64, false))
		data := s.mustBuild(t)
		hostileDecode(t, fmt.Sprintf("side=%d", side), data, hostileLimit(len(data)), nil)
	}
}

// Random segments, styles and shapes straight into tier-1.
func TestHostileTier1Random(t *testing.T) {
	rng := rand.New(rand.NewPCG(hostileSeed(1), 2))
	dec := t1.NewDecoder()
	shapes := [][2]int{{1, 1}, {1, 4}, {4, 1}, {3, 5}, {64, 64}, {1024, 4}, {4, 1024}, {2, 2048}, {1, 4096}, {4096, 1}, {7, 9}}
	for iter := range 4000 * hostileScale() {
		sh := shapes[rng.IntN(len(shapes))]
		w, h := sh[0], sh[1]
		style := t1.Style{
			Bypass: rng.IntN(2) == 0, Reset: rng.IntN(2) == 0, Terminate: rng.IntN(2) == 0,
			Causal: rng.IntN(2) == 0, Predictable: rng.IntN(2) == 0, SegSymbols: rng.IntN(2) == 0,
		}
		planes := rng.IntN(34) - 2
		var segs []t1.Segment
		for range rng.IntN(6) {
			d := make([]byte, rng.IntN(40))
			for i := range d {
				d[i] = byte(rng.Uint32())
				if rng.IntN(4) == 0 {
					d[i] = 0xFF
				}
			}
			segs = append(segs, t1.Segment{Data: d, Passes: rng.IntN(200)})
		}
		out := make([]int32, w*h)
		band := t1.Band(rng.IntN(4))
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("iter %d: panic: %v (%dx%d planes=%d style=%+v segments=%d)", iter, p, w, h, planes, style, len(segs))
				}
			}()
			dec.Decode(out, w, h, band, style, planes, segs)
		}()
	}
}

// Random packet data under many coding styles and progressions.
func TestHostilePacketRandom(t *testing.T) {
	rng := rand.New(rand.NewPCG(hostileSeed(7), 8))
	okCount := 0
	for iter := range 1500 * hostileScale() {
		nc := 1 + rng.IntN(4)
		w, h := uint32(1+rng.IntN(150)), uint32(1+rng.IntN(150))
		comps := hostileComps(nc, func(int) (int, int) { return 1 + rng.IntN(3), 1 + rng.IntN(3) })
		levels := rng.IntN(5)
		cod := hostileCOD(levels, codestream.Progression(rng.IntN(5)), 1+rng.IntN(5), 1+rng.IntN(6), 1+rng.IntN(6))
		cod.CodeBlockW, cod.CodeBlockH = 2+rng.IntN(5), 2+rng.IntN(5)
		cod.EntropyBypass = rng.IntN(2) == 0
		cod.Terminate = rng.IntN(2) == 0
		cod.ResetContexts = rng.IntN(2) == 0
		cod.VerticallyCausal = rng.IntN(2) == 0
		cod.PredictableTermination = rng.IntN(2) == 0
		cod.SegmentationSymbols = rng.IntN(2) == 0
		cod.SOP, cod.EPH = rng.IntN(2) == 0, rng.IntN(2) == 0
		cod.MCT = rng.IntN(2) == 0
		q := codestream.Quantization{Style: codestream.QuantNone, GuardBits: rng.IntN(8)}
		for range 3*levels + 1 {
			q.Exponents = append(q.Exponents, 1+rng.IntN(31))
		}
		s := hostileSpecFor(w, h, comps, cod, q)
		if rng.IntN(3) == 0 {
			s.siz.XTsiz, s.siz.YTsiz = uint32(1+rng.IntN(int(w))), uint32(1+rng.IntN(int(h)))
		}
		if rng.IntN(2) == 0 {
			// image and tile offsets
			z := &s.siz
			z.XOsiz, z.YOsiz = uint32(rng.IntN(int(w))), uint32(rng.IntN(int(h)))
			z.XTOsiz = uint32(rng.IntN(int(z.XOsiz) + 1))
			z.YTOsiz = uint32(rng.IntN(int(z.YOsiz) + 1))
			z.XTsiz = max(z.XTsiz, z.XOsiz-z.XTOsiz+1)
			z.YTsiz = max(z.YTsiz, z.YOsiz-z.YTOsiz+1)
		}
		body := make([]byte, rng.IntN(3000))
		for i := range body {
			switch rng.IntN(5) {
			case 0:
				body[i] = 0xFF
			case 1:
				body[i] = 0
			default:
				body[i] = byte(rng.Uint32())
			}
		}
		if rng.IntN(3) == 0 {
			for range 1 + rng.IntN(4) {
				s.poc = append(s.poc, codestream.POCEntry{
					ResStart: rng.IntN(3), ResEnd: 3 + rng.IntN(30), CompEnd: 1 + rng.IntN(nc),
					LayerEnd: 1 + rng.IntN(6), Progression: codestream.Progression(rng.IntN(5)),
				})
			}
		}
		s.addPart(nil, body)
		data := s.build()
		if data == nil {
			continue
		}
		reduce := 0
		if rng.IntN(3) == 0 {
			reduce = rng.IntN(levels + 1)
		}
		r := hostileDecode(t, fmt.Sprintf("iter %d", iter), data, 8<<20, &Options{Reduce: reduce, Workers: rng.IntN(4)})
		if r.err == nil {
			okCount++
		}
	}
	if okCount == 0 {
		t.Error("no stream decoded successfully")
	}
}

// Zero-bit-plane values near the upper bound make the tag-tree decoder
// restart its walk for every threshold.
func TestHostileZeroPlaneWalk(t *testing.T) {
	for _, zp := range []int{0, 500} {
		const side, cb = 2048, 4
		cbw := side / cb
		incl, zero := t2.NewTree(cbw, cbw), t2.NewTree(cbw, cbw)
		for j := range cbw {
			for i := range cbw {
				zero.SetValue(i, j, zp)
			}
		}
		var bw t2.BitWriter
		bw.WriteBit(1)
		for j := range cbw {
			for i := range cbw {
				incl.Encode(&bw, i, j, 1)
				zero.EncodeValue(&bw, i, j)
				bw.WriteBit(0) // one pass
				bw.WriteBit(0) // no Lblock increment
				bw.WriteBits(0, 3)
			}
		}
		cod := hostileCOD(0, codestream.LRCP, 1, 15, 15)
		cod.CodeBlockW, cod.CodeBlockH = 2, 2
		q := codestream.Quantization{Style: codestream.QuantNone, GuardBits: 2, Exponents: []int{10}}
		s := hostileSpecFor(side, side, hostileComps(1, hostileUnit), cod, q)
		s.addPart(nil, bw.Flush())
		data := s.mustBuild(t)
		hostileDecode(t, fmt.Sprintf("zero planes=%d", zp), data, hostileLimit(len(data)), nil)
	}
}

// The per-packet piece list grows by 32 bytes per three header bits when
// every block has many terminated passes.
func TestHostilePieceList(t *testing.T) {
	const side = 512
	cod := hostileCOD(0, codestream.LRCP, 1, 15, 15)
	cod.CodeBlockW, cod.CodeBlockH = 2, 2
	cod.Terminate = true
	q := codestream.Quantization{Style: codestream.QuantNone, GuardBits: 7, Exponents: []int{24}}
	s := hostileSpecFor(side, side, hostileComps(1, hostileUnit), cod, q)
	s.addPart(nil, singleLayerPacket(side, side, 4, 164, 0, true))
	data := s.mustBuild(t)
	for _, limit := range []int64{16 << 20, 32 << 20} {
		hostileDecode(t, fmt.Sprintf("limit=%dMiB", limit>>20), data, limit, nil)
	}
}

// Every packet of a precinct walks the inclusion tag tree of all its
// code-blocks, even when the packet header carries almost no bits.
func TestHostileInclusionWalk(t *testing.T) {
	for _, c := range []struct{ side, layers int }{{512, 1000}, {512, 8000}, {1024, 8000}, {2048, 2000}} {
		cod := hostileCOD(0, codestream.LRCP, c.layers, 15, 15)
		cod.CodeBlockW, cod.CodeBlockH = 2, 2
		q := codestream.Quantization{Style: codestream.QuantNone, GuardBits: 2, Exponents: []int{10}}
		s := hostileSpecFor(uint32(c.side), uint32(c.side), hostileComps(1, hostileUnit), cod, q)
		s.addPart(nil, bytes.Repeat([]byte{0x80}, c.layers))
		data := s.mustBuild(t)
		hostileDecode(t, fmt.Sprintf("%+v", c), data, hostileLimit(len(data)), nil)
	}
}

// One code-block, one terminated pass per layer, 65535 layers.
func TestHostileSegmentCharge(t *testing.T) {
	const layers = 65535
	cod := hostileCOD(0, codestream.LRCP, layers, 15, 15)
	cod.CodeBlockW, cod.CodeBlockH = 2, 2
	cod.Terminate = true
	q := codestream.Quantization{Style: codestream.QuantNone, GuardBits: 7, Exponents: []int{24}}
	s := hostileSpecFor(4, 4, hostileComps(1, hostileUnit), cod, q)
	body := []byte{0xE1, 0x55}
	for range layers - 1 {
		body = append(body, 0xC2, 0x55)
	}
	s.addPart(nil, body)
	data := s.mustBuild(t)
	hostileDecode(t, "segments", data, hostileLimit(len(data)), nil)
}

// Lblock pushed towards 31 and a segment length of up to 2^31-1 bytes.
func TestHostileHugeLength(t *testing.T) {
	cod := hostileCOD(0, codestream.LRCP, 1, 15, 15)
	cod.CodeBlockW, cod.CodeBlockH = 2, 2
	q := codestream.Quantization{Style: codestream.QuantNone, GuardBits: 7, Exponents: []int{24}}
	for _, inc := range []int{0, 28, 29} {
		var bw t2.BitWriter
		bw.WriteBit(1)
		bw.WriteBit(1) // included
		bw.WriteBit(1) // zero planes 0
		bw.WriteBits(0x1FF, 9)
		bw.WriteBits(127, 7) // 164 passes
		for range inc {
			bw.WriteBit(1)
		}
		bw.WriteBit(0)
		nb := min(3+inc+7, 31)
		bw.WriteBits(0xFFFFFFFF>>(32-nb), nb)
		s := hostileSpecFor(4, 4, hostileComps(1, hostileUnit), cod, q)
		s.addPart(nil, append(bw.Flush(), 1, 2, 3, 4))
		hostileDecode(t, fmt.Sprintf("lblock increment %d", inc), s.mustBuild(t), 4<<20, nil)
	}
}

// ---- JP2 boxes ----

// boxOf returns a box with the given type and payload.
func boxOf(typ string, payload ...[]byte) []byte {
	p := bytes.Join(payload, nil)
	b := binary.BigEndian.AppendUint32(nil, uint32(len(p)+8))
	b = append(b, typ...)
	return append(b, p...)
}

// boxXL returns a box with a 64-bit length field.
func boxXL(typ string, l uint64, payload []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, 1)
	b = append(b, typ...)
	b = binary.BigEndian.AppendUint64(b, l)
	return append(b, payload...)
}

// boxRaw returns a box whose length field is l whatever the payload.
func boxRaw(typ string, l uint32, payload []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, l)
	b = append(b, typ...)
	return append(b, payload...)
}

func jp2Sig() []byte { return []byte{0, 0, 0, 12, 'j', 'P', ' ', ' ', 0x0D, 0x0A, 0x87, 0x0A} }

func jp2Ftyp() []byte { return boxOf("ftyp", []byte("jp2 "), []byte{0, 0, 0, 0}, []byte("jp2 ")) }

// jp2Codestream returns a small bare codestream (gray, 16x16).
func jp2Codestream(t testing.TB) []byte {
	t.Helper()
	img := testImage(16, 16, gray(8), 1)
	var buf bytes.Buffer
	if err := Encode(&buf, img, &EncodeParams{}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// jp2File returns a JP2 file whose header box holds the given boxes
// (no header box when there are none).
func jp2File(t testing.TB, hdrBoxes ...[]byte) []byte {
	t.Helper()
	f := slices.Concat(jp2Sig(), jp2Ftyp())
	if len(hdrBoxes) > 0 {
		f = append(f, boxOf("jp2h", hdrBoxes...)...)
	}
	return append(f, boxOf("jp2c", jp2Codestream(t))...)
}

// jp2Top returns a file made of the signature, the given top-level boxes
// and a codestream box.
func jp2Top(t testing.TB, boxes ...[]byte) []byte {
	t.Helper()
	f := slices.Concat(append([][]byte{jp2Sig()}, boxes...)...)
	return append(f, boxOf("jp2c", jp2Codestream(t))...)
}

// noSizeReader hides the Size method of a bytes.Reader.
type noSizeReader struct{ r *bytes.Reader }

func (s noSizeReader) Read(p []byte) (int, error)              { return s.r.Read(p) }
func (s noSizeReader) ReadAt(p []byte, off int64) (int, error) { return s.r.ReadAt(p, off) }

// jp2Header returns the part of a codestream before the first tile-part.
func jp2Header(cs []byte) []byte { return cs[:bytes.Index(cs, []byte{0xFF, 0x90})] }

// fragFile returns a file whose codestream is split over nf fragments of
// a fragment table: the first fragment covers the whole codestream header
// plus region (which is hidden in a free box), every other fragment
// repeats region.  The parser must run through all fragments without
// finding a tile-part.
func fragFile(t testing.TB, nf int, region []byte) []byte {
	t.Helper()
	hdr := append(slices.Clone(jp2Header(jp2Codestream(t))), region...)
	f := slices.Concat(jp2Sig(), jp2Ftyp())
	csOff := len(f) + 8 + 8 + 2 + 14*nf + 8
	flst := binary.BigEndian.AppendUint16(nil, uint16(nf))
	add := func(off, ln int) {
		flst = binary.BigEndian.AppendUint64(flst, uint64(off))
		flst = binary.BigEndian.AppendUint32(flst, uint32(ln))
		flst = binary.BigEndian.AppendUint16(flst, 0)
	}
	add(csOff, len(hdr))
	for range nf - 1 {
		add(csOff+len(hdr)-len(region), len(region))
	}
	f = append(f, boxOf("ftbl", boxOf("flst", flst))...)
	f = append(f, boxOf("free", hdr)...)
	if len(f)-len(hdr) != csOff {
		t.Fatalf("fragment offset mismatch %d != %d", len(f)-len(hdr), csOff)
	}
	return f
}

// Header-box contents whose size is dominated by a repeated unit must be
// charged in proportion to the memory the parser allocates.
func TestHostileJP2Accounting(t *testing.T) {
	const n = 1 << 20
	rep := bytes.Repeat
	cmapEntries := rep([]byte{0, 0, 0, 0}, n/4)
	pclr := []byte{0, 1, 1, 7, 5}
	cdef := append([]byte{0xFF, 0xFF}, rep([]byte{0, 0, 0, 0, 0, 1}, 65535)...)
	cases := []struct {
		name string
		file []byte
	}{
		{"baseline", jp2File(t, boxOf("ihdr", make([]byte, 14)))},
		{"ftyp 1MiB brands", jp2Top(t, boxOf("ftyp", []byte("jp2 "), []byte{0, 0, 0, 0}, rep([]byte("abcd"), n/4)))},
		{"jp2h 1MiB of free boxes", jp2File(t, rep(boxOf("free"), n/8))},
		{"jp2h 1MiB of short colr boxes", jp2File(t, rep(boxOf("colr", []byte{4, 0, 0}), n/11))},
		{"jp2h 1MiB of enumerated colr boxes", jp2File(t, rep(boxOf("colr", []byte{1, 0, 0, 0, 0, 0, 16}), n/15))},
		{"bpcc 1MiB", jp2File(t, boxOf("bpcc", rep([]byte{7}, n)))},
		{"cmap 1MiB without pclr", jp2File(t, boxOf("cmap", cmapEntries))},
		{"cmap 1MiB with pclr", jp2File(t, boxOf("pclr", pclr), boxOf("cmap", cmapEntries))},
		{"cdef 65535", jp2File(t, boxOf("cdef", cdef))},
		{"pclr 1024x255", jp2File(t, boxOf("pclr", []byte{4, 0, 255}, rep([]byte{7}, 255), make([]byte, 1024*255)))},
		{"pclr 1024x1", jp2File(t, boxOf("pclr", []byte{4, 0, 1, 7}, make([]byte, 1024)))},
		{"colr ICC 4MiB", jp2File(t, boxOf("colr", []byte{2, 0, 0}, make([]byte, 4<<20)))},
		{"rreq 1MiB", jp2File(t, boxOf("rreq", make([]byte, n)))},
		{"1000 rreq boxes at top level", jp2Top(t, rep(boxOf("rreq", make([]byte, 1024)), 1000))},
		{"1000 ftyp boxes at top level", jp2Top(t, rep(boxOf("ftyp", []byte("jp2 "), []byte{0, 0, 0, 0}, make([]byte, 1024)), 1000))},
	}
	for _, c := range cases {
		// the entry lists are allocated at their final size
		factor := 3.0
		switch c.name {
		case "bpcc 1MiB", "cmap 1MiB without pclr", "cdef 65535":
			factor = 1.25
		}
		hs := hostileMeasure(1<<40, func(b *membudget.Budget) { ReadHeader(bytes.NewReader(c.file), b) })
		hs.checkFactor(t, "ReadHeader "+c.name, factor)
		hs = hostileMeasure(1<<40, func(b *membudget.Budget) { Decode(bytes.NewReader(c.file), &Options{Budget: b}) })
		hs.checkFactor(t, "Decode "+c.name, factor)
	}
}

// A palette-mapped channel list is allocated by the header builder; its
// size must be reflected in the charge.
func TestHostileJP2ChannelList(t *testing.T) {
	file := jp2File(t, boxOf("pclr", []byte{0, 1, 1, 7, 5}),
		boxOf("cmap", bytes.Repeat([]byte{0, 0, 0, 0}, 250000)))
	var h *Header
	hs := hostileMeasure(1<<40, func(b *membudget.Budget) { h, _ = ReadHeader(bytes.NewReader(file), b) })
	hs.check(t, "ReadHeader")
	if h == nil || len(h.Channels) == 0 {
		t.Fatal("header was not read")
	}
	// a budget that covers what was charged, plus a little, must suffice
	hs = hostileMeasure(hs.charged+1024, func(b *membudget.Budget) {
		if _, err := ReadHeader(bytes.NewReader(file), b); err != nil {
			t.Errorf("budget of charged size failed: %v", err)
		}
	})
	hs.check(t, "ReadHeader with exact budget")
}

// Header data retained after the call must also be charged.
func TestHostileJP2Retained(t *testing.T) {
	const n = 1 << 20
	pclr := boxOf("pclr", []byte{0, 1, 1, 7, 5})
	cases := []struct {
		name string
		file []byte
	}{
		{"colr", jp2File(t, bytes.Repeat(boxOf("colr", []byte{4, 0, 0}), n/11))},
		{"colr enumerated", jp2File(t, bytes.Repeat(boxOf("colr", []byte{1, 0, 0, 0, 0, 0, 16}), n/15))},
		{"bpcc", jp2File(t, boxOf("bpcc", bytes.Repeat([]byte{7}, n)))},
		{"cmap and pclr", jp2File(t, pclr, boxOf("cmap", bytes.Repeat([]byte{0, 0, 0, 0}, n/4)))},
		{"ftyp brands", jp2Top(t, boxOf("ftyp", []byte("jp2 "), []byte{0, 0, 0, 0}, bytes.Repeat([]byte("abcd"), n/4)))},
		{"cdef", jp2File(t, boxOf("cdef", append([]byte{0xFF, 0xFF}, bytes.Repeat([]byte{0, 0, 0, 0, 0, 1}, 65535)...)))},
	}
	for _, c := range cases {
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		b := membudget.New(1 << 40)
		h, _ := ReadHeader(bytes.NewReader(c.file), b)
		runtime.GC()
		runtime.ReadMemStats(&m1)
		charged := int64(1<<40) - b.Available() - 32
		retained := int64(m1.HeapAlloc) - int64(m0.HeapAlloc)
		runtime.KeepAlive(h)
		if retained > 3*charged+hostileSlack {
			t.Errorf("%s: retained %d bytes, charged %d", c.name, retained, charged)
		}
	}
}

// ApplyPalette allocates one plane per mapping entry, while every entry
// costs only four bytes of input.
func TestHostileApplyPaletteAmplification(t *testing.T) {
	const n = 1024
	pclr := boxOf("pclr", []byte{1, 0, 1, 7}, make([]byte, 256))
	cmap := boxOf("cmap", bytes.Repeat([]byte{0, 0, 1, 0}, n))
	img := testImage(256, 256, gray(8), 1)
	var buf bytes.Buffer
	if err := Encode(&buf, img, &EncodeParams{}); err != nil {
		t.Fatal(err)
	}
	f := slices.Concat(jp2Sig(), jp2Ftyp(), boxOf("jp2h", pclr, cmap), boxOf("jp2c", buf.Bytes()))
	dec, err := Decode(bytes.NewReader(f), &Options{Budget: membudget.New(hostileLimit(len(f)))})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Palette == nil || len(dec.Channels) != n {
		t.Fatalf("got palette %v and %d channels", dec.Palette != nil, len(dec.Channels))
	}
	hs := hostileMeasure(hostileLimit(len(f)), func(b *membudget.Budget) { _, err = ApplyPalette(dec, b) })
	hs.check(t, "ApplyPalette with decode-sized budget")
	if !hostileExceeded(err) {
		t.Errorf("decode-sized budget: err = %v, want budget failure", err)
	}
}

func TestHostileSYCC(t *testing.T) {
	for _, depth := range []int{1, 8, 16, 31, 0, 40, 63, 64, 100, -1} {
		img := &Image{}
		img.Components = []Component{{Depth: depth}, {Depth: depth}, {Depth: depth}}
		for range 3 {
			img.Planes = append(img.Planes, Plane{Width: 2, Height: 1, Pix: []int32{0, math.MaxInt32}})
		}
		hs := hostileMeasure(1<<30, func(*membudget.Budget) { SYCCToRGB(img) })
		hs.check(t, fmt.Sprintf("depth %d", depth))
		for i, p := range img.Planes {
			if len(p.Pix) != 2 {
				t.Errorf("depth %d: plane %d has %d samples", depth, i, len(p.Pix))
			}
		}
	}
	// extreme sample values and planes sharing one slice
	p := Plane{Width: 2, Height: 1, Pix: []int32{math.MinInt32, 5}}
	img := &Image{}
	img.Components = []Component{{Depth: 8}, {Depth: 8}, {Depth: 8}}
	img.Planes = []Plane{p, p, p}
	hs := hostileMeasure(1<<30, func(*membudget.Budget) { SYCCToRGB(img) })
	hs.check(t, "aliased planes")
}

// Valid images of many shapes must decode with a tight memory account.
func TestHostileAccountingSweep(t *testing.T) {
	many := make([]Component, 64)
	for i := range many {
		many[i] = Component{Depth: 8, DX: 1, DY: 1}
	}
	sub := []Component{{Depth: 8, DX: 1, DY: 1}, {Depth: 8, DX: 2, DY: 2}, {Depth: 8, DX: 2, DY: 2}}
	cases := []struct {
		name  string
		w, h  int
		comps []Component
		p     EncodeParams
	}{
		{"gray lossless", 512, 512, gray(8), EncodeParams{}},
		{"gray lossy", 512, 512, gray(8), EncodeParams{Lossy: true}},
		{"rgb lossy", 256, 256, rgb(8), EncodeParams{Lossy: true}},
		{"rgb tiles 32", 256, 256, rgb(8), EncodeParams{TileWidth: 32, TileHeight: 32}},
		{"rgb tiles 16, small blocks", 256, 256, rgb(8), EncodeParams{TileWidth: 16, TileHeight: 16, CodeBlockWidth: 4, CodeBlockHeight: 4}},
		{"rgb small blocks, 8 layers", 256, 256, rgb(8), EncodeParams{CodeBlockWidth: 4, CodeBlockHeight: 4, Layers: 8}},
		{"rgb precincts 16", 256, 256, rgb(8), EncodeParams{Precincts: []PrecinctSize{{4, 4}}, CodeBlockWidth: 4, CodeBlockHeight: 4}},
		{"64 components", 64, 64, many, EncodeParams{}},
		{"sub-sampled", 256, 256, sub, EncodeParams{}},
		{"no levels", 64, 64, gray(8), EncodeParams{Levels: NoLevels}},
		{"tile-parts", 128, 128, rgb(8), EncodeParams{TileParts: 8, TileWidth: 32, TileHeight: 32, Progression: RPCL}},
		{"packed headers", 128, 128, rgb(8), EncodeParams{PackedHeaders: PackedInMain, SOP: true, EPH: true}},
	}
	for _, c := range cases {
		img := testImage(c.w, c.h, c.comps, 7)
		var buf bytes.Buffer
		if err := Encode(&buf, img, &c.p); err != nil {
			t.Fatalf("%s: encode: %v", c.name, err)
		}
		for _, workers := range []int{1, 4} {
			name := fmt.Sprintf("%s workers=%d", c.name, workers)
			r := hostileDecode(t, name, buf.Bytes(), 1<<40, &Options{Workers: workers})
			if r.err != nil {
				t.Errorf("%s: %v", name, r.err)
			}
		}
	}
}

// Single-byte changes in the header region of valid streams must never
// make the decoder allocate much more than it charges.
func TestHostileMutationAccounting(t *testing.T) {
	img := testImage(48, 40, rgb(8), 3)
	var seeds [][]byte
	for _, p := range []EncodeParams{
		{TileWidth: 16, TileHeight: 16, Layers: 2, Precincts: []PrecinctSize{{2, 2}, {3, 3}}, CodeBlockWidth: 8, CodeBlockHeight: 8, Progression: RPCL},
		{Lossy: true, SOP: true, EPH: true, Bypass: true, Terminate: true},
		{PackedHeaders: PackedInMain, TileParts: 2, TileWidth: 20, TileHeight: 20},
	} {
		var buf bytes.Buffer
		if err := Encode(&buf, img, &p); err != nil {
			t.Fatal(err)
		}
		seeds = append(seeds, buf.Bytes())
	}
	for si, seed := range seeds {
		for pos := range min(120*hostileScale(), len(seed)) {
			for _, v := range []byte{0, 0x80, 0xff} {
				if seed[pos] == v {
					continue
				}
				mut := slices.Clone(seed)
				mut[pos] = v
				hostileDecode(t, fmt.Sprintf("seed %d pos %d value %d", si, pos, v), mut, 8<<20, nil)
			}
		}
	}
}

// Overlapping fragments must not multiply the work or the allocation.
func TestHostileJP2Fragments(t *testing.T) {
	// 15 COM segments of 64 KiB each
	com := append([]byte{0xFF, 0x64, 0xFF, 0xFF}, make([]byte, 0xFFFF-2)...)
	comRegion := bytes.Repeat(com, 15)
	for _, nf := range []int{50, 200, 400} {
		f := fragFile(t, nf, comRegion)
		for _, withBudget := range []bool{false, true} {
			var limit int64
			var budget *membudget.Budget
			if withBudget {
				limit = 8 << 20
			} else {
				limit = 4 * hostileLimit(len(f))
			}
			hs := hostileMeasure(limit, func(b *membudget.Budget) {
				if withBudget {
					budget = b
				}
				ReadHeader(noSizeReader{bytes.NewReader(f)}, budget)
			})
			if !withBudget {
				// without a budget the allowance is internal: bound the
				// absolute allocation by it
				if hs.panicked != nil {
					t.Errorf("nf=%d: panic: %v", nf, hs.panicked)
				}
				if hs.alloc > uint64(limit) {
					t.Errorf("nf=%d: allocated %d bytes without budget", nf, hs.alloc)
				}
				continue
			}
			hs.check(t, fmt.Sprintf("nf=%d with budget", nf))
		}
	}
	// the largest table the format allows
	f := fragFile(t, 65535, comRegion)
	var err error
	hs := hostileMeasure(8<<20, func(b *membudget.Budget) {
		_, err = ReadHeader(noSizeReader{bytes.NewReader(f)}, b)
	})
	hs.check(t, "nf=65535")
	if err == nil {
		t.Error("nf=65535: header without tile-part read without error")
	}
}

// Overlapping fragments made of marker segments that carry no payload
// cost CPU time only.
func TestHostileJP2FragmentMarkers(t *testing.T) {
	for name, unit := range map[string][]byte{
		"reserved marker without length": {0xFF, 0x30},
		"PRF, empty":                     {0xFF, 0x56, 0x00, 0x02},
		"COM, empty":                     {0xFF, 0x64, 0x00, 0x02},
		"TLM, empty":                     {0xFF, 0x55, 0x00, 0x02},
	} {
		region := bytes.Repeat(unit, (256<<10)/len(unit))
		f := fragFile(t, 20, region)
		hs := hostileMeasure(8<<20, func(b *membudget.Budget) { ReadHeader(noSizeReader{bytes.NewReader(f)}, b) })
		hs.check(t, name)
	}
}

// Malformed box structures must be rejected without panicking.
func TestHostileJP2Boxes(t *testing.T) {
	cs := jp2Codestream(t)
	ftyp := jp2Ftyp()
	deep := []byte{}
	for range 3000 {
		deep = boxOf("jp2h", deep)
		if len(deep) > 1<<20 {
			break
		}
	}
	pre := func(boxes ...[]byte) []byte { return slices.Concat(append([][]byte{jp2Sig(), ftyp}, boxes...)...) }
	cases := map[string][]byte{
		"jp2c XL 2^63-1":            pre(boxXL("jp2c", 1<<63-1, cs)),
		"jp2c XL 2^64-1":            pre(boxXL("jp2c", 1<<64-1, cs)),
		"jp2c XL 16":                pre(boxXL("jp2c", 16, cs)),
		"jp2c XL 15":                pre(boxXL("jp2c", 15, cs)),
		"jp2c len 0 (to EOF)":       pre(boxRaw("jp2c", 0, cs)),
		"jp2c len 8":                pre(boxRaw("jp2c", 8, cs)),
		"jp2c len 7":                pre(boxRaw("jp2c", 7, cs)),
		"jp2c len 2^32-1":           pre(boxRaw("jp2c", 1<<32-1, cs)),
		"jp2h len 2^32-1":           pre(boxRaw("jp2h", 1<<32-1, cs)),
		"jp2h len 0":                pre(boxRaw("jp2h", 0, cs)),
		"jp2h XL 2^63-1":            pre(boxXL("jp2h", 1<<63-1, cs)),
		"unknown XL 2^63-1":         pre(boxXL("zzzz", 1<<63-1, cs)),
		"unknown len 0 then cs":     pre(boxRaw("zzzz", 0, cs)),
		"deep nested jp2h":          pre(deep, boxOf("jp2c", cs)),
		"jp2h child XL 15":          jp2File(t, boxXL("colr", 15, nil)),
		"jp2h child XL 2^64-1":      jp2File(t, boxXL("colr", 1<<64-1, nil)),
		"jp2h child len 0 colr":     jp2File(t, boxRaw("colr", 0, []byte{2, 0, 0, 1, 2, 3})),
		"jp2h child len 3":          jp2File(t, boxRaw("colr", 3, nil)),
		"jp2h trailing 7 bytes":     jp2File(t, boxOf("ihdr", make([]byte, 14)), make([]byte, 7)),
		"pclr ne=0":                 jp2File(t, boxOf("pclr", []byte{0, 0, 1, 7})),
		"pclr ne=65535":             jp2File(t, boxOf("pclr", []byte{0xFF, 0xFF, 1, 7}, make([]byte, 65535))),
		"pclr npc=255, no data":     jp2File(t, boxOf("pclr", []byte{0, 1, 255}, bytes.Repeat([]byte{7}, 255), make([]byte, 255))),
		"pclr depth 31 signed":      jp2File(t, boxOf("pclr", []byte{0, 2, 1, 0x9E}, []byte{0x80, 0, 0, 0, 0x7F, 0xFF, 0xFF, 0xFF})),
		"pclr depth 38":             jp2File(t, boxOf("pclr", []byte{0, 1, 1, 37}, make([]byte, 5))),
		"pclr depth 0xFF":           jp2File(t, boxOf("pclr", []byte{0, 1, 1, 0xFF}, make([]byte, 5))),
		"cmap odd length":           jp2File(t, boxOf("cmap", []byte{0, 0, 1, 0, 0})),
		"cmap comp 65535":           jp2File(t, boxOf("pclr", []byte{0, 1, 1, 7, 5}), boxOf("cmap", []byte{0xFF, 0xFF, 1, 0})),
		"cmap column 255":           jp2File(t, boxOf("pclr", []byte{0, 1, 1, 7, 5}), boxOf("cmap", []byte{0, 0, 1, 255})),
		"cdef n=65535 short":        jp2File(t, boxOf("cdef", []byte{0xFF, 0xFF, 0, 0, 0, 0, 0, 1})),
		"cdef channel 65535":        jp2File(t, boxOf("cdef", []byte{0, 1, 0xFF, 0xFF, 0, 1, 0xFF, 0xFF})),
		"bpcc depth 0xFF":           jp2File(t, boxOf("bpcc", []byte{0xFF})),
		"ftbl without flst":         pre(boxOf("ftbl")),
		"ftbl flst nf=65535 short":  pre(boxOf("ftbl", boxOf("flst", []byte{0xFF, 0xFF}))),
		"ftbl flst length 2^32-1":   pre(boxOf("ftbl", boxOf("flst", []byte{0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0xFF, 0xFF, 0xFF, 0xFF, 0, 0}))),
		"ftbl flst offset 2^63-1":   pre(boxOf("ftbl", boxOf("flst", []byte{0, 1, 0x7F, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0, 0, 0, 1, 0, 0}))),
		"ftbl flst offset 2^64-1":   pre(boxOf("ftbl", boxOf("flst", []byte{0, 1, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0, 0, 0, 1, 0, 0}))),
		"ftbl nested box XL 2^64-1": pre(boxOf("ftbl", boxXL("flst", 1<<64-1, nil))),
		"ftyp 7 bytes":              slices.Concat(jp2Sig(), boxOf("ftyp", make([]byte, 7))),
		"signature only":            jp2Sig(),
		"signature and 7 bytes":     slices.Concat(jp2Sig(), []byte{0, 0, 0, 8, 'a', 'b', 'c'}),
	}
	for name, data := range cases {
		hs := hostileMeasure(4<<20, func(b *membudget.Budget) { ReadHeader(bytes.NewReader(data), b) })
		hs.check(t, name+" (header)")
		hostileDecode(t, name+" (decode)", data, 4<<20, nil)
		hs = hostileMeasure(4<<20, func(b *membudget.Budget) { Decode(noSizeReader{bytes.NewReader(data)}, &Options{Budget: b}) })
		hs.check(t, name+" (no size)")
		var img *Image
		hs = hostileMeasure(4<<20, func(b *membudget.Budget) {
			var err error
			if img, err = Decode(bytes.NewReader(data), &Options{Budget: b}); err == nil {
				_, err = ApplyPalette(img, b)
			}
		})
		hs.check(t, name+" (palette)")
	}
}

// Decode copies its input; the copy must be charged.
func TestHostileInputCopy(t *testing.T) {
	cs := jp2Codestream(t)
	for _, size := range []int{(32 << 20) + 1, (64 << 20) + 1} {
		f := slices.Concat(jp2Sig(), jp2Ftyp(), boxOf("free", make([]byte, size)), boxOf("jp2c", cs))
		hostileDecode(t, fmt.Sprintf("input %d MiB", len(f)>>20), f, 1<<40, nil)
		r := hostileDecode(t, "limited", f, int64(len(f)/2), nil)
		if !hostileExceeded(r.err) {
			t.Errorf("limited budget: err = %v, want budget failure", r.err)
		}
	}
}

// Absurd option values must neither spawn goroutines nor allocate in
// proportion to them.
func TestHostileHugeOptions(t *testing.T) {
	f := jp2File(t)
	r := hostileDecode(t, "workers", f, 1<<40, &Options{Workers: math.MaxInt})
	if r.err != nil {
		t.Errorf("Workers=2^40: %v", r.err)
	}
	r = hostileDecode(t, "reduce", f, 1<<40, &Options{Reduce: math.MaxInt})
	if r.err == nil {
		t.Error("Reduce=2^40 decoded without error")
	}
}

// A bare codestream that starts at a non-zero offset of a seekable
// reader is read from the reader's current position.
func TestHostileBareCodestreamOffset(t *testing.T) {
	cs := jp2Codestream(t)
	prefix := []byte("%PDF-1.7 stream follows\n")
	data := slices.Concat(prefix, cs)
	want, err := Decode(bytes.NewReader(cs), nil)
	if err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(data)
	r.Seek(int64(len(prefix)), io.SeekStart)
	got, err := Decode(r, nil)
	if err != nil {
		t.Fatalf("positioned reader: %v", err)
	}
	if diff := cmp.Diff(want.Planes, got.Planes); diff != "" {
		t.Errorf("positioned reader differs (-want +got):\n%s", diff)
	}
	h, err := ReadHeader(io.NewSectionReader(bytes.NewReader(data), int64(len(prefix)), int64(len(cs))), nil)
	if err != nil {
		t.Fatalf("section reader: %v", err)
	}
	if diff := cmp.Diff(want.Header, *h); diff != "" {
		t.Errorf("section reader header differs (-want +got):\n%s", diff)
	}
}

func FuzzJP2(f *testing.F) {
	cs := jp2Codestream(f)
	pclr := boxOf("pclr", []byte{0, 3, 2, 7, 15}, make([]byte, 9))
	cmap := boxOf("cmap", []byte{0, 0, 1, 0, 0, 0, 1, 1})
	cdef := boxOf("cdef", []byte{0, 1, 0, 0, 0, 0, 0, 1})
	ihdr := boxOf("ihdr", make([]byte, 14))
	f.Add(jp2File(f, ihdr, boxOf("colr", []byte{1, 0, 0, 0, 0, 0, 16}), pclr, cmap, cdef))
	f.Add(jp2File(f, ihdr, boxOf("bpcc", []byte{7})))
	f.Add(fragFile(f, 3, nil))
	f.Add(slices.Concat(jp2Sig(), jp2Ftyp(), cs))
	f.Fuzz(func(t *testing.T, data []byte) {
		limit := int64(1<<20 + 64*len(data))
		hs := hostileMeasure(limit, func(b *membudget.Budget) { ReadHeader(bytes.NewReader(data), b) })
		hs.check(t, "ReadHeader")
		r := hostileDecode(t, "Decode", data, limit, nil)
		if r.err == nil {
			hostileMeasure(1<<22, func(b *membudget.Budget) {
				if a, err := ApplyPalette(r.img, b); err == nil && len(a.Planes) >= 3 {
					SYCCToRGB(a)
				}
			}).check(t, "ApplyPalette")
		}
	})
}

// ---- concurrency ----

// Concurrent decoding must give the same planes as sequential decoding,
// also for damaged streams, and must not race.
func TestHostileWorkers(t *testing.T) {
	img := testImage(97, 83, rgb(8), 3)
	for _, p := range []EncodeParams{
		{TileWidth: 32, TileHeight: 32, Lossy: true},
		{TileWidth: 200, TileHeight: 200},
		{TileWidth: 16, TileHeight: 16, TileParts: 3, PackedHeaders: PackedInTile},
	} {
		var buf bytes.Buffer
		if err := Encode(&buf, img, &p); err != nil {
			t.Fatal(err)
		}
		data := buf.Bytes()
		for k := range 8 {
			d := slices.Clone(data)
			if k > 0 {
				for i := range 4 {
					d[(k*7919+i*104729)%len(d)] ^= byte(1 + k + i)
				}
				if k%5 == 0 {
					d = d[:len(d)*3/4]
				}
			}
			limit := hostileLimit(len(d))
			a, errA := Decode(bytes.NewReader(d), &Options{Budget: membudget.New(limit)})
			for _, w := range []int{2, 8, 100} {
				b, errB := Decode(bytes.NewReader(d), &Options{Workers: w, Budget: membudget.New(limit)})
				if (errA == nil) != (errB == nil) {
					t.Errorf("k=%d workers=%d: error mismatch: %v vs %v", k, w, errA, errB)
					continue
				}
				if errA == nil {
					for c := range a.Planes {
						if !slices.Equal(a.Planes[c].Pix, b.Planes[c].Pix) {
							t.Errorf("k=%d workers=%d: plane %d differs", k, w, c)
						}
					}
				}
			}
		}
	}
}

// ---- random geometry ----

func hostilePick[T any](r *rand.Rand, xs ...T) T { return xs[r.IntN(len(xs))] }

// hostileRandBody returns n bytes of one of several kinds of junk.
func hostileRandBody(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	switch r.IntN(4) {
	case 0: // zeros: empty packets
	case 1:
		for i := range b {
			b[i] = byte(r.IntN(256))
		}
	case 2:
		for i := range b {
			b[i] = hostilePick[byte](r, 0xFF, 0x80, 0x00, 0xC0, 0x7F, 0xFE)
		}
	case 3:
		for i := range b {
			if r.IntN(8) == 0 {
				b[i] = byte(r.IntN(256))
			} else {
				b[i] = 0xFF
			}
		}
	}
	return b
}

// hostileRandSpec builds a random but syntactically valid codestream
// with extreme geometry parameters, including tile-parts with their own
// COD, COC, QCD, QCC and POC markers.  It returns nil when the random
// choices violate the SIZ rules.
func hostileRandSpec(r *rand.Rand) *hostileSpec {
	big := func() uint32 {
		return hostilePick(r, uint32(1), 2, 3, 7, 16, 17, 31, 33, 64, 100, 255, 256, 257, 1000, 4097)
	}
	s := &hostileSpec{eoc: r.IntN(2) == 0}
	z := &s.siz
	tiny := r.IntN(2) == 0
	if tiny {
		big = func() uint32 { return uint32(1 + r.IntN(5)) }
	}
	z.Xsiz, z.Ysiz = big(), big()
	if r.IntN(3) == 0 {
		z.XOsiz, z.YOsiz = uint32(r.IntN(int(z.Xsiz))), uint32(r.IntN(int(z.Ysiz)))
	}
	z.XTsiz, z.YTsiz = hostilePick(r, z.Xsiz, big(), big()), hostilePick(r, z.Ysiz, big(), big())
	z.XTOsiz, z.YTOsiz = z.XOsiz, z.YOsiz
	if r.IntN(2) == 0 {
		z.XTOsiz = uint32(r.IntN(int(z.XOsiz) + 1))
		z.YTOsiz = uint32(r.IntN(int(z.YOsiz) + 1))
	}
	nc := hostilePick(r, 1, 1, 2, 3, 3, 4, 5)
	if tiny {
		nc = 3
	}
	for range nc {
		dx, dy := 1, 1
		if r.IntN(3) == 0 {
			dx, dy = hostilePick(r, 1, 2, 3, 255, 7), hostilePick(r, 1, 2, 3, 255, 5)
		}
		if tiny && r.IntN(4) != 0 {
			dx, dy = 2, hostilePick(r, 1, 2)
		}
		z.Components = append(z.Components, codestream.ComponentSIZ{
			Depth: hostilePick(r, 1, 8, 8, 12, 16, 24, 31), Signed: r.IntN(3) == 0, XRsiz: dx, YRsiz: dy})
	}
	if z.Validate() != nil {
		return nil
	}
	levels := hostilePick(r, 0, 1, 2, 3, 5, 8, 16, 32)
	s.cod = hostileCOD(levels, codestream.Progression(r.IntN(5)), hostilePick(r, 1, 1, 2, 5), 15, 15)
	c := &s.cod
	c.CodeBlockW = hostilePick(r, 2, 3, 6, 10)
	c.CodeBlockH = hostilePick(r, 2, 4, 6, 10)
	if c.CodeBlockW+c.CodeBlockH > 12 {
		c.CodeBlockH = 12 - c.CodeBlockW
	}
	for i := range c.PrecinctSizes {
		lo := min(i, 1)
		c.PrecinctSizes[i] = codestream.PrecinctSize{
			PPx: hostilePick(r, lo, lo, 1, 2, 3, 5, 8, 15, 15), PPy: hostilePick(r, lo, lo, 1, 2, 3, 5, 8, 15, 15)}
	}
	c.Reversible = r.IntN(2) == 0
	c.MCT = nc >= 3 && r.IntN(2) == 0
	c.SOP, c.EPH = r.IntN(4) == 0, r.IntN(4) == 0
	c.EntropyBypass, c.ResetContexts, c.Terminate = r.IntN(3) == 0, r.IntN(3) == 0, r.IntN(3) == 0
	c.VerticallyCausal, c.PredictableTermination, c.SegmentationSymbols = r.IntN(3) == 0, r.IntN(3) == 0, r.IntN(3) == 0
	if c.Reversible {
		s.qcd = hostileQCD(levels)
	} else {
		s.qcd = codestream.Quantization{Style: codestream.QuantExpounded, GuardBits: 2}
		for range 3*levels + 1 {
			s.qcd.Exponents = append(s.qcd.Exponents, 10)
			s.qcd.Mantissas = append(s.qcd.Mantissas, r.IntN(2048))
		}
	}
	if r.IntN(3) == 0 { // odd quantisation
		s.qcd.GuardBits = hostilePick(r, 0, 1, 7)
		for i := range s.qcd.Exponents {
			s.qcd.Exponents[i] = hostilePick(r, 0, 1, 10, 31)
		}
	}
	if r.IntN(4) == 0 {
		for range hostilePick(r, 1, 2, 5) {
			e := codestream.POCEntry{
				ResStart: r.IntN(3), CompStart: 0, LayerEnd: 1 + r.IntN(6), ResEnd: 4 + r.IntN(30), CompEnd: 1 + r.IntN(6),
				Progression: codestream.Progression(r.IntN(5))}
			e.ResEnd = max(e.ResEnd, e.ResStart+1)
			s.poc = append(s.poc, e)
		}
	}
	if r.IntN(4) == 0 { // packed headers in the main header
		for range hostilePick(r, 1, 2, 3) {
			d := hostileRandBody(r, hostilePick(r, 0, 4, 30, 300))
			if len(d) >= 4 && r.IntN(2) == 0 {
				d[0], d[1], d[2], d[3] = 0, 0, 0, byte(len(d)-4)
			}
			s.ppm = append(s.ppm, d)
		}
	}
	nx, ny, _ := z.NumTiles()
	nt := nx * ny
	for range hostilePick(r, 1, 1, 2, 4, 9) {
		tile := r.IntN(nt)
		if r.IntN(10) == 0 {
			tile = r.IntN(65536)
		}
		p := hostilePart{tile: tile, part: r.IntN(3), count: r.IntN(3)}
		if tiny && r.IntN(4) != 0 {
			p.part = 0
		}
		if r.IntN(5) == 0 || tiny {
			cc := *c
			cc.Layers = hostilePick(r, 1, 3)
			cc.Reversible = r.IntN(2) == 0
			cc.MCT = r.IntN(2) == 0
			cc.Levels = r.IntN(3)
			cc.PrecinctSizes = codestream.DefaultPrecincts(cc.Levels + 1)
			p.cod = &cc
			mk := func(rev bool, lv int) *codestream.Quantization {
				q := hostileQCD(lv)
				if !rev {
					q = hostileQCDLossy(lv)
				}
				return &q
			}
			cocRev, cocComp := r.IntN(2) == 0, r.IntN(nc)
			p.hasCOC = r.IntN(2) == 0
			p.extra = func(w *codestream.Writer) {
				w.QCD(mk(cc.Reversible, cc.Levels))
				if p.hasCOC {
					cs := cc.CodingStyle
					cs.Reversible = cocRev
					w.COC(cocComp, &cs)
					w.QCC(cocComp, mk(cocRev, cs.Levels))
				}
			}
		}
		if s.ppm == nil && r.IntN(4) == 0 {
			for range hostilePick(r, 1, 2) {
				p.ppt = append(p.ppt, hostileRandBody(r, hostilePick(r, 0, 3, 40)))
			}
		}
		if r.IntN(6) == 0 {
			p.poc = []codestream.POCEntry{{ResEnd: 1 + r.IntN(33), CompEnd: 1 + r.IntN(5), LayerEnd: 1 + r.IntN(4), Progression: codestream.Progression(r.IntN(5))}}
		}
		p.body = hostileRandBody(r, hostilePick(r, 0, 1, 3, 20, 200, 2000))
		s.parts = append(s.parts, p)
	}
	return s
}

// hostileGeometryRun decodes a random spec at several reductions and
// worker counts.
func hostileGeometryRun(t testing.TB, name string, s *hostileSpec, r *rand.Rand, limit int64) (okCount int) {
	data := s.build()
	if data == nil {
		return 0
	}
	for _, red := range []int{0, 1, s.cod.Levels} {
		res := hostileDecode(t, fmt.Sprintf("%s reduce %d", name, red), data, limit,
			&Options{Reduce: red, Workers: hostilePick(r, 0, 0, 3)})
		if res.err == nil {
			okCount++
		}
	}
	return okCount
}

func TestHostileRandomGeometry(t *testing.T) {
	r := rand.New(rand.NewPCG(hostileSeed(1), 99))
	var okCount, built, tileCOD, tileCOC, withPOC, withPPM, withPPT int
	for i := range 2000 * hostileScale() {
		s := hostileRandSpec(r)
		if s == nil {
			continue
		}
		if s.build() != nil {
			built++
		}
		for _, p := range s.parts {
			if p.cod != nil {
				tileCOD++
			}
			if p.hasCOC {
				tileCOC++
			}
			if len(p.ppt) > 0 {
				withPPT++
			}
		}
		if len(s.poc) > 0 {
			withPOC++
		}
		if len(s.ppm) > 0 {
			withPPM++
		}
		okCount += hostileGeometryRun(t, fmt.Sprintf("spec %d", i), s, r, 16<<20)
		if t.Failed() {
			t.FailNow()
		}
	}
	// the generator must keep covering the interesting features, among
	// them the per-tile coding styles of the stale-buffer regression
	for name, n := range map[string]int{
		"built": built, "tile COD": tileCOD, "tile COC": tileCOC, "POC": withPOC,
		"PPM": withPPM, "PPT": withPPT, "successful decodes": okCount,
	} {
		if n == 0 {
			t.Errorf("no random stream with %s", name)
		}
	}
}

func FuzzGeometry(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("seed one"))
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20})
	f.Fuzz(func(t *testing.T, data []byte) {
		var seed [32]byte
		copy(seed[:], data)
		r := rand.New(rand.NewChaCha8(seed))
		s := hostileRandSpec(r)
		if s == nil {
			return
		}
		hostileGeometryRun(t, "spec", s, r, 8<<20)
	})
}

// ---- encode and decode ----

// A random encoder configuration must round-trip; damaged and truncated
// versions of the result must decode without panic.
func TestHostileEncodeDecode(t *testing.T) {
	r := rand.New(rand.NewPCG(hostileSeed(5), 7))
	for it := range 60 * hostileScale() {
		w, h := 1+r.IntN(90), 1+r.IntN(90)
		nc := hostilePick(r, 1, 1, 3, 3, 4)
		var comps []Component
		sub := r.IntN(3) == 0
		for range nc {
			c := Component{Depth: hostilePick(r, 1, 8, 8, 12, 16), Signed: r.IntN(4) == 0, DX: 1, DY: 1}
			if sub {
				c.DX, c.DY = hostilePick(r, 1, 2, 3), hostilePick(r, 1, 2)
			}
			comps = append(comps, c)
		}
		img := testImage(w, h, comps, uint64(it))
		p := EncodeParams{
			Lossy: r.IntN(2) == 0, Layers: hostilePick(r, 0, 1, 3, 7),
			Levels:         hostilePick(r, 0, 1, 3, 5, 8),
			CodeBlockWidth: hostilePick(r, 4, 8, 16, 64), CodeBlockHeight: hostilePick(r, 4, 8, 16, 64),
			Progression: Progression(r.IntN(5)),
			Bypass:      r.IntN(4) == 0, ResetContexts: r.IntN(4) == 0, Terminate: r.IntN(4) == 0,
			VerticallyCausal: r.IntN(4) == 0, SegmentationSymbols: r.IntN(4) == 0,
			SOP: r.IntN(4) == 0, EPH: r.IntN(4) == 0,
			TileParts: hostilePick(r, 0, 0, 2, 5), NoMCT: r.IntN(4) == 0,
			PackedHeaders: PackedHeaders(hostilePick(r, 0, 0, 1, 2)),
		}
		if r.IntN(2) == 0 {
			p.TileWidth, p.TileHeight = 1+r.IntN(w+5), 1+r.IntN(h+5)
		}
		if r.IntN(2) == 0 {
			for range hostilePick(r, 1, 2, 4) {
				p.Precincts = append(p.Precincts, PrecinctSize{hostilePick(r, 0, 1, 2, 4, 7, 15), hostilePick(r, 0, 1, 2, 4, 7, 15)})
			}
		}
		if p.CodeBlockWidth*p.CodeBlockHeight > 4096 {
			p.CodeBlockHeight = 4096 / p.CodeBlockWidth
		}
		var buf bytes.Buffer
		if err := Encode(&buf, img, &p); err != nil {
			continue // the encoder may refuse a random configuration
		}
		data := buf.Bytes()
		name := fmt.Sprintf("it %d (%+v)", it, p)
		pristine, err := Decode(bytes.NewReader(data), &Options{Budget: membudget.New(256 << 20)})
		if err != nil {
			t.Fatalf("%s: decoding the encoder's output failed: %v", name, err)
		}
		if !p.Lossy {
			for c := range img.Planes {
				if !slices.Equal(img.Planes[c].Pix, pristine.Planes[c].Pix) {
					t.Errorf("%s: lossless round trip differs in component %d", name, c)
					break
				}
			}
		}
		for red := 0; red <= min(p.Levels, 5); red++ {
			if res := hostileDecode(t, name, data, 32<<20, &Options{Reduce: red}); res.err != nil {
				t.Errorf("%s: reduce %d: %v", name, red, res.err)
			}
		}
		for range 6 {
			d := slices.Clone(data)
			for range 1 + r.IntN(6) {
				d[r.IntN(len(d))] = byte(r.IntN(256))
			}
			hostileDecode(t, name+" flipped", d, 32<<20, &Options{Reduce: r.IntN(3)})
		}
		hostileDecode(t, name+" truncated", data[:r.IntN(len(data))], 32<<20, nil)
		if t.Failed() {
			t.FailNow()
		}
	}
}

// Tier-1 work is charged to the budget: blocks declaring many passes
// over a single data byte are stopped by a budget that covers the
// memory but not the work.
func TestHostileTier1WorkCharged(t *testing.T) {
	const side = 2048
	q := codestream.Quantization{Style: codestream.QuantNone, GuardBits: 7, Exponents: []int{24}}
	s := hostileSpecFor(side, side, hostileComps(1, hostileUnit),
		hostileCOD(0, codestream.LRCP, 1, 15, 15), q)
	s.addPart(nil, singleLayerPacket(side, side, 64, 164, 1, false))
	data := s.mustBuild(t)
	r := hostileDecode(t, "many passes", data, 64<<20, nil)
	if !hostileExceeded(r.err) {
		t.Errorf("err = %v, want budget failure", r.err)
	}

	// a normal image fits a budget of moderate size
	img := testImage(256, 256, rgb(8), 5)
	var buf bytes.Buffer
	if err := Encode(&buf, img, &EncodeParams{}); err != nil {
		t.Fatal(err)
	}
	r = hostileDecode(t, "normal", buf.Bytes(), 16<<20, nil)
	if r.err != nil {
		t.Fatalf("normal image: %v", r.err)
	}
	if diff := cmp.Diff(img.Planes, r.img.Planes); diff != "" {
		t.Errorf("round trip failed (-want +got):\n%s", diff)
	}
}
