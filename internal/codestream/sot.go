// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package codestream

import (
	"encoding/binary"
	"errors"
)

// sotLen is the total size of an SOT marker segment including the marker.
const sotLen = 12

func parseSOT(p []byte) (index int, psot uint32, part, count int, err error) {
	if len(p) != 8 {
		return 0, 0, 0, 0, errors.New("SOT segment has wrong length")
	}
	return int(binary.BigEndian.Uint16(p)), binary.BigEndian.Uint32(p[2:]),
		int(p[6]), int(p[7]), nil
}
