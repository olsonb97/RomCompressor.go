package romcompressor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olsonb97/romcompressor.go/internal/fileops"
)

const testWUATitle = "000500001234abcd_v16"

func rawWUAFixture(t *testing.T, contentName string) []byte {
	t.Helper()
	return rawWUAFixtureWithBlocks(t, contentName, 1)
}

func rawWUAFixtureWithBlocks(t *testing.T, contentName string, blocks int) []byte {
	t.Helper()
	data := make([]byte, wuaBlockSize*blocks)
	var names []byte
	nodes := make([]byte, 9*16)
	var offset uint32
	for i, entry := range []struct {
		name, payload string
		first, count  uint32
	}{
		{"", "", 1, 1}, {testWUATitle, "", 2, 3},
		{"code", "", 5, 2}, {"content", "", 7, 1}, {"meta", "", 8, 1},
		{"app.xml", "<app><title_id>000500001234abcd</title_id><title_version>0010</title_version></app>", 0, 0},
		{"cos.xml", "<app/>", 0, 0}, {contentName, "generated game assets", 0, 0}, {"meta.xml", "<menu/>", 0, 0},
	} {
		record := nodes[i*16:]
		nameOffset := uint32(len(names))
		if i == 0 {
			nameOffset = 0x7fffffff
		} else {
			names = append(names, byte(len(entry.name)))
			names = append(names, entry.name...)
		}
		if entry.payload != "" {
			binary.BigEndian.PutUint32(record, nameOffset|0x80000000)
			binary.BigEndian.PutUint32(record[4:], offset)
			binary.BigEndian.PutUint32(record[8:], uint32(len(entry.payload)))
			copy(data[offset:], entry.payload)
			offset += uint32(len(entry.payload))
		} else {
			binary.BigEndian.PutUint32(record, nameOffset)
			binary.BigEndian.PutUint32(record[4:], entry.first)
			binary.BigEndian.PutUint32(record[8:], entry.count)
		}
	}
	block := make([]byte, 40)
	for i := range blocks {
		binary.BigEndian.PutUint16(block[8+2*i:], 0xffff)
	}
	var archive []byte
	var footer [wuaFooterSize]byte
	for i, section := range [][]byte{data, block, names, nodes, nil, nil} {
		binary.BigEndian.PutUint64(footer[i*16:], uint64(len(archive)))
		binary.BigEndian.PutUint64(footer[i*16+8:], uint64(len(section)))
		archive = append(archive, section...)
	}
	binary.BigEndian.PutUint64(footer[128:], uint64(len(archive)+wuaFooterSize))
	binary.BigEndian.PutUint64(footer[136:], 0x61bf3a01169f52d6)
	archive = append(archive, footer[:]...)
	digest := sha256.Sum256(archive)
	copy(archive[len(archive)-wuaFooterSize+96:], digest[:])
	return archive
}

func TestWUAInspectAndChecksum(t *testing.T) {
	ctx := context.Background()
	archive := rawWUAFixture(t, "asset.bin")
	path := filepath.Join(t.TempDir(), "generated.wua")
	if err := os.WriteFile(path, archive, 0600); err != nil {
		t.Fatal(err)
	}
	info, err := Inspect(ctx, path)
	if err != nil || info.Format != WUA || len(info.Entries) != 4 || info.Entries[2].Name != testWUATitle+"/content/asset.bin" {
		t.Fatalf("unexpected WUA archive metadata %+v: %v", info, err)
	}
	if err := Verify(ctx, path, Options{}); err != nil {
		t.Fatal(err)
	}
	archive[0] ^= 1
	if err := os.WriteFile(path, archive, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Verify(ctx, path, Options{}); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum failure, got %v", err)
	}
}

