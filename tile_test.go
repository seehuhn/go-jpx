// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jpx

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/bits"
	"math/rand/v2"
	"testing"

	"github.com/google/go-cmp/cmp"

	"seehuhn.de/go/jpx/internal/codestream"
	"seehuhn.de/go/jpx/internal/t1"
	"seehuhn.de/go/jpx/internal/t2"
)

func TestNewCoefScale(t *testing.T) {
	none := int64(math.MaxInt64)
	cases := []struct {
		shift, dropped int
		want           coefScale
	}{
		{0, 0, coefScale{roiMin: none}},
		{0, 7, coefScale{roiMin: none, bgExp: 7}},
		{2, 0, coefScale{roiMin: 8, roiDown: 2}},
		{29, 0, coefScale{roiMin: 1 << 30, roiDown: 29}},
		{30, 0, coefScale{roiMin: 1 << 31, roiDown: 30}},
		{31, 0, coefScale{roiMin: 1 << 32, roiDown: 31}},
		{255, 0, coefScale{roiMin: 1 << 32, roiDown: 255}},
		{40, 15, coefScale{roiMin: 1 << 26, roiDown: 25, bgExp: 15}},
		{5, 5, coefScale{roiMin: 2, bgExp: 5}},
		{2, 9, coefScale{roiMin: 1, roiExp: 7, bgExp: 9}},
	}
	for _, c := range cases {
		got := newCoefScale(c.shift, c.dropped)
		if got != c.want {
			t.Errorf("newCoefScale(%d, %d) = %+v, want %+v", c.shift, c.dropped, got, c.want)
		}
	}
}

func TestShiftSat(t *testing.T) {
	cases := []struct {
		m    int64
		e    int
		want int32
	}{
		{0, 0, 0},
		{0, 300, 0},
		{5, -1, 2},
		{5, 0, 5},
		{5, 2, 20},
		{1 << 31, -1, 1 << 30},
		{1 << 30, 0, 1 << 30},
		{1 << 30, 1, math.MaxInt32},
		{1, 30, 1 << 30},
		{1, 31, math.MaxInt32},
		{1, 255, math.MaxInt32},
		{1 << 31, -63, 0},
		{1 << 31, -200, 0},
	}
	for _, c := range cases {
		if got := shiftSat(c.m, c.e); got != c.want {
			t.Errorf("shiftSat(%d, %d) = %d, want %d", c.m, c.e, got, c.want)
		}
	}
}

// passCountCode writes the code word for the number of coding passes of
// a code-block in a packet header.
func passCountCode(bw *t2.BitWriter, passes int) {
	switch {
	case passes == 1:
		bw.WriteBit(0)
	case passes == 2:
		bw.WriteBits(2, 2)
	case passes <= 5:
		bw.WriteBits(3, 2)
		bw.WriteBits(uint32(passes-3), 2)
	case passes <= 36:
		bw.WriteBits(0xF, 4)
		bw.WriteBits(uint32(passes-6), 5)
	default:
		bw.WriteBits(0x1FF, 9)
		bw.WriteBits(uint32(passes-37), 7)
	}
}

// oneBlockPacket builds the single-layer packet of a precinct holding a
// single code-block with the given zero bit-planes, passes and data, all
// in one codeword segment.
func oneBlockPacket(zero, passes int, data []byte) []byte {
	incl, zt := t2.NewTree(1, 1), t2.NewTree(1, 1)
	zt.SetValue(0, 0, zero)
	var bw t2.BitWriter
	bw.WriteBit(1)
	incl.Encode(&bw, 0, 0, 1)
	zt.EncodeValue(&bw, 0, 0)
	passCountCode(&bw, passes)
	n := 3 + bits.Len(uint(passes)) - 1
	for bits.Len(uint(len(data))) > n {
		bw.WriteBit(1)
		n++
	}
	bw.WriteBit(0)
	bw.WriteBits(uint32(len(data)), n)
	return append(bw.Flush(), data...)
}

