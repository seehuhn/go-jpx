// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

// Package dwt implements the two-dimensional discrete wavelet transforms of
// JPEG 2000: the reversible 5/3 transform on int32 samples and the
// irreversible 9/7 transform on float32 samples.
//
// All transforms work in place on a rectangular region of the coordinate
// plane, stored row major without padding.  The coordinates matter because
// the split into low-pass and high-pass samples depends on the parity of the
// region origin.  After the forward transform the buffer holds the nested
// layout: at each level the current region is split into LL (top left), HL
// (top right), LH (bottom left) and HH (bottom right), and the next level
// recurses into the LL block, which always sits at the top left of the
// current region.  The stride is the width of the full region throughout.
//
// The functions assume valid arguments: len(buf) must equal the area of the
// region, the region must not have negative extent, and levels must not be
// negative.  Callers decoding untrusted data validate these before calling.
// Levels beyond the point where the region has shrunk to nothing are
// ignored, and the only allocation is a scratch line of O(width+height).
package dwt
