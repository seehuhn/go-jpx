// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jpx

import (
	"errors"
	"iter"
	"math"
	"slices"
	"sync"
	"sync/atomic"

	"seehuhn.de/go/jpx/internal/codestream"
	"seehuhn.de/go/jpx/internal/dwt"
	"seehuhn.de/go/jpx/internal/t1"
	"seehuhn.de/go/jpx/internal/t2"
)

// tileDecoder decodes the tiles of one codestream into an image.  Its
// buffers are reused from tile to tile, so the memory charged for them
// is bounded by the largest tile rather than by the tile count.
type tileDecoder struct {
	m      *codestream.MainHeader
	img    *Image
	charge func(int) error
	// budgetErr records the first error returned by the budget.  Errors
	// from corrupt data are tolerated, budget errors are not; recording
	// them here keeps the distinction independent of how the internal
	// packages wrap errors.
	budgetErr error
	budgetMu  sync.Mutex
	ppm       [][]byte // packed packet headers per tile-part, if PPM is used

	// workers is the number of goroutines decoding the code-blocks of a
	// tile-component; one means sequential decoding.  The scratch state
	// of every goroutine is kept across tiles.
	workers int
	pool    []*blockWorker
	blocks  []blockRef
	pieces  []t2.Piece

	// one buffer per component, sized for the largest tile-component
	ibuf [][]int32
	fbuf [][]float32
}

// blockWorker holds the scratch state for decoding one code-block at a
// time.
type blockWorker struct {
	t1     *t1.Decoder
	coefs  []int32 // decoded code-block, fractional-bit representation
	segBuf []byte
	segs   []t1.Segment
}

func newTileDecoder(m *codestream.MainHeader, img *Image, charge func(int) error, workers int) *tileDecoder {
	return &tileDecoder{
		m:       m,
		img:     img,
		charge:  charge,
		workers: max(workers, 1),
		pool:    []*blockWorker{{t1: t1.NewDecoder()}},
		ibuf:    make([][]int32, len(m.SIZ.Components)),
		fbuf:    make([][]float32, len(m.SIZ.Components)),
	}
}

func (d *tileDecoder) chargeBytes(n int) error {
	if d.charge == nil {
		return nil
	}
	err := d.charge(n)
	if err != nil {
		d.budgetMu.Lock()
		if d.budgetErr == nil {
			d.budgetErr = err
		}
		d.budgetMu.Unlock()
	}
	return err
}

// failed returns the recorded budget error, if any.
func (d *tileDecoder) failed() error {
	d.budgetMu.Lock()
	defer d.budgetMu.Unlock()
	return d.budgetErr
}

// decodeTile decodes one tile from its tile-parts and stores the samples
// in the output planes.
func (d *tileDecoder) decodeTile(idx int, tps []tilePart) error {
	headers := make([]*codestream.TilePart, len(tps))
	for i, tp := range tps {
		headers[i] = tp.TilePart
	}
	tile, err := t2.NewTile(d.m, headers, idx, t2.Charge(d.chargeBytes))
	if err != nil {
		return err
	}
	for _, tc := range tile.Comps {
		if tc.Style.Levels < d.img.Reduce {
			return errors.New("jpx: tile has fewer resolution levels than requested")
		}
	}

	if err := d.readPackets(tile, tps); err != nil {
		return err
	}
	for c, tc := range tile.Comps {
		if err := d.reconstruct(c, tc); err != nil {
			return err
		}
	}
	// a component transform flag on an image with fewer than three
	// components is meaningless and ignored, as other decoders do
	if tile.MCT && len(tile.Comps) >= 3 {
		if err := d.inverseMCT(tile); err != nil {
			return err
		}
	}
	for c, tc := range tile.Comps {
		d.store(c, tc)
	}
	return nil
}

