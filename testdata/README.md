# Test data

Everything under this directory is produced by the library itself; no
external files are included.

- `fuzz/`: corpus entries recorded by the Go fuzzing engine for the
  fuzz tests in the root package (`FuzzDecode`, `FuzzReadHeader`,
  `FuzzRoundTrip`) and in the internal packages.  Each entry is an input
  that once triggered a failure and now serves as a regression test; the
  fix lives in the code, never in the corpus.

The regular tests do not read any files: the images they encode and
decode are generated at run time (see `testImage` in `encode_test.go`
and `minimalCodestream` in `decode_test.go`).

Interoperability with other implementations is checked outside the test
suite with `cmd/jpx` and OpenJPEG's `opj_decompress`/`opj_compress`:
files written by this library are decoded with OpenJPEG and compared
sample by sample, and the ISO/IEC 15444-4 conformance streams are decoded
with this library and compared against the reference decodes.  Those
files are not part of the repository.
