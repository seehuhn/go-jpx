// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package codestream

import "strconv"

// Marker codes.
const (
	markerSOC = 0xFF4F
	markerCAP = 0xFF50
	markerSIZ = 0xFF51
	markerCOD = 0xFF52
	markerCOC = 0xFF53
	markerTLM = 0xFF55
	markerPRF = 0xFF56
	markerPLM = 0xFF57
	markerPLT = 0xFF58
	markerQCD = 0xFF5C
	markerQCC = 0xFF5D
	markerRGN = 0xFF5E
	markerPOC = 0xFF5F
	markerPPM = 0xFF60
	markerPPT = 0xFF61
	markerCRG = 0xFF63
	markerCOM = 0xFF64
	markerSOT = 0xFF90
	markerSOP = 0xFF91
	markerEPH = 0xFF92
	markerSOD = 0xFF93
	markerEOC = 0xFFD9
)

// Progression is a packet progression order.
type Progression uint8

// Progression orders.
const (
	LRCP Progression = iota
	RLCP
	RPCL
	PCRL
	CPRL
)

// String returns the four-letter name of the progression order.
func (p Progression) String() string {
	switch p {
	case LRCP:
		return "LRCP"
	case RLCP:
		return "RLCP"
	case RPCL:
		return "RPCL"
	case PCRL:
		return "PCRL"
	case CPRL:
		return "CPRL"
	}
	return "Progression(" + strconv.Itoa(int(p)) + ")"
}

// QuantStyle is a quantization style.
type QuantStyle uint8

// Quantization styles.
const (
	// QuantNone is used with the reversible transform; only exponents are given.
	QuantNone QuantStyle = iota
	// QuantDerived gives one exponent and mantissa; the others are derived.
	QuantDerived
	// QuantExpounded gives an exponent and mantissa for every subband.
	QuantExpounded
)

// String returns a descriptive name of the quantization style.
func (q QuantStyle) String() string {
	switch q {
	case QuantNone:
		return "None"
	case QuantDerived:
		return "ScalarDerived"
	case QuantExpounded:
		return "ScalarExpounded"
	}
	return "QuantStyle(" + strconv.Itoa(int(q)) + ")"
}

// isNoLength reports whether the marker has no length field.
func isNoLength(m uint16) bool {
	return m >= 0xFF30 && m <= 0xFF3F ||
		m == markerSOC || m == markerSOD || m == markerEOC || m == markerEPH
}
