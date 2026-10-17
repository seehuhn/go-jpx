// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

// Package t2 implements the tier-2 building blocks of the codec: packet
// header bit I/O with 0xFF bit stuffing and tag trees.
//
// The readers never panic on short input; they return an error instead.
package t2
