// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package t2

import (
	"errors"
	"math"

	"seehuhn.de/go/jpx/internal/codestream"
	"seehuhn.de/go/jpx/internal/t1"
)

// Rect is a half-open rectangle [X0,X1) × [Y0,Y1).
type Rect struct{ X0, Y0, X1, Y1 int }

// Dx returns the width of r.
func (r Rect) Dx() int { return r.X1 - r.X0 }

// Dy returns the height of r.
func (r Rect) Dy() int { return r.Y1 - r.Y0 }

// Empty reports whether r contains no points.
func (r Rect) Empty() bool { return r.X1 <= r.X0 || r.Y1 <= r.Y0 }

// ceilDiv returns ceil(a/b) for a ≥ 0, b > 0.
func ceilDiv(a, b int) int { return (a + b - 1) / b }

// ceilShift returns ceil(a / 2^s) for a ≥ 0.
func ceilShift(a, s int) int { return (a + (1 << uint(s)) - 1) >> uint(s) }

// Tile is the coded structure of one tile: its components, resolution
// levels, precincts and code-blocks together with the packet decoding
// state.
type Tile struct {
	Index   int
	Bounds  Rect // on the reference grid
	Comps   []*TileComp
	Layers  int
	SOP     bool
	EPH     bool
	MCT     bool
	Default codestream.Progression
	POC     []codestream.POCEntry // progression order changes, empty for a single volume
}

// TileComp is one component of a tile.
type TileComp struct {
	Index    int  // component index
	Bounds   Rect // in component coordinates
	Style    codestream.CodingStyle
	Quant    codestream.Quantization
	ROIShift int
	Res      []*Resolution // Style.Levels+1 entries
	Depth    int           // sample precision of the component
	Signed   bool
	DX, DY   int // sub-sampling factors
}

// Resolution is one resolution level of a tile-component.
type Resolution struct {
	Level    int  // r, 0 = lowest
	Bounds   Rect // in resolution-level coordinates
	PPx, PPy int  // log2 precinct size at this level
	PW, PH   int  // precincts across and down
	Bands    []*Band
	// Precincts are created on first use; nil entries have not been seen.
	Precincts []*Precinct
	// nextLayer is the next layer expected for each precinct; it is only
	// used with progression order changes.  minLayer is the smallest
	// entry and minCount the number of precincts holding it, so that the
	// layers still pending can be found without scanning.
	nextLayer []int32
	minLayer  int32
	minCount  int
}

// advance records that layer l of precinct k has been emitted and keeps
// the minimum bookkeeping up to date.  Recounting happens only after all
// precincts have passed a layer, so the cost is amortised constant.
func (res *Resolution) advance(k, l int) {
	nl := &res.nextLayer[k]
	if *nl == res.minLayer {
		res.minCount--
	}
	*nl = int32(l + 1)
	if res.minCount == 0 {
		res.minLayer = math.MaxInt32
		for _, v := range res.nextLayer {
			if v < res.minLayer {
				res.minLayer, res.minCount = v, 1
			} else if v == res.minLayer {
				res.minCount++
			}
		}
	}
}

// Band is one sub-band of a resolution level.
type Band struct {
	Orient   t1.Band
	Bounds   Rect // in band coordinates
	XCB, YCB int  // log2 code-block size within this band
	Epsilon  int  // quantization exponent
	Mu       int  // quantization mantissa (irreversible only)
	Planes   int  // Mb: number of magnitude bit-planes before any ROI shift
	Gain     int  // log2 of the sub-band gain: 0 for LL, 1 for HL/LH, 2 for HH
}

// Precinct is one precinct of a resolution level, holding the code-blocks
// of every band of the level that fall inside it.
type Precinct struct {
	Bands []*PrecinctBand
}

// PrecinctBand is the part of a precinct inside one sub-band.
type PrecinctBand struct {
	Band     *Band
	Bounds   Rect // in band coordinates, clipped to the band
	CBW, CBH int  // code-blocks across and down
	Blocks   []CodeBlock
	Incl     *Tree // inclusion tag tree
	ZeroBP   *Tree // zero bit-planes tag tree
}

