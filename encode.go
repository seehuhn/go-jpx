// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jpx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"seehuhn.de/go/jpx/internal/codestream"
	"seehuhn.de/go/jpx/internal/dwt"
	"seehuhn.de/go/jpx/internal/jp2"
	"seehuhn.de/go/jpx/internal/t1"
	"seehuhn.de/go/membudget"
)

// Progression orders for the encoder.
type Progression int

// The five progression orders of JPEG 2000.
const (
	LRCP Progression = iota // layer, resolution, component, position
	RLCP                    // resolution, layer, component, position
	RPCL                    // resolution, position, component, layer
	PCRL                    // position, component, resolution, layer
	CPRL                    // component, position, resolution, layer
)

// EncodeParams controls encoding.  The zero value encodes losslessly
// with reasonable defaults.
type EncodeParams struct {
	// Lossy selects the irreversible 9/7 wavelet with quantisation
	// controlled by Quality; otherwise the reversible 5/3 wavelet is
	// used without quantisation and the image is reproduced exactly,
	// unless Rates truncates the final layer.
	Lossy bool

	// Quality sets the quantisation step for lossy encoding, from 1
	// (coarsest) to 100 (finest).  Zero selects a default of 75.
	Quality float64

	// Layers is the number of quality layers; zero means one.  Without
	// Rates, the layers hold geometrically increasing shares of the
	// coded data.
	Layers int

	// Rates optionally gives cumulative target sizes for the layers in
	// bits per reference grid point.  Its length sets the number of
	// layers.  A zero rate for the last layer means "all data".  The
	// targets count code-block data only, so the codestream is somewhat
	// larger than the rates suggest, and they are applied per tile.
	Rates []float64

	// Levels is the number of wavelet decomposition levels; zero means 5,
	// NoLevels selects no decomposition at all.
	Levels int

	// TileWidth and TileHeight set the tile size on the reference grid;
	// zero means a single tile.
	TileWidth, TileHeight int

	// CodeBlockWidth and CodeBlockHeight are powers of two between 4 and
	// 1024 with a product of at most 4096; zero means 64.
	CodeBlockWidth, CodeBlockHeight int

	// Precincts optionally gives the precinct size exponents per
	// resolution level, from the lowest resolution upwards; the last
	// entry is repeated for higher levels.
	Precincts []PrecinctSize

	Progression Progression

	// Code-block coding style options.
	Bypass, ResetContexts, Terminate, VerticallyCausal, SegmentationSymbols bool

	// SOP and EPH add packet markers to the codestream.
	SOP, EPH bool

	// TileParts splits the packets of every tile over this many
	// tile-parts (at most 255); zero or one means a single tile-part per
	// tile.  Tiles with fewer packets get fewer tile-parts.
	TileParts int

	// PackedHeaders moves the packet headers out of the tile-part bodies
	// into PPT marker segments (PackedInTile) or PPM marker segments of
	// the main header (PackedInMain).  The marker segments hold at most
	// 64 KiB each and are numbered with one byte, which limits the packed
	// headers to about 16 MiB per tile or per codestream respectively.
	PackedHeaders PackedHeaders

	// ProgressionChanges optionally replaces the single progression order
	// by a sequence of progression volumes.  Together they must cover all
	// packets; resolution and component ranges must exist in the image.
	ProgressionChanges []ProgressionChange

	// NoMCT disables the component transform, which is otherwise applied
	// whenever the first three components have identical sampling.
	NoMCT bool

	// ROI optionally selects a rectangular region of interest on the
	// reference grid that is coded with higher priority.  The region's
	// coefficients are scaled above all others, which doubles the number
	// of coded bit-planes; the option is therefore limited to images of
	// up to about 12 bits per sample.
	ROI *Rectangle

	// Wrapper selects a JP2 file wrapper around the codestream.  The
	// colour specifications, channel definitions and palette are taken
	// from the image header.  Without colour specifications an
	// enumerated colour space is chosen by the number of colour channels
	// (greyscale, sRGB or CMYK), or the colour space is marked unknown.
	Wrapper bool

	// Budget bounds the working memory; nil means no limit.
	Budget *membudget.Budget
}

