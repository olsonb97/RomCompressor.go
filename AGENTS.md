# RomCompressor.go agent guide

Read `README.md`, `ARCHITECTURE.md`, and `docs/formats.md` before changing behavior.
Read the ignored local `PROJECT_STATUS.md` when present for ongoing work context.
This is a standalone Go library and CLI. Keep application-specific integrations
in their consuming repositories.

## Project scope

Build a robust Go game compression library and intuitive CLI, comparable in quality
and usability to RomPatcher.go. Cover commonly used formats across game systems.
Avoid obscure format variants merely to lengthen the support table. Prioritize
clean reusable code, explicit capabilities, practical compatibility, bounded memory,
and real verification. Do not claim every system or emulator is supported from a
successful internal round trip alone. The format documentation records supported
layouts and compatibility limits.
Match RomPatcher.go's importable Go API, thin CLI, shared flags, and staged file
operations where they fit.

## Ownership

- Root package owns public APIs, detection, conversion orchestration, batches,
  cartridge trimming, archive trees, and complete M3U disc sets.
- `internal/fileops` owns file identity, bounded I/O, staging, scratch monitoring,
  and platform-specific exclusive publication primitives.
- `internal/process` owns executable resolution, bounded logs, subprocess execution,
  and process-tree cancellation.
- `internal/disc` owns contained CUE/GDI reference parsing.
- `internal/chd` owns CHD media layouts, metadata, engine invocation, and verification.
- `internal/engine` owns other external-tool adapters and their verification contracts.
- `internal/format` owns canonical format names and family classification.
- `internal/progress` owns progress throttling shared by file and engine workflows.
- `internal/codec` owns native format parsing and streaming encoding/decoding.
- `internal/trim` validates cartridge layouts and locates the retained boundary.
- `cmd/romcompressor` owns arguments, human output, JSON, and exit status.
- `integration` exercises the public API, including opt-in external engines.
  Keep private parser and process tests beside their internal implementations.
- Consuming applications own file selection, catalog changes, and application UI.
  Keep those dependencies out of this library.

## Engineering expectations

- Prefer standard Go packages and small concrete functions. Reuse one owner for
  format rules, bounded streaming, cancellation, and publication.
- Keep public types defined in the root package. Internal packages must not import
  the root; use narrow configuration and I/O contracts at package boundaries.
- Never load complete game images into memory. Bound indexes, archive directories,
  logs, buffers, expansion, and subprocess scratch storage.
- Preserve input files. Never replace existing outputs. Stage and verify first,
  then publish exclusively. Remove only outputs this operation created on failure.
- Respect context cancellation during I/O and terminate external engine descendants.
- Validate signatures, sizes, integer arithmetic, paths, references, and checksums.
  Malformed inputs return errors, never panic or silently truncate.
- Do not silently scrub images, discard partitions, trim ROMs, or change bytes.
  Document any format-level distinction between logical disc data and container bytes.
- Trim cartridges only through the explicit trim operation. Check the layout and
  every removed padding byte before publishing a separate copy.
- External tools are explicit local dependencies. Do not download or run tools
  automatically, discover private keys, or print key contents or secret-bearing logs.
- Use primary upstream specifications and source for external CLI contracts.
  Successful process exit alone does not establish successful verification.
- Keep public APIs documented and CLI commands predictable. Validate incompatible
  options before expensive work. Human progress goes to stderr; JSON stdout contains
  one complete machine-readable document.
- Report dependencies per operation. Locating an executable does not establish
  runtime, key, or input compatibility; capability queries must not run engines.
- Keep errors actionable for CLI and Go callers. Preserve typed dependency errors
  and standard filesystem error identity when wrapping context.
- No ROMs, keys, personal filesystem paths, temporary outputs, compiled binaries,
  downloaded engines, caches, or test corpora in version control.
- Use `apply_patch` for manual edits. Keep work on a dedicated branch. Preserve
  unrelated changes. Commit, push, tag, and publish only when explicitly requested.

## Verification

Run `gofmt`, `go test -count=1 -timeout 5m ./...`,
`go test -race -count=1 -timeout 5m ./...`, `go vet ./...`, `go mod verify`,
and `git diff --check` after meaningful changes. Add focused regression tests for
new safety behavior. Use synthetic data and `t.TempDir`; preserve user game files.
Cross-compile CLI builds for Windows, Linux, and macOS. Confirm external engines
with opt-in integration tests when installed. Distinguish compile checks, generated
fixture round trips, independent interoperability, and real-game testing.

Keep public documentation accurate when capabilities or compatibility limits change.
Do not commit local audit histories, test output, or release artifacts. Release
archives belong in GitHub Releases and are built with `build_release.py`.
