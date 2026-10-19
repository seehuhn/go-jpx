// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jpx

import (
	"encoding/binary"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func labEP(v ...uint32) []byte {
	var b []byte
	for _, x := range v {
		b = binary.BigEndian.AppendUint32(b, x)
	}
	return b
}

func TestColorSpecLabParams(t *testing.T) {
	d8 := [3]int{8, 8, 8}
	explicit := labEP(100, 0, 170, 128, 200, 96, IlluminantD65)
	cases := []struct {
		name   string
		spec   ColorSpec
		depths [3]int
		want   LabParams
		ok     bool
	}{
		{"defaults 8 bit", ColorSpec{Method: 1, Enumerated: 14}, d8,
			LabParams{100, 0, 170, 128, 200, 96, IlluminantD50}, true},
		{"defaults 16 bit", ColorSpec{Method: 1, Enumerated: 14}, [3]int{16, 16, 16},
			LabParams{100, 0, 170, 32768, 200, 24576, IlluminantD50}, true},
		{"defaults max depth", ColorSpec{Method: 1, Enumerated: 14}, [3]int{32, 32, 32},
			LabParams{100, 0, 170, 1 << 31, 200, 1<<30 + 1<<29, IlluminantD50}, true},
		{"defaults b depth 33", ColorSpec{Method: 1, Enumerated: 14}, [3]int{8, 8, 33},
			LabParams{100, 0, 170, 128, 200, 1<<31 + 1<<30, IlluminantD50}, true},
		{"explicit", ColorSpec{Method: 1, Enumerated: 14, EnumParams: explicit}, d8,
			LabParams{100, 0, 170, 128, 200, 96, IlluminantD65}, true},
		{"explicit ignores depths", ColorSpec{Method: 1, Enumerated: 14, EnumParams: explicit}, [3]int{},
			LabParams{100, 0, 170, 128, 200, 96, IlluminantD65}, true},
		{"wrong method", ColorSpec{Method: 2, Enumerated: 14}, d8, LabParams{}, false},
		{"wrong space", ColorSpec{Method: 1, Enumerated: 16}, d8, LabParams{}, false},
		{"bad length", ColorSpec{Method: 1, Enumerated: 14, EnumParams: explicit[:24]}, d8, LabParams{}, false},
		{"zero RL", ColorSpec{Method: 1, Enumerated: 14, EnumParams: labEP(0, 0, 170, 128, 200, 96, 0)}, d8, LabParams{}, false},
		{"zero RA", ColorSpec{Method: 1, Enumerated: 14, EnumParams: labEP(100, 0, 0, 128, 200, 96, 0)}, d8, LabParams{}, false},
		{"zero RB", ColorSpec{Method: 1, Enumerated: 14, EnumParams: labEP(100, 0, 170, 128, 0, 96, 0)}, d8, LabParams{}, false},
		{"L depth too small", ColorSpec{Method: 1, Enumerated: 14}, [3]int{0, 8, 8}, LabParams{}, false},
		{"L depth too large", ColorSpec{Method: 1, Enumerated: 14}, [3]int{35, 8, 8}, LabParams{}, false},
		{"a depth too small", ColorSpec{Method: 1, Enumerated: 14}, [3]int{8, 0, 8}, LabParams{}, false},
		{"a depth overflow", ColorSpec{Method: 1, Enumerated: 14}, [3]int{8, 34, 8}, LabParams{}, false},
		{"a depth too large", ColorSpec{Method: 1, Enumerated: 14}, [3]int{8, 39, 8}, LabParams{}, false},
		{"b depth too small", ColorSpec{Method: 1, Enumerated: 14}, [3]int{8, 8, 2}, LabParams{}, false},
		{"b depth overflow", ColorSpec{Method: 1, Enumerated: 14}, [3]int{8, 8, 34}, LabParams{}, false},
		{"b depth too large", ColorSpec{Method: 1, Enumerated: 14}, [3]int{8, 8, 39}, LabParams{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := c.spec.LabParams(c.depths)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if diff := cmp.Diff(c.want, got); diff != "" {
				t.Errorf("params differ (-want +got):\n%s", diff)
			}
		})
	}
}

func TestLabParamsColorTemperature(t *testing.T) {
	k, ok := LabParams{IL: 0x43541D4C}.ColorTemperature()
	if !ok || k != 7500 {
		t.Errorf("got %d, %v, want 7500, true", k, ok)
	}
	for _, il := range []uint32{IlluminantD50, IlluminantD65, IlluminantF11, 0} {
		if _, ok := (LabParams{IL: il}).ColorTemperature(); ok {
			t.Errorf("IL %#x: unexpected colour temperature", il)
		}
	}
}