// PackedHeaders selects where packet headers are stored.
type PackedHeaders int

// Packet header placements.
const (
	HeadersInline PackedHeaders = iota // in front of each packet body
	PackedInTile                       // PPT marker segments in the tile-part headers
	PackedInMain                       // PPM marker segments in the main header
)

// ProgressionChange is one progression order volume: the packets of
// resolution levels [ResStart, ResEnd), components [CompStart, CompEnd)
// and layers below LayerEnd, in the given order, as far as they have not
// been written by an earlier volume.
type ProgressionChange struct {
	ResStart, CompStart int
	ResEnd, CompEnd     int
	LayerEnd            int
	Progression         Progression
}

// NoLevels as EncodeParams.Levels selects a codestream without wavelet
// decomposition.
const NoLevels = -1

// PrecinctSize gives the precinct size exponents of one resolution level.
type PrecinctSize struct{ PPx, PPy int }

// Rectangle is a half-open rectangle on the reference grid.
type Rectangle struct{ X0, Y0, X1, Y1 int }

const (
	defaultQuality   = 75
	defaultLevels    = 5
	defaultBlockExp  = 6
	encoderGuardBits = 2
)

// Encode writes img as a JPEG 2000 codestream, or as a JP2 file when
// p.Wrapper is set.  The planes of img must be at full resolution.
func Encode(w io.Writer, img *Image, p *EncodeParams) error {
	if p == nil {
		p = &EncodeParams{}
	}
	e, err := newEncoder(img, p)
	if err != nil {
		return err
	}
	cs, err := e.codestream()
	if err != nil {
		return err
	}
	if !p.Wrapper {
		_, err := w.Write(cs)
		return err
	}
	return jp2.Write(w, e.wrapperHeader(), cs)
}

// encoder holds the state of one encoding run.
type encoder struct {
	img    *Image
	p      *EncodeParams
	charge func(int) error
	main   *codestream.MainHeader
	layers int
	rates  []float64

	t1    *t1.Encoder
	norms [][4]float64 // synthesis energy gains per level and orientation
}

func newEncoder(img *Image, p *EncodeParams) (*encoder, error) {
	if err := validateImage(img); err != nil {
		return nil, err
	}
	e := &encoder{img: img, p: p, charge: chargeFunc(p.Budget), t1: t1.NewEncoder()}

	e.layers = max(p.Layers, 1)
	if len(p.Rates) > 0 {
		e.layers = len(p.Rates)
		e.rates = p.Rates
	}
	if e.layers > 65535 {
		return nil, errors.New("jpx: too many layers")
	}

	m, err := e.buildMainHeader()
	if err != nil {
		return nil, err
	}
	e.main = m
	e.norms = synthesisGains(m.COD.Levels, !m.COD.Reversible)
	return e, nil
}

