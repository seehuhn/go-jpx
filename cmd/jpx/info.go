// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"seehuhn.de/go/jpx"
)

var channelKinds = map[jpx.ChannelKind]string{
	jpx.ChannelColor:                "color",
	jpx.ChannelOpacity:              "opacity",
	jpx.ChannelPremultipliedOpacity: "premultiplied opacity",
	jpx.ChannelUnknown:              "unknown",
}

// runInfo implements the info subcommand.
func runInfo(args []string) error {
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: jpx info FILE")
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	h, err := jpx.ReadHeader(f, nil)
	if err != nil {
		return err
	}
	printHeader(h)
	return nil
}

func printHeader(h *jpx.Header) {
	kind := "bare codestream"
	if h.Wrapped {
		kind = "JP2 wrapper"
	}
	fmt.Printf("size:        %d x %d (%s)\n", h.Width, h.Height, kind)
	fmt.Printf("tiles:       %d\n", h.Tiles)
	fmt.Printf("resolutions: %d\n", h.Resolutions)
	for i, c := range h.Components {
		sign := "unsigned"
		if c.Signed {
			sign = "signed"
		}
		fmt.Printf("component %d: %d bit %s, sub-sampling %dx%d, size %d x %d\n",
			i, c.Depth, sign, c.DX, c.DY, c.Width, c.Height)
	}
	for i, ch := range h.Channels {
		fmt.Printf("channel %d:   component %d, palette column %d, %s, assoc %d\n",
			i, ch.Component, ch.PaletteColumn, channelKinds[ch.Kind], ch.Assoc)
	}
	for i, cs := range h.ColorSpecs {
		fmt.Printf("colour spec %d: method %d, precedence %d, approx %d",
			i, cs.Method, cs.Precedence, cs.Approx)
		switch cs.Method {
		case 1:
			fmt.Printf(", enumerated %d", cs.Enumerated)
		case 2, 3:
			fmt.Printf(", ICC profile of %d bytes", len(cs.ICC))
		case 4:
			fmt.Printf(", vendor data of %d bytes", len(cs.Vendor))
		}
		fmt.Println()
	}
	if p := h.Palette; p != nil {
		fmt.Printf("palette:     %d entries x %d columns, depths", len(p.Entries), len(p.Columns))
		for _, c := range p.Columns {
			fmt.Printf(" %d", c.Depth)
		}
		fmt.Println()
	}
}
