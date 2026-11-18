# Format details

The [README](../README.md) summarizes supported formats. These restrictions protect
data that a format or engine cannot represent faithfully. Keep original images
until you have confirmed the result works in your intended emulator or device.

## Native formats

- **ZIP / gzip:** ZIP accepts a file or directory. Single-member extraction uses
  an explicit destination; `--all` restores the complete ZIP tree, including empty
  directories. Verification checks all ZIP members by default. Gzip restoration
  reads concatenated members in order under one output limit.
  Folder restoration uses private owner read/write permissions and preserves the
  owner-executable bit on Unix. Original modification times are not restored.
- **CSO / ZSO:** creation writes CSO v1 or ZSO v1. Restoration accepts CSO v0/v1
  and ZSO v1. CSO v2 is rejected. These containers do not provide a whole-image
  checksum, so standalone structural verification cannot establish provenance.
- **ECM:** models recognized aligned CD sectors and preserves every source byte.
  Keep a separate CUE with the original track set. ECM processes one image file.
- **WBFS / Nintendo CISO:** creation requires full-sized images and omits only
  all-zero blocks. CISO also accepts GameCube `.gcm` images. Restoration fills
  omitted blocks with zeros; data scrubbed by another tool cannot be recovered.
  Creation retains the final block to preserve layer size. Split WBFS is read
  from `.wbfs` with a complete, contiguous `.wbf1` through `.wbf9` sequence.
- **Xbox CCI:** one XISO with an XDVDFS volume at sector 32, in an unnumbered
  `.cci` file. The `CCIM` signature distinguishes it from Nintendo 3DS cartridges
  sharing the extension. Split CCI is outside this route.
- **WUX:** full 25,025,314,816-byte WUD images with 32 KiB sectors. The default
  encoder and decoder run in Go. `--tool jwudtool=PATH` explicitly selects the
  optional legacy Java/JWUDTool adapter instead.

## CHD