// readPackets parses all packets of the tile and attaches their data to
// the code-blocks.  Truncated or corrupt data ends the parsing; what has
// been read so far is kept.
func (d *tileDecoder) readPackets(tile *t2.Tile, tps []tilePart) error {
	seq, err := tile.Packets(t2.Charge(d.chargeBytes))
	if err != nil {
		return err
	}
	next, stop := iter.Pull(seq)
	defer stop()

	// Each tile-part is a pair of header stream and body stream.  With
	// in-line headers both are the tile-part body; with PPM the headers
	// are the tile-part's chunk from the main header; with PPT the
	// headers of all tile-parts form one stream, and since the number of
	// packets per tile-part is then not recoverable from the data, the
	// bodies are joined into one stream as well.
	type part struct{ hdr, body *t2.Stream }
	var parts []part
	switch {
	case d.m.UsesPPM():
		if err := d.loadPPM(); err != nil {
			return err
		}
		for _, tp := range tps {
			if tp.seq >= len(d.ppm) {
				break
			}
			parts = append(parts, part{&t2.Stream{Data: d.ppm[tp.seq]}, &t2.Stream{Data: tp.Data}})
		}
	case usesPPT(tps):
		if err := d.chargeBytes(24 * len(tps)); err != nil {
			return err
		}
		hs := make([][]byte, 0, len(tps))
		nHdr, nBody := 0, 0
		for _, tp := range tps {
			h, err := tp.PackedHeaders(d.chargeBytes)
			if d.budgetErr != nil {
				return d.budgetErr
			}
			if err != nil {
				break // damaged PPT segments end the usable headers
			}
			hs = append(hs, h)
			nHdr += len(h)
			nBody += len(tp.Data)
		}
		if err := d.chargeBytes(nHdr + nBody); err != nil {
			return err
		}
		hdrs := make([]byte, 0, nHdr)
		bodies := make([]byte, 0, nBody)
		for i, h := range hs {
			hdrs = append(hdrs, h...)
			bodies = append(bodies, tps[i].Data...)
		}
		parts = []part{{&t2.Stream{Data: hdrs}, &t2.Stream{Data: bodies}}}
	default:
		for _, tp := range tps {
			body := &t2.Stream{Data: tp.Data}
			parts = append(parts, part{body, body})
		}
	}

	for _, p := range parts {
		for p.hdr.Remaining() > 0 && (p.hdr != p.body || p.body.Remaining() > 0) {
			pk, ok := next()
			if !ok {
				return nil
			}
			err := tile.ReadPacket(pk, p.hdr, p.body, &d.pieces, t2.Charge(d.chargeBytes))
			if d.budgetErr != nil {
				return d.budgetErr
			}
			if err != nil {
				// truncated or corrupt packet data: keep what we have
				return nil
			}
		}
	}
	return nil
}

// loadPPM parses the packed packet headers of the main header, once.  A
// truncated PPM list still yields the complete chunks.
func (d *tileDecoder) loadPPM() error {
	if d.ppm != nil || !d.m.UsesPPM() {
		return nil
	}
	chunks, err := d.m.PackedHeaders(d.chargeBytes)
	if err != nil && (d.budgetErr != nil || chunks == nil) {
		return err
	}
	d.ppm = chunks
	return nil
}

// usesPPT reports whether any tile-part carries packed packet headers.
func usesPPT(tps []tilePart) bool {
	for _, tp := range tps {
		if len(tp.PPT) > 0 {
			return true
		}
	}
	return false
}

