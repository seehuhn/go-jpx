// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package mq

// state is one row of the probability estimation table.
type state struct {
	qe   uint32 // probability estimate of the less probable symbol
	nmps uint8  // next index after coding a more probable symbol
	nlps uint8  // next index after coding a less probable symbol
	sw   uint8  // 1 if the MPS sense flips after an LPS
}

var table = [47]state{
	{0x5601, 1, 1, 1},
	{0x3401, 2, 6, 0},
	{0x1801, 3, 9, 0},
	{0x0AC1, 4, 12, 0},
	{0x0521, 5, 29, 0},
	{0x0221, 38, 33, 0},
	{0x5601, 7, 6, 1},
	{0x5401, 8, 14, 0},
	{0x4801, 9, 14, 0},
	{0x3801, 10, 14, 0},
	{0x3001, 11, 17, 0},
	{0x2401, 12, 18, 0},
	{0x1C01, 13, 20, 0},
	{0x1601, 29, 21, 0},
	{0x5601, 15, 14, 1},
	{0x5401, 16, 14, 0},
	{0x5101, 17, 15, 0},
	{0x4801, 18, 16, 0},
	{0x3801, 19, 17, 0},
	{0x3401, 20, 18, 0},
	{0x3001, 21, 19, 0},
	{0x2801, 22, 19, 0},
	{0x2401, 23, 20, 0},
	{0x2201, 24, 21, 0},
	{0x1C01, 25, 22, 0},
	{0x1801, 26, 23, 0},
	{0x1601, 27, 24, 0},
	{0x1401, 28, 25, 0},
	{0x1201, 29, 26, 0},
	{0x1101, 30, 27, 0},
	{0x0AC1, 31, 28, 0},
	{0x09C1, 32, 29, 0},
	{0x08A1, 33, 30, 0},
	{0x0521, 34, 31, 0},
	{0x0441, 35, 32, 0},
	{0x02A1, 36, 33, 0},
	{0x0221, 37, 34, 0},
	{0x0141, 38, 35, 0},
	{0x0111, 39, 36, 0},
	{0x0085, 40, 37, 0},
	{0x0049, 41, 38, 0},
	{0x0025, 42, 39, 0},
	{0x0015, 43, 40, 0},
	{0x0009, 44, 41, 0},
	{0x0005, 45, 42, 0},
	{0x0001, 45, 43, 0},
	{0x5601, 46, 46, 0},
}

// Context holds the adaptive state of one coding context: the index into
// the probability table and the current more-probable symbol.  The zero
// value is state 0 with MPS 0.
type Context struct {
	index uint8
	mps   uint8
}

// Reset sets the context to the given initial state.  The index must be
// in the range 0..46 and mps must be 0 or 1.  Index 46 is the uniform
// state; coding itself never moves a context beyond index 45.
func (c *Context) Reset(index, mps uint8) {
	c.index = index
	c.mps = mps
}