// CodeBlock is the decoding state of one code-block.
type CodeBlock struct {
	Bounds     Rect // in band coordinates
	Included   bool // has appeared in a packet
	LBlock     int
	ZeroPlanes int
	Passes     int // coding passes received so far
	Segments   []Segment
}

// Segment is a codeword segment under construction: data pieces from
// one or more packets and the number of passes they hold.
type Segment struct {
	Chunks [][]byte
	Passes int
	Closed bool // terminated; later data starts a new segment
}

// Charge accounts for memory; nil means unlimited.
type Charge func(n int) error

func (c Charge) charge(n int) error {
	if c == nil {
		return nil
	}
	return c(n)
}

// NewTile builds the tile structure for tile index idx from the main
// header and the tile's tile-part headers.  Coding parameters come from
// the first tile-part; progression order changes may be spread over all
// of them.  Only cheap, fixed-size per-resolution information is created
// here; precincts and code-blocks are created lazily while packets are
// decoded, so that memory follows the data actually present.
func NewTile(m *codestream.MainHeader, tps []*codestream.TilePart, idx int, charge Charge) (*Tile, error) {
	tp := tps[0]
	siz := &m.SIZ
	nx, _, ok := siz.NumTiles()
	if !ok {
		return nil, errors.New("t2: invalid tile grid")
	}
	p, q := idx%nx, idx/nx
	t := &Tile{Index: idx}
	t.Bounds = Rect{
		X0: max(int(siz.XTOsiz)+p*int(siz.XTsiz), int(siz.XOsiz)),
		Y0: max(int(siz.YTOsiz)+q*int(siz.YTsiz), int(siz.YOsiz)),
		X1: min(int(siz.XTOsiz)+(p+1)*int(siz.XTsiz), int(siz.Xsiz)),
		Y1: min(int(siz.YTOsiz)+(q+1)*int(siz.YTsiz), int(siz.Ysiz)),
	}
	if t.Bounds.Empty() {
		return nil, errors.New("t2: empty tile")
	}

	cod := m.COD
	if tp.COD != nil {
		cod = tp.COD
	}
	t.Layers = cod.Layers
	t.SOP, t.EPH, t.MCT = cod.SOP, cod.EPH, cod.MCT
	t.Default = cod.Progression
	for _, part := range tps {
		t.POC = append(t.POC, part.POC...)
	}
	if len(t.POC) == 0 {
		t.POC = m.POC
	}

	roi := make(map[int]int)
	for _, r := range m.RGN {
		roi[r.Component] = r.Shift
	}
	for _, r := range tp.RGN {
		roi[r.Component] = r.Shift
	}

	if err := charge.charge(len(siz.Components) * 256); err != nil {
		return nil, err
	}
	t.Comps = make([]*TileComp, len(siz.Components))
	for c, cs := range siz.Components {
		style, quant, err := codestream.Resolve(m, tp, c)
		if err != nil {
			return nil, err
		}
		tc := &TileComp{
			Index:    c,
			Style:    style,
			Quant:    quant,
			ROIShift: roi[c],
			Depth:    cs.Depth,
			Signed:   cs.Signed,
			DX:       cs.XRsiz,
			DY:       cs.YRsiz,
		}
		tc.Bounds = Rect{
			X0: ceilDiv(t.Bounds.X0, cs.XRsiz),
			Y0: ceilDiv(t.Bounds.Y0, cs.YRsiz),
			X1: ceilDiv(t.Bounds.X1, cs.XRsiz),
			Y1: ceilDiv(t.Bounds.Y1, cs.YRsiz),
		}
		if err := tc.buildResolutions(charge); err != nil {
			return nil, err
		}
		t.Comps[c] = tc
	}
	return t, nil
}