// reconstruct decodes the code-blocks of tile-component c, dequantises
// them into the component buffer and applies the inverse wavelet
// transform.
func (d *tileDecoder) reconstruct(c int, tc *t2.TileComp) error {
	levels := tc.Style.Levels - d.img.Reduce
	r := dwt.Rect{
		X0: ceilShift(tc.Bounds.X0, d.img.Reduce),
		Y0: ceilShift(tc.Bounds.Y0, d.img.Reduce),
		X1: ceilShift(tc.Bounds.X1, d.img.Reduce),
		Y1: ceilShift(tc.Bounds.Y1, d.img.Reduce),
	}
	w, h := r.X1-r.X0, r.Y1-r.Y0
	n := w * h
	rev := tc.Style.Reversible
	if n == 0 {
		// leave no stale samples from an earlier tile behind
		d.ibuf[c], d.fbuf[c] = d.ibuf[c][:0], d.fbuf[c][:0]
		return nil
	}
	if rev {
		if cap(d.ibuf[c]) < n {
			if err := d.chargeBytes(4 * n); err != nil {
				return err
			}
			d.ibuf[c] = make([]int32, n)
		}
		d.ibuf[c] = d.ibuf[c][:n]
		clear(d.ibuf[c])
	} else {
		if cap(d.fbuf[c]) < n {
			if err := d.chargeBytes(4 * n); err != nil {
				return err
			}
			d.fbuf[c] = make([]float32, n)
		}
		d.fbuf[c] = d.fbuf[c][:n]
		clear(d.fbuf[c])
	}

	style := t1.Style{
		Bypass:      tc.Style.EntropyBypass,
		Reset:       tc.Style.ResetContexts,
		Terminate:   tc.Style.Terminate,
		Causal:      tc.Style.VerticallyCausal,
		Predictable: tc.Style.PredictableTermination,
		SegSymbols:  tc.Style.SegmentationSymbols,
	}
	// collect the blocks with data; every block covers its own part of
	// the buffer, so they can be decoded in any order or concurrently
	blocks := d.blocks[:0]
	for _, res := range tc.Res[:levels+1] {
		for _, p := range res.Precincts {
			if p == nil {
				continue
			}
			for _, pb := range p.Bands {
				for i := range pb.Blocks {
					cb := &pb.Blocks[i]
					if cb.Passes == 0 || cb.DataLen() == 0 {
						// Encoders include blocks with zero bytes of data
						// when rate allocation leaves nothing for them;
						// decoding the padding instead would produce noise.
						continue
					}
					if len(blocks) == cap(blocks) {
						// grow in charged steps; the list is reused for every tile
						if err := d.chargeBytes(24 * max(cap(blocks), 64)); err != nil {
							return err
						}
						blocks = slices.Grow(blocks, max(cap(blocks), 64))
					}
					blocks = append(blocks, blockRef{res, pb.Band, cb})
				}
			}
		}
	}
	d.blocks = blocks
	decode := func(w *blockWorker, b blockRef) error {
		err := d.decodeBlock(w, c, tc, b.res, b.band, b.cb, style, r)
		if err != nil && d.failed() != nil {
			return err
		}
		return nil // corrupt block data: leave the block empty
	}
	if d.workers > 1 && len(blocks) > 1 {
		if err := d.decodeBlocksConcurrently(blocks, decode); err != nil {
			return err
		}
	} else {
		for _, b := range blocks {
			if err := decode(d.pool[0], b); err != nil {
				return err
			}
		}
	}

	// the transform keeps a scratch line of the longer side
	if err := d.chargeBytes(4 * (w + h + 8)); err != nil {
		return err
	}
	if rev {
		dwt.Inverse53(d.ibuf[c], r, levels)
	} else {
		dwt.Inverse97(d.fbuf[c], r, levels)
	}
	return nil
}

// blockRef identifies a code-block to decode.
type blockRef struct {
	res  *t2.Resolution
	band *t2.Band
	cb   *t2.CodeBlock
}

// decodeBlocksConcurrently decodes the blocks on up to d.workers
// goroutines, each with its own scratch state from the pool, and returns
// the first error.
func (d *tileDecoder) decodeBlocksConcurrently(blocks []blockRef, decode func(*blockWorker, blockRef) error) error {
	n := min(d.workers, len(blocks))
	for len(d.pool) < n {
		// a decoder's working state is bounded by the largest block
		if err := d.chargeBytes(blockWorkerSize); err != nil {
			return err
		}
		d.pool = append(d.pool, &blockWorker{t1: t1.NewDecoder()})
	}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		next     atomic.Int64
	)
	for _, w := range d.pool[:n] {
		wg.Go(func() {
			for {
				i := int(next.Add(1) - 1)
				if i >= len(blocks) {
					return
				}
				mu.Lock()
				stop := firstErr != nil
				mu.Unlock()
				if stop {
					return
				}
				if err := decode(w, blocks[i]); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
			}
		})
	}
	wg.Wait()
	return firstErr
}

