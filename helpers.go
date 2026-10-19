// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jpx

import (
	"errors"
	"fmt"
	"math"

	"seehuhn.de/go/membudget"
)

// maxPlaneBits is the largest number of significant bits of the values
// the helpers accept, the capacity of an int32.
const maxPlaneBits = 31

// ApplyPalette resolves the palette of img: the result has one plane per
// channel, with palette lookups performed for the channels that use a
// palette column.  Channels that read a component directly share the
// component's plane.  The returned image has no palette; its Components
// describe the channels, with the Truncation rule of decoded components,
// so that values of columns deeper than MaxPrecision bits lose their
// lowest bits.  Images without a palette are returned
// unchanged.  The new planes are charged to the budget; a nil budget
// imposes no limit.
func ApplyPalette(img *Image, budget *membudget.Budget) (*Image, error) {
	if img.Palette == nil {
		return img, nil
	}
	charge := chargeFunc(budget)
	if err := chargeOpt(charge, len(img.Channels)*128); err != nil {
		return nil, err
	}
	pal := img.Palette
	out := &Image{Header: img.Header, Reduce: img.Reduce}
	out.Palette = nil
	out.Components = nil
	out.Channels = nil
	for i, ch := range img.Channels {
		if ch.Component < 0 || ch.Component >= len(img.Planes) || ch.Component >= len(img.Components) {
			return nil, errors.New("jpx: channel refers to a missing component")
		}
		src := img.Planes[ch.Component]
		comp := img.Components[ch.Component]
		if ch.PaletteColumn < 0 {
			out.Planes = append(out.Planes, src)
			out.Components = append(out.Components, comp)
		} else {
			if ch.PaletteColumn >= len(pal.Columns) {
				return nil, errors.New("jpx: channel refers to a missing palette column")
			}
			if len(pal.Entries) == 0 {
				return nil, errors.New("jpx: empty palette")
			}
			if err := chargeOpt(charge, 4*len(src.Pix)); err != nil {
				return nil, err
			}
			col := pal.Columns[ch.PaletteColumn]
			// palette values are held at up to 31 bits, plane values at
			// up to MaxPrecision bits
			trunc := depthTruncation(col.Depth)
			shift := uint(max(trunc-col.Truncation, 0))
			dst := Plane{Width: src.Width, Height: src.Height, Pix: make([]int32, len(src.Pix))}
			// the index is the sample value; of a truncated sample only
			// the retained bits are known
			for k, v := range src.Pix {
				idx := min(max(int64(v)<<comp.Truncation, 0), int64(len(pal.Entries)-1))
				dst.Pix[k] = pal.Entries[idx][ch.PaletteColumn] >> shift
			}
			out.Planes = append(out.Planes, dst)
			out.Components = append(out.Components, Component{
				Depth: col.Depth, Signed: col.Signed, Truncation: trunc,
				DX: comp.DX, DY: comp.DY, Width: comp.Width, Height: comp.Height,
			})
		}
		out.Channels = append(out.Channels, Channel{Component: i, PaletteColumn: -1, Kind: ch.Kind, Assoc: ch.Assoc})
	}
	return out, nil
}

