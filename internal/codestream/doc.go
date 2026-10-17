// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

// Package codestream parses and writes the marker segments and tile-part
// structure of a JPEG 2000 codestream.
//
// The parsers validate field ranges and the structural rules of the
// codestream syntax, but they do not compute large derived quantities such
// as the total number of tiles times components; that is left to the caller,
// who can charge such work to a budget.  Parsing never allocates in
// proportion to declared sizes: every marker segment is at most 64 KiB and
// tile-part bodies are sliced from the input.
//
// Reader walks a codestream held in memory.  ReadMainHeaderFrom parses only
// the main header from a stream.  Writer emits marker segments mirroring the
// parsed structures.  Resolve applies the precedence rules between main and
// tile headers.
package codestream
