// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

// Package mq implements the MQ adaptive binary arithmetic coder used by
// tier-1 coding.
//
// The coder turns a sequence of binary decisions, each tagged with a
// [Context], into a byte string and back.  Encoder and decoder must see the
// same decisions in the same contexts.  The encoder output never contains
// two consecutive bytes forming a value in the range 0xFF90..0xFFFF, so
// such pairs can serve as segment terminators in the surrounding format.
//
// Register names in the source (A, C, CT, BP) follow the usual
// description of the coder.  The decoder reads past the end of its input as
// if the data were followed by an endless run of 0xFF bytes, so truncated
// input decodes to well-defined bits.
package mq
