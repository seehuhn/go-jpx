// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jpx

import (
	"errors"
	"math"

	"seehuhn.de/go/membudget"
)

// ApplyPalette resolves the palette of img: the result has one plane per
// channel, with palette lookups performed for the channels that use a
// palette column.  Channels that read a component directly share the
// component's plane.  The returned image has no palette; its Components
// describe the channels.  Images without a palette are returned
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
			dst := Plane{Width: src.Width, Height: src.Height, Pix: make([]int32, len(src.Pix))}
			for k, v := range src.Pix {
				idx := min(max(int(v), 0), len(pal.Entries)-1)
				dst.Pix[k] = pal.Entries[idx][ch.PaletteColumn]
			}
			out.Planes = append(out.Planes, dst)
			out.Components = append(out.Components, Component{
				Depth: col.Depth, Signed: col.Signed,
				DX: comp.DX, DY: comp.DY, Width: comp.Width, Height: comp.Height,
			})
		}
		out.Channels = append(out.Channels, Channel{Component: i, PaletteColumn: -1, Kind: ch.Kind, Assoc: ch.Assoc})
	}
	return out, nil
}

// SYCCToRGB converts the first three planes of img from the sYCC colour
// space (enumerated colour space 18) to RGB in place.  The planes must
// have the same size and depth; chroma planes of sub-sampled images must
// be upsampled by the caller first.  Signed components are not
// supported.
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
	if c[0].Depth != c[1].Depth || c[0].Depth != c[2].Depth || c[0].Signed || c[1].Signed || c[2].Signed {
		return errors.New("jpx: sYCC planes differ in depth or are signed")
	}
	maxVal := float64(uint64(1)<<uint(c[0].Depth) - 1)
	offset := math.Ldexp(1, c[0].Depth-1)
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
