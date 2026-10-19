// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"

	"seehuhn.de/go/jpx"
)

// writePGX writes one component plane in PGX format.  Truncated
// components are written at the depth of their plane.
func writePGX(w io.Writer, c jpx.Component, p *jpx.Plane) error {
	sign := "+"
	if c.Signed {
		sign = "-"
	}
	depth := c.Depth - c.Truncation
	bw := bufio.NewWriter(w)
	fmt.Fprintf(bw, "PG ML %s %d %d %d\n", sign, depth, p.Width, p.Height)
	nb := (depth + 7) / 8
	for _, v := range p.Pix {
		for i := nb - 1; i >= 0; i-- {
			bw.WriteByte(byte(uint32(v) >> (8 * uint(i))))
		}
	}
	return bw.Flush()
}

// readPGX reads one PGX file as a component with its plane.
func readPGX(r io.Reader) (jpx.Component, jpx.Plane, error) {
	var c jpx.Component
	var p jpx.Plane
	br := bufio.NewReader(r)
	line, err := br.ReadString('\n')
	if err != nil {
		return c, p, errors.New("invalid PGX header")
	}
	f := strings.Fields(line)
	if len(f) != 6 || f[0] != "PG" || f[1] != "ML" || (f[2] != "+" && f[2] != "-") {
		return c, p, errors.New("unsupported PGX header")
	}
	var w, h int
	if _, err := fmt.Sscan(f[3]+" "+f[4]+" "+f[5], &c.Depth, &w, &h); err != nil {
		return c, p, errors.New("invalid PGX header")
	}
	if c.Depth < 1 || c.Depth > jpx.MaxPrecision || w < 1 || h < 1 || h > math.MaxInt32/w {
		return c, p, errors.New("invalid PGX header")
	}
	c.Signed = f[2] == "-"
	c.DX, c.DY = 1, 1
	c.Width, c.Height = w, h

	// samples are read incrementally so that a bogus header cannot
	// cause a huge allocation
	nb := (c.Depth + 7) / 8
	buf := make([]byte, nb)
	for range w * h {
		if _, err := io.ReadFull(br, buf); err != nil {
			return c, p, errors.New("truncated PGX data")
		}
		var u uint32
		for _, b := range buf {
			u = u<<8 | uint32(b)
		}
		v := int32(u)
		if c.Signed && u&(1<<uint(c.Depth-1)) != 0 {
			v = int32(u | ^uint32(0)<<uint(c.Depth))
		}
		p.Pix = append(p.Pix, v)
	}
	p.Width, p.Height = w, h
	return c, p, nil
}

// readPGXImage reads the PGX file name.  If the name ends in _0.pgx, the
// siblings _1.pgx, _2.pgx, ... are read as further components.
func readPGXImage(name string) (*jpx.Image, error) {
	names := []string{name}
	if base, ok := strings.CutSuffix(name, "_0.pgx"); ok {
		for i := 1; ; i++ {
			n := fmt.Sprintf("%s_%d.pgx", base, i)
			if _, err := os.Stat(n); err != nil {
				break
			}
			names = append(names, n)
		}
	}
	img := &jpx.Image{}
	for _, n := range names {
		f, err := os.Open(n)
		if err != nil {
			return nil, err
		}
		c, p, err := readPGX(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		img.Components = append(img.Components, c)
		img.Planes = append(img.Planes, p)
	}
	img.Width = img.Planes[0].Width
	img.Height = img.Planes[0].Height
	for i, p := range img.Planes {
		if p.Width != img.Width || p.Height != img.Height {
			return nil, fmt.Errorf("%s: size differs from first component", names[i])
		}
	}
	return img, nil
}
