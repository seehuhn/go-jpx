# seehuhn.de/go/jpx

A JPEG 2000 codec in pure Go: decoder and encoder for the core coding
system (ISO/IEC 15444-1) together with the JP2 file format and the parts
of JPX that PDF files use.  The library was written to serve as the
`JPXDecode` filter of a PDF library, so it exposes everything a PDF reader
needs to interpret the samples (colour specifications, channel
definitions, palettes) without applying any of it, and it treats its
input as hostile.

```go
img, err := jpx.Decode(r, &jpx.Options{Budget: budget})
// img.Planes[c].Pix holds the samples of component c as int32,
// at the component's native depth and signedness.

err = jpx.Encode(w, img, &jpx.EncodeParams{Lossy: true, Quality: 80})
```

## What is implemented

Decoder (Part 1, complete):

- JP2/JPX wrapper (`ihdr`, `bpcc`, `colr` incl. ICC and vendor methods,
  `cdef`, `pclr`/`cmap`, `rreq`, fragment tables, several codestreams:
  the first is decoded) and bare codestreams.
- Both wavelet filters (reversible 5/3, irreversible 9/7), both component
  transforms (RCT, ICT), bit depths from 1 to 34 bits, signed
  samples, sub-sampled components, image and tile offsets.
- Tiling, tile-parts in any order, precincts, all five progression orders,
  progression order changes (POC), multiple quality layers, all
  code-block coding styles (bypass, reset, termination on every pass,
  vertically causal contexts, predictable termination, segmentation
  symbols), packed packet headers (PPM, PPT), SOP/EPH markers, region of
  interest (Maxshift), reduced-resolution decoding.
- Truncated and partially corrupt codestreams decode to the image
  received so far.

Encoder:

- Lossless (5/3 + RCT) and lossy (9/7 + ICT with a quality setting),
  quality layers with rate-distortion optimised truncation (optionally
  with target rates), image offsets, tiling and tile-parts, precincts,
  code-block sizes and coding styles, all progression orders and
  progression order changes, packed packet headers (PPT, PPM), SOP/EPH,
  region of interest, bare codestream or JP2 wrapper with colour
  specification, channel definition and palette boxes.

Helpers for the conversions a caller usually wants (palette expansion,
upsampling of sub-sampled components to the reference grid, conversion
to unsigned samples of a common depth, sYCC to RGB) are provided as
separate functions.

## Limits and deviations

- The decoder and the encoder accept component depths from 1 to 34
  bits (`MaxDepth`); JPEG 2000 allows up to 38.  `Decode` rejects
  deeper components; `ReadHeader` still reports them.  The limit is the
  largest depth at which every decoded image can be written back
  exactly: deeper samples can need more magnitude bit-planes than a
  codestream can signal for the high-pass sub-bands.
- Planes hold at most 24 significant bits (`MaxPrecision`).  Components
  deeper than 24 bits are returned without their lowest `Depth-24` bits
  (`Component.Truncation`): the plane holds the samples divided by
  `2^Truncation`, computed by the reversible transform from the
  coefficients without their lowest bits, so that a value can differ
  slightly from the exact quotient when the dropped bits were not zero.
- Palette columns may have 1 to 38 bits.  Values of columns deeper than
  31 bits are held without their lowest `Depth-31` bits
  (`PaletteColumn.Truncation`), rounded towards negative infinity, and
  written back multiplied by `2^Truncation`.
- Every image `Decode` returns at full resolution is accepted by
  `Encode`, and losslessly encoding it and decoding the result gives
  the same image again.  For truncated components the encoder codes
  the coefficients of the held values at the scale of the full samples,
  with zero low-order bits.  Up to a depth of about 27 bits every plane
  is coded, and any decoder recovers the samples exactly; beyond that
  the lowest planes exceed what tier-1 handles and are left uncoded, so
  that other decoders reconstruct them by their own rule, typically at
  the middle of the interval.  `Encode` accepts only
  the truncation `Decode` produces, `max(0, Depth-24)` for components
  and `max(0, Depth-31)` for palette columns.  Lossy encoding of
  truncated components uses quantisation steps at the scale of the full
  samples, like any other depth.
- Tier-1 reconstructs at most 30 magnitude bit-planes per code-block.
  Deeper blocks (depths above about 27 bits, or large region-of-interest
  shifts) are decoded for their 30 most significant planes, as if the
  stream had been truncated after them: coefficients lose the bits of
  the remaining planes and are reconstructed at the middle of the
  uncertainty interval.  With a region-of-interest shift s, background
  coefficients lie below 2^s and keep their scale, so they lose their
  low-order bits first, and are lost entirely once s is at least about
  30; coefficients of the region keep the bits that were decoded.
- Decoding is single-threaded unless `Options.Workers` asks for
  concurrency, which is then applied to tiles or, for images with few
  tiles, to the code-blocks within each tile.
- Fully decoded irreversible coefficients are reconstructed at the
  middle of their quantisation interval, as the standard suggests; other
  decoders may reconstruct at the lower end, which gives differences of
  ±1 in a small fraction of samples.
- PPT marker segments are numbered continuously across the tile-parts
  of a tile, which is what widely used decoders expect; the decoder
  accepts both this and per-tile-part numbering.
- Tolerated irregularities seen in files in circulation: more coding
  passes than bit-planes (surplus passes are ignored), zero bit-plane
  counts at or above the number of planes (the block is empty), code
  blocks included with zero bytes, fewer quantisation entries than
  decomposition levels (the entries are assigned to the finest levels),
  a component transform flag on images with fewer than three components
  (ignored), coding parameters in later tile-parts (ignored), and
  progression order changes naming more components or levels than exist
  (clamped).

## Memory

Every allocation whose size depends on the input is charged to the
`*membudget.Budget` (from `seehuhn.de/go/membudget`) passed in `Options`
before it is made, including the decoded planes.  Decoding work is
charged at one byte per coefficient per coding pass, so the budget also
bounds CPU time, at roughly a few nanoseconds per charged byte.
Exceeding the budget fails with `membudget.ErrExceeded`.  Without a
budget, as with the standard library's image decoders, nothing is
limited: callers decoding untrusted input must pass one.

Work is bounded by the input as well: packets are only parsed while data
is present, structures for precincts and code-blocks are created when
their first packet arrives, tag-tree walks and progression iterators do
work in proportion to the bits consumed and the precincts present, and
tiles are decoded one at a time into buffers that are reused.  Work
that a hostile file could multiply without supplying data (scans for
progression order changes, coding passes over absent data) is charged
to the budget like memory.

`hostile_test.go` and `mutate_test.go` hold the regression suite from
the security review: hand-built hostile codestreams and files, a random
geometry generator, and header mutation, all asserting that decoding
neither panics nor allocates more than it charges.

## Development

```
go build ./... && go vet ./... && go test ./... && golangci-lint run
```

`cmd/jpx` is a small tool to inspect, decode (to PGX or PNG) and encode
(from PNG or PGX) files.  See `testdata/README.md` for how the test data
is produced; all of it is generated by this library.
