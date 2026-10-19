// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jpx

import (
	"errors"
	"math"

	"seehuhn.de/go/jpx/internal/codestream"
	"seehuhn.de/go/jpx/internal/dwt"
	"seehuhn.de/go/jpx/internal/t1"
	"seehuhn.de/go/jpx/internal/t2"
)

// tileData is the encoder state of one tile.
type tileData struct {
	tile *t2.Tile
	// per component: per resolution: per precinct: per band
	enc [][][][]*t2.Encoded
	// zero mirrors enc with the zero bit-plane count of every block
	zero [][][][][]int
}

// encodeTile encodes tile idx and returns its tile-parts.  The first
// tile-part carries the coding parameters fitted to the tile's
// coefficients where they differ from the main header.
func (e *encoder) encodeTile(idx int) ([]tilePartData, error) {
	tp := &codestream.TilePart{Index: idx, PartCount: 1}
	tile, err := t2.NewTile(e.main, []*codestream.TilePart{tp}, idx, t2.Charge(e.charge))
	if err != nil {
		return nil, err
	}
	// forward component transform needs all components first
	bufs, err := e.transformComponents(tile)
	if err != nil {
		return nil, err
	}
	for c, tc := range tile.Comps {
		forwardTransform(tc, bufs[c])
	}
	if err := e.fitTile(tp, tile, bufs); err != nil {
		return nil, err
	}
	// the tile with the fitted quantization
	tile, err = t2.NewTile(e.main, []*codestream.TilePart{tp}, idx, t2.Charge(e.charge))
	if err != nil {
		return nil, err
	}
	td := &tileData{tile: tile}
	td.enc = make([][][][]*t2.Encoded, len(tile.Comps))
	td.zero = make([][][][][]int, len(tile.Comps))

	for c, tc := range tile.Comps {
		if err := e.encodeComponent(td, c, tc, bufs[c]); err != nil {
			return nil, err
		}
	}
	if err := e.assignLayers(td); err != nil {
		return nil, err
	}

	// prepare the tag trees from the layer assignment
	for c, tc := range tile.Comps {
		for r, res := range tc.Res {
			for k, prec := range res.Precincts {
				for bi, pb := range prec.Bands {
					zero := td.zero[c][r][k][bi]
					pb.InitEncoder(td.enc[c][r][k][bi], func(i int) int { return zero[i] }, e.layers)
				}
			}
		}
	}

	seq, err := tile.Packets(t2.Charge(e.charge))
	if err != nil {
		return nil, err
	}
	// write every packet, then distribute the packets over the tile-parts
	type packet struct{ hdr, body []byte }
	var packets []packet
	for pk := range seq {
		c := pk.Comp.Index
		prec := pk.Res.Precincts[pk.Precinct]
		hdr, body := tile.WritePacket(nil, nil, pk.Comp, prec, td.enc[c][pk.Res.Level][pk.Precinct], pk.Layer)
		if err := chargeOpt(e.charge, len(hdr)+len(body)+48); err != nil {
			return nil, err
		}
		packets = append(packets, packet{hdr, body})
	}
	if len(tile.POC) > 0 && len(packets) != tile.NumPackets() {
		return nil, errors.New("jpx: progression order changes do not cover all packets")
	}
	numParts := max(e.p.TileParts, 1)
	if len(packets) == 0 {
		numParts = 1
	}
	numParts = min(numParts, max(len(packets), 1))
	parts := make([]tilePartData, numParts)
	parts[0].header = tp
	inline := e.p.PackedHeaders == HeadersInline
	for i, pk := range packets {
		part := &parts[i*numParts/len(packets)]
		if tile.SOP {
			part.body = append(part.body, t2.SOPMarker(i)...)
		}
		if inline {
			part.body = append(part.body, pk.hdr...)
		} else {
			part.headers = append(part.headers, pk.hdr...)
			part.headerLens = append(part.headerLens, len(pk.hdr))
		}
		part.body = append(part.body, pk.body...)
	}
	return parts, nil
}

// sampleBuffer holds the samples of one tile-component, as integers for
// the reversible path and as floats for the irreversible one.
type sampleBuffer struct {
	i []int32
	f []float32
}

