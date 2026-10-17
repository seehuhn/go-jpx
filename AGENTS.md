# AGENTS.md — seehuhn.de/go/jpx

Pure-Go JPEG 2000 (ISO/IEC 15444-1 "Part 1", with the JP2/JPX box structure)
encoder and decoder.  The library is the back end for the `JPXDecode` filter
in the Quire PDF library (`seehuhn.de/go/pdf`); it must read and write
anything that filter can carry.  This file is the working brief for every
agent (human or model) touching the repository.  Read it fully before
editing.

## Ground rules (non-negotiable)

1. **Clean room.**  Implement from the ITU-T T.800 / T.801 texts (available
   locally under `~/ref/`) and from first principles.  Do *not* read or
   consult the source code of OpenJPEG, JasPer, Kakadu, Grok, pdf.js,
   OpenJPH, jpylyzer or any other JPEG 2000 implementation.  `opj_decompress`
   and `opj_compress` may be used only as black boxes to cross-check output.
2. **No external data or text in the repo.**  Nothing from `~/ref/`,
   `~/src/openjpeg-data/` or any other external source is committed: no
   sample files, no spec text, no table captions, no quoted clauses.  All
   `testdata/` is produced by our own code.  Doc comments explain rules in
   our own words and do not cite spec clause numbers.  Format-mandated
   constants (lifting coefficients, MQ probability table, context rules)
   are expressed as code.
3. **Dependencies.**  Standard library plus `seehuhn.de/go/membudget`
   (memory accounting; BSD-3-Clause like this module).
   `github.com/google/go-cmp` is allowed in tests.  Anything else needs
   explicit approval from the maintainer first.
4. **Go 1.27.**  Use modern features freely (`min`/`max`, range-over-func,
   generics, `slices`, `maps`, `iter`).  Backwards compatibility is not a
   concern.
5. **Untrusted input is the default.**  The decoder is fed bytes from PDF
   files found in the wild.  It must never panic, never hang, and never
   allocate or compute in proportion to *declared* sizes — only in
   proportion to the input actually present.  No `recover()` to hide bugs,
   no arbitrary caps to paper over unbounded loops.  Every input-dependent
   allocation is charged to the `*membudget.Budget`.
6. **Git.**  Author is `Jochen Voss <voss@seehuhn.de>` (already set in
   `.git/config`).  Every commit message ends with
   `Co-Authored-By: Claude <noreply@anthropic.com>`.  No session links, no
   tool names in messages.  Commit in meaningful units; the tree must build
   and pass tests at every commit.
7. **Adversarial review before every commit.**  Nothing is committed until
   an independent reviewer (a separate sub-agent, see below) has tried to
   break the change: wrong spec interpretation, hostile-input paths
   (panics, unbounded work, allocation from declared sizes), contract
   gaps between packages, missing tests, style violations.  Every finding
   is fixed or explicitly rebutted before the commit.

## Module layout

```
seehuhn.de/go/jpx            public API: Header, Image, ReadHeader, Decode,
                             Encode, Options, helpers
internal/jp2                 JP2/JPX box reader and writer (ihdr, colr, cdef,
                             pclr, cmap, rreq, jp2c, ftbl skipping, ...)
internal/codestream          marker segments: SIZ, COD, COC, QCD, QCC, RGN,
                             POC, PPM, PPT, TLM, PLT, CRG, COM, SOT, SOD, EOC;
                             tile-part assembly; geometry (tiles, resolutions,
                             precincts, code-blocks)
internal/t2                  tier-2: tag trees, packet header coding, progression
                             iterators, layer assembly
internal/t1                  tier-1: MQ coder (enc+dec), EBCOT context modelling
                             and the three passes, all code-block styles
internal/dwt                 5/3 reversible and 9/7 irreversible wavelet,
                             forward and inverse, 2-D with arbitrary origins
internal/mct                 RCT and ICT, forward and inverse
cmd/jpx                      dev tool: info / decode (PGX, PNG) / encode (PNG, PGX)
testdata/                    only self-generated fixtures; README.md documents how
```

Keep packages small and their contracts explicit.  A package's doc.go
states what it does and what it assumes about its inputs.

## Public API (target shape; refine, don't drift)

```go
// memory accounting uses *membudget.Budget; nil = unlimited (or the
// default allowance in Decode)

type Header struct {
    Width, Height int          // reference grid size after image offset
    Components    []Component  // from SIZ
    Channels      []Channel    // from cdef, synthesised when absent
    ColorSpecs    []ColorSpec  // every colr box, file order
    Palette       *Palette     // pclr+cmap, nil when absent
    Resolutions   int          // min over components of (levels+1); bound for Options.Reduce
    // plus per-component plane geometry helpers
}
type Component struct{ Depth int; Signed bool; DX, DY int }
type Channel struct{ Component int; Kind ChannelKind; Assoc int }
type ColorSpec struct{ Method, Precedence, Approx, Enumerated int; ICC []byte }

func ReadHeader(r io.Reader, budget *membudget.Budget) (*Header, error)
func Decode(r io.Reader, opt *Options) (*Image, error)   // Options{Budget, Reduce}
func Encode(w io.Writer, img *Image, p *EncodeParams) error
```

