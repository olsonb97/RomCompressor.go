# Architecture

The root package defines the stable, importable API and coordinates complete file
operations. Internal packages provide focused mechanisms without importing the root:

| Package | Responsibility |
| --- | --- |
| Root `romcompressor` | Public types, inspection, conversion and trimming workflows, ZIP/WUA trees, playlists, publication order |
| `internal/codec` | Streaming native codecs and shared Zstandard frame validation |
| `internal/trim` | Cartridge layout validation and retained boundaries |
| `internal/fileops` | File identity, cancellation-aware I/O, staging, scratch limits, exclusive filesystem operations |
| `internal/process` | Executable discovery, bounded logs, subprocesses and process-tree termination |
| `internal/disc` | CUE/GDI reference parsing and path confinement |
| `internal/chd` | CHD media parsing, metadata, engine commands and integrity checks |
| `internal/engine` | Dolphin, Azahar, NSZ, 7z, PSXPackager and legacy JWUDTool adapters |
| `internal/format` | Canonical names, format families and shared default limits |
| `internal/progress` | Phase-aware progress throttling |
| `cmd/romcompressor` | CLI syntax, human output, JSON and exit status |

Public types remain defined in the root package. Its engine bridge translates
options and archive entries at the boundary. The Dolphin adapter receives the
root inspector as a callback so signature detection has one implementation.
Engines write into caller-owned staging; they do not publish destination files.
Native WBFS/CISO restoration before Dolphin conversion stays in the root workflow.
Unit tests live beside their implementations; public engine workflows are covered
in `integration/` and run through the same exported API as consumers.

`Capabilities` reports engine requirements separately for compression, restoration,
and verification, using the caller's tool paths. It locates executables without
running them and reports key requirements separately. `Formats` retains its legacy
creation-engine summary. Missing engines expose `DependencyError`; output
collisions retain `os.ErrExist` through `os.PathError`.

`PlanConversions` shares source preflight with `Convert`: bounded native geometry,
headers, descriptors, directory metadata, and external-format route checks have
one owner. It reports eligible targets and media/entry requirements with a list of
dependencies, including multiple engines needed by a mixed playlist. Planning
does not execute tools, read keys, create staging, or establish payload integrity.

`Convert` is the file lifecycle owner. It validates a source and destination,
snapshots source identities, CUE/GDI tracks, and matching SBI companions, and
creates a private staging directory beside the output. Track snapshots use the
same path as the engine. `Convert` runs the selected codec or engine, verifies
the result, checks source stability, and publishes exclusively. The file
lifecycle checks the identity of its staging directory before reuse and cleanup.
`Verify` checks format-specific options and rechecks the source snapshot after
verification. `Options.TempDir`
selects its existing scratch parent; Dolphin, NSZ, and Azahar use private monitored
stages there with identity-safe cleanup. Conventional child TMPDIR/TMP/TEMP values
are redirected to the private stage. This is not an OS sandbox for external tools.
Conversion staging remains beside its destination for exclusive publication.
`Inspect` reports signature-based type and ZIP entries. `Batch` invokes the same conversion
path sequentially and returns a result for each attempted job.

Dreamcast restoration reads the GD-ROM track table and gives chdman private,
fixed output names. The adapter builds the public GDI from that table and
checks the normalized track data against the CHD raw SHA-1. The file workflow
publishes the verified tracks before their descriptor. Playlist restoration
reserves all track names before choosing the next disc's name.

CHD inspection shares the bounded metadata parser used before engine verification.
It reports explicit media tags, rejects conflicting tags and malformed chains,
and leaves untagged raw media unidentified. Extraction retains its explicit media
choice; metadata inspection does not verify the compressed payload.

`PlanTrim` and `Trim` are independent of compression. The cartridge parsers
validate the recorded layout and return a retained boundary. The file layer
opens the source read-only and checks the complete padding suffix before staging
any retained bytes. `PlanTrimWithOptions` adds preview progress and rechecks source
stability after callbacks. An already trimmed image is identifiable through
`ErrNoRemovablePadding`. After the final
progress callback before publication, it revalidates the layout, scans the full
suffix for padding, and verifies the staged prefix byte for byte and by SHA-256.
It checks that the staged file is the one created by this trim operation.
It then publishes a separate file through the common exclusive publication path.