func TestWUAVerifyChecksCompressedBlocks(t *testing.T) {
	// One single-segment 64 KiB Zstandard RLE frame.
	frame := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x60, 0, 0xff, 3, 0, 8, 0x5a}
	zeroDictionary := append([]byte{0x28, 0xb5, 0x2f, 0xfd, 0x61, 0}, frame[5:]...)
	requiredDictionary := bytes.Clone(zeroDictionary)
	requiredDictionary[5] = 0x2a
	shortFrame := bytes.Clone(frame)
	// The content size still says 64 KiB, but the block produces 65,535 bytes.
	copy(shortFrame[7:10], []byte{0xfb, 0xff, 7})
	// These frames use a 64 KiB window and omit the frame content size.
	windowed := []byte{0x28, 0xb5, 0x2f, 0xfd, 0, 0x30, 3, 0, 8, 0x5a}
	shortWindowed := append(bytes.Clone(windowed[:6]), 0xfb, 0xff, 7, 0x5a)
	longWindowed := bytes.Clone(windowed)
	longWindowed[6] &^= 1
	longWindowed = append(longWindowed, 0x0b, 0, 0, 0x5a)
	for _, tc := range []struct {
		name  string
		block []byte
		valid bool
	}{
		{"complete frame", frame, true},
		{"explicit zero dictionary ID", zeroDictionary, true},
		{"unavailable dictionary", requiredDictionary, false},
		{"complete frame without content size", windowed, true},
		{"short expansion with matching frame header", shortFrame, false},
		{"short expansion without content size", shortWindowed, false},
		{"excess expansion without content size", longWindowed, false},
		{"invalid frame", []byte("not zstd"), false},
		{"trailing bytes", append(bytes.Clone(frame), 0xa5), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := rawWUAFixture(t, "asset.bin")
			blockEnd := (len(tc.block) + 7) &^ 7
			archive := make([]byte, blockEnd)
			copy(archive, tc.block)
			archive = append(archive, original[wuaBlockSize:]...)
			footer := archive[len(archive)-wuaFooterSize:]
			binary.BigEndian.PutUint64(footer[8:], uint64(len(tc.block)))
			for i := 1; i < 6; i++ {
				offset := binary.BigEndian.Uint64(footer[i*16:])
				binary.BigEndian.PutUint64(footer[i*16:], offset-wuaBlockSize+uint64(blockEnd))
			}
			binary.BigEndian.PutUint16(archive[blockEnd+8:], uint16(len(tc.block)-1))
			binary.BigEndian.PutUint64(footer[128:], uint64(len(archive)))
			clear(footer[96:128])
			digest := sha256.Sum256(archive)
			copy(footer[96:128], digest[:])
			path := filepath.Join(t.TempDir(), "compressed.wua")
			if err := os.WriteFile(path, archive, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Inspect(context.Background(), path); err != nil {
				t.Fatalf("inspect should read the archive directory: %v", err)
			}
			err := Verify(context.Background(), path, Options{})
			if tc.valid && err != nil || !tc.valid && (err == nil || !strings.Contains(err.Error(), "WUA compressed block")) {
				t.Fatalf("Verify = %v, valid frame = %v", err, tc.valid)
			}
		})
	}
}

func TestWUARejectsUnsafeMetadata(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"../asset.bin", "..", `C:\file.bin`, `content\escape`, "NUL.dat", "asset. ", "long" + strings.Repeat("x", 125)} {
		t.Run(name, func(t *testing.T) {
			archive := rawWUAFixture(t, name)
			if _, err := inspectWUA(ctx, bytes.NewReader(archive), int64(len(archive))); err == nil {
				t.Fatal("accepted unsafe WUA member")
			}
		})
	}
	archive := rawWUAFixture(t, "asset.bin")
	footer := archive[len(archive)-wuaFooterSize:]
	beginTree := int(binary.BigEndian.Uint64(footer[48:]))
	// The first title directory refers back to the root, creating a cycle.
	binary.BigEndian.PutUint32(archive[beginTree+16+4:], 0)
	if _, err := inspectWUA(ctx, bytes.NewReader(archive), int64(len(archive))); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected directory cycle rejection, got %v", err)
	}
}

func TestWUARejectsFileExtentsThatSkipOrOverlapData(t *testing.T) {
	for _, test := range []struct {
		name   string
		index  int
		offset func(uint32) uint32
	}{
		{"overlap", 7, func(uint32) uint32 { return 0 }},
		{"gap", 8, func(v uint32) uint32 { return v + 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			archive := rawWUAFixture(t, "asset.bin")
			footer := archive[len(archive)-wuaFooterSize:]
			tree := int(binary.BigEndian.Uint64(footer[48:]))
			offset := archive[tree+test.index*16+4:]
			binary.BigEndian.PutUint32(offset, test.offset(binary.BigEndian.Uint32(offset)))
			clear(footer[96:128])
			digest := sha256.Sum256(archive)
			copy(footer[96:128], digest[:])
			path := filepath.Join(t.TempDir(), "inconsistent.wua")
			if err := os.WriteFile(path, archive, 0600); err != nil {
				t.Fatal(err)
			}
			if err := Verify(context.Background(), path, Options{}); err == nil || !strings.Contains(err.Error(), "WUA file extents") {
				t.Fatalf("expected WUA geometry rejection despite valid checksum, got %v", err)
			}
		})
	}
}

func TestWUARejectsUnusedFullDataBlock(t *testing.T) {
	archive := rawWUAFixtureWithBlocks(t, "asset.bin", 2)
	_, err := inspectWUA(context.Background(), bytes.NewReader(archive), int64(len(archive)))
	if err == nil || !strings.Contains(err.Error(), "unused data blocks") {
		t.Fatalf("expected WUA to reject a full unmapped block, got %v", err)
	}
}

