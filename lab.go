// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package jpx

import "encoding/binary"

// Enumerated colour space value for CIELab.
const enumCIELab = 14

// Illuminant codes for [LabParams.IL].
//
// Besides these codes, an illuminant can be given as a colour temperature:
// the first two bytes of IL are the ASCII characters 'C' and 'T', and the
// last two bytes are the temperature in Kelvin as a big-endian uint16.
// For example, 7500 K is 0x43541D4C.  Use [LabParams.ColorTemperature] to
// decode this form.
const (
	IlluminantD50 uint32 = 0x00443530
	IlluminantD65 uint32 = 0x00443635
	IlluminantD75 uint32 = 0x00443735
	IlluminantSA  uint32 = 0x00005341
	IlluminantSC  uint32 = 0x00005343
	IlluminantF2  uint32 = 0x00004632
	IlluminantF7  uint32 = 0x00004637
	IlluminantF11 uint32 = 0x00463131
)

// LabParams holds the parameters of the CIELab enumerated colour space.
// For a component with n bits, the encoded value v is mapped to
// L* = (v-OL)*RL/(2^n-1) for the L channel, and analogously for the a*
// and b* channels using (RA, OA) and (RB, OB).  The ranges are never zero.
type LabParams struct {
	// RL and OL are the range and offset of the L* channel.
	RL, OL uint32

	// RA and OA are the range and offset of the a* channel.
	RA, OA uint32

	// RB and OB are the range and offset of the b* channel.
	RB, OB uint32

	// IL is the illuminant, either one of the Illuminant constants or
	// a colour temperature code (see [LabParams.ColorTemperature]).
	IL uint32
}

// ColorTemperature decodes an illuminant given as a colour temperature.
// If IL is not of this form, ok is false.
func (p LabParams) ColorTemperature() (kelvin int, ok bool) {
	if p.IL>>16 != 'C'<<8|'T' {
		return 0, false
	}
	return int(p.IL & 0xFFFF), true
}

// LabParams returns the CIELab parameters of a colour specification.
// The depths are the bit depths of the three colour channels, from 1 to
// [MaxDepth]; they are used only to compute the default offsets when the
// specification gives no parameters.
//
// The result is false if the specification is not an enumerated CIELab
// colour space, if the parameters are malformed, or if the default offsets
// do not fit in 32 bits.
func (s ColorSpec) LabParams(depths [3]int) (LabParams, bool) {
	if s.Method != 1 || s.Enumerated != enumCIELab {
		return LabParams{}, false
	}

	var p LabParams
	switch len(s.EnumParams) {
	case 0:
		for _, n := range depths {
			if n < 1 || n > MaxDepth {
				return LabParams{}, false
			}
		}
		na, nb := depths[1], depths[2]
		if nb < 3 {
			return LabParams{}, false
		}
		oa := uint64(1) << (na - 1)
		ob := uint64(1)<<(nb-2) + uint64(1)<<(nb-3)
		if oa > 0xFFFFFFFF || ob > 0xFFFFFFFF {
			return LabParams{}, false
		}
		p = LabParams{
			RL: 100, OL: 0,
			RA: 170, OA: uint32(oa),
			RB: 200, OB: uint32(ob),
			IL: IlluminantD50,
		}
	case 28:
		ep := s.EnumParams
		f := func(i int) uint32 { return binary.BigEndian.Uint32(ep[4*i:]) }
		p = LabParams{RL: f(0), OL: f(1), RA: f(2), OA: f(3), RB: f(4), OB: f(5), IL: f(6)}
	default:
		return LabParams{}, false
	}

	if p.RL == 0 || p.RA == 0 || p.RB == 0 {
		return LabParams{}, false
	}
	return p, true
}