`ConvertPlaylist` resolves an M3U into a bounded ordered set and records the
identity of each resolved disc. Each disc reuses `Convert` or a verified copy
when already in the target format. A new companion folder holds images. Before
publishing, the set rechecks playlist references, source discs, and companions.
Windows resolves junction targets for CUE, GDI, and M3U containment checks.
It checks the staged set against its recorded file list and identities. When
a progress callback is configured, it also rechecks the SHA-256 of each staged
disc and companion before publication.
Its playlist is published last. Rollback checks file
identity before removing a published path. A crash during multi-file publication can leave
companions without a playlist; the workflow never overwrites an existing set.
Raw restoration selects per-disc output extensions and tagged CHD media, reserves
whole stems for CUE/BIN/SBI companions, and records referenced output tracks in the
publication manifest. Already-raw single images use verified streaming copies;
raw descriptors are refused so their tracks cannot be silently omitted.

Native codecs receive ReaderAt, a known size, and a streaming output. CSO and ZSO
share the indexed block container implementation, with DEFLATE and LZ4 respectively.
Index pages and compression buffers stay bounded independently of total image size.
ZIP/GZIP use Go's standard codecs and validate checksums while decoding. ZIP
checks local member headers against the central directory and bounds each
member and its data descriptor by the next physical record. Single-member
extraction uses the caller's explicit destination.
ECM reconstructs CD sectors and verifies the whole-image EDC. WBFS and Nintendo
CISO validate bounded block maps and synthesize zeros for omitted blocks.
Split WBFS inputs join the selected part set through ReaderAt and snapshot
every part. Xbox CCI validates a sector index and uses the existing LZ4
dependency. WUX bounds its sector table by one disc and compares candidate
sectors in the staged file before reusing them.
Native compression hashes source bytes as the encoder reads them, then compares
against a full decode of the output after its final pre-publication callback.
Native restoration with a progress callback also compares the staged output
against the SHA-256 computed during decoding. These checks use bounded buffers.

The PBP adapter normalizes supported CUE track sets for PSXPackager. Its native
decoder verifies sector bytes and the track timeline before compressed output
is published. Restoration creates the CUE exclusively and publishes it after
the BIN. When a progress callback is configured, restoration rechecks the
staged BIN against the digest computed during decoding.

The WUA directory lifecycle stages a private input before calling ZArchive.
It validates archive bounds, member paths, title layouts, and the footer
checksum. Compression compares restored file hashes to the staged inputs.
Both conversion directions recheck the staged archive's footer and checksum
after the engine extracts it and before publication.
Extraction validates the complete output tree and publishes it by exclusive
directory rename.
Whole-tree ZIP uses the same directory snapshot and contained publication
principles. It verifies archive member paths and hashes and stages full
extraction in a private directory. Files retain their owner's executable bit on
Unix while keeping owner-only read/write permissions; setuid, setgid, and sticky
bits are not restored. Full permission and timestamp preservation is outside this
workflow's contract.

External adapters use local engines with exact argument arrays. Their output goes
into private staging. No shell interpolation is used. Captured process output is
limited to 4 MiB; failure errors retain at most the final 16 KiB of diagnostics
with an explicit truncation marker. Engine-specific success checks supplement
exit status. Cancellation terminates
the process tree. Linux signals the remaining process group at engine exit before
reaping its leader, and Windows closes the engine's job object. PBP and DVD CHD
metadata updates require the opened staging file to have exactly one hard link.
The scratch monitor scans bounded batches during execution and at engine exit.
It checks size, entry count, and file types.
The monitor is a resource guard, not an operating-system sandbox for external tools.

Publication uses exclusive MoveFile on Windows, Renameat2 with RENAME_NOREPLACE
on Linux, and RenamexNp with RENAME_EXCL on macOS. Other Unix targets use hard links.
These operations keep existing destinations intact during concurrent conversions.
The destination filesystem must support the required exclusive operation; otherwise
publication returns an error. On Linux, the legacy exfat-fuse 1.3.0 driver
and some Windows-mounted WSL filesystems lack exclusive rename support.
Staged files are synced before publication. Multi-file
publication supports rollback on reported failures, not a persistent crash journal.

The CLI adapts the public API. It owns syntax, defaults, help, progress presentation,
JSON, and process exit codes. Temporary picker state, Library imports, and catalog
mutation belong to applications integrating this module.
