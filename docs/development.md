# Development

Use Go 1.25 or newer. Use a currently supported, patched Go toolchain when building
release binaries. The module has no CGO dependencies.

```console
gofmt -w .
go test -count=1 -timeout 5m ./...
go test -race -count=1 -timeout 5m ./...
go vet ./...
go mod verify
git diff --check
```

Tests generate synthetic data in temporary directories. Source tests belong in
version control; local results, coverage, compiled binaries, and audit notes do not.
Parser fuzz targets live in `internal/codec` and `internal/trim`.
Azahar container fuzzing also lives in `internal/engine`. Public conversion and
optional engine round trips are in `integration/`; `go test ./...` includes them.

## Optional integration tests

Set an engine variable to the path of an already installed executable, then run
the tests above. No engine is downloaded by the test suite.

| Variable | Independent check |
| --- | --- |
| `ROMCOMPRESSOR_MAXCSO` | CSO/ZSO interoperability with maxcso |
| `ROMCOMPRESSOR_CHDMAN` | CHD CD, DVD, and HDD conversion |
| `ROMCOMPRESSOR_7Z` | 7z creation, listing, and restoration |
| `ROMCOMPRESSOR_DOLPHIN` | RVZ/WIA/GCZ conversion |
| `ROMCOMPRESSOR_AZAHAR` | ZCCI/ZCIA/ZCXI/Z3DSX conversion |
| `ROMCOMPRESSOR_PSXPACKAGER` | PBP creation and native restoration |
| `ROMCOMPRESSOR_WIT` | WBFS interoperability with Wiimms ISO Tools |
| `ROMCOMPRESSOR_ZARCHIVE` | WUA title-tree conversion |

`ROMCOMPRESSOR_TEST_LARGE_WBFS=1` enables full-size generated Wii disc fixtures.
Allow several GiB of temporary disk space.

For the optional legacy WUX adapter, set `ROMCOMPRESSOR_JWUDTOOL` to its JAR and
install Java on PATH. This check writes full-size generated WUD files: reserve
at least 50 GiB and run with `-timeout 20m`.

Switch integration requires `ROMCOMPRESSOR_NSZ`, `ROMCOMPRESSOR_NSZ_KEYS`, and
explicit fixture paths in `ROMCOMPRESSOR_NSZ_NSP` and/or `ROMCOMPRESSOR_NSZ_XCI`.
Use only fixtures and keys you are authorized to use. Allow space for compressed
and restored copies and run with `-timeout 20m`. No keys or fixtures belong in the
repository. These tests remain skipped unless explicitly configured.

## Releases

```console
python build_release.py 0.1.1
python build_release.py 0.1.1 -t windows/amd64
python build_release.py --list-targets
```

The script requires Python 3.9+ and Go 1.25+. It builds nine targets across Windows,
Linux, and macOS with CGO disabled, stamps the CLI version, removes local build
paths, and writes archives plus SHA-256 `checksums.txt` to `dist/vVERSION/`.
Use `-t` for selected targets or `-o` for a new output directory. Existing output
directories are refused. Python is needed only for release packaging.

Archives contain only the binary, MIT license, and third-party notices.
Review source, archive contents, version stamps, and checksums before publishing.
Keep format limitations current in [formats.md](formats.md). Do not bundle external
engines without checking their separate distribution terms.

The initial supported release is v0.1.1. The self-retracted v1.0.1 tag publishes
Go retraction metadata for the withdrawn versions and itself, allowing `@latest`
to select v0.1.1. Keep that metadata tag available; it has no GitHub Release or
binary downloads. Do not move published tags or reuse version numbers.