func TestWUARejectsNonzeroFinalStoredPadding(t *testing.T) {
	archive := rawWUAFixture(t, "asset.bin")
	// The last byte of the final stored block is outside every file extent.
	archive[wuaBlockSize-1] = 0x42
	footer := archive[len(archive)-wuaFooterSize:]
	clear(footer[96:128])
	digest := sha256.Sum256(archive)
	copy(footer[96:128], digest[:])
	path := filepath.Join(t.TempDir(), "hidden-data.wua")
	if err := os.WriteFile(path, archive, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Verify(context.Background(), path, Options{}); err == nil || !strings.Contains(err.Error(), "nonzero padding") {
		t.Fatalf("expected padding rejection despite valid checksum, got %v", err)
	}
}

func TestWUAPreparationLayoutAndBounds(t *testing.T) {
	source := filepath.Join(t.TempDir(), "generated-title")
	makeWUATitleFixture(t, source, "000500001234abcd", "0010")
	ctx := context.Background()
	manifest, _, prefix, err := prepareWUATitleTree(ctx, source, defaultMaxOutput)
	if err != nil || prefix != testWUATitle+"/" || manifest.entries[prefix+"content/data.bin"] == 0 {
		t.Fatalf("unexpected WUA title tree: %+v, %q, %v", manifest, prefix, err)
	}
	if _, _, _, err := prepareWUATitleTree(ctx, source, 10); err == nil || !strings.Contains(err.Error(), "expansion limit") {
		t.Fatalf("expected expansion limit, got %v", err)
	}
	if _, err := Convert(ctx, source, filepath.Join(source, "self.wua"), Options{Format: WUA}); err == nil || !strings.Contains(err.Error(), "outside the source") {
		t.Fatalf("expected output inside source rejection, got %v", err)
	}
}

func TestWUATitleMetadataUsesLiteralElementNames(t *testing.T) {
	const fields = "<title_id>000500001234abcd</title_id><title_version>0010</title_version>"
	for _, test := range []struct {
		name  string
		xml   string
		valid bool
	}{
		{"unqualified", "<app>" + fields + "</app>", true},
		{"typed scalars with whitespace", "<app><title_id type=\"hexBinary\" length=\"8\">\n 000500001234abcd \t</title_id><title_version type=\"hexBinary\" length=\"2\"> 0010\n</title_version></app>", true},
		{"single CDATA", "<app><title_id><![CDATA[000500001234abcd]]></title_id><title_version><![CDATA[ 0010 ]]></title_version></app>", true},
		{"nested ID fragment", "<app><title_id>00050000<part/>1234abcd</title_id><title_version>0010</title_version></app>", false},
		{"split version across CDATA", "<app><title_id>000500001234abcd</title_id><title_version>00<![CDATA[10]]></title_version></app>", false},
		{"split across comment", "<app><title_id>00050000<!-- comment -->1234abcd</title_id><title_version>0010</title_version></app>", false},
		{"nonbreaking space before ID", "<app><title_id>&#xa0;000500001234abcd</title_id><title_version>0010</title_version></app>", false},
		{"UTF-8 BOM and comments", "\ufeff<?xml version=\"1.0\"?>\n<!-- title --><app>" + fields + "</app>\n<!-- end -->", true},
		{"duplicate title ID", "<app>" + fields + "<title_id>0005000011111111</title_id></app>", false},
		{"duplicate version", "<app>" + fields + "<title_version>0011</title_version></app>", false},
		{"second root", "<app>" + fields + "</app><app>" + fields + "</app>", false},
		{"leading junk", "incomplete<app>" + fields + "</app>", false},
		{"trailing junk", "<app>" + fields + "</app>incomplete", false},
		{"truncated closing document", "<app>" + fields + "</app><", false},
		{"default namespace", `<app xmlns="urn:app">` + fields + "</app>", true},
		{"prefixed root", `<other:app xmlns:other="urn:other">` + fields + "</other:app>", false},
		{"prefixed ID", `<app xmlns:other="urn:other"><other:title_id>000500001234abcd</other:title_id><title_version>0010</title_version></app>`, false},
		{"prefixed version", `<app xmlns:other="urn:other"><title_id>000500001234abcd</title_id><other:title_version>0010</other:title_version></app>`, false},
		{"lookalikes ignored", `<app xmlns:other="urn:other">` + fields + "<other:title_id>0123456789abcdef</other:title_id><other:title_version>ffff</other:title_version></app>", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "app.xml")
			if err := os.WriteFile(file, []byte(test.xml), 0600); err != nil {
				t.Fatal(err)
			}
			name, err := readWUATitleName(context.Background(), file)
			if test.valid && (err != nil || name != testWUATitle) {
				t.Fatalf("readWUATitleName = %q, %v; want %q", name, err, testWUATitle)
			}
			if !test.valid && err == nil {
				t.Fatalf("accepted ambiguous or malformed XML metadata as %q", name)
			}
		})
	}
}