// validateImage checks that the image is consistent with its header and
// that every sample fits its component's range.
func validateImage(img *Image) error {
	if img == nil {
		return errors.New("jpx: nil image")
	}
	h := &img.Header
	if h.Width <= 0 || h.Height <= 0 || uint64(h.Width) > math.MaxUint32 || uint64(h.Height) > math.MaxUint32 {
		return errors.New("jpx: invalid image size")
	}
	if len(h.Components) == 0 || len(h.Components) > 16384 {
		return errors.New("jpx: invalid number of components")
	}
	if len(img.Planes) != len(h.Components) {
		return errors.New("jpx: number of planes does not match the components")
	}
	if img.Reduce != 0 {
		return errors.New("jpx: cannot encode a reduced image")
	}
	for i, c := range h.Components {
		if c.Depth < 1 || c.Depth > MaxEncodeDepth {
			return fmt.Errorf("jpx: component %d has unsupported depth %d", i, c.Depth)
		}
		if c.DX < 1 || c.DX > 255 || c.DY < 1 || c.DY > 255 {
			return fmt.Errorf("jpx: component %d has invalid sub-sampling", i)
		}
		w, hh := ceilDiv(h.Width, c.DX), ceilDiv(h.Height, c.DY)
		pl := img.Planes[i]
		if pl.Width != w || pl.Height != hh || hh > math.MaxInt/8/w || len(pl.Pix) != w*hh {
			return fmt.Errorf("jpx: plane %d has size %dx%d, want %dx%d", i, pl.Width, pl.Height, w, hh)
		}
		lo, hi := sampleRange(c)
		for _, v := range pl.Pix {
			if v < lo || v > hi {
				return fmt.Errorf("jpx: sample %d of plane %d is outside the %d-bit range", v, i, c.Depth)
			}
		}
	}
	for i, ch := range h.Channels {
		if ch.Component < 0 || ch.Component >= len(h.Components) {
			return fmt.Errorf("jpx: channel %d refers to component %d", i, ch.Component)
		}
		if ch.PaletteColumn >= 0 && (h.Palette == nil || ch.PaletteColumn >= len(h.Palette.Columns)) {
			return fmt.Errorf("jpx: channel %d refers to a missing palette column", i)
		}
		if ch.Assoc < 0 || ch.Assoc > 65535 {
			return fmt.Errorf("jpx: channel %d has an invalid association", i)
		}
	}
	if h.Palette != nil {
		if len(h.Palette.Entries) == 0 || len(h.Palette.Entries) > 1024 || len(h.Palette.Columns) == 0 || len(h.Palette.Columns) > 255 {
			return errors.New("jpx: invalid palette size")
		}
		for _, e := range h.Palette.Entries {
			if len(e) != len(h.Palette.Columns) {
				return errors.New("jpx: palette entry with wrong number of columns")
			}
		}
	}
	for i, cs := range h.ColorSpecs {
		if cs.Method < 1 || cs.Method > 255 || cs.Precedence < -128 || cs.Precedence > 127 || cs.Approx < 0 || cs.Approx > 255 || cs.Enumerated < 0 || uint64(cs.Enumerated) > math.MaxUint32 {
			return fmt.Errorf("jpx: colour specification %d has values out of range", i)
		}
	}
	return nil
}

// sampleRange returns the smallest and largest sample value of a
// component.
func sampleRange(c Component) (lo, hi int32) {
	if c.Signed {
		return -(1 << uint(c.Depth-1)), 1<<uint(c.Depth-1) - 1
	}
	return 0, 1<<uint(c.Depth) - 1
}

// MaxEncodeDepth is the largest sample precision the encoder accepts.
// The limit keeps the number of coded bit-planes within the coder's range
// with the usual guard bits.
const MaxEncodeDepth = 24

