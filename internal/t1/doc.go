// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

// Package t1 implements the tier-1 entropy coding of JPEG 2000 code-blocks:
// bit-plane coding with context modelling and the MQ arithmetic coder.
//
// A code-block is coded from its most significant non-zero bit-plane down
// to the least significant one.  The first bit-plane is coded in a single
// cleanup pass; every further bit-plane is coded in three passes
// (significance propagation, magnitude refinement, cleanup).  The passes
// of a block are numbered consecutively starting from zero.
//
// Coefficients are exchanged with callers as signed 32-bit integers that
// carry one extra fractional bit: the value v represents the coefficient
// magnitude |v|/2.  The decoder uses the fractional bit for mid-point
// reconstruction of bit-planes that were not decoded, so callers that
// need an exact integer (reversible coding, all planes decoded) simply
// shift the magnitude right by one.
package t1
