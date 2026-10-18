// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jpx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"sync"
	"sync/atomic"

	"seehuhn.de/go/jpx/internal/codestream"
	"seehuhn.de/go/jpx/internal/jp2"
)

// readChunk is the unit in which input is read, so that the budget is
// charged before memory is used.
const readChunk = 64 << 10

// readAll reads r to the end, charging the budget as the data arrives.
// Readers that know their remaining length get a single exact
// allocation; others are read in growing steps.
func readAll(r io.Reader, charge func(int) error) ([]byte, error) {
	var buf []byte
	if lr, ok := r.(interface{ Len() int }); ok {
		n := lr.Len() + 1 // room for the final read to report EOF
		if err := chargeOpt(charge, n); err != nil {
			return nil, err
		}
		buf = make([]byte, 0, n)
	}
	for {
		if len(buf) == cap(buf) {
			grow := max(readChunk, cap(buf))
			if err := chargeOpt(charge, grow); err != nil {
				return nil, err
			}
			nb := make([]byte, len(buf), cap(buf)+grow)
			copy(nb, buf)
			buf = nb
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// Decode reads a JPEG 2000 image from r.  Truncated or partially corrupt
// codestreams produce the image decoded so far; an error is returned
// only when the data cannot be interpreted at all or when a limit is
// exceeded.  Without a budget in opt, memory use and decoding work are
// not limited; untrusted input should be decoded with a budget.
func Decode(r io.Reader, opt *Options) (*Image, error) {
	if opt == nil {
		opt = &Options{}
	}
	if opt.Reduce < 0 {
		return nil, errors.New("jpx: negative reduction")
	}

	charge := chargeFunc(opt.Budget)
	data, err := readAll(r, charge)
	if err != nil {
		return nil, err
	}

	br := bytes.NewReader(data)
	wrapper, csr, err := jp2.Read(br, charge)
	if err != nil {
		return nil, err
	}
	cs, err := codestreamBytes(data, br, csr, charge)
	if err != nil {
		return nil, err
	}

	cr := codestream.NewReader(cs, charge)
	m, err := cr.ReadMainHeader()
	if err != nil {
		return nil, err
	}
	if err := checkSupported(m); err != nil {
		return nil, err
	}
	h, err := buildHeader(wrapper, m, charge)
	if err != nil {
		return nil, err
	}
	if opt.Reduce >= h.Resolutions {
		return nil, fmt.Errorf("jpx: cannot discard %d of %d resolution levels", opt.Reduce, h.Resolutions)
	}

	img := &Image{Header: *h, Reduce: opt.Reduce}
	if err := img.allocPlanes(m, charge); err != nil {
		return nil, err
	}

	// Collect the tile-parts of every tile.  Tile-parts of one tile may
	// be interleaved with those of other tiles; they must be decoded in
	// order and together.
	parts, err := collectTileParts(cr, h.Tiles, charge)
	if err != nil {
		return nil, err
	}

	if err := decodeTiles(m, img, charge, parts, opt.Workers); err != nil {
		return nil, err
	}
	return img, nil
}

// decodeTiles decodes all tiles, concurrently when more than one worker
// is requested.  With at least as many tiles as workers, the workers
// decode whole tiles, each with its own buffers and writing to its own
// region of the shared output planes; otherwise the tiles are decoded
// one after the other and the workers share the code-blocks of each.
func decodeTiles(m *codestream.MainHeader, img *Image, charge func(int) error, parts map[int][]tilePart, workers int) error {
	indices := slices.Sorted(maps.Keys(parts))
	workers = max(1, workers)
	if workers > len(indices) {
		dec := newTileDecoder(m, img, charge, workers)
		for _, idx := range indices {
			if err := dec.decodeTile(idx, parts[idx]); err != nil {
				return err
			}
		}
		return nil
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		next     atomic.Int64
	)
	// the packed packet headers of the main header are parsed once and
	// shared read-only by all workers
	shared := newTileDecoder(m, img, charge, 1)
	if err := shared.loadPPM(); err != nil {
		return err
	}
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dec := newTileDecoder(m, img, charge, 1)
			dec.ppm = shared.ppm
			for {
				i := int(next.Add(1) - 1)
				if i >= len(indices) {
					return
				}
				mu.Lock()
				failed := firstErr != nil
				mu.Unlock()
				if failed {
					return
				}
				if err := dec.decodeTile(indices[i], parts[indices[i]]); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// codestreamBytes returns the codestream found by the box reader as a
// slice of the input where possible, so that the input is not copied a
// second time.  Fragmented codestreams are assembled into a new buffer.
func codestreamBytes(data []byte, br *bytes.Reader, csr io.Reader, charge func(int) error) ([]byte, error) {
	switch r := csr.(type) {
	case *bytes.Reader:
		if r == br {
			// the rest of the input: a bare codestream (rewound to the
			// start) or a codestream box running to the end of the file
			return data[len(data)-br.Len():], nil
		}
	case *io.LimitedReader:
		if r.R == br {
			start := len(data) - br.Len()
			end := min(len(data), start+int(min(r.N, int64(len(data)))))
			return data[start:end], nil
		}
	}
	return readAll(csr, charge)
}

// allocPlanes creates the output planes at the requested reduction.
func (img *Image) allocPlanes(m *codestream.MainHeader, charge func(int) error) error {
	siz := &m.SIZ
	img.Planes = make([]Plane, len(siz.Components))
	for c, cs := range siz.Components {
		x0 := ceilShift(ceilDiv(int(siz.XOsiz), cs.XRsiz), img.Reduce)
		x1 := ceilShift(ceilDiv(int(siz.Xsiz), cs.XRsiz), img.Reduce)
		y0 := ceilShift(ceilDiv(int(siz.YOsiz), cs.YRsiz), img.Reduce)
		y1 := ceilShift(ceilDiv(int(siz.Ysiz), cs.YRsiz), img.Reduce)
		w, h := x1-x0, y1-y0
		if w < 0 || h < 0 || (w > 0 && h > math.MaxInt/8/w) {
			return errors.New("jpx: image too large")
		}
		n := w * h
		if charge != nil {
			if err := charge(n * 4); err != nil {
				return err
			}
		}
		img.Planes[c] = Plane{Width: w, Height: h, Pix: make([]int32, n)}
	}
	return nil
}

// tilePart is a tile-part together with its position in the codestream,
// which selects its packed packet headers when PPM is used.
type tilePart struct {
	*codestream.TilePart
	seq int
}

// collectTileParts reads all tile-part headers and groups the tile-parts
// by tile index, preserving their order.
func collectTileParts(cr *codestream.Reader, numTiles int, charge func(int) error) (map[int][]tilePart, error) {
	parts := make(map[int][]tilePart)
	for seq := 0; ; seq++ {
		tp, err := cr.NextTilePart()
		if err != nil {
			// a damaged tile-part header ends what can be interpreted
			if len(parts) > 0 {
				return parts, nil
			}
			return nil, err
		}
		if tp == nil {
			return parts, nil
		}
		if tp.Index >= numTiles {
			// a tile-part for a tile that does not exist ends what can
			// be interpreted
			return parts, nil
		}
		if charge != nil {
			if err := charge(256); err != nil {
				return nil, err
			}
		}
		parts[tp.Index] = append(parts[tp.Index], tilePart{tp, seq})
	}
}

func ceilShift(a, s int) int { return (a + (1 << uint(s)) - 1) >> uint(s) }