// buildMainHeader derives the codestream parameters from the image and
// the encoding parameters.
func (e *encoder) buildMainHeader() (*codestream.MainHeader, error) {
	img, p := e.img, e.p
	m := &codestream.MainHeader{}
	m.SIZ = codestream.SIZ{
		Xsiz:  uint32(img.Width),
		Ysiz:  uint32(img.Height),
		XTsiz: uint32(img.Width),
		YTsiz: uint32(img.Height),
	}
	if p.TileWidth > 0 || p.TileHeight > 0 {
		if p.TileWidth <= 0 || p.TileHeight <= 0 {
			return nil, errors.New("jpx: tile width and height must both be set")
		}
		m.SIZ.XTsiz, m.SIZ.YTsiz = uint32(p.TileWidth), uint32(p.TileHeight)
	}
	for _, c := range img.Components {
		m.SIZ.Components = append(m.SIZ.Components, codestream.ComponentSIZ{
			Depth: c.Depth, Signed: c.Signed, XRsiz: c.DX, YRsiz: c.DY,
		})
	}
	if err := m.SIZ.Validate(); err != nil {
		return nil, err
	}
	if _, _, ok := m.SIZ.NumTiles(); !ok {
		return nil, errors.New("jpx: invalid tile geometry")
	}

	levels := p.Levels
	switch {
	case levels == 0:
		levels = defaultLevels
	case levels == NoLevels:
		levels = 0
	case levels < 0 || levels > 32:
		return nil, errors.New("jpx: invalid number of decomposition levels")
	}
	if math.IsNaN(p.Quality) || math.IsInf(p.Quality, 0) || p.Quality < 0 || p.Quality > 100 {
		return nil, errors.New("jpx: quality must be between 0 and 100")
	}
	for _, r := range p.Rates {
		if math.IsNaN(r) || math.IsInf(r, 0) || r < 0 {
			return nil, errors.New("jpx: invalid rate")
		}
	}
	if p.Layers < 0 {
		return nil, errors.New("jpx: invalid number of layers")
	}
	xcb, ycb := defaultBlockExp, defaultBlockExp
	if p.CodeBlockWidth != 0 {
		xcb = log2Exact(p.CodeBlockWidth)
	}
	if p.CodeBlockHeight != 0 {
		ycb = log2Exact(p.CodeBlockHeight)
	}
	if xcb < 2 || ycb < 2 || xcb > 10 || ycb > 10 || xcb+ycb > 12 {
		return nil, errors.New("jpx: invalid code-block size")
	}
	precincts := codestream.DefaultPrecincts(levels + 1)
	if len(p.Precincts) > 0 {
		for r := range precincts {
			ps := p.Precincts[min(r, len(p.Precincts)-1)]
			if ps.PPx < 0 || ps.PPx > 15 || ps.PPy < 0 || ps.PPy > 15 || (r > 0 && (ps.PPx < 1 || ps.PPy < 1)) {
				return nil, errors.New("jpx: invalid precinct size")
			}
			precincts[r] = codestream.PrecinctSize{PPx: ps.PPx, PPy: ps.PPy}
		}
	}
	if p.Progression < LRCP || p.Progression > CPRL {
		return nil, errors.New("jpx: invalid progression order")
	}
	if p.TileParts < 0 || p.TileParts > 255 {
		return nil, errors.New("jpx: invalid number of tile-parts")
	}
	if p.PackedHeaders < HeadersInline || p.PackedHeaders > PackedInMain {
		return nil, errors.New("jpx: invalid packet header placement")
	}
	for _, pc := range p.ProgressionChanges {
		if pc.ResStart < 0 || pc.ResEnd <= pc.ResStart || pc.ResEnd > levels+1 ||
			pc.CompStart < 0 || pc.CompEnd <= pc.CompStart || pc.CompEnd > len(img.Components) ||
			pc.LayerEnd < 1 || pc.LayerEnd > e.layers || pc.Progression < LRCP || pc.Progression > CPRL {
			return nil, errors.New("jpx: invalid progression order change")
		}
		m.POC = append(m.POC, codestream.POCEntry{
			ResStart: pc.ResStart, CompStart: pc.CompStart, LayerEnd: pc.LayerEnd,
			ResEnd: pc.ResEnd, CompEnd: pc.CompEnd, Progression: codestream.Progression(pc.Progression),
		})
	}

	m.COD = &codestream.COD{
		CodingStyle: codestream.CodingStyle{
			EntropyBypass:       p.Bypass,
			ResetContexts:       p.ResetContexts,
			Terminate:           p.Terminate,
			VerticallyCausal:    p.VerticallyCausal,
			SegmentationSymbols: p.SegmentationSymbols,
			Levels:              levels,
			CodeBlockW:          xcb,
			CodeBlockH:          ycb,
			Reversible:          !p.Lossy,
			PrecinctSizes:       precincts,
		},
		Progression: codestream.Progression(p.Progression),
		Layers:      e.layers,
		MCT:         e.useMCT(),
		SOP:         p.SOP,
		EPH:         p.EPH,
	}

	// quantization: QCD for component 0, QCC where others differ
	m.QCC = make(map[int]*codestream.Quantization)
	for c, comp := range img.Components {
		q := e.quantization(comp, c, levels, m.COD.MCT)
		if c == 0 {
			m.QCD = q
		} else if !quantEqual(q, m.QCD) {
			m.QCC[c] = q
		}
	}

	if p.ROI != nil {
		if p.ROI.X0 >= p.ROI.X1 || p.ROI.Y0 >= p.ROI.Y1 {
			return nil, errors.New("jpx: empty region of interest")
		}
		for c := range img.Components {
			// the shift must exceed the magnitude bits of any background
			// coefficient; the number of planes of the deepest band is a
			// safe choice
			q := m.QCD
			if qq, ok := m.QCC[c]; ok {
				q = qq
			}
			shift := q.GuardBits - 1
			for _, eps := range q.Exponents {
				shift = max(shift, q.GuardBits+eps-1)
			}
			if 2*shift > t1.MaxPlanes {
				return nil, errors.New("jpx: region of interest not possible at this bit depth")
			}
			m.RGN = append(m.RGN, codestream.RGN{Component: c, Shift: shift})
		}
	}
	return m, nil
}