func TestWUAPlanAndConversionRefuseConflictingTitleIDs(t *testing.T) {
	root := t.TempDir()
	makeWUATitleFixture(t, root, "000500001234abcd", "0010")
	metadata := []byte("<app><title_id>000500001234abcd</title_id><title_id>0005000011111111</title_id><title_version>0010</title_version></app>")
	if err := os.WriteFile(filepath.Join(root, "code", "app.xml"), metadata, 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanConversions(t.Context(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range plan.Targets {
		if target.Format == WUA {
			t.Fatal("planner offered WUA for conflicting title IDs")
		}
	}
	out := filepath.Join(t.TempDir(), "title.wua")
	if _, err := Convert(t.Context(), root, out, Options{Format: WUA}); err == nil || !strings.Contains(err.Error(), "title_id") {
		t.Fatalf("expected a metadata error before running ZArchive, got %v", err)
	}
	if _, err := os.Lstat(out); !os.IsNotExist(err) {
		t.Fatalf("unexpected archive after metadata error: %v", err)
	}
}

func makeWUATitleFixture(t *testing.T, root, id, version string) {
	t.Helper()
	for _, dir := range []string{"code", "content/empty", "meta"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string][]byte{
		"code/app.xml":      []byte(fmt.Sprintf("<app><title_id type=\"hexBinary\" length=\"8\">%s</title_id><title_version type=\"hexBinary\" length=\"2\">%s</title_version></app>", id, version)),
		"code/cos.xml":      []byte("<app><argstr>sample.rpx</argstr></app>"),
		"content/data.bin":  bytes.Repeat([]byte("sample payload\x00\xff"), 8192),
		"content/empty.txt": {},
		"meta/meta.xml":     []byte("<menu><longname_en>Generated fixture</longname_en></menu>"),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestZArchiveWUAIntegration(t *testing.T) {
	tool := os.Getenv("ROMCOMPRESSOR_ZARCHIVE")
	if tool == "" {
		t.Skip("ROMCOMPRESSOR_ZARCHIVE is not set")
	}
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, bundled := range []bool{false, true} {
		t.Run(fmt.Sprintf("bundled=%t", bundled), func(t *testing.T) {
			dir := filepath.Join(root, fmt.Sprintf("bundle-%t", bundled))
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(dir, "titles")
			base := source
			if bundled {
				base = filepath.Join(source, testWUATitle)
				update := filepath.Join(source, "0005000e1234abcd_v32")
				makeWUATitleFixture(t, update, "0005000e1234abcd", "0020")
				if err := os.RemoveAll(filepath.Join(update, "content")); err != nil {
					t.Fatal(err)
				}
				makeWUATitleFixture(t, filepath.Join(source, "0005000c1234abcd_v1"), "0005000c1234abcd", "0001")
			}
			makeWUATitleFixture(t, base, "000500001234abcd", "0010")
			archive := filepath.Join(dir, "generated.wua")
			restored := filepath.Join(dir, "restored")
			opts := Options{Format: WUA, Tools: map[string]string{"zarchive": tool}}
			packed, err := Convert(ctx, source, archive, opts)
			if err != nil || !packed.Verified {
				t.Fatalf("pack did not verify: %+v, %v", packed, err)
			}
			if err := Verify(ctx, archive, Options{}); err != nil {
				t.Fatal(err)
			}
			opts.Format = Raw
			unpacked, err := Convert(ctx, archive, restored, opts)
			if err != nil || !unpacked.Verified || unpacked.OutputBytes != packed.InputBytes {
				t.Fatalf("unpack did not verify: %+v, %v", unpacked, err)
			}
			manifest, _, prefix, err := prepareWUATitleTree(ctx, source, defaultMaxOutput)
			if err != nil {
				t.Fatal(err)
			}
			actual, _, err := readWUATree(ctx, restored, defaultMaxOutput)
			if err != nil || !sameWUAEntries(manifest, actual) {
				t.Fatalf("restored WUA tree differs: %v", err)
			}
			for name, size := range manifest.entries {
				if size >= 0 {
					before := filepath.Join(source, filepath.FromSlash(strings.TrimPrefix(name, prefix)))
					after := filepath.Join(restored, filepath.FromSlash(name))
					if err := fileops.CompareFiles(ctx, before, after); err != nil {
						t.Fatalf("restored WUA member %q differs: %v", name, err)
					}
				}
			}
			if _, err := Convert(ctx, archive, restored, opts); err == nil || !strings.Contains(err.Error(), "already exists") {
				t.Fatalf("expected overwrite rejection, got %v", err)
			}
		})
	}
}
