# External engine setup

Install only the engines for the formats you use. ZIP, gzip, CSO, ZSO, ECM, WBFS,
Nintendo CISO, Xbox CCI, WUX, and cartridge trimming need no separate program.
PBP restoration and verification are native too; only PBP creation needs an engine.
WUA verification also runs natively; its creation and restoration require ZArchive.

The same engine requirements apply to the CLI and the Go library. RomCompressor
does not bundle, download, or install these tools. Keep any support files required
by the upstream distribution beside its executable.

## Choose an engine

Use the exact engine ID shown below with `--tool` or `Options.Tools`. On Windows,
executable names normally have an `.exe` extension.

| Formats | Engine ID | Executable names checked on PATH | Upstream download or setup |
| --- | --- | --- | --- |
| CHD | `chdman` | `chdman` | [MAME tools](https://www.mamedev.org/release.html) |
| PBP creation | `psxpackager` | `psxpackager` | [PSXPackager CLI releases](https://github.com/RupertAvery/PSXPackager/releases) |
| RVZ, WIA, GCZ | `dolphin-tool` | `dolphin-tool`, `DolphinTool` | [Dolphin downloads](https://dolphin-emu.org/download/) |
| NSZ, XCZ | `nsz` | `nsz` | [NSZ installation](https://github.com/nicoboss/nsz#running-this-tool) |
| WUA | `zarchive` | `zarchive` | [ZArchive releases](https://github.com/Exzap/ZArchive/releases) |
| ZCCI, ZCIA, ZCXI, Z3DSX | `azahar` | `azahar` | [Azahar releases](https://github.com/azahar-emu/azahar/releases) |
| 7z | `7z` | `7z`, `7zz`, `7za`, `7zr` | [7-Zip console downloads](https://www.7-zip.org/download.html) |

Select a command-line engine rather than a GUI-only program. If an upstream
download uses another filename, point to it explicitly; renaming it is unnecessary.

Some engines need a separate runtime:

- PSXPackager v1.8.0's Windows CLI needs the
  [.NET 10 Runtime](https://dotnet.microsoft.com/en-us/download/dotnet/10.0).
  Its Linux and macOS builds include their runtime. See the
  [upstream release requirements](https://github.com/RupertAvery/PSXPackager/releases/tag/v1.8.0).
- Dolphin's Windows builds need the
  [Visual C++ v14 redistributable](https://learn.microsoft.com/en-us/cpp/windows/latest-supported-vc-redist).
  The [Dolphin download page](https://dolphin-emu.org/download/) links the supported runtime.
- NSZ offers packaged command-line executables. Installing its Python package or
  running from source instead requires Python and the upstream Python dependencies.
  Follow [NSZ's installation instructions](https://github.com/nicoboss/nsz#running-this-tool)
  for your chosen distribution. RomCompressor requires NSZ 5.0 or newer.

Use the runtime requirements for the engine release you install; newer upstream
releases can change them.

## Select installed paths

Add the engine directory to PATH, or pass its executable path directly. Quote paths
containing spaces. For example, in PowerShell:

```powershell
.\romcompressor.exe compress game.cue -f chd --tool "chdman=C:\Tools\MAME\chdman.exe"
.\romcompressor.exe formats --tool "chdman=C:\Tools\MAME\chdman.exe"
```

On Linux or macOS:

```sh
./romcompressor compress game.cue -f chd --tool chdman=/opt/mame/chdman
```

Go applications can use private engine directories without changing the user's PATH:

```go
opts := romcompressor.Options{
    Format: romcompressor.CHD,
    Tools: map[string]string{"chdman": `C:\Tools\MAME\chdman.exe`},
}
```

`romcompressor formats` and `Capabilities(opts)` report requirements separately for
compression, restoration, and verification. Availability locates the selected
engine; it does not run a version or runtime check. A located executable can still
need a runtime, keys, or an input layout supported by that engine.

NSZ/XCZ additionally require your explicit keys file through `--keys PATH` or
`Options.Keys`. RomCompressor does not find or obtain keys for you.

## Optional legacy WUX adapter

WUX works natively by default. To choose the legacy Java adapter, set `jwudtool` to
an installed [JWUDTool JAR](https://github.com/Maschell/JWUDTool) and make `java`
available on PATH, or also set `--tool java=PATH`. The JAR is a file path, not an
executable searched on PATH.

See [format details](formats.md) for supported layouts and verification limits.
