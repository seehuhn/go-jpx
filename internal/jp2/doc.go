// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

// Package jp2 reads and writes the box structure that wraps a JPEG 2000
// codestream in a JP2 or JPX file.
//
// The reader understands the boxes needed to interpret the first
// codestream of a file (image header, bit depths, colour specifications,
// palette, component mapping and channel definitions) and skips every
// other box.  The codestream itself is never interpreted or buffered:
// [Read] returns a reader positioned at its first byte.
//
// All input is treated as untrusted.  Memory use is proportional to the
// bytes actually read, never to declared lengths: header box payloads are
// read in chunks of at most 64 KiB, and every input-dependent allocation
// is charged to the callback given to [Read] before it is made, so the
// caller's budget bounds the total.  Malformed ancillary boxes in the
// header superbox are skipped rather than reported.
package jp2