// truncatedBlock codes the w×h block of coefficients x, which have mb
// magnitude bit-planes, as an encoder truncating the block to its most
// significant t1.MaxPlanes non-zero planes would.  It returns the data,
// the number of passes and zero bit-planes for the packet header, and the
// number k of low-order planes lost.
func truncatedBlock(t *testing.T, x []int64, w, h, mb int) (data []byte, passes, zero, k int) {
	t.Helper()
	maxBits := 0
	for _, v := range x {
		maxBits = max(maxBits, bits.Len64(uint64(abs64(v))))
	}
	k = max(0, maxBits-t1.MaxPlanes)
	y := make([]int32, len(x))
	for i, v := range x {
		m := int32(abs64(v) >> k)
		if v < 0 {
			m = -m
		}
		y[i] = m
	}
	eb, err := t1.NewEncoder().Encode(y, w, h, t1.BandLL, t1.Style{}, t1.MaxPlanes)
	if err != nil {
		t.Fatal(err)
	}
	zero = mb - k - t1.MaxPlanes + eb.ZeroPlanes
	if zero < 0 {
		t.Fatalf("coefficients need more than %d planes", mb)
	}
	return append([]byte(nil), eb.Data...), len(eb.Passes), zero, k
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// oneBlockImage builds the codestream of an unsigned grey image of size
// w×h (at most 64×64) and the given depth, without decomposition levels,
// whose samples have the coefficients x.  The levels-0 transform is the
// identity, so that every sample is its coefficient plus the DC offset.
// With roi > 0, the coefficients marked in inROI are scaled up by roi
// and an RGN marker segment is written.  It returns the codestream and
// the number of low-order planes lost.
func oneBlockImage(t *testing.T, w, h, depth int, signed, rev bool, q codestream.Quantization, x []int64, roi int, inROI func(i int) bool) ([]byte, int) {
	t.Helper()
	cod := hostileCOD(0, codestream.LRCP, 1, 15, 15)
	cod.Reversible = rev
	comps := []codestream.ComponentSIZ{{Depth: depth, Signed: signed, XRsiz: 1, YRsiz: 1}}
	s := hostileSpecFor(uint32(w), uint32(h), comps, cod, q)

	coefs := make([]int64, len(x))
	for i, v := range x {
		coefs[i] = v
		if roi > 0 && inROI(i) {
			coefs[i] = v << roi
		}
	}
	mb := q.GuardBits + q.Exponents[0] - 1 + roi
	data, passes, zero, k := truncatedBlock(t, coefs, w, h, mb)
	s.addPart(nil, oneBlockPacket(zero, passes, data))
	if roi > 0 {
		s.parts[0].extra = func(cw *codestream.Writer) { cw.RGN(codestream.RGN{Component: 0, Shift: roi}) }
	}
	return s.mustBuild(t), k
}

// raiseExponents returns a copy of the codestream data with every
// exponent of the expounded QCD marker segment raised so that the
// largest one is 31.
func raiseExponents(t *testing.T, data []byte) []byte {
	t.Helper()
	data = append([]byte(nil), data...)
	pos := 2
	for binary.BigEndian.Uint16(data[pos:]) != 0xFF5C {
		pos += 2 + int(binary.BigEndian.Uint16(data[pos+2:]))
	}
	n := int(binary.BigEndian.Uint16(data[pos+2:]))
	if data[pos+4]&0x1F != 2 {
		t.Fatal("unexpected quantisation style")
	}
	entries := data[pos+5 : pos+2+n]
	maxEps := 0
	for i := 0; i < len(entries); i += 2 {
		maxEps = max(maxEps, int(entries[i]>>3))
	}
	k := 31 - maxEps
	if k <= 0 {
		t.Fatalf("exponent %d leaves no room", maxEps)
	}
	for i := 0; i < len(entries); i += 2 {
		entries[i] += byte(k << 3)
	}
	return data
}

// A quantisation step 2^k times finer, with the same coded data, leaves
// the coefficients unchanged: the k extra planes at the bottom carry no
// data.  With exponent 31 the blocks can have more planes than the
// tier-1 decoder reconstructs, and the dropped planes must be accounted
// for in the dequantisation.
func TestDecodeExcessPlanes(t *testing.T) {
	img := testImage(64, 64, gray(8), 1)
	var buf bytes.Buffer
	if err := Encode(&buf, img, &EncodeParams{Lossy: true, Quality: 100, Levels: 3}); err != nil {
		t.Fatal(err)
	}

	// The planes of a block then number bits(q)+k ≤ 31, where q is the
	// largest quantised coefficient of the original stream, so at most
	// one plane is dropped.  The encoder leaves headroom, so a block
	// with a coefficient of the full nominal range is built by hand:
	// sample 0 of an 8-bit image without decomposition has coefficient
	// -128, which needs 8 planes at step size 1 (exponent 8).
	const w, h = 16, 8
	rng := rand.New(rand.NewPCG(8, 8))
	x := make([]int64, w*h)
	for i := range x {
		x[i] = rng.Int64N(256) - 128
	}
	x[0] = -128
	q := codestream.Quantization{Style: codestream.QuantExpounded, GuardBits: 2, Exponents: []int{8}, Mantissas: []int{0}}
	hand, _ := oneBlockImage(t, w, h, 8, false, false, q, x, 0, nil)

	for _, orig := range [][]byte{buf.Bytes(), hand} {
		want, err := Decode(bytes.NewReader(orig), nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(bytes.NewReader(raiseExponents(t, orig)), nil)
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(want.Planes, got.Planes); diff != "" {
			t.Errorf("finer quantisation changed the image (-want +got):\n%s", diff)
		}
	}
}

// A region of interest whose shift makes the blocks deeper than the
// tier-1 decoder can reconstruct: the region is restored, the background
// loses the planes that were not decoded.
func TestDecodeROITruncated(t *testing.T) {
	const w, h = 32, 32
	cases := []struct {
		depth, guard, eps, shift int
		bgMax                    int64 // largest background coefficient magnitude
	}{
		{depth: 16, guard: 2, eps: 16, shift: 20, bgMax: 1<<15 - 1},
		{depth: 12, guard: 2, eps: 12, shift: 40, bgMax: 1<<11 - 1},
		{depth: MaxDepth, guard: 7, eps: 31, shift: 2, bgMax: 3},
	}
	for _, c := range cases {
		rng := rand.New(rand.NewPCG(uint64(c.depth), uint64(c.shift)))
		offset := int64(1) << (c.depth - 1)
		samples := make([]int64, w*h)
		x := make([]int64, w*h)
		inROI := func(i int) bool { return i%w < w/2 }
		for i := range samples {
			if inROI(i) {
				samples[i] = rng.Int64N(2 * offset)
			} else {
				samples[i] = offset + rng.Int64N(2*c.bgMax+1) - c.bgMax
			}
		}
		samples[0] = 0 // the deepest coefficient lies in the region
		for i, s := range samples {
			x[i] = s - offset
		}
		q := codestream.Quantization{Style: codestream.QuantNone, GuardBits: c.guard, Exponents: []int{c.eps}}
		data, k := oneBlockImage(t, w, h, c.depth, false, true, q, x, c.shift, inROI)
		if k == 0 {
			t.Fatalf("depth %d, shift %d: no planes lost", c.depth, c.shift)
		}

		img, err := Decode(bytes.NewReader(data), nil)
		if err != nil {
			t.Fatalf("depth %d, shift %d: %v", c.depth, c.shift, err)
		}
		trunc := max(0, c.depth-MaxPrecision)
		if got := img.Components[0].Truncation; got != trunc {
			t.Errorf("depth %d: truncation %d, want %d", c.depth, got, trunc)
		}
		roiTol := int64(0)
		if trunc > 0 {
			roiTol = 1
		}
		bgTol := int64(1)<<k>>trunc + roiTol
		for i, s := range samples {
			want := s >> trunc
			got := int64(img.Planes[0].Pix[i])
			tol := bgTol
			if inROI(i) {
				tol = roiTol
			}
			if abs64(got-want) > tol {
				t.Errorf("depth %d, shift %d: sample %d (roi=%t) is %d, want %d±%d",
					c.depth, c.shift, i, inROI(i), got, want, tol)
				break
			}
		}
	}
}

// deepImage returns a grey codestream of the given depth without wavelet
// decomposition, and its samples.  The samples are random, including
// their lowest bits.
func deepImage(t *testing.T, depth int, signed, rev bool) ([]byte, []int64) {
	t.Helper()
	const w, h = 24, 20
	rng := rand.New(rand.NewPCG(uint64(depth), 1))
	lo := int64(0)
	if signed {
		lo = -1 << (depth - 1)
	}
	samples := make([]int64, w*h)
	for i := range samples {
		samples[i] = lo + rng.Int64N(1<<depth)
	}
	samples[0], samples[1] = lo, lo+1<<depth-1
	offset := int64(1) << (depth - 1)
	if signed {
		offset = 0
	}
	q := codestream.Quantization{Style: codestream.QuantNone, GuardBits: 7, Exponents: []int{31}}
	div := int64(1)
	if !rev {
		// step 2^(depth-eps)
		eps := min(depth, 31)
		q = codestream.Quantization{Style: codestream.QuantExpounded, GuardBits: 7, Exponents: []int{eps}, Mantissas: []int{0}}
		div = 1 << (depth - eps)
	}
	x := make([]int64, len(samples))
	for i, s := range samples {
		x[i] = (s - offset) / div
	}
	data, _ := oneBlockImage(t, w, h, depth, signed, rev, q, x, 0, nil)
	return data, samples
}

// Samples deeper than MaxPrecision bits are returned without their lowest
// bits, and written back exactly.
func TestDecodeDeep(t *testing.T) {
	const trunc = MaxDepth - MaxPrecision
	for _, rev := range []bool{true, false} {
		data, samples := deepImage(t, MaxDepth, false, rev)
		img, err := Decode(bytes.NewReader(data), nil)
		if err != nil {
			t.Fatalf("rev=%t: %v", rev, err)
		}
		hdr, err := ReadHeader(bytes.NewReader(data), nil)
		if err != nil {
			t.Fatal(err)
		}
		wantComp := Component{Depth: MaxDepth, Truncation: trunc, DX: 1, DY: 1, Width: 24, Height: 20}
		if diff := cmp.Diff(wantComp, img.Components[0]); diff != "" {
			t.Errorf("rev=%t: wrong component (-want +got):\n%s", rev, diff)
		}
		if diff := cmp.Diff(hdr.Components, img.Components); diff != "" {
			t.Errorf("rev=%t: ReadHeader and Decode disagree (-want +got):\n%s", rev, diff)
		}

		// coefficient magnitudes are truncated rather than the samples
		tol := int64(1)
		for i, s := range samples {
			got := int64(img.Planes[0].Pix[i])
			if want := s >> trunc; abs64(got-want) > tol {
				t.Fatalf("rev=%t: sample %d is %d, want %d±%d", rev, i, got, want, tol)
			}
		}

		u, err := ToUnsigned(img, 16, nil)
		if err != nil {
			t.Fatal(err)
		}
		if c := u.Components[0]; c.Depth != 16 || c.Truncation != 0 || c.Signed {
			t.Errorf("rev=%t: ToUnsigned gave component %+v", rev, c)
		}
		for i, s := range samples {
			want := int64(math.Round(float64(s) * 65535 / (1<<MaxDepth - 1)))
			if got := int64(u.Planes[0].Pix[i]); abs64(got-want) > 1 {
				t.Fatalf("rev=%t: 16-bit sample %d is %d, want %d", rev, i, got, want)
			}
		}

		up, err := Upsample(img, nil)
		if err != nil {
			t.Fatal(err)
		}
		if up.Components[0].Truncation != trunc {
			t.Errorf("rev=%t: Upsample lost the truncation", rev)
		}

		writeBack(t, img, &EncodeParams{Levels: NoLevels})
	}
}

// Signed samples deeper than MaxPrecision bits are offset by the
// truncated mid-point when converted to unsigned.
func TestToUnsignedTruncated(t *testing.T) {
	// 33-bit signed samples -2^32, 0 and 2^32-1, stored divided by 2^9
	img := &Image{
		Width: 3, Height: 1, Components: []Component{{Depth: 33, Truncation: 9, Signed: true, DX: 1, DY: 1, Width: 3, Height: 1}},
		Planes: []Plane{{Width: 3, Height: 1, Pix: []int32{-1 << 23, 0, 1<<23 - 1}}},
	}
	got, err := ToUnsigned(img, 8, nil)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]int32{0, 128, 255}, got.Planes[0].Pix); diff != "" {
		t.Errorf("wrong samples (-want +got):\n%s", diff)
	}
	same, err := ToUnsigned(&Image{
		Width: 1, Height: 1, Components: []Component{{Depth: 30, Truncation: 6, DX: 1, DY: 1, Width: 1, Height: 1}},
		Planes: []Plane{{Width: 1, Height: 1, Pix: []int32{12345}}},
	}, 24, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c := same.Components[0]; c.Depth != 24 || c.Truncation != 0 || same.Planes[0].Pix[0] != 12345 {
		t.Errorf("24-bit view of a truncated 30-bit plane changed: %+v %v", c, same.Planes[0].Pix)
	}
}

// Truncated components are coded with every plane the coder handles, so
// that a decoder reconstructing uncoded planes at the mid-point still
// recovers the samples exactly for depths whose bands fit the coder.
// Deeper bands leave exactly their surplus planes uncoded.
func TestEncodeDeepCodesAllPlanes(t *testing.T) {
	for _, depth := range []int{MaxPrecision + 1, 27, 28, MaxDepth} {
		img := truncatedImage(40, 36, 1, depth, false, uint64(depth))
		var buf bytes.Buffer
		if err := Encode(&buf, img, &EncodeParams{Levels: 2}); err != nil {
			t.Fatal(err)
		}
		cr := codestream.NewReader(buf.Bytes(), nil)
		m, err := cr.ReadMainHeader()
		if err != nil {
			t.Fatal(err)
		}
		parts, err := collectTileParts(cr, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		d := newTileDecoder(m, &Image{}, nil, 1)
		headers := []*codestream.TilePart{parts[0][0].TilePart}
		tile, err := t2.NewTile(m, headers, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.readPackets(tile, parts[0]); err != nil {
			t.Fatal(err)
		}
		blocks := 0
		for _, tc := range tile.Comps {
			for _, res := range tc.Res {
				for _, p := range res.Precincts {
					for _, pb := range p.Bands {
						total := tc.TotalPlanes(pb.Band)
						dropped := max(0, total-t1.MaxPlanes)
						for i := range pb.Blocks {
							cb := &pb.Blocks[i]
							if cb.Passes == 0 {
								continue
							}
							blocks++
							want := t1.MaxPasses(total - cb.ZeroPlanes - dropped)
							if cb.Passes != want {
								t.Errorf("depth %d: block has %d passes, want %d", depth, cb.Passes, want)
							}
						}
					}
				}
			}
		}
		if blocks == 0 {
			t.Errorf("depth %d: no coded blocks", depth)
		}
	}
}

// The inverse component transform combines components of different
// truncations at the scale of the full samples.
func TestInverseMCTMixedTruncation(t *testing.T) {
	depths := []int{MaxPrecision + 1, MaxPrecision + 2, MaxPrecision + 3}
	rng := rand.New(rand.NewPCG(3, 4))
	const n = 64
	for _, rev := range []bool{true, false} {
		tile := &t2.Tile{}
		d := &tileDecoder{ibuf: make([][]int32, 3), fbuf: make([][]float32, 3)}
		for _, depth := range depths {
			tile.Comps = append(tile.Comps, &t2.TileComp{Depth: depth, Style: codestream.CodingStyle{Reversible: rev}})
		}
		// transformed values of valid samples at full scale, as the
		// planes hold them without their lowest bits
		full := make([][]int64, 3)
		for c := range 3 {
			full[c] = make([]int64, n)
			d.ibuf[c] = make([]int32, n)
			d.fbuf[c] = make([]float32, n)
		}
		for i := range n {
			var rgb [3]int64
			for c, depth := range depths {
				rgb[c] = rng.Int64N(1<<depth) - 1<<(depth-1)
			}
			r, g, b := rgb[0], rgb[1], rgb[2]
			var yuv [3]int64
			if rev {
				yuv = [3]int64{(r + 2*g + b) >> 2, b - g, r - g}
			} else {
				fr, fg, fb := float64(r), float64(g), float64(b)
				yuv = [3]int64{
					int64(math.Round(0.299*fr + 0.587*fg + 0.114*fb)),
					int64(math.Round(-0.16875*fr - 0.331260*fg + 0.5*fb)),
					int64(math.Round(0.5*fr - 0.41869*fg - 0.08131*fb)),
				}
			}
			for c, depth := range depths {
				trunc := depthTruncation(depth)
				v := yuv[c] >> trunc
				full[c][i] = v << trunc
				d.ibuf[c][i] = int32(v)
				d.fbuf[c][i] = float32(v)
			}
		}
		if err := d.inverseMCT(tile); err != nil {
			t.Fatal(err)
		}
		for i := range n {
			y, u, v := full[0][i], full[1][i], full[2][i]
			var want [3]float64
			if rev {
				g := y - (u+v)>>2
				want = [3]float64{float64(v + g), float64(g), float64(u + g)}
			} else {
				fy, fu, fv := float64(y), float64(u), float64(v)
				want = [3]float64{fy + 1.402*fv, fy - 0.34413*fu - 0.71414*fv, fy + 1.772*fu}
			}
			for c, depth := range depths {
				w := math.Ldexp(want[c], -depthTruncation(depth))
				var got float64
				if rev {
					got = float64(d.ibuf[c][i])
				} else {
					got = float64(d.fbuf[c][i])
				}
				// floor for the integers, float32 rounding for the floats
				diff := got - w
				if rev && (diff < -1 || diff > 0) || !rev && math.Abs(diff) > 1e-6*math.Abs(w)+1 {
					t.Errorf("rev=%t: component %d sample %d is %g, want %g", rev, c, i, got, w)
				}
			}
		}
	}
}
