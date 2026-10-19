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
	"time"

	"seehuhn.de/go/jpx"
	"seehuhn.de/go/membudget"
)

// runDecode implements the decode subcommand.
func runDecode(args []string) error {
	fs := flag.NewFlagSet("decode", flag.ExitOnError)
	reduce := fs.Int("reduce", 0, "number of resolution levels to discard")
	mib := fs.Int("budget", 0, "memory budget in MiB (0 for no limit)")
	workers := fs.Int("workers", 1, "number of tiles decoded concurrently")
	asPNG := fs.Bool("png", false, "write a PNG file (default)")
	asPGX := fs.Bool("pgx", false, "write one PGX file per component")
	fs.Parse(args)
	if fs.NArg() != 2 {
		return errors.New("usage: jpx decode [flags] FILE OUTBASE")
	}
	if *asPNG && *asPGX {
		return errors.New("-png and -pgx are mutually exclusive")
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()

	var budget *membudget.Budget
	if *mib > 0 {
		budget = membudget.New(int64(*mib) << 20)
	}
	opt := &jpx.Options{Reduce: *reduce, Workers: *workers, Budget: budget}
	start := time.Now()
	img, err := jpx.Decode(f, opt)
	if err != nil {
		return err
	}
	fmt.Printf("decoded in %v\n", time.Since(start))
	for i, p := range img.Planes {
		fmt.Printf("plane %d: %d x %d\n", i, p.Width, p.Height)
	}

	base := fs.Arg(1)
	if *asPGX {
		for i, p := range img.Planes {
			name := fmt.Sprintf("%s_%d.pgx", base, i)
			out, err := os.Create(name)
			if err != nil {
				return err
			}
			err = writePGX(out, img.Components[i], &p)
			if err2 := out.Close(); err == nil {
				err = err2
			}
			if err != nil {
				return err
			}
		}
		return nil
	}

	pic, err := toPNGImage(img, budget)
	if err != nil {
		return err
	}
	out, err := os.Create(base + ".png")
	if err != nil {
		return err
	}
	err = png.Encode(out, pic)
	if err2 := out.Close(); err == nil {
		err = err2
	}
	return err
}

// toPNGImage converts the planes of img to a Go image.  Sub-sampled
// components are upsampled, and the samples are converted to unsigned
// 8- or 16-bit values.  The intermediate planes and the result are
// charged to the budget; a nil budget imposes no limit.
func toPNGImage(img *jpx.Image, budget *membudget.Budget) (image.Image, error) {
	n := len(img.Planes)
	if n < 1 || n > 4 {
		return nil, fmt.Errorf("cannot write %d components as PNG, use -pgx", n)
	}
	bits := 8
	for _, c := range img.Components {
		if c.Depth > 8 {
			bits = 16
		}
	}
	img, err := jpx.Upsample(img, budget)
	if err != nil {
		return nil, err
	}
	img, err = jpx.ToUnsigned(img, bits, budget)
	if err != nil {
		return nil, err
	}
	w, h := img.Planes[0].Width, img.Planes[0].Height
	// the Go image holds bits/8 bytes per sample, four samples per pixel
	// unless grey
	perPixel := bits / 8
	if n > 1 {
		perPixel *= 4
	}
	if budget != nil {
		if err := budget.ChargeN(w*h, perPixel); err != nil {
			return nil, err
		}
	}
	sample := func(c, i int) uint16 { return uint16(img.Planes[c].Pix[i]) }

	rect := image.Rect(0, 0, w, h)
	switch {
	case n == 1 && bits == 8:
		pic := image.NewGray(rect)
		for i := range pic.Pix {
			pic.Pix[i] = uint8(sample(0, i))
		}
		return pic, nil
	case n == 1:
		pic := image.NewGray16(rect)
		for i := range w * h {
			v := sample(0, i)
			pic.Pix[2*i], pic.Pix[2*i+1] = byte(v>>8), byte(v)
		}
		return pic, nil
	}

	var pic8 *image.NRGBA
	var pic16 *image.NRGBA64
	if bits == 16 {
		pic16 = image.NewNRGBA64(rect)
	} else {
		pic8 = image.NewNRGBA(rect)
	}
	for y := range h {
		for x := range w {
			i := y*w + x
			var c [4]uint16
			switch n {
			case 2:
				c = [4]uint16{sample(0, i), sample(0, i), sample(0, i), sample(1, i)}
			case 3:
				c = [4]uint16{sample(0, i), sample(1, i), sample(2, i), 0xffff >> uint(16-bits)}
			case 4:
				c = [4]uint16{sample(0, i), sample(1, i), sample(2, i), sample(3, i)}
			}
			if bits == 16 {
				pic16.SetNRGBA64(x, y, color.NRGBA64{c[0], c[1], c[2], c[3]})
			} else {
				pic8.SetNRGBA(x, y, color.NRGBA{uint8(c[0]), uint8(c[1]), uint8(c[2]), uint8(c[3])})
			}
		}
	}
	if bits == 16 {
		return pic16, nil
	}
	return pic8, nil
}