// useMCT reports whether the component transform is applied.
func (e *encoder) useMCT() bool {
	if e.p.NoMCT || len(e.img.Components) < 3 {
		return false
	}
	c := e.img.Components
	for i := 1; i < 3; i++ {
		if c[i].DX != c[0].DX || c[i].DY != c[0].DY || c[i].Depth != c[0].Depth || c[i].Signed != c[0].Signed {
			return false
		}
	}
	return true
}

// quantization returns the quantization parameters for component c.
func (e *encoder) quantization(comp Component, c, levels int, mct bool) *codestream.Quantization {
	q := &codestream.Quantization{GuardBits: encoderGuardBits}
	// the reversible component transform adds one bit to the chroma
	// components
	extra := 0
	if !e.p.Lossy && mct && c > 0 && c < 3 {
		extra = 1
	}
	if !e.p.Lossy {
		q.Style = codestream.QuantNone
		for b := range 3*levels + 1 {
			q.Exponents = append(q.Exponents, comp.Depth+bandGain(b)+extra)
		}
		return q
	}

	q.Style = codestream.QuantExpounded
	quality := e.p.Quality
	if quality == 0 {
		quality = defaultQuality
	}
	quality = min(max(quality, 1), 100)
	// base step relative to the nominal sample range
	base := math.Pow(10, -quality/50) * math.Ldexp(1, comp.Depth)
	gains := synthesisGains(levels, true)
	for b := range 3*levels + 1 {
		lev, orient := bandLevel(b, levels)
		delta := base / math.Sqrt(gains[lev][orient])
		rb := comp.Depth + bandGain(b)
		eps, mu := stepToExpMant(delta, rb)
		q.Exponents = append(q.Exponents, eps)
		q.Mantissas = append(q.Mantissas, mu)
	}
	return q
}

// stepToExpMant converts a quantization step to the exponent/mantissa
// representation relative to the nominal range rb.
func stepToExpMant(delta float64, rb int) (eps, mu int) {
	if delta <= 0 {
		return 31, 0
	}
	fr, exp := math.Frexp(delta) // delta = fr * 2^exp, fr in [0.5, 1)
	// delta = 2^(rb-eps) * (1 + mu/2^11) with 1 <= 1+mu/2^11 < 2
	eps = rb - (exp - 1)
	mu = int(math.Round((2*fr - 1) * 2048))
	if mu >= 2048 {
		mu = 0
		eps--
	}
	if eps < 0 {
		eps, mu = 0, 0
	}
	if eps > maxExponent {
		// finer steps than the coder can represent are pointless
		eps, mu = maxExponent, 0
	}
	return eps, mu
}

// maxExponent is the largest quantization exponent the encoder uses, so
// that the number of magnitude bit-planes stays within the coder's limit.
const maxExponent = t1.MaxPlanes + 1 - encoderGuardBits