func (tc *TileComp) buildResolutions(charge Charge) error {
	nl := tc.Style.Levels
	if err := charge.charge((nl + 1) * 512); err != nil {
		return err
	}
	tc.Res = make([]*Resolution, nl+1)
	for r := 0; r <= nl; r++ {
		res := &Resolution{Level: r}
		shift := nl - r
		res.Bounds = Rect{
			X0: ceilShift(tc.Bounds.X0, shift),
			Y0: ceilShift(tc.Bounds.Y0, shift),
			X1: ceilShift(tc.Bounds.X1, shift),
			Y1: ceilShift(tc.Bounds.Y1, shift),
		}
		ps := tc.Style.PrecinctSizes[r]
		res.PPx, res.PPy = ps.PPx, ps.PPy
		if r > 0 && (res.PPx < 1 || res.PPy < 1) {
			return errors.New("t2: precinct size too small")
		}
		if !res.Bounds.Empty() {
			res.PW = ceilShift(res.Bounds.X1, res.PPx) - res.Bounds.X0>>uint(res.PPx)
			res.PH = ceilShift(res.Bounds.Y1, res.PPy) - res.Bounds.Y0>>uint(res.PPy)
		}

		// code-block size within the bands of this level
		xcb, ycb := tc.Style.CodeBlockW, tc.Style.CodeBlockH
		if r == 0 {
			xcb, ycb = min(xcb, res.PPx), min(ycb, res.PPy)
		} else {
			xcb, ycb = min(xcb, res.PPx-1), min(ycb, res.PPy-1)
		}

		var orients []t1.Band
		if r == 0 {
			orients = []t1.Band{t1.BandLL}
		} else {
			orients = []t1.Band{t1.BandHL, t1.BandLH, t1.BandHH}
		}
		for _, o := range orients {
			b, err := tc.newBand(r, o, xcb, ycb)
			if err != nil {
				return err
			}
			res.Bands = append(res.Bands, b)
		}
		tc.Res[r] = res
	}
	return nil
}

// newBand computes the geometry and quantization parameters of one
// sub-band of resolution level r.
func (tc *TileComp) newBand(r int, o t1.Band, xcb, ycb int) (*Band, error) {
	nl := tc.Style.Levels
	b := &Band{Orient: o, XCB: xcb, YCB: ycb}

	// nb is the number of decomposition levels from the tile-component
	// down to this band; xob, yob select the band within its level.
	nb := nl - r + 1
	if r == 0 {
		nb = nl
	}
	xob, yob := 0, 0
	switch o {
	case t1.BandHL:
		xob, b.Gain = 1, 1
	case t1.BandLH:
		yob, b.Gain = 1, 1
	case t1.BandHH:
		xob, yob, b.Gain = 1, 1, 2
	}
	den := 1 << uint(nb)
	offX, offY := 0, 0
	if nb > 0 {
		offX, offY = xob<<uint(nb-1), yob<<uint(nb-1)
	}
	b.Bounds = Rect{
		X0: ceilDiv(max(tc.Bounds.X0-offX, 0), den),
		Y0: ceilDiv(max(tc.Bounds.Y0-offY, 0), den),
		X1: ceilDiv(max(tc.Bounds.X1-offX, 0), den),
		Y1: ceilDiv(max(tc.Bounds.Y1-offY, 0), den),
	}

	// index of the band's quantization parameters: LL first, then the
	// three bands of each resolution level in order HL, LH, HH
	var qi int
	if r > 0 {
		qi = 3*(r-1) + 1 + int(o-t1.BandHL)
	}
	q := &tc.Quant
	switch q.Style {
	case codestream.QuantDerived:
		if len(q.Exponents) < 1 {
			return nil, errors.New("t2: missing quantization parameters")
		}
		b.Epsilon = q.Exponents[0] - nl + nb
		b.Mu = q.Mantissas[0]
	default:
		// Codestreams in circulation declare more decomposition levels
		// than they provide quantization entries for.  The surplus
		// levels are the coarse ones, which are trivial for such small
		// images, so the entries present belong to the finest levels:
		// align them from the end.
		if len(q.Exponents) == 0 {
			return nil, errors.New("t2: missing quantization parameters")
		}
		if missing := 3*nl + 1 - len(q.Exponents); missing > 0 {
			qi = max(qi-missing, 0)
		}
		b.Epsilon = q.Exponents[qi]
		if q.Style == codestream.QuantExpounded {
			b.Mu = q.Mantissas[qi]
		}
	}
	b.Planes = q.GuardBits + b.Epsilon - 1
	if b.Planes < 0 {
		return nil, errors.New("t2: negative number of bit-planes")
	}
	return b, nil
}