Install [MAME chdman](https://github.com/mamedev/mame) separately. For a bare image,
select `--media cd`, `--media dvd`, or `--media hdd`. CUE/GDI selects CD media.
Ordinary CD restoration writes CUE/BIN; supported GD-ROMs use GDI and split
BINs. Restoration stops if it would omit subcode. DVD creation
uses raw image commands and standard DVD metadata. HDD images require complete
512-byte sectors and an exact CHS geometry; restoration validates v5 geometry
and stored checksums. Verification requires an embedded checksum.

Run `inspect image.chd` to see the media type recorded in its metadata and the
matching restoration flag. The Go API exposes this as `FileInfo.Media`.
Untagged raw CHDs remain unidentified; a 2048-byte sector size does not prove DVD
media. Restoration still requires an explicit media choice. Metadata inspection
does not decode or verify the compressed payload.

Use a current upstream chdman for discs with Zstandard compression (`cdzs`).
Version 0.251 lacks that codec and can crash intermittently on very small
multi-track CUE inputs; an engine failure aborts conversion before publication.
Very small CD images can also trigger compressed-map errors in chdman 0.251 and 0.289.
The library refuses publication when the generated CHD cannot pass verification.

Descriptor lines must fit chdman's 510-byte read limit, including line endings.
Use uppercase, unquoted layout directives and type/subcode tokens, ASCII whitespace,
and a newline after every CUE directive. Quote filenames containing apostrophes
with double quotes. On Linux and macOS, use `/` for directories in CUE and GDI
track references. chdman also requires the descriptor's own path to be free of
literal backslashes on these platforms.

Supported CD layouts have these additional restrictions:

- CUE input supports a single session. Multisession and explicit session lead-in
  or lead-out markers are refused because chdman can omit sectors from a shared BIN.
  A plain `REM SESSION 01` label is accepted. Before publishing a CHD from
  a CUE, the library compares the track layout and a checksum of the source
  sectors, including audio and subcode, with the generated CHD.
- Restoration preserves the `MODE2_FORM_MIX` track declaration. chdman also
  uses `MODE2/2336` for plain Mode 2 tracks, so the library uses the CHD
  track table to distinguish them. For standard CD track tables, planning
  excludes CUE/BIN restoration if it would discard subcode or a distinct
  stored pregap mode.
- Dreamcast GD-ROM v5 CHDs with dedicated track metadata restore to a GDI and
  one BIN per track. Use `--media cd` for a file; M3U restoration chooses the
  descriptor per disc. GDI preserves the start sector, data or audio type and
  sector size for MODE1, MODE1_RAW and AUDIO tracks. Restoration checks the
  reconstructed sectors against the CHD's raw SHA-1, including audio byte
  order, inter-track padding and four-frame alignment. Layouts with separate
  pregap/postgap metadata, subcode, a padded final track, or nonzero omitted
  sector data cannot be restored through this GDI route.
- CUE binary tracks must contain complete sectors. Track flags, indexes above 01,
  MOTOROLA tracks, multiple tracks sharing a WAVE, and multi-file CUEs with
  multiple tracks sharing a BIN are refused.
- WAVE tracks require 44.1 kHz, 16-bit stereo PCM, plain AUDIO mode, a single
  data chunk after the format chunk, complete sectors, and indexes within the
  audio data. Odd-sized chunks before audio are refused.
- A virtual `PREGAP` combined with stored sectors between `INDEX 00` and `INDEX 01`
  is refused. Dreamcast multi-CUE density layouts are also refused because chdman
  can discard stored pregaps.
- GDI requires complete sectors, first-track LBA zero, zero file offsets, and
  space-separated fields. Leave names without spaces unquoted; use double quotes
  for names containing single spaces.
- NRG input requires one Nero 5.5+ DAOX session below 4 GiB with consecutive,
  complete tracks and no stored pregaps. Restoration produces CUE/BIN rather
  than recreating the original Nero container.

## PBP

Creation uses [PSXPackager](https://github.com/RupertAvery/PSXPackager) and takes a
CUE with raw `MODE2/2352` data and optional `AUDIO` tracks in BINARY files.
Stored pregaps and track indices survive restoration. Virtual pregaps, unsupported
CUE directives, encryption, and multi-disc PBP are refused. Use an M3U to convert
multiple discs into individual PBPs. Verification and restoration run natively;
PSXPackager is required only for creation.

## Dolphin formats

RVZ, WIA, and GCZ use
[Dolphin Tool](https://github.com/dolphin-emu/dolphin). Verification compares
logical disc hashes. Planning, conversion, and verification check the declared
ISO size against the configured output limit. The disc hash covers logical disc
data. Inputs with different container bytes or reconstructed padding can share the
same logical data. Check the target emulator's supported formats.

## Switch formats

NSZ/XCZ require [NSZ 5.0+](https://github.com/nicoboss/nsz) and an explicit
`--keys PATH`. The library never searches for keys. Compression restores and
compares the complete NSP/XCI byte for byte before publication. NSZ 5 reconstructs
the gamecard header with its root HFS0 at offset `0xf000`. Planning and conversion
refuse XCI/XCZ files with another root offset or nonzero bytes between the
512-byte header and that partition, including the gamecard certificate.
Other differences in reconstructed XCI layouts also prevent publication.
Standalone verification checks the NCA hashes the engine can verify. An
upstream-generated multi-partition XCZ passed those checks, but NSZ could
not verify the restored XCI. Restoration failed without publishing that file.

An encrypted NSP and a generated single-partition XCI containing encrypted
content both round-tripped byte for byte with NSZ 5.0. A retail-layout XCI
with nonzero certificate-region data was refused. NSZ may stage an input
whose resolved filename lacks the expected lowercase extension. Reserve space
for the compressed output, private copies, and uncompressed verification output.

## Wii U title archives

WUA uses [ZArchive](https://github.com/Exzap/ZArchive). Input is a decrypted title
directory or multiple `TITLEID_vVERSION` folders. Names are derived from
`code/app.xml`, which must declare one title ID and one version in a single
`app` element. Conflicting declarations are refused. Restoration checks the
metadata against the folder name and writes the title trees to a new directory.

The route accepts up to 100,000 nodes, portable ASCII member names of at most
127 bytes, and paths of at most 1,024 bytes. Standalone verification checks archive
checksums, expansion bounds, and Zstandard frame structure. ZArchive decodes
payloads during restoration. Scratch limits cover the private tree, archive,
and verification data while each exists.

## Nintendo 3DS

ZCCI/ZCIA/ZCXI/Z3DSX use the [Azahar CLI](https://github.com/azahar-emu/azahar)
with decrypted inputs. The reader accepts both unpadded metadata and the
zero-aligned metadata written by Azahar 2126.2. Verification can require an
uncompressed temporary copy.

## 7z

7z uses [7-Zip](https://www.7-zip.org/) or `7zr` for one selected file.
The filename must be valid UTF-8, contain no control characters, and start
with a character other than `@` so the engine can select it for restoration.
On Windows, `--entry nested/file.bin` also selects the member displayed as
`nested\file.bin`. Use ZIP for a complete directory tree.

## M3U disc sets

M3U compression and restoration create a new playlist plus a new companion folder,
preserving order and supported SBI files. Restoration accepts CSO/ZSO, RVZ/WIA/GCZ,
WBFS, Nintendo CISO, Xbox CCI, ECM, single-disc PBP, and supported CD/DVD CHD media.
PBP and ordinary CD CHD restore as CUE/BIN. Dreamcast GD-ROM CHDs restore as
GDI and split track files. Image containers restore their individual image.
ECM restores its selected image without reconstructing a separate CUE.

Tagged CHD CD/DVD media is selected per disc unless the caller supplies a media
choice. An unidentified CHD requires an explicit supported choice; untagged CD
images lack the required track metadata and remain refused. HDD/laserdisc CHDs
and already-raw CUE/GDI entries are not playlist-restoration inputs. Already-raw
single-image entries (`.iso`, `.gcm`, `.xiso`, `.bin`, `.img`, `.cdr`, `.toast`,
`.cdi`, and `.nrg`) are copied with byte verification. No referenced source is
modified, and the replacement playlist is published only after the full set passes.

## Cartridge trimming

`trim` is independent of compression. Supported inputs are DS/DSi cartridges,
3DS CARD1 cartridges, and standard Switch XCI gamecards with T1 headers.
Writable CARD2/NAND regions, DSiWare, ambiguous layouts, and non-padding tails
are refused. XCI checks agreement between the header and secure HFS0 partition,
including declared SHA-256 regions. The full proposed suffix must contain uniform
`00` or `FF` padding, and every retained byte is checked before a new copy is
published. Keep the source unchanged during trimming.
The preview scans the proposed padding without writing output. If an image has no
removable padding, trimming writes no copy.

## Verification limits

`Result.Verified` means the applicable format checks completed. Formats without
embedded checksums cannot establish that a pre-existing image has never changed.
Native creation compares decoded bytes against the source; external verification
uses the documented engine and format contracts. Internal round trips do not
replace independent compatibility checks or emulator testing. Retail-game
playthroughs are outside the verification performed for this release. JWUDTool
0.4 compressed and restored a generated 25,025,314,816-byte WUD byte for byte.