// bandGain returns the log2 sub-band gain of quantization entry b.
func bandGain(b int) int {
	if b == 0 {
		return 0
	}
	switch (b - 1) % 3 {
	case 2:
		return 2
	default:
		return 1
	}
}

// bandLevel returns the decomposition level (1 = finest) and orientation
// index (0 LL, 1 HL, 2 LH, 3 HH) of quantization entry b.
func bandLevel(b, levels int) (lev, orient int) {
	if b == 0 {
		return levels, 0
	}
	r := (b-1)/3 + 1
	return levels - r + 1, (b-1)%3 + 1
}

func quantEqual(a, b *codestream.Quantization) bool {
	if a.Style != b.Style || a.GuardBits != b.GuardBits || len(a.Exponents) != len(b.Exponents) || len(a.Mantissas) != len(b.Mantissas) {
		return false
	}
	for i := range a.Exponents {
		if a.Exponents[i] != b.Exponents[i] {
			return false
		}
	}
	for i := range a.Mantissas {
		if a.Mantissas[i] != b.Mantissas[i] {
			return false
		}
	}
	return true
}

func log2Exact(n int) int {
	for i := range 32 {
		if 1<<uint(i) == n {
			return i
		}
	}
	return -1
}

// synthesisGains returns the energy gain of each sub-band's synthesis
// basis functions, for levels 1 (finest) through levels.  Index
// [lev][orient] with orient 0 LL, 1 HL, 2 LH, 3 HH; entry [levels][0] is
// the lowest LL band.  The two-dimensional gains are products of the
// one-dimensional gains of the low-pass and high-pass synthesis filters,
// which are measured by reconstructing a unit impulse.  Beyond
// gainLevelsMeasured levels the gains grow by a constant factor per level
// and are extrapolated, which keeps the cost independent of the level
// count.
func synthesisGains(levels int, irreversible bool) [][4]float64 {
	measured := min(levels, gainLevelsMeasured)
	low := make([]float64, measured+1)  // energy of the low-pass basis at level lev
	high := make([]float64, measured+1) // energy of the high-pass basis at level lev
	for lev := 1; lev <= measured; lev++ {
		n := 16 << uint(lev)
		low[lev], high[lev] = impulseEnergy(n, lev, irreversible)
	}
	gains := make([][4]float64, levels+1)
	gains[0] = [4]float64{1, 1, 1, 1} // without decomposition the samples are the band
	for lev := 1; lev <= levels; lev++ {
		l, h := low[min(lev, measured)], high[min(lev, measured)]
		if lev > measured {
			// one more low-pass synthesis level per step beyond the table
			ratio := low[measured] / low[measured-1]
			f := math.Pow(ratio, float64(lev-measured))
			l, h = l*f, h*f
		}
		gains[lev] = [4]float64{l * l, h * l, l * h, h * h}
	}
	return gains
}

// gainLevelsMeasured is the number of decomposition levels for which the
// synthesis gains are measured rather than extrapolated.
const gainLevelsMeasured = 8

// impulseEnergy reconstructs one-dimensional unit impulses placed in the
// low-pass and in the high-pass band at level lev of a signal of length
// n and returns the energies of the results.
func impulseEnergy(n, lev int, irreversible bool) (low, high float64) {
	r := dwt.Rect{X0: 0, Y0: 0, X1: n, Y1: 1}
	ll := n >> uint(lev) // size of the low-pass band at level lev
	energy := func(pos int) float64 {
		var e float64
		if irreversible {
			buf := make([]float32, n)
			buf[pos] = 1
			dwt.Inverse97(buf, r, lev)
			for _, v := range buf {
				e += float64(v) * float64(v)
			}
		} else {
			// the reversible transform is nonlinear; measure the linear
			// part with a large impulse
			const amp = 1 << 16
			buf := make([]int32, n)
			buf[pos] = amp
			dwt.Inverse53(buf, r, lev)
			for _, v := range buf {
				f := float64(v) / amp
				e += f * f
			}
		}
		return e
	}
	return energy(ll / 2), energy(ll + ll/2)
}

