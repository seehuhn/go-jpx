// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package codestream

import "errors"

// RGN is a region-of-interest marker segment (implicit max-shift method).
type RGN struct {
	Component int
	Shift     int // 0..255
}

func parseRGN(p []byte, csiz int) (RGN, error) {
	c, p, err := readComponentIndex(p, csiz)
	if err != nil {
		return RGN{}, err
	}
	if len(p) != 2 {
		return RGN{}, errors.New("RGN segment has wrong length")
	}
	if p[0] != 0 {
		return RGN{}, errors.New("unsupported ROI style")
	}
	return RGN{Component: c, Shift: int(p[1])}, nil
}

func encodeRGN(r RGN, csiz int) ([]byte, error) {
	if r.Component < 0 || r.Component >= csiz || r.Shift < 0 || r.Shift > 255 {
		return nil, errors.New("invalid RGN")
	}
	return append(appendComponentIndex(nil, r.Component, csiz), 0, byte(r.Shift)), nil
}