// Precinct returns the precinct with index k of resolution res, creating
// it on first use.
func (tc *TileComp) Precinct(res *Resolution, k int, charge Charge) (*Precinct, error) {
	n, err := res.numPrecincts()
	if err != nil {
		return nil, err
	}
	if k < 0 || k >= n {
		return nil, errors.New("t2: precinct index out of range")
	}
	if res.Precincts == nil {
		if err := charge.charge(n * 8); err != nil {
			return nil, err
		}
		res.Precincts = make([]*Precinct, n)
	}
	if p := res.Precincts[k]; p != nil {
		return p, nil
	}
	px, py := k%res.PW, k/res.PW
	// precinct bounds in resolution-level coordinates
	pr := Rect{
		X0: (res.Bounds.X0>>uint(res.PPx) + px) << uint(res.PPx),
		Y0: (res.Bounds.Y0>>uint(res.PPy) + py) << uint(res.PPy),
	}
	pr.X1 = pr.X0 + 1<<uint(res.PPx)
	pr.Y1 = pr.Y0 + 1<<uint(res.PPy)

	p := &Precinct{}
	for _, b := range res.Bands {
		pb, err := newPrecinctBand(b, pr, res.Level, charge)
		if err != nil {
			return nil, err
		}
		p.Bands = append(p.Bands, pb)
	}
	if err := charge.charge(64); err != nil {
		return nil, err
	}
	res.Precincts[k] = p
	return p, nil
}

// newPrecinctBand creates the code-blocks of band b inside the precinct
// with bounds pr (given in resolution-level coordinates).
func newPrecinctBand(b *Band, pr Rect, level int, charge Charge) (*PrecinctBand, error) {
	// precinct bounds in band coordinates
	br := pr
	if level > 0 {
		br = Rect{pr.X0 >> 1, pr.Y0 >> 1, pr.X1 >> 1, pr.Y1 >> 1}
	}
	br = Rect{
		X0: max(br.X0, b.Bounds.X0),
		Y0: max(br.Y0, b.Bounds.Y0),
		X1: min(br.X1, b.Bounds.X1),
		Y1: min(br.Y1, b.Bounds.Y1),
	}
	pb := &PrecinctBand{Band: b, Bounds: br}
	if br.Empty() {
		pb.Incl = NewTree(0, 0)
		pb.ZeroBP = NewTree(0, 0)
		return pb, nil
	}
	cbx0 := br.X0 >> uint(b.XCB)
	cby0 := br.Y0 >> uint(b.YCB)
	pb.CBW = ceilShift(br.X1, b.XCB) - cbx0
	pb.CBH = ceilShift(br.Y1, b.YCB) - cby0
	n := pb.CBW * pb.CBH
	// code-block records plus two tag trees of roughly 4/3 n nodes each
	if err := charge.charge(n*(96+2*32*4/3) + 128); err != nil {
		return nil, err
	}
	pb.Blocks = make([]CodeBlock, n)
	for j := range pb.CBH {
		for i := range pb.CBW {
			cb := &pb.Blocks[j*pb.CBW+i]
			cb.Bounds = Rect{
				X0: max((cbx0+i)<<uint(b.XCB), br.X0),
				Y0: max((cby0+j)<<uint(b.YCB), br.Y0),
				X1: min((cbx0+i+1)<<uint(b.XCB), br.X1),
				Y1: min((cby0+j+1)<<uint(b.YCB), br.Y1),
			}
			cb.LBlock = 3
		}
	}
	pb.Incl = NewTree(pb.CBW, pb.CBH)
	pb.ZeroBP = NewTree(pb.CBW, pb.CBH)
	return pb, nil
}

// NumPackets returns the total number of packets of the tile.
func (t *Tile) NumPackets() int {
	n := 0
	for _, tc := range t.Comps {
		for _, res := range tc.Res {
			n += res.PW * res.PH * t.Layers
		}
	}
	return n
}

// TotalPlanes returns the number of magnitude bit-planes of code-blocks in
// band b of tile-component tc, including the ROI shift.
func (tc *TileComp) TotalPlanes(b *Band) int {
	return b.Planes + tc.ROIShift
}
