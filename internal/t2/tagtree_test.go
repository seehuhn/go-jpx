// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package t2

import (
	"math/rand/v2"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestBitsRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for iter := range 300 {
		n := rng.IntN(200)
		bits := make([]int, n)
		ones := iter%2 == 0
		for i := range bits {
			if ones && rng.IntN(10) != 0 {
				bits[i] = 1
			} else {
				bits[i] = rng.IntN(2)
			}
		}
		var w BitWriter
		for _, b := range bits {
			w.WriteBit(b)
		}
		data := w.Flush()
		if len(data) > 0 && data[len(data)-1] == 0xFF {
			t.Fatalf("data ends in 0xFF")
		}
		for i := 0; i+1 < len(data); i++ {
			if data[i] == 0xFF && data[i+1]&0x80 != 0 {
				t.Fatalf("missing stuffed bit at %d", i+1)
			}
		}
		r := NewBitReader(data)
		got := make([]int, n)
		for i := range got {
			b, err := r.ReadBit()
			if err != nil {
				t.Fatal(err)
			}
			got[i] = b
		}
		if diff := cmp.Diff(bits, got); diff != "" {
			t.Fatalf("round trip failed (-want +got):\n%s", diff)
		}
		if a := r.Align(); a != len(data) {
			t.Errorf("align = %d, want %d", a, len(data))
		}
	}
}

func TestBitsStuffedEnd(t *testing.T) {
	var w BitWriter
	w.WriteBits(0xFF, 8)
	data := w.Flush()
	if diff := cmp.Diff([]byte{0xFF, 0x00}, data); diff != "" {
		t.Errorf("flush failed (-want +got):\n%s", diff)
	}
	r := NewBitReader(append(data, 0xAA))
	if v, err := r.ReadBits(8); err != nil || v != 0xFF {
		t.Fatalf("read = %x, %v", v, err)
	}
	if a := r.Align(); a != 2 {
		t.Errorf("align = %d, want 2", a)
	}

	w.Reset()
	w.WriteBits(0x7F, 7)
	w.WriteBits(1, 1) // 0111 1111 1 -> 0x7F, 0x80 ... check mixing
	data = w.Flush()
	r = NewBitReader(data)
	v, _ := r.ReadBits(7)
	b, _ := r.ReadBit()
	if v != 0x7F || b != 1 {
		t.Errorf("got %x %d", v, b)
	}
}

func TestBitsEOF(t *testing.T) {
	r := NewBitReader([]byte{0x80})
	if _, err := r.ReadBits(9); err == nil {
		t.Error("expected error")
	}
	r = NewBitReader(nil)
	if _, err := r.ReadBit(); err == nil {
		t.Error("expected error")
	}
	if a := r.Align(); a != 0 {
		t.Errorf("align = %d", a)
	}
}

func TestTreeBitStrings(t *testing.T) {
	read := func(w *BitWriter) []int {
		r := NewBitReader(w.Flush())
		var out []int
		for {
			b, err := r.ReadBit()
			if err != nil {
				return out
			}
			out = append(out, b)
		}
	}
	tr := NewTree(1, 1)
	tr.SetValue(0, 0, 3)
	var w BitWriter
	for th := 1; th <= 4; th++ {
		tr.Encode(&w, 0, 0, th)
	}
	if got := read(&w)[:4]; !cmp.Equal([]int{0, 0, 0, 1}, got) {
		t.Errorf("1x1 bits = %v", got)
	}

	// 2x1 tree, values (1,3): root is 1
	tr = NewTree(2, 1)
	tr.SetValue(0, 0, 1)
	tr.SetValue(1, 0, 3)
	w.Reset()
	tr.Encode(&w, 0, 0, 2)
	tr.Encode(&w, 0, 0, 2) // repeat: nothing
	tr.Encode(&w, 0, 0, 1) // smaller: nothing
	tr.EncodeValue(&w, 1, 0)
	got := read(&w)
	want := []int{0, 1, 1, 0, 0, 1}
	if !cmp.Equal(want, got[:6]) {
		t.Errorf("2x1 bits = %v, want %v", got[:6], want)
	}
	if len(got) > 8 {
		t.Errorf("too many bits: %v", got)
	}
}

func TestTreeRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for iter := range 300 {
		wd, ht := rng.IntN(18), rng.IntN(18)
		if iter < 5 {
			wd = 0
		}
		enc, dec := NewTree(wd, ht), NewTree(wd, ht)
		vals := make([]int, wd*ht)
		for i := range vals {
			vals[i] = rng.IntN(21)
			enc.SetValue(i%wd, i/wd, vals[i])
		}
		type q struct{ x, y, th int }
		var qs []q
		for i := range vals {
			x, y := i%wd, i/wd
			th := 0
			for th <= 21 {
				th += rng.IntN(5)
				qs = append(qs, q{x, y, th})
				if rng.IntN(3) == 0 {
					break
				}
			}
		}
		if iter%2 == 1 {
			rng.Shuffle(len(qs), func(i, j int) { qs[i], qs[j] = qs[j], qs[i] })
		}
		var w BitWriter
		var want []bool
		for _, c := range qs {
			enc.Encode(&w, c.x, c.y, c.th)
			want = append(want, vals[c.y*wd+c.x] < c.th)
		}
		r := NewBitReader(w.Flush())
		for i, c := range qs {
			got, err := dec.Decode(r, c.x, c.y, c.th)
			if err != nil {
				t.Fatal(err)
			}
			if got != want[i] {
				t.Fatalf("query %v: got %v, want %v", c, got, want[i])
			}
			if got && dec.Value(c.x, c.y) != vals[c.y*wd+c.x] {
				t.Fatalf("query %v: value %d, want %d", c, dec.Value(c.x, c.y), vals[c.y*wd+c.x])
			}
		}
	}
}

func TestTreeValue(t *testing.T) {
	enc, dec := NewTree(5, 3), NewTree(5, 3)
	var w BitWriter
	for i := range 15 {
		enc.SetValue(i%5, i/5, (i*7)%11)
	}
	for i := range 15 {
		enc.EncodeValue(&w, i%5, i/5)
	}
	r := NewBitReader(w.Flush())
	for i := range 15 {
		v, err := dec.DecodeValue(r, i%5, i/5, 1000)
		if err != nil || v != (i*7)%11 {
			t.Fatalf("leaf %d: %d, %v", i, v, err)
		}
	}
}

func TestTreeTruncated(t *testing.T) {
	enc := NewTree(4, 4)
	enc.SetValue(3, 3, 9)
	var w BitWriter
	enc.EncodeValue(&w, 3, 3)
	data := w.Flush()
	dec := NewTree(4, 4)
	if _, err := dec.DecodeValue(NewBitReader(data[:1]), 3, 3, 1000); err == nil {
		t.Error("expected error")
	}
}

func TestDecodeValueZeros(t *testing.T) {
	dec := NewTree(2, 2)
	if _, err := dec.DecodeValue(NewBitReader(make([]byte, 16)), 1, 1, 1000); err == nil {
		t.Error("expected error")
	}
}

func TestDecodeValueBounded(t *testing.T) {
	enc := NewTree(1, 1)
	enc.SetValue(0, 0, 7)
	var w BitWriter
	enc.EncodeValue(&w, 0, 0)
	data := w.Flush()
	if v, err := NewTree(1, 1).DecodeValue(NewBitReader(data), 0, 0, 7); err != nil || v != 7 {
		t.Errorf("got %d, %v; want 7", v, err)
	}
	if _, err := NewTree(1, 1).DecodeValue(NewBitReader(data), 0, 0, 6); err == nil {
		t.Error("value above the limit was accepted")
	}
}

func TestReadBitsRange(t *testing.T) {
	r := NewBitReader(make([]byte, 8))
	if _, err := r.ReadBits(33); err == nil {
		t.Error("33 bits were accepted")
	}
	if v, err := r.ReadBits(0); err != nil || v != 0 {
		t.Errorf("0 bits: got %d, %v", v, err)
	}
}

func TestAlignAfterFF(t *testing.T) {
	// a header ending in 0xFF is followed by a stuffed byte
	var w BitWriter
	w.WriteBits(0xFF, 8)
	data := w.Flush()
	if len(data) != 2 {
		t.Fatalf("got %d bytes, want 2", len(data))
	}
	r := NewBitReader(append(data, 0xAB))
	if _, err := r.ReadBits(8); err != nil {
		t.Fatal(err)
	}
	if n := r.Align(); n != 2 {
		t.Errorf("Align after trailing 0xFF consumed %d bytes, want 2", n)
	}
	// a 0xFF that is the last byte available does not make Align overrun
	r = NewBitReader([]byte{0xFF})
	if _, err := r.ReadBits(8); err != nil {
		t.Fatal(err)
	}
	if n := r.Align(); n != 1 {
		t.Errorf("Align at end consumed %d bytes, want 1", n)
	}
}
