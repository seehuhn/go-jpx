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
	"sync"
	"time"

	"seehuhn.de/go/jpx"
)

// budget limits the memory available to the decoder.  It is safe for
// concurrent use, as decoding with several workers requires.
type budget struct {
	mu   sync.Mutex
	left int64
}

func (b *budget) Charge(n int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n < 0 || int64(n) > b.left {
		return jpx.ErrBudget
	}
	b.left -= int64(n)
	return nil
}

// runDecode implements the decode subcommand.
func runDecode(args []string) error {
	fs := flag.NewFlagSet("decode", flag.ExitOnError)
	reduce := fs.Int("reduce", 0, "number of resolution levels to discard")
	mib := fs.Int("budget", 0, "memory budget in MiB (0 for the default)")
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

	opt := &jpx.Options{Reduce: *reduce, Workers: *workers}
	if *mib > 0 {
		opt.Budget = &budget{left: int64(*mib) << 20}
	}
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

	pic, err := toPNGImage(img)
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

// toPNGImage converts the planes of img to a Go image.  Samples are
// shifted to 8 or 16 bits; signed samples are offset to be unsigned.
func toPNGImage(img *jpx.Image) (image.Image, error) {
	n := len(img.Planes)
	if n < 1 || n > 4 {
		return nil, fmt.Errorf("cannot write %d components as PNG, use -pgx", n)
	}
	w, h := img.Planes[0].Width, img.Planes[0].Height
	wide := false
	for i, c := range img.Components {
		if c.Depth > 8 {
			wide = true
		}
		if img.Planes[i].Width != w || img.Planes[i].Height != h {
			return nil, errors.New("components differ in size, use -pgx")
		}
	}
	bits := 8
	if wide {
		bits = 16
	}
	// sample returns component c at index i scaled to bits bits, so
	// that the full range maps onto the full range
	sample := func(c, i int) uint16 {
		comp := img.Components[c]
		v := int64(img.Planes[c].Pix[i])
		if comp.Signed {
			v += 1 << uint(comp.Depth-1)
		}
		maxIn := int64(1)<<uint(comp.Depth) - 1
		maxOut := int64(1)<<uint(bits) - 1
		return uint16((v*maxOut + maxIn/2) / maxIn)
	}

	rect := image.Rect(0, 0, w, h)
	switch {
	case n == 1 && !wide:
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

	pic8 := image.NewNRGBA(rect)
	pic16 := image.NewNRGBA64(rect)
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
			if wide {
				pic16.SetNRGBA64(x, y, color.NRGBA64{c[0], c[1], c[2], c[3]})
			} else {
				pic8.SetNRGBA(x, y, color.NRGBA{uint8(c[0]), uint8(c[1]), uint8(c[2]), uint8(c[3])})
			}
		}
	}
	if wide {
		return pic16, nil
	}
	return pic8, nil
}
