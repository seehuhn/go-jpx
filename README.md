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
  transforms (RCT, ICT), arbitrary bit depths up to 31 bits, signed
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
  with target rates), tiling and tile-parts, precincts, code-block sizes
  and coding styles, all progression orders and progression order
  changes, packed packet headers (PPT, PPM), SOP/EPH, region of interest,
  bare codestream or JP2 wrapper with colour specification, channel
  definition and palette boxes.

Helpers for the conversions a caller usually wants (palette expansion,
sYCC to RGB) are provided as separate functions.

## Limits and deviations

- Samples deeper than 31 bits, and code-blocks with more than 30
  magnitude bit-planes (which happens for depths above about 27 bits, or
  with large region-of-interest shifts), are rejected as unsupported.
  The encoder accepts depths up to 24 bits.
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
