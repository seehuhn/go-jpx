// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jp2

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
)

var testCodestream = []byte{0xFF, 0x4F, 0xFF, 0x51, 1, 2, 3, 4, 5, 6, 7, 8, 0xFF, 0xD9}

func fullHeader() *Header {
	return &Header{
		HasWrapper: true,
		Brand:      "jpx ",
		Compat:     []string{"jp2 ", "jpx "},
		Image: &ImageHeader{Height: 17, Width: 33, NC: 3,
			BPC: BitDepth{Depth: 0}, C: 7, UnkC: true, IPR: true},
		BitDepths: []BitDepth{{8, false}, {12, true}, {1, false}},
		Colors: []ColorSpec{
			{Method: 1, Precedence: 0, Approx: 1, EnumCS: 16},
			{Method: 1, Approx: 1, EnumCS: 14, EnumParams: []byte{1, 2, 3, 4, 5}},
			{Method: 2, Precedence: -3, Approx: 2, ICC: []byte{1, 2, 3, 4, 5}},
			{Method: 3, Precedence: 5, Approx: 3, ICC: []byte{9}},
			{Method: 4, Precedence: 1, Approx: 4, Vendor: []byte("vendor data")},
		},
		Palette: &Palette{
			Entries: 3,
			Depths:  []BitDepth{{8, false}, {5, true}, {12, false}, {16, true}},
			Values: [][]int32{
				{0, -16, 4095, -32768},
				{255, 15, 0, 32767},
				{7, -1, 1000, -1},
			},
		},
		ComponentMap: []ComponentMapping{{0, 1, 0}, {0, 1, 1}, {0, 1, 2}},
		Channels:     []ChannelDef{{0, 0, 1}, {1, 0, 2}, {2, 0, 3}, {3, 1, 0}},

		ReaderRequirements: []byte{1, 2, 3},
	}
}