// transformComponents extracts the tile's samples, applies the DC level
// shift and the component transform, and returns one buffer per
// component.
func (e *encoder) transformComponents(tile *t2.Tile) ([]sampleBuffer, error) {
	rev := !e.p.Lossy
	bufs := make([]sampleBuffer, len(tile.Comps))
	siz := &e.main.SIZ
	for c, tc := range tile.Comps {
		w, h := tc.Bounds.Dx(), tc.Bounds.Dy()
		n := w * h
		if err := chargeOpt(e.charge, 4*n); err != nil {
			return nil, err
		}
		plane := &e.img.Planes[c]
		px0 := ceilDiv(int(siz.XOsiz), tc.DX)
		py0 := ceilDiv(int(siz.YOsiz), tc.DY)
		// the DC offset at the scale of the plane
		var shift int32
		if !tc.Signed {
			shift = int32(int64(1) << uint(tc.Depth-1) >> uint(depthTruncation(tc.Depth)))
		}
		if rev {
			bufs[c].i = make([]int32, n)
			for y := range h {
				src := plane.Pix[(tc.Bounds.Y0-py0+y)*plane.Width+tc.Bounds.X0-px0:][:w]
				dst := bufs[c].i[y*w:][:w]
				for x, v := range src {
					dst[x] = v - shift
				}
			}
		} else {
			bufs[c].f = make([]float32, n)
			for y := range h {
				src := plane.Pix[(tc.Bounds.Y0-py0+y)*plane.Width+tc.Bounds.X0-px0:][:w]
				dst := bufs[c].f[y*w:][:w]
				for x, v := range src {
					dst[x] = float32(v - shift)
				}
			}
		}
	}
	if tile.MCT {
		if rev {
			a, b, c := bufs[0].i, bufs[1].i, bufs[2].i
			for i := range a {
				r, g, bb := a[i], b[i], c[i]
				a[i] = (r + 2*g + bb) >> 2
				b[i] = bb - g
				c[i] = r - g
			}
		} else {
			a, b, c := bufs[0].f, bufs[1].f, bufs[2].f
			for i := range a {
				r, g, bb := a[i], b[i], c[i]
				a[i] = 0.299*r + 0.587*g + 0.114*bb
				b[i] = -0.16875*r - 0.331260*g + 0.5*bb
				c[i] = 0.5*r - 0.418688*g - 0.081312*bb
			}
		}
	}
	return bufs, nil
}

func chargeOpt(charge func(int) error, n int) error {
	if charge == nil {
		return nil
	}
	return charge(n)
}

// encodeComponent quantises and entropy codes the transformed
// coefficients of one tile-component.
func (e *encoder) encodeComponent(td *tileData, c int, tc *t2.TileComp, buf sampleBuffer) error {
	levels := tc.Style.Levels
	rev := tc.Style.Reversible
	stride := tc.Bounds.Dx()

	style := t1.Style{
		Bypass:      tc.Style.EntropyBypass,
		Reset:       tc.Style.ResetContexts,
		Terminate:   tc.Style.Terminate,
		Causal:      tc.Style.VerticallyCausal,
		Predictable: tc.Style.PredictableTermination,
		SegSymbols:  tc.Style.SegmentationSymbols,
	}
	td.enc[c] = make([][][]*t2.Encoded, levels+1)
	td.zero[c] = make([][][][]int, levels+1)
	coefs := make([]int32, t1.MaxBlockArea)

	for ri, res := range tc.Res {
		np := res.PW * res.PH
		td.enc[c][ri] = make([][]*t2.Encoded, np)
		td.zero[c][ri] = make([][][]int, np)
		for k := range np {
			prec, err := tc.Precinct(res, k, t2.Charge(e.charge))
			if err != nil {
				return err
			}
			for _, pb := range prec.Bands {
				enc := &t2.Encoded{Blocks: make([]t2.BlockData, len(pb.Blocks))}
				zero := make([]int, len(pb.Blocks))
				band := pb.Band
				bx, by := bandOffset(res, band)
				// The coefficients of a truncated component are those of
				// the plane values, trunc planes below the scale of the
				// full samples that the band's planes refer to.  They are
				// shifted up and coded with all planes where the coder
				// allows, so that any decoder recovers them exactly; when
				// the band has more planes than the coder handles, the
				// lowest ones are left uncoded instead, like a stream
				// truncated after the coded passes.  Quantization keeps
				// the shift within the coder's limit.
				planes := tc.TotalPlanes(band)
				dropped := max(0, planes-t1.MaxPlanes)
				planes -= dropped
				shift := 0
				if rev {
					shift = depthTruncation(tc.Depth) - dropped
				}
				for i := range pb.Blocks {
					cb := &pb.Blocks[i]
					w, h := cb.Bounds.Dx(), cb.Bounds.Dy()
					block := coefs[:w*h]
					x0 := bx + cb.Bounds.X0 - band.Bounds.X0
					y0 := by + cb.Bounds.Y0 - band.Bounds.Y0
					if rev {
						for y := range h {
							row := buf.i[(y0+y)*stride+x0:][:w]
							for x, v := range row {
								block[y*w+x] = v << shift
							}
						}
					} else {
						inv := quantScale(tc, band)
						for y := range h {
							row := buf.f[(y0+y)*stride+x0:][:w]
							for x, v := range row {
								block[y*w+x] = quantise(v, inv)
							}
						}
					}
					if tc.ROIShift > 0 {
						e.applyROI(block, cb, band, tc, res)
					}
					eb, err := e.t1.Encode(block, w, h, band.Orient, style, planes)
					if err != nil {
						return err
					}
					if err := chargeOpt(e.charge, len(eb.Data)+32*len(eb.Passes)); err != nil {
						return err
					}
					bd := &enc.Blocks[i]
					bd.Data = append([]byte(nil), eb.Data...)
					bd.Passes = append([]t1.PassInfo(nil), eb.Passes...)
					zero[i] = eb.ZeroPlanes
					// weight the distortion by the quantization step and the
					// synthesis gain so that blocks of different bands and
					// components compare at the scale of the plane values
					weight := e.norms[levelOf(res, levels)][int(band.Orient)]
					if rev {
						weight = math.Ldexp(weight, -2*shift)
					} else {
						d := math.Ldexp(quantStep(tc, band), -depthTruncation(tc.Depth))
						weight *= d * d
					}
					for pi := range bd.Passes {
						bd.Passes[pi].Dist *= weight
					}
				}
				td.enc[c][ri][k] = append(td.enc[c][ri][k], enc)
				td.zero[c][ri][k] = append(td.zero[c][ri][k], zero)
			}
		}
	}
	return nil
}