// blockWorkerSize is the memory charged for the fixed-size scratch state
// of one block worker: the tier-1 decoder's state arrays and the
// coefficient buffer.
const blockWorkerSize = 8 * t1.MaxBlockArea

// decodeBlock decodes one code-block with the scratch state of w and
// places its dequantised coefficients into the component buffer covering
// rect r.
func (d *tileDecoder) decodeBlock(w *blockWorker, c int, tc *t2.TileComp, res *t2.Resolution, band *t2.Band, cb *t2.CodeBlock, style t1.Style, r dwt.Rect) error {
	bw, bh := cb.Bounds.Dx(), cb.Bounds.Dy()
	if cap(w.coefs) < bw*bh {
		if err := d.chargeBytes(4 * t1.MaxBlockArea); err != nil {
			return err
		}
		w.coefs = make([]int32, t1.MaxBlockArea)
	}
	coefs := w.coefs[:bw*bh]
	planes := tc.TotalPlanes(band) - cb.ZeroPlanes
	// tier-1 decodes at most MaxPlanes planes and drops the lowest ones
	coded := min(planes, t1.MaxPlanes)
	dropped := planes - coded
	if n := cb.DataLen(); n > cap(w.segBuf) {
		if err := d.chargeBytes(n); err != nil {
			return err
		}
		w.segBuf = make([]byte, 0, n)
	}
	w.segBuf, w.segs = cb.Data(w.segBuf, w.segs)

	// Tier-1 work is bounded by the declared passes, not by the data
	// present, so it is charged to the budget like memory.
	if err := d.chargeBytes(min(cb.Passes, t1.MaxPasses(coded)) * bw * bh); err != nil {
		return err
	}
	if err := w.t1.Decode(coefs, bw, bh, band.Orient, style, planes, w.segs); err != nil {
		return err
	}

	// Within the nested layout the region of this resolution level sits
	// at the top left of the buffer; the high-pass bands follow the
	// low-pass samples of the level.
	lx0, lx1, _, _ := dwt.Split(res.Bounds.X0, res.Bounds.X1)
	ly0, ly1, _, _ := dwt.Split(res.Bounds.Y0, res.Bounds.Y1)
	offX, offY := 0, 0
	switch band.Orient {
	case t1.BandHL:
		offX = lx1 - lx0
	case t1.BandLH:
		offY = ly1 - ly0
	case t1.BandHH:
		offX, offY = lx1-lx0, ly1-ly0
	}
	bx := offX + cb.Bounds.X0 - band.Bounds.X0
	by := offY + cb.Bounds.Y0 - band.Bounds.Y0
	stride := r.X1 - r.X0

	// Coefficients are brought to the scale of the samples divided by
	// 2^trunc, so that all bands of the tile-component agree before the
	// inverse transform.
	sc := newCoefScale(tc.ROIShift, dropped)
	trunc := depthTruncation(tc.Depth)
	if tc.Style.Reversible {
		// the fractional bit is shifted out as well
		roiExp, bgExp := sc.roiExp-trunc-1, sc.bgExp-trunc-1
		buf := d.ibuf[c]
		for y := range bh {
			row := buf[(by+y)*stride+bx:][:bw]
			for x := range row {
				v := coefs[y*bw+x]
				mag, e := int64(v), bgExp
				if v < 0 {
					mag = -mag
				}
				if mag >= sc.roiMin {
					mag, e = mag>>sc.roiDown, roiExp
				}
				r := shiftSat(mag, e)
				if v < 0 {
					r = -r
				}
				row[x] = r
			}
		}
	} else {
		buf := d.fbuf[c]
		rb := tc.Depth + band.Gain
		delta := math.Ldexp(1+float64(band.Mu)/2048, rb-band.Epsilon)
		roiScale := float32(math.Ldexp(delta/2, sc.roiExp-trunc))
		bgScale := float32(math.Ldexp(delta/2, sc.bgExp-trunc))
		for y := range bh {
			row := buf[(by+y)*stride+bx:][:bw]
			for x := range row {
				v := coefs[y*bw+x]
				if v == 0 {
					// the scale may overflow to infinity for hostile
					// parameters, and 0·∞ is not a number
					row[x] = 0
					continue
				}
				mag, scale := int64(v), bgScale
				if v < 0 {
					mag = -mag
				}
				if mag >= sc.roiMin {
					mag, scale = mag>>sc.roiDown, roiScale
				}
				r := float32(mag) * scale
				if v < 0 {
					r = -r
				}
				row[x] = r
			}
		}
	}
	return nil
}

