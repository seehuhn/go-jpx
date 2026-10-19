// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"runtime"
	"strings"
	"testing"

	"seehuhn.de/go/jpx"
	"seehuhn.de/go/membudget"
)

// PGX headers whose dimensions overflow must be rejected rather than
// yield a plane with no samples.
func TestPGXOverflowRejected(t *testing.T) {
	for _, hdr := range []string{
		"PG ML + 8 4294967296 4294967296\n",
		"PG ML + 8 3037000500 3037000500\n",
		"PG ML + 8 9223372036854775807 2\n",
		"PG ML + 8 2 9223372036854775807\n",
	} {
		if _, _, err := readPGX(strings.NewReader(hdr)); err == nil {
			t.Errorf("%q accepted", strings.TrimSpace(hdr))
		}
	}
}

// Converting to PNG allocates one output image, not one per depth.
func TestPNGConversionAllocation(t *testing.T) {
	const w, h = 512, 512
	img := &jpx.Image{Width: w, Height: h}
	for range 3 {
		img.Components = append(img.Components, jpx.Component{Depth: 8, DX: 1, DY: 1, Width: w, Height: h})
		img.Planes = append(img.Planes, jpx.Plane{Width: w, Height: h, Pix: make([]int32, w*h)})
	}
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	if _, err := toPNGImage(img, nil); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&m1)
	if got := m1.TotalAlloc - m0.TotalAlloc; got > 6*w*h {
		t.Errorf("conversion allocated %d bytes for a %d-byte 8-bit image", got, 4*w*h)
	}
}

// Upsampling a sub-sampled component to a huge reference grid is held by
// the budget, so that a small file cannot force a large allocation.
func TestPNGConversionBudget(t *testing.T) {
	const w, h = 1 << 16, 1 << 16
	const sub = 255
	cw, ch := (w+sub-1)/sub, (h+sub-1)/sub
	img := &jpx.Image{Width: w, Height: h}
	img.Components = []jpx.Component{{Depth: 8, DX: sub, DY: sub, Width: cw, Height: ch}}
	img.Planes = []jpx.Plane{{Width: cw, Height: ch, Pix: make([]int32, cw*ch)}}
	if _, err := toPNGImage(img, membudget.New(1<<20)); err == nil {
		t.Error("no error for a huge upsampled image")
	}
}