// forwardTransform applies the forward wavelet transform to the samples
// of tile-component tc in place.
func forwardTransform(tc *t2.TileComp, buf sampleBuffer) {
	r := dwt.Rect{X0: tc.Bounds.X0, Y0: tc.Bounds.Y0, X1: tc.Bounds.X1, Y1: tc.Bounds.Y1}
	if tc.Style.Reversible {
		dwt.Forward53(buf.i, r, tc.Style.Levels)
	} else {
		dwt.Forward97(buf.f, r, tc.Style.Levels)
	}
}

// fitTile sets the quantization of every component of the tile so that
// each sub-band has the magnitude bit-planes its coefficients need, and
// derives the region of interest shifts from the resulting numbers of
// planes.  Parameters which differ from the main header go into the
// tile-part header tp.  bufs holds the transformed coefficients of the
// tile; for the irreversible path the planes are those of the quantised
// coefficients.
func (e *encoder) fitTile(tp *codestream.TilePart, tile *t2.Tile, bufs []sampleBuffer) error {
	qs := make([]*codestream.Quantization, len(tile.Comps))
	differ := false
	for c, tc := range tile.Comps {
		mags := bandMagnitudes(tc, bufs[c])
		q := tc.Quant
		q.Exponents = append([]int(nil), q.Exponents...)
		fitQuantization(&q, mags, depthTruncation(tc.Depth))
		qs[c] = &q
		differ = differ || !quantEqual(&q, &tc.Quant)
	}
	tp.QCD, tp.QCC = nil, nil
	if differ {
		tp.QCD, tp.QCC = splitQuantization(qs)
	}

	tp.RGN = nil
	if e.p.ROI != nil {
		for c, q := range qs {
			// the shift must exceed the magnitude bits of any background
			// coefficient; the number of planes of the deepest band is a
			// safe choice
			shift := q.GuardBits - 1
			for _, eps := range q.Exponents {
				shift = max(shift, q.GuardBits+eps-1)
			}
			if 2*shift > t1.MaxPlanes {
				return errors.New("jpx: region of interest not possible at this bit depth")
			}
			tp.RGN = append(tp.RGN, codestream.RGN{Component: c, Shift: shift})
		}
	}
	return nil
}

var errTooManyPlanes = errors.New("jpx: coefficients need more bit-planes than a codestream can signal")

