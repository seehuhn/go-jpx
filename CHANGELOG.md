# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [v0.7.6] (2026-11-10)

Initial tagged release.

Provides a JPEG 2000 codec in pure Go, written for the `JPXDecode`
filter of PDF files.  It returns the samples together with the
information needed to interpret them and does not apply any colour
conversion itself.

### Added
- `Decode` reads JP2/JPX files and bare codestreams (JPEG 2000 Part 1)
  into an `Image` with one plane of `int32` samples per component, at
  the component's native depth and signedness.  `ReadHeader` reports the
  image header without decoding the samples.
- The image header reports colour specifications (`ColorSpec`, including
  ICC and vendor methods), channel definitions (`Channel`) and palettes.
  `ColorSpec.LabParams` decodes CIELab parameters.
- Decoder support for both wavelet filters, both component transforms,
  tiling, precincts, all progression orders, quality layers, all
  code-block coding styles, packed packet headers, region of interest
  and reduced-resolution decoding.  Truncated or partly corrupt
  codestreams decode to the image received so far.
- `Encode` writes lossless or lossy codestreams, or JP2 files, using
  `EncodeParams`.  It supports quality layers with rate-distortion
  optimisation, tiling, precincts, all progression orders and the
  coding options of the decoder.
- Helper functions for common conversions: `ApplyPalette`, `Upsample`,
  `ToUnsigned` and `SYCCToRGB`.
- Component depths from 1 to `MaxDepth` (34) bits are accepted.  Planes
  hold at most `MaxPrecision` (24) significant bits.
- Every allocation depending on the input, and the decoding work, is
  charged to the `membudget.Budget` given in `Options`.  Decoding
  untrusted input without a budget is unbounded.
- `Options.Workers` selects concurrent decoding of tiles or code-blocks.
- The `cmd/jpx` tool inspects files, decodes them to PGX or PNG and
  encodes them from PNG or PGX.
