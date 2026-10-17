// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package codestream

// TLM, PLM and PLT segments are kept as raw payloads (the bytes following the
// length field, starting with the Z index).  The decoder does not need them.