Samples are planar `int32`, native depth and signedness, RCT/ICT undone,
palette *not* applied, no colour conversion.  Helpers (`ApplyPalette`,
`SYCCToRGB`) are separate functions.  Depth > 31 bits is rejected with an
error.  Decoding is single-threaded unless opted in.

## Code style

- `goimports -w` every file; `go vet ./...` and `golangci-lint run` clean
  (config in `.golangci.yml`: staticcheck all checks, errcheck off, tests
  excluded).
- Every Go file starts with
  ```go
  // Copyright 2026 Jochen Voss <voss@seehuhn.de>
  // SPDX-License-Identifier: BSD-3-Clause
  ```
- File names: lower-case, no underscores except `_test.go`.
- Prefer code that reads clearly over comments explaining unclear code.
  Section comments are a short lower-case phrase without full stop
  (`// packet header for this precinct`).  Longer comments are full
  sentences.  Comment design decisions and non-obvious algorithms, never
  history ("changed from…").
- Every exported symbol has a doc comment in complete sentences for a
  reader who knows JPEG 2000 but not this library.
- Errors are returned, not logged.  `errors.New` for static messages,
  `fmt.Errorf` with data; wrap only to pinpoint a location in malformed
  input.  One line, < 100 characters, lower-case start.
- Tolerate what can be tolerated when reading (truncated codestream →
  partial image, zero coefficients for missing packets).  Fail only when
  the data cannot be interpreted.  Never invent a plausible value for a
  broken one.

## Tests

- Test the contract, not the implementation.  A test should still pass if
  the internals are rewritten.
- Construct test data programmatically at run time wherever possible.
  Commit files only when a stable on-disk artefact is genuinely needed
  (fuzz seeds, regression inputs), and generate them with our own encoder
  via `//go:generate` or a documented `cmd/jpx` invocation.
- `foo.go` → `foo_test.go`.  Failure messages start lower-case.  Never
  assert on error message text.  Use `cmp.Diff(want, got)` for structures:
  `t.Errorf("round trip failed (-want +got):\n%s", diff)`.
- Round trips are the backbone: lossless must be bit-exact; lossy within a
  stated PSNR/tolerance.  Cover 1-bit, 8-bit gray/RGB/CMYK, 16-bit, mixed
  depths, signed, subsampled, opacity channel, palette, bare codestream vs
  JP2, tiling, precincts, layers, every progression order, every
  code-block style, ROI, PPM/PPT, SOP/EPH, `Reduce`.
- Fuzzers for `ReadHeader`, `Decode`, and encode→decode.  Budget sized
  from input length so the fuzzer stays fast.  Crashers found are
  committed under `testdata/fuzz/<Fuzz*>/` and fixed, not worked around.
- Hostile-input tests: tiny file, huge declared dimensions / code-block
  counts / tile counts, truncated tile-parts, recursive or oversize boxes.
  Assert that the budget (not luck or wall-clock) stops the decode.
- Benchmarks report ratios not absolute times.
- Tests never require external tools.  Cross-checks against
  `opj_decompress` live in the dev workflow (`cmd/jpx` + scripts outside
  the repo), not in `go test`.

## Dev workflow

```
export PATH=$PATH:$(go env GOPATH)/bin
go build ./... && go vet ./... && go test ./... && golangci-lint run
```

Interop checks (not committed, not required by tests):

- `opj_decompress -i x.jp2 -o x.pgx` to produce reference planes;
  `opj_compress` to produce inputs exercising specific features
  (`-r`, `-n`, `-p`, `-t`, `-c`, `-M`, `-SOP`, `-EPH`, `-ROI`, `-I`, `-s`).
- `~/src/openjpeg-data/` holds the 15444-4 conformance streams (never copy
  any into the repo).  Decode them with `cmd/jpx` and compare against the
  PGX references there.

## Working with sub-agents

The lead agent decomposes work into tasks with a crisp contract (package,
exported API, behaviour, test expectations) and hands them to sub-agents
run as `pi -p --model <model> ...` in a scratch checkout or on a disjoint
set of files.  Pick the cheapest model that can do the task:
Haiku for mechanical/boilerplate work and tests from a given spec,
Sonnet for self-contained components with a clear specification,
the lead model for cross-cutting design, EBCOT/tier-2, rate control and
debugging.  The lead reviews every result before committing.  Sub-agents
must follow this file; it is loaded automatically.

Review step: `~/briefs/review.sh <name>` hands the staged diff to a
reviewer agent whose only job is to find faults (it does not edit).  Its
report is read by the lead, who fixes the valid findings, re-runs the
tests and only then commits.
