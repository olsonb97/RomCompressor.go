package main

import (
	"fmt"
	"io"
)

func writeHelp(w io.Writer, command string) {
	if command == "" {
		fmt.Fprintln(w, `Usage: romcompressor COMMAND [options]

Commands:
  compress    Compress a game or file
  decompress  Restore an uncompressed copy
  trim        Write a cartridge copy without verified trailing padding
  inspect     Show a file's format and archive entries
  verify      Check a compressed file
  formats     List formats and required engines
  plan        Preview conversion choices for an input
  batch       Convert files from a JSON manifest
  version     Show the program version (also --version)

Examples:
  romcompressor compress game.iso -f cso
  romcompressor compress game.m3u -f chd -o compressed.m3u
  romcompressor decompress game.cso -o restored.iso
  romcompressor inspect games.zip
  romcompressor plan game.iso --json
  romcompressor batch jobs.json --json

Native formats work without extra tools. Other formats require separately
installed engines; run formats to check each operation. Download links are in
https://github.com/olsonb97/RomCompressor.go#format-support

Run romcompressor COMMAND --help for command options.`)
		return
	}
	switch command {
	case "compress":
		fmt.Fprintln(w, `Usage: romcompressor compress SOURCE -f FORMAT [-o OUTPUT]

  -f, --format FORMAT  Output format; see romcompressor formats
  -o, --output PATH    Output filename (derived from source by default)
  --media cd|dvd|hdd   Media type when creating CHD from a raw image

An .m3u source converts its discs together and writes a new .m3u playlist.
Use an .m3u output filename for a playlist conversion.
WUA compression takes a decrypted Wii U title directory as SOURCE.`)
	case "decompress":
		fmt.Fprintln(w, `Usage: romcompressor decompress SOURCE [-o OUTPUT] [-s ENTRY]

  -o, --output PATH    Output path; required when ambiguous
  -s, --entry NAME     Exact archive entry to extract (see inspect)
  --all                Restore a complete ZIP tree to a new directory (-o required)
  --media cd|dvd|hdd   CHD media type; CD output uses a .cue with a .bin sibling

Use inspect to see the media type recorded in a CHD, when tagged.
An .m3u source restores every supported disc into a new companion folder and writes
a new playlist (default: NAME-restored.m3u). Tagged CHD media is selected per disc;
unidentified CHDs need an explicit supported --media value. CD track metadata is
required for CD extraction. Raw CUE/GDI entries are not restored.
PBP extraction writes a .cue and .bin. WUA extraction writes a new directory.`)
	case "trim":
		fmt.Fprintln(w, `Usage: romcompressor trim SOURCE [-n | -o OUTPUT]

  -n, --dry-run       Report the verified padding and resulting size
  -o, --output PATH   New output file (default: NAME.trim.EXT)

Supported inputs are DS/DSi cartridges, 3DS CARD1 cartridges, and Switch XCI.
The source is opened read-only. Mixed or non-padding tails are rejected.`)
	case "inspect":
		fmt.Fprintln(w, "Usage: romcompressor inspect FILE [--json]\n\n  --tool ENGINE=PATH  Override the 7z executable when listing a 7z archive")
	case "verify":
		fmt.Fprintln(w, `Usage: romcompressor verify FILE [-s ENTRY]

  -s, --entry NAME     Exact ZIP entry to verify
  --temp-dir PATH     Existing parent folder for verification scratch data
  --media cd|dvd|hdd   Check the requested CHD media type`)
	case "plan":
		fmt.Fprintln(w, `Usage: romcompressor plan SOURCE [--json]

Reports input-specific conversion choices and required engines without creating
output files or running engines. Preflight does not verify the full payload or
guarantee engine/runtime compatibility.
  --tool ENGINE=PATH  Check a selected engine path (repeatable)
  --max-output SIZE   Expanded/final byte limit used during preflight`)
	case "formats":
		fmt.Fprintln(w, `Usage: romcompressor formats [--tool ENGINE=PATH] [--json]

Shows compression, decompression, and verification dependencies separately.
  --tool ENGINE=PATH  Check a particular engine installation (repeatable)

Availability checks executable paths, not engine versions, keys, or game layouts.
External tools are installed separately; see the README for download links.`)
	case "version":
		fmt.Fprintln(w, "Usage: romcompressor version [--json]")
	case "batch":
		fmt.Fprintln(w, `Usage: romcompressor batch MANIFEST [--json]

Manifest:
  {"jobs":[{"source":"game.iso","output":"game.cso","format":"cso"}]}

Paths are relative to the manifest. Jobs run sequentially. Successful outputs
are kept if another job fails. Any failure returns a nonzero exit status.
Use "format":"raw" for a decompression job.
  --tool ENGINE=PATH  Override an engine executable (repeatable)
  --keys PATH         Override the key file for Switch jobs`)
	}
	if command == "compress" || command == "decompress" || command == "verify" {
		fmt.Fprintln(w, `  --tool ENGINE=PATH  Select a separately installed engine (repeatable)
  --keys PATH         Key file used for Switch conversions and verification
  --max-output SIZE   Byte count or whole KiB/MiB/GiB/TiB size (for example 4GiB)
                      Expanded/final byte limit and external staging budget
                      (default 1 TiB; includes verification scratch data)`)
	}
	if command == "compress" || command == "decompress" || command == "trim" {
		fmt.Fprintln(w, "Sources are preserved. Existing output paths are never replaced.")
	}
	fmt.Fprintln(w, "  -j, --json          Write machine-readable output\n  -h, --help          Show command help")
}
