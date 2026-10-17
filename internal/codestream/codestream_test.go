// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package codestream

import (
	"bytes"
	"encoding/binary"
	"errors"
	"runtime"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func testSIZ(ncomp int) *SIZ {
	s := &SIZ{Xsiz: 100, Ysiz: 80, XTsiz: 64, YTsiz: 80, XOsiz: 3, YOsiz: 2}
	for i := range ncomp {
		s.Components = append(s.Components, ComponentSIZ{Depth: 8 + i%8, Signed: i == 1, XRsiz: 1 + i%2, YRsiz: 1})
	}
	return s
}

func testCOD() *COD {
	return &COD{
		CodingStyle: CodingStyle{
			EntropyBypass: true, Terminate: true, SegmentationSymbols: true,
			Levels: 2, CodeBlockW: 6, CodeBlockH: 5, Reversible: true,
			PrecinctSizes: []PrecinctSize{{7, 7}, {8, 6}, {15, 15}},
		},
		Progression: RPCL, Layers: 3, MCT: true, SOP: true, EPH: true,
	}
}

func testQCD() *Quantization {
	return &Quantization{Style: QuantNone, GuardBits: 2, Exponents: []int{8, 9, 9, 10, 9, 9, 10}}
}

// writeMain builds a codestream up to and including the main header (ending
// in EOC) and parses it back.  The COD and QCD segments are written first
// unless the given pointers are nil; f may add further segments.
func writeMain(t testing.TB, csiz int, cod *COD, qcd *Quantization, f func(w *Writer)) (*MainHeader, error) {
	t.Helper()
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SOC()
	w.SIZ(testSIZ(csiz))
	if cod != nil {
		w.COD(cod)
	}
	if qcd != nil {
		w.QCD(qcd)
	}
	if f != nil {
		f(w)
	}
	w.EOC()
	if err := w.Err(); err != nil {
		return nil, err
	}
	return NewReader(buf.Bytes(), nil).ReadMainHeader()
}

func mustMain(t testing.TB, csiz int, cod *COD, qcd *Quantization, f func(w *Writer)) *MainHeader {
	t.Helper()
	h, err := writeMain(t, csiz, cod, qcd, f)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestMarkerRoundTrips(t *testing.T) {
	siz := testSIZ(3)
	h := mustMain(t, 3, testCOD(), testQCD(), nil)
	if d := cmp.Diff(*siz, h.SIZ); d != "" {
		t.Errorf("SIZ round trip failed (-want +got):\n%s", d)
	}

	for _, cod := range []*COD{testCOD(), {
		CodingStyle: CodingStyle{Levels: 1, CodeBlockW: 4, CodeBlockH: 4, PrecinctSizes: DefaultPrecincts(2)},
		Layers:      1,
	}} {
		q := testQCD()
		if !cod.Reversible {
			q = &Quantization{Style: QuantDerived, Exponents: []int{8}, Mantissas: []int{3}}
		}
		h := mustMain(t, 3, cod, q, nil)
		if d := cmp.Diff(cod, h.COD); d != "" {
			t.Errorf("COD round trip failed (-want +got):\n%s", d)
		}
		for _, n := range []int{3, 300} {
			h := mustMain(t, n, cod, q, func(w *Writer) { w.COC(2, &cod.CodingStyle) })
			if d := cmp.Diff(map[int]*CodingStyle{2: &cod.CodingStyle}, h.COC); d != "" {
				t.Errorf("COC round trip failed (-want +got):\n%s", d)
			}
		}
	}

	for _, q := range []*Quantization{
		testQCD(),
		{Style: QuantDerived, GuardBits: 1, Exponents: []int{12}, Mantissas: []int{0x7FF}},
		{Style: QuantExpounded, GuardBits: 7, Exponents: []int{1, 31}, Mantissas: []int{5, 0}},
	} {
		h := mustMain(t, 3, testCOD(), q, nil)
		if d := cmp.Diff(q, h.QCD); d != "" {
			t.Errorf("QCD round trip failed (-want +got):\n%s", d)
		}
		h = mustMain(t, 300, testCOD(), testQCD(), func(w *Writer) { w.QCC(1, q) })
		if d := cmp.Diff(map[int]*Quantization{1: q}, h.QCC); d != "" {
			t.Errorf("QCC round trip failed (-want +got):\n%s", d)
		}
	}

	for _, n := range []int{3, 1000} {
		r := RGN{Component: 2, Shift: 11}
		es := []POCEntry{
			{0, 0, 2, 3, 3, RLCP},
			{1, 1, 65535, 33, 2, CPRL},
		}
		h := mustMain(t, n, testCOD(), testQCD(), func(w *Writer) {
			w.RGN(r)
			w.POC(es)
		})
		if d := cmp.Diff([]RGN{r}, h.RGN); d != "" {
			t.Errorf("RGN round trip failed (-want +got):\n%s", d)
		}
		if d := cmp.Diff(es, h.POC); d != "" {
			t.Errorf("POC round trip failed (-want +got):\n%s", d)
		}
	}

	g := &CRG{Xcrg: []uint16{1, 2, 3}, Ycrg: []uint16{4, 5, 6}}
	h = mustMain(t, 3, testCOD(), testQCD(), func(w *Writer) { w.CRG(g) })
	if d := cmp.Diff(g, h.CRG); d != "" {
		t.Errorf("CRG round trip failed (-want +got):\n%s", d)
	}
}

func TestPOCComponentRange(t *testing.T) {
	poc := func(compEnd int) func(w *Writer) {
		return func(w *Writer) { w.POC([]POCEntry{{0, 0, 1, 2, compEnd, LRCP}}) }
	}
	// the 8-bit form writes 256 as zero and reads it back
	h := mustMain(t, 256, testCOD(), testQCD(), poc(256))
	if h.POC[0].CompEnd != 256 {
		t.Errorf("CompEnd = %d, want 256", h.POC[0].CompEnd)
	}
	// the 16-bit form has no such rule
	h = mustMain(t, 300, testCOD(), testQCD(), poc(256))
	if h.POC[0].CompEnd != 256 {
		t.Errorf("16-bit CompEnd = %d, want 256", h.POC[0].CompEnd)
	}
	// 16-bit zero is invalid
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SOC()
	w.SIZ(testSIZ(300))
	w.COD(testCOD())
	w.QCD(testQCD())
	// Lpoc=11, RS=0, CS=0, LYE=1, RE=2, CE=0 (16 bit), P=0
	buf.Write([]byte{0xFF, 0x5F, 0, 11, 0, 0, 0, 0, 1, 2, 0, 0, 0})
	w.EOC()
	if _, err := NewReader(buf.Bytes(), nil).ReadMainHeader(); err == nil {
		t.Error("16-bit CEpoc of zero accepted")
	}

	// the writer rejects invalid entries
	bad := [][]POCEntry{
		{{0, 0, 1, 2, 257, LRCP}}, // CompEnd beyond the 8-bit field
		{{0, 3, 1, 2, 3, LRCP}},   // CompStart >= CompEnd
		{{0, 0, 0, 2, 3, LRCP}},   // LayerEnd 0
		{{2, 0, 1, 2, 3, LRCP}},   // ResStart >= ResEnd
		{{0, 0, 1, 34, 3, LRCP}},  // ResEnd > 33
	}
	for i, es := range bad {
		w := NewWriter(&bytes.Buffer{})
		w.SIZ(testSIZ(3))
		if w.POC(es) == nil {
			t.Errorf("bad POC %d accepted by writer", i)
		}
	}
}

// buildStream writes a synthetic codestream with two tiles; tile 0 has two
// tile-parts.
func buildStream(t testing.TB) (data []byte, bodies [][]byte) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SOC()
	w.SIZ(testSIZ(3))
	w.COD(testCOD())
	cs := testCOD().CodingStyle
	cs.Levels = 1
	cs.PrecinctSizes = DefaultPrecincts(2)
	w.COC(1, &cs)
	w.QCD(testQCD())
	w.QCC(2, &Quantization{Style: QuantDerived, GuardBits: 2, Exponents: []int{9}, Mantissas: []int{100}})
	w.RGN(RGN{0, 4})
	w.POC([]POCEntry{{0, 0, 1, 3, 3, LRCP}})
	w.TLM([]byte{0, 0x50, 1, 2})
	w.CRG(&CRG{[]uint16{0, 0, 0}, []uint16{0, 0, 0}})
	w.COM(Comment{1, []byte("hello")})
	bodies = [][]byte{[]byte("tile0-part0"), []byte("tile0-part1"), []byte("tile1")}
	for i, b := range bodies {
		tile, part, cnt := 0, i, 2
		if i == 2 {
			tile, part, cnt = 1, 0, 1
		}
		off, _ := w.SOT(tile, part, cnt)
		if i == 0 {
			w.COD(&COD{CodingStyle: cs, Layers: 1})
			w.PLT([]byte{0, 1, 2})
			w.PPT(1, []byte("B"))
			w.PPT(0, []byte("A"))
		}
		w.SOD()
		w.raw(b)
		if err := PatchPsot(buf.Bytes(), int(off), uint32(int(w.Offset())-int(off))); err != nil {
			t.Fatal(err)
		}
	}
	w.EOC()
	if w.Err() != nil {
		t.Fatal(w.Err())
	}
	return buf.Bytes(), bodies
}

func TestReaderWalk(t *testing.T) {
	data, bodies := buildStream(t)
	r := NewReader(data, nil)
	h, err := r.ReadMainHeader()
	if err != nil {
		t.Fatal(err)
	}
	if d := cmp.Diff(testCOD(), h.COD); d != "" {
		t.Errorf("COD mismatch (-want +got):\n%s", d)
	}
	if len(h.COC) != 1 || len(h.QCC) != 1 || len(h.RGN) != 1 || len(h.POC) != 1 ||
		len(h.TLM) != 1 || h.CRG == nil || len(h.Comments) != 1 {
		t.Errorf("main header content incomplete: %+v", h)
	}
	for i, want := range bodies {
		tp, err := r.NextTilePart()
		if err != nil || tp == nil {
			t.Fatalf("tile-part %d: %v", i, err)
		}
		if !bytes.Equal(tp.Data, want) {
			t.Errorf("tile-part %d: body %q, want %q", i, tp.Data, want)
		}
		if i == 0 {
			ph, err := tp.PackedHeaders(nil)
			if err != nil || tp.COD == nil || len(tp.PLT) != 1 || !bytes.Equal(ph, []byte("AB")) {
				t.Errorf("tile-part header incomplete: %+v", tp)
			}
			if tp.PartCount != 2 || tp.PartIndex != 0 || tp.Index != 0 {
				t.Errorf("wrong SOT fields: %+v", tp)
			}
		}
	}
	tp, err := r.NextTilePart()
	if tp != nil || err != nil {
		t.Errorf("expected end, got %v %v", tp, err)
	}

	// streaming header read
	var rd bytes.Reader
	rd.Reset(data)
	h2, err := ReadMainHeaderFrom(&rd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := cmp.Diff(h, h2); d != "" {
		t.Errorf("streaming header differs (-want +got):\n%s", d)
	}
	// the stream is left right after the first SOT marker
	if want := len(data) - bytes.Index(data, []byte{0xFF, 0x90}) - 2; rd.Len() != want {
		t.Errorf("streaming reader left %d bytes, want %d", rd.Len(), want)
	}
}

func TestTruncation(t *testing.T) {
	data, _ := buildStream(t)
	for n := range len(data) + 1 {
		r := NewReader(data[:n], nil)
		if _, err := r.ReadMainHeader(); err != nil {
			continue
		}
		for range 10 {
			tp, err := r.NextTilePart()
			if tp == nil || err != nil {
				break
			}
		}
	}
	// a clamped tile-part keeps the available bytes
	cut := data[:len(data)-4]
	r := NewReader(cut, nil)
	r.ReadMainHeader()
	var last *TilePart
	for {
		tp, err := r.NextTilePart()
		if err != nil || tp == nil {
			break
		}
		last = tp
	}
	if last == nil || last.Index != 1 || len(last.Data) != 3 {
		t.Errorf("truncated last tile-part not clamped: %+v", last)
	}
}

// readSIZ parses a raw SIZ payload as part of an otherwise valid header.
func readSIZ(p []byte) (*SIZ, error) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SOC()
	buf.Write([]byte{0xFF, 0x51, byte((len(p) + 2) >> 8), byte(len(p) + 2)})
	buf.Write(p)
	w.COD(testCOD())
	w.QCD(testQCD())
	w.EOC()
	h, err := NewReader(buf.Bytes(), nil).ReadMainHeader()
	if err != nil {
		return nil, err
	}
	return &h.SIZ, nil
}

func TestHostile(t *testing.T) {
	mk := func(f func(s *SIZ)) []byte {
		s := testSIZ(1)
		f(s)
		p := make([]byte, 36+3)
		binary.BigEndian.PutUint16(p, s.Rsiz)
		for i, v := range []uint32{s.Xsiz, s.Ysiz, s.XOsiz, s.YOsiz, s.XTsiz, s.YTsiz, s.XTOsiz, s.YTOsiz} {
			binary.BigEndian.PutUint32(p[2+4*i:], v)
		}
		binary.BigEndian.PutUint16(p[34:], uint16(len(s.Components)))
		p[36], p[37], p[38] = 7, 1, 1
		return p
	}
	bad := map[string]func(s *SIZ){
		"zero tile":   func(s *SIZ) { s.XTsiz = 0 },
		"huge":        func(s *SIZ) { s.Xsiz, s.Ysiz, s.XTsiz, s.YTsiz = 0xFFFFFFFF, 0xFFFFFFFF, 1, 1 },
		"empty":       func(s *SIZ) { s.XOsiz = s.Xsiz },
		"tile gap":    func(s *SIZ) { s.XTOsiz, s.XTsiz, s.XOsiz = 0, 2, 5 },
		"tile offset": func(s *SIZ) { s.XTOsiz = s.XOsiz + 1 },
		"overflow":    func(s *SIZ) { s.XTOsiz, s.XTsiz, s.XOsiz, s.Xsiz = 0, 0xFFFFFFFF, 0xFFFFFFFE, 0xFFFFFFFF },
	}
	for name, f := range bad {
		if name == "overflow" {
			// valid geometry must not misbehave
			if _, err := readSIZ(mk(f)); err != nil {
				t.Errorf("%s: unexpected error %v", name, err)
			}
			continue
		}
		if _, err := readSIZ(mk(f)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	p := mk(func(*SIZ) {})
	if _, err := readSIZ(p[:30]); err == nil {
		t.Error("short SIZ accepted")
	}
	binary.BigEndian.PutUint16(p[34:], 0)
	if _, err := readSIZ(p); err == nil {
		t.Error("Csiz=0 accepted")
	}
	binary.BigEndian.PutUint16(p[34:], 2)
	if _, err := readSIZ(p); err == nil {
		t.Error("Csiz/length mismatch accepted")
	}
	s := &SIZ{Xsiz: 0xFFFFFFFF, XTsiz: 0}
	if _, _, ok := s.NumTiles(); ok {
		t.Error("NumTiles ok with zero tile size")
	}
	s = &SIZ{Xsiz: 0xFFFFFFFF, Ysiz: 0xFFFFFFFF, XTsiz: 1, YTsiz: 1}
	if nx, ny, ok := s.NumTiles(); !ok || nx != 0xFFFFFFFF || ny != 0xFFFFFFFF {
		t.Errorf("NumTiles = %d %d %v", nx, ny, ok)
	}

	// structural problems in whole streams
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SOC()
	w.SIZ(testSIZ(1))
	good := buf.Bytes()
	sizSeg := good[2:]

	if _, err := NewReader(sizSeg, nil).ReadMainHeader(); err == nil {
		t.Error("stream without SOC accepted")
	}
	sot := []byte{0xFF, 0x4F, 0xFF, 0x90, 0, 10, 0, 0, 0, 0, 0, 14, 0, 1}
	if _, err := NewReader(sot, nil).ReadMainHeader(); err == nil {
		t.Error("SOT before SIZ accepted")
	}
	dup := append(append([]byte{}, good...), sizSeg...)
	if _, err := NewReader(dup, nil).ReadMainHeader(); err == nil {
		t.Error("duplicate SIZ accepted")
	}
	if _, err := NewReader(good, nil).NextTilePart(); err == nil {
		t.Error("NextTilePart before main header succeeded")
	}

	// unknown markers: with length is skipped, reserved range has none
	var b2 bytes.Buffer
	w2 := NewWriter(&b2)
	w2.SOC()
	w2.SIZ(testSIZ(1))
	b2.Write([]byte{0xFF, 0x70, 0, 4, 9, 9})    // unknown with length
	b2.Write([]byte{0xFF, 0x35})                // reserved, no length
	b2.Write([]byte{0xFF, 0x56, 0, 5, 1, 2, 3}) // PRF
	w2.COD(testCOD())
	w2.QCD(testQCD())
	w2.EOC()
	h, err := NewReader(b2.Bytes(), nil).ReadMainHeader()
	if err != nil || h.COD == nil {
		t.Errorf("unknown markers not skipped: %v", err)
	}
	// Lsiz too large / small
	bad2 := append([]byte{}, good...)
	binary.BigEndian.PutUint16(bad2[4:], 2000)
	if _, err := NewReader(bad2, nil).ReadMainHeader(); err == nil {
		t.Error("oversize Lsiz accepted")
	}
	binary.BigEndian.PutUint16(bad2[4:], 3)
	if _, err := NewReader(bad2, nil).ReadMainHeader(); err == nil {
		t.Error("tiny Lsiz accepted")
	}
}

func quantN(levels, first int) *Quantization {
	q := &Quantization{Style: QuantNone, Exponents: make([]int, 3*levels+1)}
	q.Exponents[0] = first
	return q
}

func TestResolve(t *testing.T) {
	mcod := testCOD()
	mcoc := mcod.CodingStyle
	mcoc.Levels = 1
	mcoc.PrecinctSizes = DefaultPrecincts(2)
	tcod := &COD{CodingStyle: mcod.CodingStyle, Layers: 1}
	tcod.Levels = 0
	tcod.PrecinctSizes = DefaultPrecincts(1)
	tcoc := mcod.CodingStyle
	tcoc.Levels = 5
	tcoc.PrecinctSizes = DefaultPrecincts(6)

	m := &MainHeader{
		SIZ: *testSIZ(3), COD: mcod, QCD: quantN(2, 1),
		COC: map[int]*CodingStyle{1: &mcoc}, QCC: map[int]*Quantization{1: quantN(1, 2)},
	}
	tp := &TilePart{
		COD: tcod, QCD: quantN(0, 3),
		COC: map[int]*CodingStyle{2: &tcoc}, QCC: map[int]*Quantization{2: quantN(5, 4)},
	}

	type want struct {
		levels, exp int
	}
	tests := []struct {
		name string
		t    *TilePart
		c    int
		want want
	}{
		{"main default", nil, 0, want{2, 1}},
		{"main COC/QCC", nil, 1, want{1, 2}},
		{"tile COD beats main COC", tp, 1, want{0, 3}},
		{"tile COD", tp, 0, want{0, 3}},
		{"tile COC/QCC", tp, 2, want{5, 4}},
		{"empty tile", &TilePart{}, 1, want{1, 2}},
	}
	for _, tc := range tests {
		cs, q, err := Resolve(m, tc.t, tc.c)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got := (want{cs.Levels, q.Exponents[0]}); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestResolveErrors(t *testing.T) {
	irrev := testCOD()
	irrev.Reversible = false
	short := &Quantization{Style: QuantExpounded, Exponents: []int{1, 2}, Mantissas: []int{1, 2}}
	derived := &Quantization{Style: QuantDerived, Exponents: []int{1}, Mantissas: []int{1}}
	tests := []struct {
		name string
		m    *MainHeader
		c    int
		ok   bool
	}{
		{"valid", &MainHeader{COD: testCOD(), QCD: quantN(2, 1)}, 0, true},
		{"valid derived", &MainHeader{COD: irrev, QCD: derived}, 0, true},
		{"fewer entries than levels", &MainHeader{COD: testCOD(), QCD: quantN(1, 1)}, 0, true},
		{"fewer expounded entries than levels", &MainHeader{COD: irrev, QCD: short}, 0, true},
		{"no entries", &MainHeader{COD: testCOD(), QCD: &Quantization{Style: QuantNone, GuardBits: 2}}, 0, false},
		{"reversible with scalar", &MainHeader{COD: testCOD(), QCD: derived}, 0, false},
		{"irreversible with none", &MainHeader{COD: irrev, QCD: quantN(2, 1)}, 0, false},
		{"bad component", &MainHeader{COD: testCOD(), QCD: quantN(2, 1)}, 5, false},
		{"derived with two entries", &MainHeader{COD: irrev, QCD: &Quantization{
			Style: QuantDerived, Exponents: []int{1, 1}, Mantissas: []int{1, 1}}}, 0, false},
	}
	for _, tc := range tests {
		tc.m.SIZ = *testSIZ(1)
		_, _, err := Resolve(tc.m, nil, tc.c)
		if (err == nil) != tc.ok {
			t.Errorf("%s: got error %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestPackedHeadersPPM(t *testing.T) {
	c1, c2 := []byte("first-chunk"), []byte("second")
	var all []byte
	for _, c := range [][]byte{c1, c2, nil} {
		all = binary.BigEndian.AppendUint32(all, uint32(len(c)))
		all = append(all, c...)
	}
	// split inside the length field of chunk 2 and inside a payload
	cut1, cut2 := 4+len(c1)+2, 4+len(c1)+4+3
	h := &MainHeader{PPM: []PPMSegment{
		{2, all[cut2:]}, {0, all[:cut1]}, {1, all[cut1:cut2]},
	}}
	got, err := h.PackedHeaders(nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := cmp.Diff([][]byte{c1, c2, {}}, got); d != "" {
		t.Errorf("PPM assembly failed (-want +got):\n%s", d)
	}
	h.PPM = h.PPM[:2]
	if _, err := h.PackedHeaders(nil); err == nil {
		t.Error("truncated PPM data accepted")
	}
	if got, err := (&MainHeader{}).PackedHeaders(nil); got != nil || err != nil {
		t.Error("expected nil without PPM")
	}
	if got, err := (&TilePart{}).PackedHeaders(nil); got != nil || err != nil {
		t.Error("expected nil without PPT")
	}
	tp := &TilePart{PPT: []PPTSegment{{0, []byte("a")}, {2, []byte("c")}}}
	if _, err := tp.PackedHeaders(nil); err == nil {
		t.Error("PPT gap accepted")
	}
	tp = &TilePart{PPT: []PPTSegment{{0, []byte("a")}, {0, []byte("b")}}}
	if _, err := tp.PackedHeaders(nil); err == nil {
		t.Error("duplicate PPT index accepted")
	}
	if (&MainHeader{}).UsesPPM() || !h.UsesPPM() {
		t.Error("wrong UsesPPM result")
	}
	w := NewWriter(&bytes.Buffer{})
	if w.PPM(256, nil) == nil {
		t.Error("PPM index 256 accepted")
	}
	w = NewWriter(&bytes.Buffer{})
	if w.PPT(-1, nil) == nil {
		t.Error("PPT index -1 accepted")
	}
}

func TestValidation(t *testing.T) {
	bad := []*COD{testCOD(), testCOD(), testCOD(), testCOD()}
	bad[0].CodeBlockW = 11
	bad[1].CodeBlockW, bad[1].CodeBlockH = 8, 6
	bad[2].PrecinctSizes = bad[2].PrecinctSizes[:2]
	bad[3].Layers = 0
	for i, c := range bad {
		if NewWriter(&bytes.Buffer{}).COD(c) == nil {
			t.Errorf("bad COD %d accepted", i)
		}
	}
	q := Quantization{Style: QuantNone, Exponents: []int{1, 2}}
	if q.Validate(1) != nil || q.Validate(0) != nil {
		t.Error("Quantization.Validate gives wrong result")
	}
	q = Quantization{Style: QuantNone}
	if q.Validate(0) == nil {
		t.Error("Quantization.Validate accepts an empty entry list")
	}
	if Progression(2).String() != "RPCL" {
		t.Error("wrong Progression string")
	}
}

// tilePartStream builds a codestream with a single tile whose second
// tile-part header is written by f.
func tilePartStream(t testing.TB, f func(w *Writer)) []byte {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SOC()
	s := testSIZ(1)
	s.XTsiz, s.YTsiz = 200, 200
	w.SIZ(s)
	w.COD(testCOD())
	w.QCD(testQCD())
	for part := range 2 {
		off, _ := w.SOT(0, part, 2)
		if part == 1 {
			f(w)
		}
		w.SOD()
		w.raw([]byte("xy"))
		if err := PatchPsot(buf.Bytes(), int(off), uint32(int(w.Offset())-int(off))); err != nil {
			t.Fatal(err)
		}
	}
	w.EOC()
	return buf.Bytes()
}

func readAllParts(data []byte) error {
	r := NewReader(data, nil)
	if _, err := r.ReadMainHeader(); err != nil {
		return err
	}
	for {
		tp, err := r.NextTilePart()
		if err != nil || tp == nil {
			return err
		}
	}
}

func TestLaterTilePartMarkers(t *testing.T) {
	cs := testCOD().CodingStyle
	for name, f := range map[string]func(w *Writer){
		"COD": func(w *Writer) { w.COD(testCOD()) },
		"COC": func(w *Writer) { w.COC(0, &cs) },
		"QCD": func(w *Writer) { w.QCD(testQCD()) },
		"QCC": func(w *Writer) { w.QCC(0, testQCD()) },
		"RGN": func(w *Writer) { w.RGN(RGN{0, 1}) },
	} {
		r := NewReader(tilePartStream(t, f), nil)
		if _, err := r.ReadMainHeader(); err != nil {
			t.Fatal(err)
		}
		for part := range 2 {
			tp, err := r.NextTilePart()
			if err != nil || tp == nil {
				t.Fatalf("%s: part %d: %v", name, part, err)
			}
			if part == 1 && (tp.COD != nil || tp.QCD != nil || tp.COC != nil ||
				tp.QCC != nil || tp.RGN != nil) {
				t.Errorf("%s in a later tile-part not ignored", name)
			}
		}
	}
	allowed := map[string]func(w *Writer){
		"POC": func(w *Writer) { w.POC([]POCEntry{{0, 0, 1, 2, 1, LRCP}}) },
		"PLT": func(w *Writer) { w.PLT([]byte{0, 1}) },
		"PPT": func(w *Writer) { w.PPT(0, []byte{1}) },
		"COM": func(w *Writer) { w.COM(Comment{1, []byte("x")}) },
	}
	for name, f := range allowed {
		if err := readAllParts(tilePartStream(t, f)); err != nil {
			t.Errorf("%s in a later tile-part rejected: %v", name, err)
		}
	}
	dup := func(w *Writer) {
		e := []POCEntry{{0, 0, 1, 2, 1, LRCP}}
		w.POC(e)
		w.POC(e)
	}
	if err := readAllParts(tilePartStream(t, dup)); err == nil {
		t.Error("two POC segments in one header accepted")
	}
}

func TestTruncatedFlag(t *testing.T) {
	data, _ := buildStream(t)
	r := NewReader(data, nil)
	r.ReadMainHeader()
	for {
		tp, err := r.NextTilePart()
		if err != nil || tp == nil {
			break
		}
		if tp.Truncated {
			t.Errorf("complete tile-part %d reported as truncated", tp.Index)
		}
	}
	r = NewReader(data[:len(data)-4], nil)
	r.ReadMainHeader()
	var last *TilePart
	for {
		tp, err := r.NextTilePart()
		if err != nil || tp == nil {
			break
		}
		last = tp
	}
	if last == nil || !last.Truncated {
		t.Errorf("truncated tile-part not flagged: %+v", last)
	}
}

// emptyTLMStream returns a stream whose main header holds n TLM segments
// without payload.
func emptyTLMStream(t testing.TB, n int) []byte {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SOC()
	w.SIZ(testSIZ(1))
	w.COD(testCOD())
	w.QCD(testQCD())
	for range n {
		w.TLM(nil)
	}
	w.EOC()
	if w.Err() != nil {
		t.Fatal(w.Err())
	}
	return buf.Bytes()
}

func TestManyEmptySegmentsAllocation(t *testing.T) {
	data := emptyTLMStream(t, 100000)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	h, err := NewReader(data, nil).ReadMainHeader()
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 3*uint64(len(data)) {
		t.Errorf("parsing allocated %d bytes for %d bytes of input", got, len(data))
	}
	if len(h.TLM) != 0 {
		t.Errorf("recorded %d empty TLM segments", len(h.TLM))
	}
}

func TestChargeStopsParsing(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SOC()
	w.SIZ(testSIZ(1))
	w.COD(testCOD())
	w.QCD(testQCD())
	for range 10000 {
		w.COM(Comment{1, []byte("abc")})
	}
	w.EOC()
	data := buf.Bytes()

	errBudget := errors.New("budget exhausted")
	limited := func(limit int) func(int) error {
		return func(n int) error {
			limit -= n
			if limit < 0 {
				return errBudget
			}
			return nil
		}
	}
	if _, err := NewReader(data, limited(20000)).ReadMainHeader(); !errors.Is(err, errBudget) {
		t.Errorf("reader: got %v, want budget error", err)
	}
	if _, err := ReadMainHeaderFrom(bytes.NewReader(data), limited(20000)); !errors.Is(err, errBudget) {
		t.Errorf("stream: got %v, want budget error", err)
	}
	if _, err := NewReader(data, limited(1<<30)).ReadMainHeader(); err != nil {
		t.Errorf("reader with ample budget: %v", err)
	}
	if _, err := ReadMainHeaderFrom(bytes.NewReader(data), limited(1<<30)); err != nil {
		t.Errorf("stream with ample budget: %v", err)
	}
}

func TestStreamDeclaredLength(t *testing.T) {
	// a segment declaring 64 KiB but providing 3 bytes must fail cheaply
	data := []byte{0xFF, 0x4F, 0xFF, 0x51, 0xFF, 0xFF, 1, 2, 3}
	total := 0
	_, err := ReadMainHeaderFrom(bytes.NewReader(data), func(n int) error {
		total += n
		return nil
	})
	if err == nil {
		t.Fatal("truncated segment accepted")
	}
	if total > maxChunk {
		t.Errorf("charged %d bytes for a truncated segment", total)
	}
}

func FuzzMainHeader(f *testing.F) {
	data, _ := buildStream(f)
	f.Add(data)
	f.Add(data[:len(data)/3])
	f.Add(emptyTLMStream(f, 3))
	f.Fuzz(func(t *testing.T, data []byte) {
		h1, err1 := ReadMainHeaderFrom(bytes.NewReader(data), nil)
		h2, err2 := NewReader(data, nil).ReadMainHeader()
		if (err1 == nil) != (err2 == nil) {
			t.Fatalf("stream error %v, memory error %v", err1, err2)
		}
		if err1 == nil {
			if d := cmp.Diff(h1, h2, cmpopts.EquateEmpty()); d != "" {
				t.Errorf("readers disagree (-stream +memory):\n%s", d)
			}
			for c := range min(len(h1.SIZ.Components), 4) {
				Resolve(h1, nil, c)
			}
			h1.PackedHeaders(nil)
		}
	})
}

func FuzzTileParts(f *testing.F) {
	data, _ := buildStream(f)
	f.Add(data)
	f.Add(data[:len(data)-5])
	f.Add(tilePartStream(f, func(w *Writer) { w.PPT(0, []byte{1}) }))
	f.Fuzz(func(t *testing.T, data []byte) {
		r := NewReader(data, nil)
		h, err := r.ReadMainHeader()
		if err != nil {
			return
		}
		for range len(data) {
			tp, err := r.NextTilePart()
			if err != nil || tp == nil {
				return
			}
			tp.PackedHeaders(nil)
			for c := range min(len(h.SIZ.Components), 4) {
				Resolve(h, tp, c)
			}
		}
		t.Error("tile-part loop did not terminate")
	})
}

func TestPackedHeadersCharge(t *testing.T) {
	var all []byte
	for _, c := range [][]byte{[]byte("abc"), []byte("defgh")} {
		all = binary.BigEndian.AppendUint32(all, uint32(len(c)))
		all = append(all, c...)
	}
	var total int
	count := func(n int) error { total += n; return nil }
	h := &MainHeader{PPM: []PPMSegment{{1, all[5:]}, {0, all[:5]}}}
	if _, err := h.PackedHeaders(count); err != nil {
		t.Fatal(err)
	}
	if total < len(all) {
		t.Errorf("charged %d bytes, want at least %d", total, len(all))
	}
	total = 0
	tp := &TilePart{PPT: []PPTSegment{{0, []byte("abc")}, {1, []byte("de")}}}
	got, err := tp.PackedHeaders(count)
	if err != nil {
		t.Fatal(err)
	}
	if total < len(got) {
		t.Errorf("charged %d bytes, want at least %d", total, len(got))
	}
	stop := errors.New("stop")
	if _, err := tp.PackedHeaders(func(int) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("charge error not returned: %v", err)
	}
	if _, err := h.PackedHeaders(func(int) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("charge error not returned: %v", err)
	}
}

func TestWriterComponentRange(t *testing.T) {
	cs := testCOD().CodingStyle
	for _, c := range []int{-1, 2, 100} {
		w := NewWriter(&bytes.Buffer{})
		w.SIZ(testSIZ(2))
		if w.COC(c, &cs) == nil {
			t.Errorf("COC component %d accepted", c)
		}
		w = NewWriter(&bytes.Buffer{})
		w.SIZ(testSIZ(2))
		if w.QCC(c, testQCD()) == nil {
			t.Errorf("QCC component %d accepted", c)
		}
	}
	if NewWriter(&bytes.Buffer{}).COC(0, &cs) == nil {
		t.Error("COC before SIZ accepted")
	}
	if NewWriter(&bytes.Buffer{}).QCC(0, testQCD()) == nil {
		t.Error("QCC before SIZ accepted")
	}
}

func TestTruncatedTilePartHeaderEndsReader(t *testing.T) {
	data := tilePartStream(t, func(w *Writer) {})
	r := NewReader(data, nil)
	if _, err := r.ReadMainHeader(); err != nil {
		t.Fatal(err)
	}
	// cut right after the SOT segment, before SOD
	r.data = r.data[:r.pos+13]
	if _, err := r.NextTilePart(); err == nil {
		t.Fatal("truncated tile-part header accepted")
	}
	if tp, err := r.NextTilePart(); tp != nil || err != nil {
		t.Errorf("reader did not reach the end: %v, %v", tp, err)
	}
}

func TestDuplicateCRG(t *testing.T) {
	_, err := writeMain(t, 1, testCOD(), testQCD(), func(w *Writer) {
		w.segment(markerCRG, make([]byte, 4), nil)
		w.segment(markerCRG, make([]byte, 4), nil)
	})
	if err == nil {
		t.Error("duplicate CRG accepted")
	}
}