// bandMagnitudes returns, per quantization entry, the bitwise or of the
// magnitudes of the coefficients of tile-component tc to be coded, whose
// bit length is the number of magnitude bit-planes the band needs.  For
// the irreversible path these are the quantised coefficients.
func bandMagnitudes(tc *t2.TileComp, buf sampleBuffer) []uint32 {
	mags := make([]uint32, 3*tc.Style.Levels+1)
	stride := tc.Bounds.Dx()
	for _, res := range tc.Res {
		for _, band := range res.Bands {
			bx, by := bandOffset(res, band)
			w, h := band.Bounds.Dx(), band.Bounds.Dy()
			var acc uint32
			if tc.Style.Reversible {
				for y := range h {
					for _, v := range buf.i[(by+y)*stride+bx:][:w] {
						acc |= magnitude(v)
					}
				}
			} else {
				inv := quantScale(tc, band)
				for y := range h {
					for _, v := range buf.f[(by+y)*stride+bx:][:w] {
						acc |= magnitude(quantise(v, inv))
					}
				}
			}
			mags[quantIndex(res.Level, band.Orient)] |= acc
		}
	}
	return mags
}

// quantIndex returns the index of the quantization entry of the band
// with orientation o at resolution level r: LL first, then the three
// bands of each resolution level in order HL, LH, HH.
func quantIndex(r int, o t1.Band) int {
	if r == 0 {
		return 0
	}
	return 3*(r-1) + 1 + int(o-t1.BandHL)
}

func magnitude(v int32) uint32 {
	if v < 0 {
		return uint32(-v)
	}
	return uint32(v)
}

// quantScale returns the factor which maps the irreversible coefficients
// of band, at the scale of the plane values, to multiples of the
// quantization step at the scale of the full samples.
func quantScale(tc *t2.TileComp, band *t2.Band) float32 {
	delta := math.Ldexp(quantStep(tc, band), -depthTruncation(tc.Depth))
	return float32(1 / delta)
}

// quantise returns the quantization index of the coefficient v for the
// scale inv returned by quantScale.
func quantise(v, inv float32) int32 {
	return int32(math.Trunc(float64(v * inv)))
}

// levelOf returns the decomposition level (1 = finest) of resolution res;
// the lowest resolution is at level levels.
func levelOf(res *t2.Resolution, levels int) int {
	if res.Level == 0 {
		return levels
	}
	return levels - res.Level + 1
}

// bandOffset returns the position of band within the nested coefficient
// layout of its resolution level.
func bandOffset(res *t2.Resolution, band *t2.Band) (bx, by int) {
	lx0, lx1, _, _ := dwt.Split(res.Bounds.X0, res.Bounds.X1)
	ly0, ly1, _, _ := dwt.Split(res.Bounds.Y0, res.Bounds.Y1)
	switch band.Orient {
	case t1.BandHL:
		bx = lx1 - lx0
	case t1.BandLH:
		by = ly1 - ly0
	case t1.BandHH:
		bx, by = lx1-lx0, ly1-ly0
	}
	return bx, by
}

// quantStep returns the quantization step of band in tile-component tc.
func quantStep(tc *t2.TileComp, band *t2.Band) float64 {
	rb := tc.Depth + band.Gain
	return math.Ldexp(1+float64(band.Mu)/2048, rb-band.Epsilon)
}

// applyROI scales the coefficients of the region of interest up by the
// component's shift.  The region is mapped into the band by scaling the
// reference-grid rectangle to the band's resolution.
func (e *encoder) applyROI(block []int32, cb *t2.CodeBlock, band *t2.Band, tc *t2.TileComp, res *t2.Resolution) {
	roi := e.p.ROI
	nb := tc.Style.Levels - res.Level + 1
	if res.Level == 0 {
		nb = tc.Style.Levels
	}
	// region in band coordinates, with one coefficient of margin for the
	// filter support
	den := tc.DX << uint(nb)
	rx0 := roi.X0/den - 1
	rx1 := ceilDiv(roi.X1, den) + 1
	den = tc.DY << uint(nb)
	ry0 := roi.Y0/den - 1
	ry1 := ceilDiv(roi.Y1, den) + 1
	w := cb.Bounds.Dx()
	for y := cb.Bounds.Y0; y < cb.Bounds.Y1; y++ {
		if y < ry0 || y >= ry1 {
			continue
		}
		for x := cb.Bounds.X0; x < cb.Bounds.X1; x++ {
			if x < rx0 || x >= rx1 {
				continue
			}
			i := (y-cb.Bounds.Y0)*w + x - cb.Bounds.X0
			block[i] <<= uint(tc.ROIShift)
		}
	}
}
