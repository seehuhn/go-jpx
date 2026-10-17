// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package t2

import "errors"

type node struct {
	value int  // true value (encoder side)
	low   int  // current lower bound
	known bool // value determined
}

type level struct {
	w, h  int
	nodes []node
}

// Tree is a tag tree over a w×h array of non-negative integers.  Level 0
// holds the leaves; each higher level halves the size (rounding up) until
// a single root remains.
type Tree struct {
	levels []level
}

// maxTreeLeaves bounds the leaf count of a tree.  Callers charge the
// memory for their trees to a budget before constructing them; the bound
// only protects against integer overflow in the size computation.
const maxTreeLeaves = 1 << 30

// NewTree creates a tree for w×h leaves.  A tree with no leaves is
// allowed.  The leaf count must not exceed 2^30; the caller is
// responsible for charging the O(w*h) memory to its budget first.
func NewTree(w, h int) *Tree {
	t := &Tree{}
	if w <= 0 || h <= 0 {
		return t
	}
	if w > maxTreeLeaves || h > maxTreeLeaves || w*h > maxTreeLeaves {
		panic("t2: tag tree too large")
	}
	for {
		t.levels = append(t.levels, level{w, h, make([]node, w*h)})
		if w == 1 && h == 1 {
			break
		}
		w, h = (w+1)/2, (h+1)/2
	}
	return t
}

// Reset forgets all values and coding state.
func (t *Tree) Reset() {
	for _, l := range t.levels {
		clear(l.nodes)
	}
}

func (t *Tree) inRange(x, y int) bool {
	return len(t.levels) > 0 && x >= 0 && y >= 0 && x < t.levels[0].w && y < t.levels[0].h
}

// SetValue sets the value of leaf (x,y) for encoding.  Values must be
// non-negative and must be set before encoding starts; leaves that are
// never set have value zero.  Leaves outside the tree are ignored.
func (t *Tree) SetValue(x, y, v int) {
	if !t.inRange(x, y) || v < 0 {
		return
	}
	t.levels[0].nodes[y*t.levels[0].w+x].value = v
	// recompute the minima along the path to the root
	for i := 1; i < len(t.levels); i++ {
		x, y = x/2, y/2
		child := &t.levels[i-1]
		m := int(^uint(0) >> 1)
		for cy := 2 * y; cy < min(2*y+2, child.h); cy++ {
			for cx := 2 * x; cx < min(2*x+2, child.w); cx++ {
				m = min(m, child.nodes[cy*child.w+cx].value)
			}
		}
		t.levels[i].nodes[y*t.levels[i].w+x].value = m
	}
}

// maxLevels bounds the depth of a tree; with at most 2^30 leaves the
// depth is at most 31.
const maxLevels = 32

// path returns the nodes from the root down to leaf (x,y).
func (t *Tree) path(x, y int, buf *[maxLevels]*node) []*node {
	p := buf[:0]
	for i := len(t.levels) - 1; i >= 0; i-- {
		l := &t.levels[i]
		p = append(p, &l.nodes[(y>>uint(i))*l.w+(x>>uint(i))])
	}
	return p
}

// Encode emits the bits that let a decoder learn whether the value of leaf
// (x,y) is below threshold, and its exact value if it is.
func (t *Tree) Encode(w *BitWriter, x, y, threshold int) {
	if !t.inRange(x, y) {
		return
	}
	var buf [maxLevels]*node
	low := 0
	for _, n := range t.path(x, y, &buf) {
		low = max(low, n.low)
		n.low = low
		for !n.known && n.low < threshold {
			if n.value == n.low {
				w.WriteBit(1)
				n.known = true
			} else {
				w.WriteBit(0)
				n.low++
			}
		}
		low = n.low
	}
}

// Decode reads the bits written by Encode with the same arguments.  It
// reports whether the value of leaf (x,y) is known and below threshold.
func (t *Tree) Decode(r *BitReader, x, y, threshold int) (bool, error) {
	if !t.inRange(x, y) {
		return false, errors.New("tag tree leaf out of range")
	}
	var buf [maxLevels]*node
	low := 0
	var leaf *node
	for _, n := range t.path(x, y, &buf) {
		low = max(low, n.low)
		n.low = low
		for !n.known && n.low < threshold {
			b, err := r.ReadBit()
			if err != nil {
				return false, err
			}
			if b == 1 {
				n.known = true
			} else {
				n.low++
			}
		}
		low = n.low
		leaf = n
	}
	return leaf.known && leaf.low < threshold, nil
}

// Value returns the current lower bound of leaf (x,y), which is the exact
// value once it is known.
func (t *Tree) Value(x, y int) int {
	if !t.inRange(x, y) {
		return 0
	}
	return t.levels[0].nodes[y*t.levels[0].w+x].low
}

// EncodeValue emits all bits needed to convey the exact value of leaf (x,y).
func (t *Tree) EncodeValue(w *BitWriter, x, y int) {
	if !t.inRange(x, y) {
		return
	}
	t.Encode(w, x, y, t.levels[0].nodes[y*t.levels[0].w+x].value+1)
}

// DecodeValue queries leaf (x,y) with increasing thresholds until its
// value is known, and returns it.  It fails when the data runs out or
// when the value would exceed maxValue.  Thresholds below what the tree
// already establishes as a lower bound would read nothing and are
// skipped, so the cost is proportional to the bits consumed.
func (t *Tree) DecodeValue(r *BitReader, x, y, maxValue int) (int, error) {
	if !t.inRange(x, y) {
		return 0, errors.New("tag tree leaf out of range")
	}
	for th := t.lowerBound(x, y) + 1; th <= maxValue+1; th++ {
		ok, err := t.Decode(r, x, y, th)
		if err != nil {
			return 0, err
		}
		if ok {
			return t.Value(x, y), nil
		}
	}
	return 0, errors.New("tag tree value too large")
}

// lowerBound returns the largest lower bound established on the path
// from the root to leaf (x,y), which bounds the leaf's value from below.
func (t *Tree) lowerBound(x, y int) int {
	low := 0
	for i := len(t.levels) - 1; i >= 0; i-- {
		l := &t.levels[i]
		low = max(low, l.nodes[(y>>uint(i))*l.w+(x>>uint(i))].low)
	}
	return low
}

// SkipWidth reports how many leaves, starting at (x,y) and moving right
// along the row, are already known to have values of at least threshold,
// so that decoding them with this threshold would read no bits: the
// width of the largest node on the path to (x,y) whose lower bound has
// reached the threshold, clipped to the row.  Zero means the leaf must be
// decoded.  Lower bounds are propagated downwards like Decode does, so a
// subsequent Decode behaves identically.
func (t *Tree) SkipWidth(x, y, threshold int) int {
	if !t.inRange(x, y) {
		return 0
	}
	low := 0
	for i := len(t.levels) - 1; i >= 0; i-- {
		l := &t.levels[i]
		n := &l.nodes[(y>>uint(i))*l.w+(x>>uint(i))]
		low = max(low, n.low)
		n.low = low
		if low >= threshold {
			end := ((x >> uint(i)) + 1) << uint(i)
			return min(end, t.levels[0].w) - x
		}
	}
	return 0
}