// coefScale maps the values decoded by tier-1 for a code-block to
// coefficients, both in the fractional-bit representation.  A decoded
// value v with |v| ≥ roiMin belongs to the region of interest and stands
// for the coefficient magnitude (|v|>>roiDown)·2^roiExp; any other value
// stands for |v|·2^bgExp.
type coefScale struct {
	roiMin  int64
	roiDown int
	roiExp  int
	bgExp   int
}

// newCoefScale returns the mapping for a code-block of a component with
// region-of-interest shift s (zero for none), from whose magnitudes
// tier-1 dropped the lowest dropped bit-planes.
//
// Under the Maxshift method, a coefficient c of the region is coded as
// c·2^s and every background coefficient is below 2^s.  A decoded value
// v stands for v·2^dropped, so the region is told apart by
// v·2^dropped ≥ 2·2^s (the factor two is the fractional bit).  When
// s+1 ≤ dropped, every background coefficient was lost with the dropped
// planes, and any non-zero value belongs to the region.  Background
// values keep their scale and lose their lowest dropped bits, as in a
// stream truncated after the decoded passes.
func newCoefScale(s, dropped int) coefScale {
	sc := coefScale{roiMin: math.MaxInt64, bgExp: dropped}
	switch {
	case s == 0:
		// no region of interest
	case s >= dropped:
		// decoded magnitudes are below 2^31, so that larger thresholds
		// are never reached
		sc.roiMin = 1 << min(s-dropped+1, 32)
		sc.roiDown = s - dropped
	default:
		sc.roiMin = 1
		sc.roiExp = dropped - s
	}
	return sc
}

// shiftSat returns m·2^e for a magnitude m ≥ 0, rounded towards zero and
// saturated at the largest int32.
func shiftSat(m int64, e int) int32 {
	switch {
	case m == 0:
		return 0
	case e >= 0:
		if e >= 31 || m > math.MaxInt32>>e {
			return math.MaxInt32
		}
		return int32(m << e)
	case e <= -63:
		return 0
	default:
		return int32(min(m>>-e, math.MaxInt32))
	}
}