// wrapperHeader builds the JP2 header boxes from the image header.
func (e *encoder) wrapperHeader() *jp2.Header {
	h := &e.img.Header
	w := &jp2.Header{HasWrapper: true, Brand: "jp2 "}
	nc := len(h.Components)
	w.Image = &jp2.ImageHeader{
		Height: uint32(h.Height), Width: uint32(h.Width), NC: uint16(nc), C: 7,
	}
	uniform := true
	for _, c := range h.Components {
		if c.Depth != h.Components[0].Depth || c.Signed != h.Components[0].Signed {
			uniform = false
		}
	}
	if uniform {
		w.Image.BPC = jp2.BitDepth{Depth: h.Components[0].Depth, Signed: h.Components[0].Signed}
	} else {
		for _, c := range h.Components {
			w.BitDepths = append(w.BitDepths, jp2.BitDepth{Depth: c.Depth, Signed: c.Signed})
		}
	}
	for _, cs := range h.ColorSpecs {
		w.Colors = append(w.Colors, jp2.ColorSpec{
			Method: uint8(cs.Method), Precedence: int8(cs.Precedence), Approx: uint8(cs.Approx),
			EnumCS: uint32(cs.Enumerated), EnumParams: cs.EnumParams, ICC: cs.ICC, Vendor: cs.Vendor,
		})
	}
	if len(w.Colors) == 0 {
		enum := uint32(0)
		switch countColorChannels(h) {
		case 1:
			enum = 17
		case 3:
			enum = 16
		case 4:
			enum = 12
		}
		if enum != 0 {
			w.Colors = append(w.Colors, jp2.ColorSpec{Method: 1, EnumCS: enum})
		} else {
			w.Image.UnkC = true
		}
	}
	if h.Palette != nil {
		pal := &jp2.Palette{Entries: len(h.Palette.Entries), Values: h.Palette.Entries}
		for _, c := range h.Palette.Columns {
			pal.Depths = append(pal.Depths, jp2.BitDepth{Depth: c.Depth, Signed: c.Signed})
		}
		w.Palette = pal
		for _, ch := range h.Channels {
			cm := jp2.ComponentMapping{Component: uint16(ch.Component)}
			if ch.PaletteColumn >= 0 {
				cm.MappingType, cm.PaletteColumn = 1, uint8(ch.PaletteColumn)
			}
			w.ComponentMap = append(w.ComponentMap, cm)
		}
	}
	if needsChannelDefinitions(h) {
		for i, ch := range h.Channels {
			typ := uint16(0)
			switch ch.Kind {
			case ChannelOpacity:
				typ = 1
			case ChannelPremultipliedOpacity:
				typ = 2
			case ChannelUnknown:
				typ = 65535
			}
			w.Channels = append(w.Channels, jp2.ChannelDef{Channel: uint16(i), Type: typ, Assoc: uint16(ch.Assoc)})
		}
	}
	return w
}

// countColorChannels returns the number of colour channels of the image.
func countColorChannels(h *Header) int {
	if len(h.Channels) == 0 {
		return len(h.Components)
	}
	n := 0
	for _, ch := range h.Channels {
		if ch.Kind == ChannelColor {
			n++
		}
	}
	return n
}

// needsChannelDefinitions reports whether the channel list differs from
// the default of one colour channel per component in order.
func needsChannelDefinitions(h *Header) bool {
	for i, ch := range h.Channels {
		if ch.Kind != ChannelColor || ch.Assoc != i+1 {
			return true
		}
	}
	return false
}