func TestRoundTrip(t *testing.T) {
	want := fullHeader()
	buf := &bytes.Buffer{}
	if err := Write(buf, want, testCodestream); err != nil {
		t.Fatal(err)
	}
	got, cs, err := Read(bytes.NewReader(buf.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("round trip failed (-want +got):\n%s", d)
	}
	data, _ := io.ReadAll(cs)
	if !bytes.Equal(data, testCodestream) {
		t.Errorf("wrong codestream %x", data)
	}
}

func TestMinimalHeader(t *testing.T) {
	want := &Header{HasWrapper: true, Brand: "jp2 ",
		Image: &ImageHeader{Height: 1, Width: 1, NC: 1, BPC: BitDepth{8, false}, C: 7}}
	buf := &bytes.Buffer{}
	if err := Write(buf, want, testCodestream); err != nil {
		t.Fatal(err)
	}
	got, _, err := Read(buf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("round trip failed (-want +got):\n%s", d)
	}
}

func TestWriteInvalid(t *testing.T) {
	h := fullHeader()
	h.Palette.Values[1] = []int32{1}
	if err := Write(io.Discard, h, nil); err == nil {
		t.Error("bad palette accepted")
	}
	h = fullHeader()
	h.Brand = "x"
	if err := Write(io.Discard, h, nil); err == nil {
		t.Error("bad brand accepted")
	}
	h = fullHeader()
	h.Image.BPC = BitDepth{Depth: 0, Signed: true}
	if err := Write(io.Discard, h, nil); err == nil {
		t.Error("signed unspecified depth accepted")
	}
	h = fullHeader()
	h.BitDepths[0].Depth = 39
	if err := Write(io.Discard, h, nil); err == nil {
		t.Error("depth 39 accepted")
	}
}

func box(typ string, payload ...[]byte) []byte {
	return appendBox(nil, typ, bytes.Join(payload, nil))
}

func ihdrBox() []byte {
	p := make([]byte, 14)
	binary.BigEndian.PutUint32(p, 2)
	binary.BigEndian.PutUint32(p[4:], 3)
	binary.BigEndian.PutUint16(p[8:], 1)
	p[10] = 7
	p[11] = 7
	return box("ihdr", p)
}

func TestToEndOfFile(t *testing.T) {
	file := bytes.Join([][]byte{signature, box("ftyp", []byte("jp2 \x00\x00\x00\x00jp2 ")),
		box("jp2h", ihdrBox())}, nil)
	file = append(file, 0, 0, 0, 0, 'j', 'p', '2', 'c')
	file = append(file, testCodestream...)
	h, cs, err := Read(bytes.NewReader(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.Image == nil || h.Image.Width != 3 || h.Image.BPC != (BitDepth{8, false}) {
		t.Errorf("wrong header %+v", h.Image)
	}
	data, _ := io.ReadAll(cs)
	if !bytes.Equal(data, testCodestream) {
		t.Errorf("wrong codestream %x", data)
	}
}

func TestBareCodestream(t *testing.T) {
	h, cs, err := Read(bytes.NewReader(testCodestream), nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.HasWrapper {
		t.Error("bare codestream reported as wrapped")
	}
	data, _ := io.ReadAll(cs)
	if !bytes.Equal(data, testCodestream) {
		t.Errorf("wrong codestream %x", data)
	}
}

func TestGarbage(t *testing.T) {
	for _, in := range [][]byte{nil, {1}, []byte("hello world, this is not jp2"), make([]byte, 100),
		bytes.Repeat([]byte{0xFF}, 100)} {
		if _, _, err := Read(bytes.NewReader(in), nil); err == nil {
			t.Errorf("garbage %q accepted", in)
		}
	}
}

func TestUnknownAndExtendedBoxes(t *testing.T) {
	// extended-length unknown box before jp2h, nested unknown box in jp2h
	xl := binary.BigEndian.AppendUint32(nil, 1)
	xl = append(xl, "abcd"...)
	xl = binary.BigEndian.AppendUint64(xl, 16+5)
	xl = append(xl, 1, 2, 3, 4, 5)

	nested := box("zzzz", box("yyyy", []byte{1, 2, 3}))
	jp2h := box("jp2h", nested, ihdrBox(), nested)
	jp2h2 := box("jp2h", box("ihdr", make([]byte, 14))) // ignored duplicate

	xlc := binary.BigEndian.AppendUint32(nil, 1)
	xlc = append(xlc, "jp2c"...)
	xlc = binary.BigEndian.AppendUint64(xlc, uint64(16+len(testCodestream)))
	xlc = append(xlc, testCodestream...)

	file := bytes.Join([][]byte{signature, xl, box("ftyp", []byte("jpx \x00\x00\x00\x00jp2 ")),
		jp2h, jp2h2, xlc, []byte("trailing garbage")}, nil)
	h, cs, err := Read(bytes.NewReader(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.Image == nil || h.Image.Height != 2 || h.Brand != "jpx " {
		t.Errorf("wrong header %+v", h)
	}
	data, _ := io.ReadAll(cs)
	if !bytes.Equal(data, testCodestream) {
		t.Errorf("wrong codestream %x", data)
	}
}

func TestMissingJp2h(t *testing.T) {
	file := bytes.Join([][]byte{signature, box("jp2c", testCodestream)}, nil)
	h, cs, err := Read(bytes.NewReader(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.Image != nil {
		t.Error("unexpected image header")
	}
	data, _ := io.ReadAll(cs)
	if !bytes.Equal(data, testCodestream) {
		t.Errorf("wrong codestream %x", data)
	}
}

func TestInvalidLengths(t *testing.T) {
	good := bytes.Join([][]byte{signature, box("jp2h", ihdrBox()), box("jp2c", testCodestream)}, nil)
	mutate := func(off int, v uint32) []byte {
		b := bytes.Clone(good)
		binary.BigEndian.PutUint32(b[off:], v)
		return b
	}
	cases := map[string][]byte{
		"short box":       mutate(12, 4),
		"jp2h too long":   mutate(12, 1<<20),
		"ihdr exceeds":    mutate(20, 1000),
		"huge header box": mutate(12, 0xFFFFFFF0),
		"no codestream":   bytes.Join([][]byte{signature, box("free", []byte{1})}, nil),
		"jp2h to the end": bytes.Join([][]byte{signature, []byte{0, 0, 0, 0}, []byte("jp2h")}, nil),
		"xl too small":    bytes.Join([][]byte{signature, []byte{0, 0, 0, 1}, []byte("free"), make([]byte, 8)}, nil),
		"xl huge":         bytes.Join([][]byte{signature, []byte{0, 0, 0, 1}, []byte("free"), bytes.Repeat([]byte{0xFF}, 8)}, nil),
		"truncated skip":  bytes.Join([][]byte{signature, []byte{0, 0, 1, 0}, []byte("free"), []byte{1}}, nil),
	}
	for name, in := range cases {
		if _, _, err := Read(bytes.NewReader(in), nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPrefixes(t *testing.T) {
	buf := &bytes.Buffer{}
	if err := Write(buf, fullHeader(), testCodestream); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	for n := range len(data) {
		h, cs, err := Read(bytes.NewReader(data[:n]), nil)
		if err == nil {
			io.Copy(io.Discard, cs)
			_ = h
		}
	}
	// any prefix short of the jp2c payload must fail
	for n := range len(data) - len(testCodestream) - 8 {
		if _, _, err := Read(bytes.NewReader(data[:n]), nil); err == nil {
			t.Errorf("prefix of length %d accepted", n)
		}
	}
}

func ftblFile(t *testing.T, dr uint16) ([]byte, []byte) {
	t.Helper()
	pre := bytes.Join([][]byte{signature, box("ftyp", []byte("jpx \x00\x00\x00\x00jpx "))}, nil)
	// two fragments stored in separate free-form boxes after the ftbl
	flstLen := 2 + 2*14
	ftblLen := 8 + 8 + flstLen
	base := len(pre) + ftblLen
	frag1 := box("free", testCodestream[:5])
	frag2 := box("free", testCodestream[5:])
	p := binary.BigEndian.AppendUint16(nil, 2)
	p = binary.BigEndian.AppendUint64(p, uint64(base+8))
	p = binary.BigEndian.AppendUint32(p, 5)
	p = binary.BigEndian.AppendUint16(p, 0)
	p = binary.BigEndian.AppendUint64(p, uint64(base+len(frag1)+8))
	p = binary.BigEndian.AppendUint32(p, uint32(len(testCodestream)-5))
	p = binary.BigEndian.AppendUint16(p, dr)
	ftbl := box("ftbl", box("flst", p))
	if len(ftbl) != ftblLen {
		t.Fatalf("bad ftbl length")
	}
	return bytes.Join([][]byte{pre, ftbl, frag1, frag2}, nil), testCodestream
}

func TestFragmentTable(t *testing.T) {
	file, want := ftblFile(t, 0)
	_, cs, err := Read(bytes.NewReader(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(cs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("wrong codestream %x", got)
	}

	// plain reader without ReaderAt
	if _, _, err := Read(io.MultiReader(bytes.NewReader(file)), nil); err == nil {
		t.Error("fragment table accepted on plain reader")
	}

	// external data reference
	file, _ = ftblFile(t, 1)
	if _, _, err := Read(bytes.NewReader(file), nil); err == nil {
		t.Error("external fragment accepted")
	}
}

func FuzzRead(f *testing.F) {
	buf := &bytes.Buffer{}
	Write(buf, fullHeader(), testCodestream)
	f.Add(buf.Bytes())
	f.Add(testCodestream)
	f.Fuzz(func(t *testing.T, data []byte) {
		_, cs, err := Read(bytes.NewReader(data), nil)
		if err == nil {
			io.Copy(io.Discard, cs)
		}
	})
}

func jp2File(inner ...[]byte) []byte {
	return bytes.Join([][]byte{signature, box("jp2h", inner...), box("jp2c", testCodestream)}, nil)
}

func readCodestream(t *testing.T, file []byte) *Header {
	t.Helper()
	h, cs, err := Read(bytes.NewReader(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(cs)
	if !bytes.Equal(data, testCodestream) {
		t.Errorf("wrong codestream %x", data)
	}
	return h
}

func TestChargeICC(t *testing.T) {
	icc := bytes.Repeat([]byte{0xAB}, 100<<10)
	colr := box("colr", []byte{2, 0, 0}, icc)
	file := jp2File(ihdrBox(), colr)

	total := 0
	h, cs, err := Read(bytes.NewReader(file), func(n int) error {
		total += n
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if total < len(icc) {
		t.Errorf("charged only %d bytes for a %d byte profile", total, len(icc))
	}
	if len(h.Colors) != 1 || !bytes.Equal(h.Colors[0].ICC, icc) {
		t.Error("profile not returned intact")
	}
	if data, _ := io.ReadAll(cs); !bytes.Equal(data, testCodestream) {
		t.Errorf("wrong codestream %x", data)
	}

	errBudget := errors.New("budget exceeded")
	used := 0
	_, _, err = Read(bytes.NewReader(file), func(n int) error {
		used += n
		if used > 16<<10 {
			return errBudget
		}
		return nil
	})
	if !errors.Is(err, errBudget) {
		t.Errorf("budget error not returned: %v", err)
	}
}

func TestChargeBeforeDeclaredLength(t *testing.T) {
	// a huge declared box with almost no data must be stopped by the budget
	// or by truncation after at most one chunk
	file := bytes.Join([][]byte{signature, {0x7F, 0xFF, 0xFF, 0xFF}, []byte("jp2h"), {1, 2, 3}}, nil)
	total := 0
	_, _, err := Read(bytes.NewReader(file), func(n int) error {
		total += n
		return nil
	})
	if err == nil {
		t.Error("truncated box accepted")
	}
	if total > chunkSize {
		t.Errorf("charged %d bytes for a 3 byte input", total)
	}
}

func TestMalformedBoxesSkipped(t *testing.T) {
	good := box("colr", []byte{1, 0, 0, 0, 0, 0, 16})
	cases := map[string][]byte{
		"colr short":    box("colr", []byte{1, 0}),
		"colr enum":     box("colr", []byte{1, 0, 0, 0, 0}),
		"cdef count":    box("cdef", []byte{0, 5, 0, 0, 0, 0, 0, 1}),
		"cdef tiny":     box("cdef", []byte{0}),
		"cmap length":   box("cmap", []byte{0, 0, 1, 0, 7}),
		"pclr entries":  box("pclr", []byte{0, 0, 1, 7}),
		"pclr short":    box("pclr", []byte{0, 2, 1, 7, 1}),
		"pclr depth 0":  box("pclr", []byte{0, 1, 1, 0xFF, 1}),
		"pclr depth 40": box("pclr", []byte{0, 1, 1, 39, 1, 2, 3, 4, 5}),
		"bpcc depth 40": box("bpcc", []byte{7, 39, 7}),
		"ihdr length":   box("ihdr", []byte{1}),
		"ihdr long":     box("ihdr", make([]byte, 15)),
		"ihdr depth 40": box("ihdr", []byte{0, 0, 0, 1, 0, 0, 0, 1, 0, 1, 39, 7, 0, 0}),
	}
	for name, bad := range cases {
		h := readCodestream(t, jp2File(bad, good))
		if len(h.Colors) != 1 || h.Colors[0].EnumCS != 16 {
			t.Errorf("%s: later box lost: %+v", name, h.Colors)
		}
		if h.Image != nil || h.Palette != nil || h.ComponentMap != nil ||
			h.Channels != nil || h.BitDepths != nil {
			if name != "colr short" && name != "colr enum" {
				t.Errorf("%s: broken box not skipped: %+v", name, h)
			}
		}
	}
	// the broken colr boxes are dropped too
	for _, name := range []string{"colr short", "colr enum"} {
		h := readCodestream(t, jp2File(cases[name]))
		if h.Colors != nil {
			t.Errorf("%s: broken colr kept", name)
		}
	}
}

func TestBpccMismatch(t *testing.T) {
	h := readCodestream(t, jp2File(ihdrBox(), box("bpcc", []byte{7, 7})))
	if h.BitDepths != nil {
		t.Errorf("bpcc with wrong length kept: %v", h.BitDepths)
	}
	h = readCodestream(t, jp2File(box("bpcc", []byte{7, 7}), ihdrBox()))
	if h.BitDepths != nil {
		t.Errorf("bpcc with wrong length kept: %v", h.BitDepths)
	}
	h = readCodestream(t, jp2File(ihdrBox(), box("bpcc", []byte{0x87})))
	if want := []BitDepth{{8, true}}; !slices.Equal(h.BitDepths, want) {
		t.Errorf("wrong bit depths %v", h.BitDepths)
	}
}

func TestDeepPaletteIgnored(t *testing.T) {
	p := []byte{0, 1, 1, 31, 1, 2, 3, 4, 5}
	h := readCodestream(t, jp2File(ihdrBox(), box("pclr", p)))
	if h.Palette != nil {
		t.Error("32 bit palette accepted")
	}
	if h.Image == nil {
		t.Error("other header boxes lost")
	}
}

func TestJp2hAfterFtbl(t *testing.T) {
	file, want := ftblFile(t, 0)
	// insert a jp2h box after the ftbl; fragments are located by absolute
	// offset, so build the file around a fixed-size ftbl
	jp2h := box("jp2h", ihdrBox())
	i := bytes.Index(file, []byte("ftbl")) - 4
	ftblEnd := i + 8 + 8 + 2 + 28
	shifted := bytes.Join([][]byte{file[:ftblEnd], jp2h, file[ftblEnd:]}, nil)
	// fix up fragment offsets
	off := i + 8 + 8 + 2
	binary.BigEndian.PutUint64(shifted[off:], binary.BigEndian.Uint64(shifted[off:])+uint64(len(jp2h)))
	binary.BigEndian.PutUint64(shifted[off+14:], binary.BigEndian.Uint64(shifted[off+14:])+uint64(len(jp2h)))
	h, cs, err := Read(bytes.NewReader(shifted), nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.Image == nil || h.Image.Width != 3 {
		t.Error("jp2h after ftbl not parsed")
	}
	got, _ := io.ReadAll(cs)
	if !bytes.Equal(got, want) {
		t.Errorf("wrong codestream %x", got)
	}
}

func TestJp2cBeatsFtbl(t *testing.T) {
	file, _ := ftblFile(t, 1) // unusable table
	file = append(file, box("jp2c", testCodestream)...)
	readCodestream(t, file)
}

func TestWriteValidation(t *testing.T) {
	bad := map[string]func(h *Header){
		"unsigned overflow": func(h *Header) { h.Palette.Values[0][0] = 1 << 20 },
		"negative unsigned": func(h *Header) { h.Palette.Values[0][0] = -1 },
		"signed overflow": func(h *Header) {
			h.Palette.Depths[0] = BitDepth{Depth: 4, Signed: true}
			h.Palette.Values[0][0] = 8
		},
		"icc on enumerated": func(h *Header) { h.Colors = []ColorSpec{{Method: 1, EnumCS: 16, ICC: []byte{1}}} },
		"enum on icc":       func(h *Header) { h.Colors = []ColorSpec{{Method: 2, EnumCS: 16}} },
		"params on icc":     func(h *Header) { h.Colors = []ColorSpec{{Method: 3, EnumParams: []byte{1}}} },
	}
	for name, f := range bad {
		h := fullHeader()
		h.Palette.Depths[0] = BitDepth{Depth: 8}
		f(h)
		if err := Write(io.Discard, h, nil); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := Write(io.Discard, nil, nil); err == nil {
		t.Error("nil header accepted")
	}
}

func TestWriteEmptySlicesOmitted(t *testing.T) {
	h := &Header{BitDepths: []BitDepth{}, ComponentMap: []ComponentMapping{}}
	buf := &bytes.Buffer{}
	if err := Write(buf, h, testCodestream); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"bpcc", "cmap"} {
		if bytes.Contains(buf.Bytes(), []byte(typ)) {
			t.Errorf("empty %s box written", typ)
		}
	}
}

func TestWriteCompatWithoutBrand(t *testing.T) {
	buf := &bytes.Buffer{}
	if err := Write(buf, &Header{Compat: []string{"jpx "}}, testCodestream); err != nil {
		t.Fatal(err)
	}
	h, _, err := Read(bytes.NewReader(buf.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.Brand != "jp2 " || !slices.Equal(h.Compat, []string{"jpx "}) {
		t.Errorf("got brand %q compat %q", h.Brand, h.Compat)
	}
}

func TestDuplicatesFirstWins(t *testing.T) {
	ihdr2 := box("ihdr", []byte{0, 0, 0, 9, 0, 0, 0, 9, 0, 1, 7, 7, 0, 0})
	pclr := func(v byte) []byte { return box("pclr", []byte{0, 1, 1, 7, v}) }
	cdef := func(a byte) []byte { return box("cdef", []byte{0, 1, 0, 0, 0, 0, 0, a}) }
	h := readCodestream(t, jp2File(ihdrBox(), ihdr2, pclr(1), pclr(2), cdef(1), cdef(2)))
	if h.Image.Width != 3 {
		t.Errorf("second ihdr won")
	}
	if h.Palette.Values[0][0] != 1 {
		t.Errorf("second pclr won")
	}
	if h.Channels[0].Assoc != 1 {
		t.Errorf("second cdef won")
	}
}

func TestFtypEdgeCases(t *testing.T) {
	cases := []struct {
		name   string
		data   []byte
		brand  string
		compat []string
	}{
		{"empty", nil, "", nil},
		{"short", []byte("jp2 \x00\x00"), "", nil},
		{"no compat", []byte("jp2 \x00\x00\x00\x00"), "jp2 ", nil},
		{"partial entry", []byte("jp2 \x00\x00\x00\x00jp2 jp"), "jp2 ", []string{"jp2 "}},
		{"two entries", []byte("jpx \x00\x00\x00\x00jp2 jpx "), "jpx ", []string{"jp2 ", "jpx "}},
	}
	for _, c := range cases {
		file := bytes.Join([][]byte{signature, box("ftyp", c.data), box("jp2c", testCodestream)}, nil)
		h := readCodestream(t, file)
		if h.Brand != c.brand {
			t.Errorf("%s: brand %q, want %q", c.name, h.Brand, c.brand)
		}
		if d := cmp.Diff(c.compat, h.Compat); d != "" {
			t.Errorf("%s: compat mismatch (-want +got):\n%s", c.name, d)
		}
	}
}

func TestEmptyFragmentList(t *testing.T) {
	pre := bytes.Join([][]byte{signature, box("ftyp", []byte("jpx \x00\x00\x00\x00jpx "))}, nil)
	for name, flst := range map[string][]byte{
		"empty":     {0, 0},
		"truncated": {0, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5, 0, 0},
	} {
		file := bytes.Join([][]byte{pre, box("ftbl", box("flst", flst))}, nil)
		if _, _, err := Read(bytes.NewReader(file), nil); err == nil {
			t.Errorf("%s fragment list accepted", name)
		}
	}
}
