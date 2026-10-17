// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"strconv"
	"strings"

	"seehuhn.de/go/jpx"
)

// runEncode implements the encode subcommand.
func runEncode(args []string) error {
	fs := flag.NewFlagSet("encode", flag.ExitOnError)
	var p jpx.EncodeParams
	fs.BoolVar(&p.Lossy, "lossy", false, "use the irreversible 9/7 wavelet")
	fs.Float64Var(&p.Quality, "quality", 0, "quality for lossy coding, 1 to 100")
	fs.IntVar(&p.Layers, "layers", 0, "number of quality layers")
	rates := fs.String("rates", "", "comma-separated cumulative layer rates in bits per sample")
	fs.IntVar(&p.Levels, "levels", 0, "number of wavelet levels")
	tile := fs.String("tile", "", "tile size WxH")
	cb := fs.String("cb", "", "code-block size WxH")
	prog := fs.String("prog", "LRCP", "progression order")
	fs.BoolVar(&p.SOP, "sop", false, "write SOP markers")
	fs.BoolVar(&p.EPH, "eph", false, "write EPH markers")
	fs.BoolVar(&p.Bypass, "bypass", false, "selective arithmetic coding bypass")
	fs.BoolVar(&p.ResetContexts, "reset", false, "reset contexts after each pass")
	fs.BoolVar(&p.Terminate, "termall", false, "terminate every pass")
	fs.BoolVar(&p.VerticallyCausal, "causal", false, "vertically causal contexts")
	fs.BoolVar(&p.SegmentationSymbols, "segsym", false, "segmentation symbols")
	fs.BoolVar(&p.NoMCT, "nomct", false, "disable the component transform")
	roi := fs.String("roi", "", "region of interest x0,y0,x1,y1")
	fs.BoolVar(&p.Wrapper, "jp2", false, "write a JP2 file")
	fs.Parse(args)
	if fs.NArg() != 2 {
		return errors.New("usage: jpx encode [flags] IN OUT")
	}

	var err error
	if *rates != "" {
		for s := range strings.SplitSeq(*rates, ",") {
			r, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return fmt.Errorf("invalid rate %q", s)
			}
			p.Rates = append(p.Rates, r)
		}
	}
	if *tile != "" {
		if p.TileWidth, p.TileHeight, err = parseSize(*tile); err != nil {
			return err
		}
	}
	if *cb != "" {
		if p.CodeBlockWidth, p.CodeBlockHeight, err = parseSize(*cb); err != nil {
			return err
		}
	}
	orders := map[string]jpx.Progression{
		"LRCP": jpx.LRCP, "RLCP": jpx.RLCP, "RPCL": jpx.RPCL,
		"PCRL": jpx.PCRL, "CPRL": jpx.CPRL,
	}
	o, ok := orders[strings.ToUpper(*prog)]
	if !ok {
		return fmt.Errorf("unknown progression order %q", *prog)
	}
	p.Progression = o
	if *roi != "" {
		f := strings.Split(*roi, ",")
		var v [4]int
		if len(f) != 4 {
			return errors.New("invalid -roi, want x0,y0,x1,y1")
		}
		for i := range v {
			if v[i], err = strconv.Atoi(f[i]); err != nil {
				return errors.New("invalid -roi, want x0,y0,x1,y1")
			}
		}
		p.ROI = &jpx.Rectangle{X0: v[0], Y0: v[1], X1: v[2], Y1: v[3]}
	}

	var img *jpx.Image
	if strings.HasSuffix(strings.ToLower(fs.Arg(0)), ".pgx") {
		img, err = readPGXImage(fs.Arg(0))
	} else {
		img, err = readPNGImage(fs.Arg(0))
	}
	if err != nil {
		return err
	}

	out, err := os.Create(fs.Arg(1))
	if err != nil {
		return err
	}
	err = jpx.Encode(out, img, &p)
	if err2 := out.Close(); err == nil {
		err = err2
	}
	if err != nil {
		return err
	}
	st, err := os.Stat(fs.Arg(1))
	if err != nil {
		return err
	}
	fmt.Printf("wrote %d bytes\n", st.Size())
	return nil
}

func parseSize(s string) (w, h int, err error) {
	a, b, ok := strings.Cut(strings.ToLower(s), "x")
	if ok {
		w, err = strconv.Atoi(a)
		if err == nil {
			h, err = strconv.Atoi(b)
		}
	}
	if !ok || err != nil || w < 0 || h < 0 {
		return 0, 0, fmt.Errorf("invalid size %q, want WxH", s)
	}
	return w, h, nil
}

func readPNGImage(name string) (*jpx.Image, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	pic, err := png.Decode(f)
	if err != nil {
		return nil, err
	}
	return fromPNGImage(pic), nil
}

// fromPNGImage converts a Go image to planes: gray stays gray, everything
// else becomes RGB, with an alpha component if the image is not opaque.
func fromPNGImage(pic image.Image) *jpx.Image {
	b := pic.Bounds()
	w, h := b.Dx(), b.Dy()
	depth := 8
	gray := false
	switch pic.(type) {
	case *image.Gray:
		gray = true
	case *image.Gray16:
		gray, depth = true, 16
	case *image.NRGBA64, *image.RGBA64:
		depth = 16
	}
	alpha := false
	if o, ok := pic.(interface{ Opaque() bool }); ok && !gray {
		alpha = !o.Opaque()
	}

	n := 3
	if gray {
		n = 1
	}
	if alpha {
		n++
	}
	img := &jpx.Image{}
	img.Width, img.Height = w, h
	for range n {
		img.Components = append(img.Components,
			jpx.Component{Depth: depth, DX: 1, DY: 1, Width: w, Height: h})
		img.Planes = append(img.Planes, jpx.Plane{Width: w, Height: h, Pix: make([]int32, w*h)})
	}
	shift := uint(16 - depth)
	for y := range h {
		for x := range w {
			i := y*w + x
			if gray {
				v := color.Gray16Model.Convert(pic.At(b.Min.X+x, b.Min.Y+y)).(color.Gray16)
				img.Planes[0].Pix[i] = int32(v.Y >> shift)
				continue
			}
			c := color.NRGBA64Model.Convert(pic.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA64)
			img.Planes[0].Pix[i] = int32(c.R >> shift)
			img.Planes[1].Pix[i] = int32(c.G >> shift)
			img.Planes[2].Pix[i] = int32(c.B >> shift)
			if alpha {
				img.Planes[3].Pix[i] = int32(c.A >> shift)
			}
		}
	}
	if alpha {
		img.Channels = []jpx.Channel{
			{Component: 0, PaletteColumn: -1, Kind: jpx.ChannelColor, Assoc: 1},
			{Component: 1, PaletteColumn: -1, Kind: jpx.ChannelColor, Assoc: 2},
			{Component: 2, PaletteColumn: -1, Kind: jpx.ChannelColor, Assoc: 3},
			{Component: 3, PaletteColumn: -1, Kind: jpx.ChannelOpacity, Assoc: 0},
		}
	}
	return img
}