// SYCCToRGB converts the first three planes of img from the sYCC colour
// space (enumerated colour space 18) to RGB in place.  The planes must
// have the same size, depth and truncation; chroma planes of sub-sampled
// images must be upsampled by the caller first.  Signed components are
// not supported.
func SYCCToRGB(img *Image) error {
	if len(img.Planes) < 3 || len(img.Components) < 3 {
		return errors.New("jpx: sYCC conversion needs three planes")
	}
	y, cb, cr := img.Planes[0], img.Planes[1], img.Planes[2]
	c := img.Components
	if y.Width != cb.Width || y.Height != cb.Height || y.Width != cr.Width || y.Height != cr.Height ||
		len(y.Pix) != y.Width*y.Height || len(cb.Pix) != len(y.Pix) || len(cr.Pix) != len(y.Pix) {
		return errors.New("jpx: sYCC planes differ in size")
	}
	if c[0].Depth != c[1].Depth || c[0].Depth != c[2].Depth || c[0].Signed || c[1].Signed || c[2].Signed ||
		c[0].Truncation != c[1].Truncation || c[0].Truncation != c[2].Truncation {
		return errors.New("jpx: sYCC planes differ in depth or are signed")
	}
	d, t := c[0].Depth, c[0].Truncation
	if t < 0 || t >= d || d-t > maxPlaneBits {
		return errors.New("jpx: invalid sYCC depth")
	}
	maxVal := float64(uint64(1)<<uint(d-t) - 1)
	offset := math.Ldexp(1, d-1-t)
	clamp := func(v float64) int32 {
		return int32(min(max(math.Round(v), 0), maxVal))
	}
	for i := range y.Pix {
		yy := float64(y.Pix[i])
		u := float64(cb.Pix[i]) - offset
		v := float64(cr.Pix[i]) - offset
		y.Pix[i] = clamp(yy + 1.402*v)
		cb.Pix[i] = clamp(yy - 0.344136*u - 0.714136*v)
		cr.Pix[i] = clamp(yy + 1.772*u)
	}
	return nil
}

// Upsample returns an image in which every component has the resolution
// of the reference grid: every plane has the size of the image area and
// every component has DX = DY = 1.  Grid point X takes the component
// sample at component coordinate floor(X/DX), the sample whose area on
// the grid contains X; grid points before the first or after the last
// sample of a component take that sample.  Rows are treated likewise.
//
// For an image decoded with Reduce = r > 0, the image area on the
// reduced grid is [ceil(XOffset/2^r), ceil((XOffset+Width)/2^r)), and a
// component keeps its sub-sampling factors and covers
// [ceil(ceil(XOffset/DX)/2^r), ceil(ceil((XOffset+Width)/DX)/2^r)) on
// that grid; the result's planes have the size of the reduced image
// area.
//
// A component without samples in the image area, which can happen when
// no grid point of a narrow image lies on the component's sample lattice
// or when Reduce removes the last sample, has no value to replicate; such
// an image yields an error.
//
// Planes that already have the target size are shared with img.  The
// image geometry, channels, colour specifications and palette are kept;
// the Width and Height of every component become those of the image.
// The new planes are charged to the budget; a nil budget imposes no
// limit.  The input image is not modified.
func Upsample(img *Image, budget *membudget.Budget) (*Image, error) {
	if len(img.Planes) != len(img.Components) {
		return nil, errors.New("jpx: number of planes does not match the components")
	}
	charge := chargeFunc(budget)
	r := img.Reduce
	gx0, gx1 := ceilShift(img.XOffset, r), ceilShift(img.XOffset+img.Width, r)
	gy0, gy1 := ceilShift(img.YOffset, r), ceilShift(img.YOffset+img.Height, r)
	w, h := gx1-gx0, gy1-gy0
	if w < 0 || h < 0 || (w > 0 && h > math.MaxInt/8/w) {
		return nil, errors.New("jpx: image too large")
	}

	out := &Image{Header: img.Header, Reduce: r, Planes: make([]Plane, len(img.Planes))}
	out.Components = make([]Component, len(img.Components))
	for c, comp := range img.Components {
		if comp.DX < 1 || comp.DY < 1 {
			return nil, fmt.Errorf("jpx: component %d has invalid sub-sampling", c)
		}
		cx0 := ceilShift(ceilDiv(img.XOffset, comp.DX), r)
		cx1 := ceilShift(ceilDiv(img.XOffset+img.Width, comp.DX), r)
		cy0 := ceilShift(ceilDiv(img.YOffset, comp.DY), r)
		cy1 := ceilShift(ceilDiv(img.YOffset+img.Height, comp.DY), r)
		src := img.Planes[c]
		if src.Width != cx1-cx0 || src.Height != cy1-cy0 || len(src.Pix) != src.Width*src.Height {
			return nil, fmt.Errorf("jpx: plane %d does not match the image geometry", c)
		}

		comp.DX, comp.DY = 1, 1
		comp.Width, comp.Height = img.Width, img.Height
		out.Components[c] = comp

		if src.Width == w && src.Height == h {
			out.Planes[c] = src
			continue
		}
		if src.Width == 0 || src.Height == 0 {
			return nil, fmt.Errorf("jpx: component %d has no samples", c)
		}
		if err := chargeOpt(charge, 4*w*h+8*w); err != nil {
			return nil, err
		}

		// source column of every output column
		dx, dy := img.Components[c].DX, img.Components[c].DY
		cols := make([]int, w)
		for x := range cols {
			cols[x] = min(max((gx0+x)/dx, cx0), cx1-1) - cx0
		}
		dst := Plane{Width: w, Height: h, Pix: make([]int32, w*h)}
		prev := -1
		for y := range h {
			sy := min(max((gy0+y)/dy, cy0), cy1-1) - cy0
			row := dst.Pix[y*w : (y+1)*w]
			if sy == prev {
				copy(row, dst.Pix[(y-1)*w:y*w])
				continue
			}
			prev = sy
			srow := src.Pix[sy*src.Width : (sy+1)*src.Width]
			for x, sx := range cols {
				row[x] = srow[sx]
			}
		}
		out.Planes[c] = dst
	}
	return out, nil
}