// inverseMCT undoes the multiple component transform on the first three
// components.
func (d *tileDecoder) inverseMCT(tile *t2.Tile) error {
	if len(tile.Comps) < 3 {
		return errors.New("jpx: component transform needs three components")
	}
	c0, c1, c2 := tile.Comps[0], tile.Comps[1], tile.Comps[2]
	if c0.Bounds != c1.Bounds || c0.Bounds != c2.Bounds {
		return errors.New("jpx: component transform on components of different size")
	}
	rev := c0.Style.Reversible
	if c1.Style.Reversible != rev || c2.Style.Reversible != rev {
		return errors.New("jpx: component transform on mixed transform types")
	}
	// The planes hold the samples divided by 2^trunc of their component.
	// Components of equal truncation are transformed as they are; the
	// reversible transform is then exact.  Otherwise the values are
	// brought to the scale of the full samples first, and the results
	// divided by their own component's factor again.
	ta, tb, tc := depthTruncation(c0.Depth), depthTruncation(c1.Depth), depthTruncation(c2.Depth)
	same := ta == tb && ta == tc
	if rev {
		a, b, c := d.ibuf[0], d.ibuf[1], d.ibuf[2]
		if len(a) != len(b) || len(a) != len(c) {
			return errors.New("jpx: component transform on buffers of different size")
		}
		if same {
			for i := range a {
				y, u, v := a[i], b[i], c[i]
				g := y - (u+v)>>2
				r := v + g
				bb := u + g
				a[i], b[i], c[i] = r, g, bb
			}
		} else {
			for i := range a {
				y, u, v := int64(a[i])<<ta, int64(b[i])<<tb, int64(c[i])<<tc
				g := y - (u+v)>>2
				r := v + g
				bb := u + g
				a[i], b[i], c[i] = sat32(r>>ta), sat32(g>>tb), sat32(bb>>tc)
			}
		}
	} else {
		a, b, c := d.fbuf[0], d.fbuf[1], d.fbuf[2]
		if len(a) != len(b) || len(a) != len(c) {
			return errors.New("jpx: component transform on buffers of different size")
		}
		sa, sb, sc := float32(1), float32(1), float32(1)
		if !same {
			sa, sb, sc = float32(math.Ldexp(1, ta)), float32(math.Ldexp(1, tb)), float32(math.Ldexp(1, tc))
		}
		for i := range a {
			y, cb, cr := a[i]*sa, b[i]*sb, c[i]*sc
			a[i] = (y + 1.402*cr) / sa
			b[i] = (y - 0.34413*cb - 0.71414*cr) / sb
			c[i] = (y + 1.772*cb) / sc
		}
	}
	return nil
}

// sat32 returns v saturated to the range of int32.
func sat32(v int64) int32 {
	return int32(min(max(v, math.MinInt32), math.MaxInt32))
}

// store applies the DC level shift and clamping and copies the
// tile-component into the output plane.  Samples of components deeper
// than MaxPrecision bits are stored divided by 2^depthTruncation(depth).
func (d *tileDecoder) store(c int, tc *t2.TileComp) {
	red := d.img.Reduce
	tx0, ty0 := ceilShift(tc.Bounds.X0, red), ceilShift(tc.Bounds.Y0, red)
	tx1, ty1 := ceilShift(tc.Bounds.X1, red), ceilShift(tc.Bounds.Y1, red)
	w, h := tx1-tx0, ty1-ty0
	if w <= 0 || h <= 0 {
		return
	}
	siz := &d.m.SIZ
	px0 := ceilShift(ceilDiv(int(siz.XOsiz), tc.DX), red)
	py0 := ceilShift(ceilDiv(int(siz.YOsiz), tc.DY), red)
	plane := &d.img.Planes[c]

	// the sample range and DC offset, divided by 2^trunc like the samples
	trunc := depthTruncation(tc.Depth)
	var lo, hi, offset int64
	if tc.Signed {
		lo, hi = -(1<<(tc.Depth-1))>>trunc, (1<<(tc.Depth-1)-1)>>trunc
	} else {
		lo, hi = 0, (1<<tc.Depth-1)>>trunc
		offset = (1 << (tc.Depth - 1)) >> trunc
	}
	for y := range h {
		dst := plane.Pix[(ty0-py0+y)*plane.Width+tx0-px0:][:w]
		if tc.Style.Reversible {
			src := d.ibuf[c][y*w:][:w]
			for x, v := range src {
				dst[x] = int32(min(max(int64(v)+offset, lo), hi))
			}
		} else {
			src := d.fbuf[c][y*w:][:w]
			for x, v := range src {
				r := math.Floor(float64(v)+0.5) + float64(offset)
				if r != r { // NaN from overflowing hostile coefficients
					r = float64(offset)
				}
				dst[x] = int32(min(max(r, float64(lo)), float64(hi)))
			}
		}
	}
}