// codestream encodes all tiles and assembles the codestream.
func (e *encoder) codestream() ([]byte, error) {
	var buf bytes.Buffer
	w := codestream.NewWriter(&buf)
	w.SOC()
	w.SIZ(&e.main.SIZ)
	w.COD(e.main.COD)
	w.QCD(e.main.QCD)
	for c := range e.main.SIZ.Components {
		if q, ok := e.main.QCC[c]; ok {
			w.QCC(c, q)
		}
	}
	for _, r := range e.main.RGN {
		w.RGN(r)
	}
	if len(e.main.POC) > 0 {
		w.POC(e.main.POC)
	}
	if w.Err() != nil {
		return nil, w.Err()
	}

	nx, ny, _ := e.main.SIZ.NumTiles()
	var tiles [][]tilePartData
	for idx := range nx * ny {
		parts, err := e.encodeTile(idx)
		if err != nil {
			return nil, err
		}
		tiles = append(tiles, parts)
	}

	if e.p.PackedHeaders == PackedInMain {
		if err := e.writePPM(w, tiles); err != nil {
			return nil, err
		}
	}

	for idx, parts := range tiles {
		// The PPT index continues across the tile-parts of a tile.  The
		// standard can be read either way, and widely used decoders
		// reject an index that restarts in a later tile-part.
		z := 0
		for part, tp := range parts {
			sot := buf.Len()
			w.SOT(idx, part, len(parts))
			if e.p.PackedHeaders == PackedInTile {
				segs, err := splitHeaders(tp.headers, tp.headerLens)
				if err != nil {
					return nil, err
				}
				for _, seg := range segs {
					if err := w.PPT(z, seg); err != nil {
						return nil, err
					}
					z++
				}
			}
			w.SOD()
			if err := w.Err(); err != nil {
				return nil, err
			}
			buf.Write(tp.body)
			if int64(buf.Len())-int64(sot) > math.MaxUint32 {
				return nil, errors.New("jpx: tile-part too large")
			}
			if err := codestream.PatchPsot(buf.Bytes(), sot, uint32(buf.Len()-sot)); err != nil {
				return nil, err
			}
		}
	}
	w.EOC()
	return buf.Bytes(), w.Err()
}

// writePPM writes the packet headers of all tile-parts as PPM marker
// segments: one chunk per tile-part in codestream order, split into
// segments at packet header boundaries.
func (e *encoder) writePPM(w *codestream.Writer, tiles [][]tilePartData) error {
	var packed []byte
	var lens []int
	for _, parts := range tiles {
		for _, tp := range parts {
			if err := chargeOpt(e.charge, len(tp.headers)+4); err != nil {
				return err
			}
			packed = binary.BigEndian.AppendUint32(packed, uint32(len(tp.headers)))
			packed = append(packed, tp.headers...)
			// the chunk length travels with the first header of the chunk
			if len(tp.headerLens) > 0 {
				lens = append(lens, 4+tp.headerLens[0])
				lens = append(lens, tp.headerLens[1:]...)
			} else {
				lens = append(lens, 4)
			}
		}
	}
	segs, err := splitHeaders(packed, lens)
	if err != nil {
		return err
	}
	for z, seg := range segs {
		if err := w.PPM(z, seg); err != nil {
			return err
		}
	}
	return nil
}

// maxSegmentPayload is the most data a PPM or PPT marker segment can hold
// besides its index byte.
const maxSegmentPayload = 65535 - 3

// splitHeaders splits concatenated packet headers, whose individual
// lengths are given by lens, into marker segment payloads that each end
// on a header boundary.  An empty header list yields one empty segment.
func splitHeaders(data []byte, lens []int) ([][]byte, error) {
	var segs [][]byte
	start, pos := 0, 0
	for _, n := range lens {
		if n > maxSegmentPayload {
			return nil, errors.New("jpx: packet header too large for a marker segment")
		}
		if pos+n-start > maxSegmentPayload {
			segs = append(segs, data[start:pos])
			start = pos
		}
		pos += n
	}
	if pos != len(data) {
		return nil, errors.New("jpx: inconsistent packet header lengths")
	}
	return append(segs, data[start:pos]), nil
}

// tilePartData is the output of the encoder for one tile-part: the
// packet headers (empty when stored in-line) with their individual
// lengths, and the body.
type tilePartData struct {
	headers    []byte
	headerLens []int
	body       []byte
}
