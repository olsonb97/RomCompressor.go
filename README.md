# RomCompressor.go

A Go library and command-line tool for compressing game images, restoring
uncompressed copies, and explicitly trimming supported cartridge dumps.
The sister project to [RomPatcher.go](https://github.com/olsonb97/RomPatcher.go),
it provides an importable backend and a thin CLI without a graphical frontend.
Building from source requires Go 1.25 or newer. Prebuilt binaries do not require Go.

## AI-assisted development

This project was developed with substantial assistance from OpenAI Codex.
Although it has been reviewed and tested, AI-assisted code can contain mistakes.
Users should independently evaluate and test it for their use case.

## Format support

**Formats that name a dependency below require that program to be installed
separately.** This applies to both the CLI and the Go library. Release archives
contain RomCompressor only; external engines and keys are not included or installed
automatically. Install only the engines needed for your chosen formats.

| Format | Typical use | Required external dependency |
| --- | --- | --- |
| ZIP, gzip | Files from any system; ZIP also preserves game folders | None (native Go) |
| CSO v1, ZSO v1 | PSP and PS2 ISO compression | None (native Go) |
| ECM | Individual CD image files | None (native Go) |
| CHD | CD, DVD, and raw hard-drive images | [MAME chdman](https://www.mamedev.org/release.html) |
| PBP | Single-disc PlayStation CUE/BIN | [PSXPackager](https://github.com/RupertAvery/PSXPackager/releases) for creation; none for restoration or verification |
| RVZ, WIA, GCZ | GameCube and Wii discs | [Dolphin Tool](https://dolphin-emu.org/download/) |
| CISO | GameCube and Wii Compact ISO | None (native Go) |
| WBFS | Single-disc Wii images | None (native Go) |
| CCI | Single-part original Xbox XISO compression | None (native Go) |
| NSZ, XCZ | Switch NSP and XCI | [NSZ 5.0+](https://github.com/nicoboss/nsz) and your keys file |
| WUX | Wii U WUD images | None (native Go) |
| WUA | Decrypted Wii U titles, updates, and DLC | [ZArchive](https://github.com/Exzap/ZArchive/releases) for creation/restoration; none for verification |
| ZCCI, ZCIA, ZCXI, Z3DSX | Decrypted Nintendo 3DS images and homebrew | [Azahar CLI](https://github.com/azahar-emu/azahar/releases) |
| 7z | Individual files from any system | [7-Zip](https://www.7-zip.org/download.html) (`7z` or `7zr`) |

Trimming is a separate operation for validated DS/DSi cartridges, 3DS CARD1
cartridges, and standard Switch XCI gamecards. It always writes a new copy.

Support is limited to the layouts documented in [format details](docs/formats.md).
Emulator compatibility varies by version and platform. Switch conversions require
an exact round trip. NSZ cannot retain every XCI layout or gamecard certificate.

## Installation

The first supported release is **v0.1.1**. Earlier version tags are retracted.

Download and extract a platform archive from
[GitHub Releases](https://github.com/olsonb97/RomCompressor.go/releases/latest).
From the extracted directory, start the CLI with:

```powershell
# Windows PowerShell
.\romcompressor.exe --help
```

```sh
# Linux or macOS
./romcompressor --help
```

Add that directory to PATH to use `romcompressor` from other folders. Alternatively,
install with Go:

```console
go install github.com/olsonb97/romcompressor.go/cmd/romcompressor@latest
```

Go installs the command into `GOBIN`, or `bin` inside `go env GOPATH` when `GOBIN`
is unset. Add that directory to PATH if your shell cannot find `romcompressor`.

For an external format, follow [engine setup](docs/engines.md). The dependency must
be on PATH or selected with `--tool ENGINE=PATH`. For example:

```console
romcompressor compress game.cue -f chd --tool chdman=/path/to/chdman
```

`romcompressor formats` lists compression, restoration, and verification
availability. It honors `--tool` overrides. Availability means the selected engine
can be located; its version, runtime requirements, and input compatibility still
apply.

## Library

Add the module to your application:

```console
go get github.com/olsonb97/romcompressor.go
```

```go
package main

import (
    "context"
    "fmt"
    "log"

    romcompressor "github.com/olsonb97/romcompressor.go"
)

func main() {
    result, err := romcompressor.Convert(context.Background(), "game.iso", "game.cso",
        romcompressor.Options{Format: romcompressor.CSO})
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println(result.Output)
}
```

`Convert` accepts cancellation, progress callbacks, an output byte limit, and
explicit engine paths through `Options.Tools`:

```go
opts := romcompressor.Options{
    Format: romcompressor.CHD,
    Tools: map[string]string{"chdman": "/path/to/chdman"},
}
```

`Inspect`, `InspectWithOptions`, `Verify`, `ConvertPlaylist`, and `Batch` are also
available. `Capabilities(opts)` reports per-operation engine availability using
your configured paths. `PlanConversions(ctx, source, opts)` reports input-specific
targets, media choices, archive-selection requirements, and required engines using
the same source preflight as conversion. It runs no engines and writes no files.
Only `Tools` and `MaxOutput` affect this discovery query; a plan does not verify the
full payload, keys, engine runtime, or emulator compatibility.

Use `PlanTrim` for a read-only trimming preview, or
`PlanTrimWithOptions` to receive preview progress, and `Trim` to write a new copy.
For standalone `Verify`, set `Options.TempDir` to an existing scratch parent on
the desired drive. Private verification data is staged there; cleanup is best effort
and preserves a directory replaced by another process.
An empty value uses the operating system's temporary directory. Conversion staging
stays beside its output and does not accept this verification-only option.

Importing this module does not require the compiled CLI. Native codecs use Go's
standard library plus LZ4; platform file and process operations use `golang.org/x/sys`.
External formats invoke their configured local executables.

Outputs are staged, verified, and published without replacing existing paths.
Native codecs stream with bounded buffers and indexes. `MaxOutput` defaults to
1 TiB, including monitored external scratch data. Verification may need an
uncompressed temporary copy. Keep source files unchanged during an operation.
See [architecture](ARCHITECTURE.md) for lifecycle and resource limits.

## CLI

Flags work before or after filenames. Quote paths containing spaces.

```console
romcompressor compress game.iso -f cso
romcompressor decompress game.cso -o restored.iso
romcompressor compress game.cue -f chd
romcompressor decompress game.chd --media cd -o restored.cue
romcompressor decompress dreamcast.chd --media cd -o restored.gdi
romcompressor compress game.iso -f chd --media dvd
romcompressor compress game.iso -f rvz --tool dolphin-tool=/path/to/DolphinTool
romcompressor inspect games.zip --json
romcompressor plan game.iso --json
romcompressor verify game.cso
romcompressor verify game.rvz --temp-dir "D:\Scratch"
romcompressor formats
romcompressor --version
```

Run `romcompressor COMMAND --help` for options. Human progress and errors use stderr;
`--json` writes one machine-readable document to stdout. Failures return a nonzero
exit status. Existing destinations and source files are preserved.
`--max-output` accepts byte counts or sizes such as `32GiB` and `512MiB`.

### Trim cartridge images

```console
romcompressor trim game.nds --dry-run --json
romcompressor trim game.nds
romcompressor trim game.3ds -o game-small.3ds
```

Trim validates the cartridge layout and scans the entire proposed suffix for
uniform `00` or `FF` padding. It refuses ambiguous layouts, writable cartridge
regions, and non-padding tails. Before publication, it compares every retained
byte with the read-only source and checks the full suffix again. The default output
is `NAME.trim.EXT`; `--dry-run` writes no files. Preview JSON reports planned sizes
and the padding byte. A completed trim also returns the new copy's SHA-256.
If no padding is removable, no copy is written. Library callers can identify that
case with `errors.Is(err, romcompressor.ErrNoRemovablePadding)`.

### Game folders and disc sets

```console
romcompressor compress "Game folder" -f zip
romcompressor decompress "Game folder.zip" --all -o "Restored game"
romcompressor compress "Game.m3u" -f chd -o "Game compressed.m3u"
romcompressor decompress "Game compressed.m3u" -o "Game restored.m3u"
```

ZIP folder restoration includes empty directories and rejects unsafe paths, links,
collisions, and expansion beyond the byte limit. Individual ZIP/7z extraction uses
`--entry NAME` when an archive contains multiple files.
ZIP restoration preserves file contents, names, empty directories, and owner-execute
on Unix. Restored files use private owner read/write permissions; modification
times are newly created rather than restored from the archive.

M3U conversion creates a new companion folder and publishes its playlist after
every disc succeeds, preserving disc order and matching PlayStation SBI companions.
The example above uses CUE/BIN discs; add `--media dvd` for a playlist of DVD ISOs.
Decompression restores supported disc containers into a new set, including CUE/BIN
companions for CD CHD and PBP. Supported Dreamcast GD-ROM CHDs restore to a
GDI descriptor and one BIN per track. The default playlist is `NAME-restored.m3u`.
Tagged CHD CD/DVD media is selected per disc. Unidentified CHDs need an explicit
supported media choice; CD extraction requires track metadata. Already-raw
single-image entries are copied and checked. Raw CUE/GDI entries and HDD/laserdisc
CHDs are refused during playlist restoration; ECM restores its individual image
without inventing a CUE. See [format details](docs/formats.md) for the exact scope.
References must remain within the selected descriptor or playlist folder. Nested
playlists and `..` path components are refused. A reported failure rolls back this
operation's outputs; a process crash can leave companions without a playlist.

### Batch manifests

```json
{
  "jobs": [
    {"source": "first.iso", "output": "first.cso", "format": "cso"},
    {"source": "second.cue", "output": "second.chd", "format": "chd"},
    {"source": "third.cso", "output": "third.iso", "format": "raw"}
  ]
}
```

```console
romcompressor batch jobs.json --json
```

`"format":"raw"` restores an uncompressed copy. Source, output, and per-job key
paths are relative to the manifest. Jobs run sequentially with independent results;
a failed job keeps earlier successful outputs. Cancellation stops new jobs.
Manifests are limited to 8 MiB and 10,000 jobs. `--tool` and `--keys` can override
engine and Switch key settings for the batch.

## Building from source

```console
go build -trimpath -o romcompressor ./cmd/romcompressor
```

On Windows, use `-o romcompressor.exe`.
Release archive packaging is described in [development](docs/development.md#releases).

## Verification

```console
go test -count=1 -timeout 5m ./...
go test -race -count=1 -timeout 5m ./...
go vet ./...
go mod verify
```

Tests use synthetic fixtures and temporary directories. They cover malformed
formats, checksums, output collisions, source preservation, trimming boundaries,
cancellation, and bounded I/O. Optional external-engine checks are documented in
[development](docs/development.md). No games or keys are included.

## License

[MIT](LICENSE). Bundled dependency attribution is in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md). Separately installed engines have
their own licenses.