// ToUnsigned returns an image whose components are all unsigned with
// the given depth, which must be between 1 and MaxPrecision, and have no
// truncation.
//
// The samples of a component with depth d and truncation t are treated
// as having d-t bits, which must be between 1 and 31.  Signed samples
// are first offset by 2^(d-1-t), which maps [-2^(d-1-t), 2^(d-1-t)-1]
// onto [0, 2^(d-t)-1].  Values
// outside [0, 2^(d-t)-1] are clamped to this range.  The values are then
// rescaled to the new range: v is mapped to the integer nearest to
// v·(2^depth-1)/(2^(d-t)-1), so that 0 and 2^(d-t)-1 map to 0 and
// 2^depth-1.
//
// Planes that are already unsigned at the target depth are shared with
// img.  The new planes are charged to the budget; a nil budget imposes
// no limit.  The input image is not modified.
func ToUnsigned(img *Image, depth int, budget *membudget.Budget) (*Image, error) {
	if depth < 1 || depth > MaxPrecision {
		return nil, fmt.Errorf("jpx: invalid target depth %d", depth)
	}
	if len(img.Planes) != len(img.Components) {
		return nil, errors.New("jpx: number of planes does not match the components")
	}
	charge := chargeFunc(budget)
	out := &Image{Header: img.Header, Reduce: img.Reduce, Planes: make([]Plane, len(img.Planes))}
	out.Components = make([]Component, len(img.Components))
	mOut := int64(1)<<uint(depth) - 1
	for c, comp := range img.Components {
		// the effective depth of the plane
		d := comp.Depth - comp.Truncation
		if comp.Truncation < 0 || d < 1 || d > maxPlaneBits {
			return nil, fmt.Errorf("jpx: component %d has unsupported depth %d", c, comp.Depth)
		}
		src := img.Planes[c]
		signed := comp.Signed
		comp.Depth, comp.Signed, comp.Truncation = depth, false, 0
		out.Components[c] = comp
		if d == depth && !signed {
			out.Planes[c] = src
			continue
		}

		if err := chargeOpt(charge, 4*len(src.Pix)); err != nil {
			return nil, err
		}
		var shift int64
		if signed {
			shift = 1 << uint(d-1)
		}
		mIn := int64(1)<<uint(d) - 1
		// With d, depth ≤ 31, 2·v·mOut + mIn < 2^63, and no value lies
		// exactly half-way between two integers, since mIn is odd.
		dst := Plane{Width: src.Width, Height: src.Height, Pix: make([]int32, len(src.Pix))}
		for i, v := range src.Pix {
			u := min(max(int64(v)+shift, 0), mIn)
			if d == depth {
				dst.Pix[i] = int32(u)
			} else {
				dst.Pix[i] = int32((2*u*mOut + mIn) / (2 * mIn))
			}
		}
		out.Planes[c] = dst
	}
	return out, nil
}
