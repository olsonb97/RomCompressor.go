package engine

import (
	"bytes"
	"testing"

	kind "github.com/olsonb97/romcompressor.go/internal/format"
)

func TestParse7zEntries(t *testing.T) {
	listing := "Path = directory\r\nSize = 0\r\nAttributes = RHD_ drwxr-xr-x\r\n\r\nPath = directory/game name.gba\r\nSize = 1024\r\nAttributes = A_ -rw-r--r--\r\n\r\nPath = empty.bin\r\nSize = 0\r\nFolder = -\r\n"
	entries, err := parse7zEntries(listing)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name != "directory/game name.gba" || entries[0].Size != 1024 || entries[1].Size != 0 {
		t.Fatalf("unexpected entries: %+v", entries)
	}
}

func TestParse7zEntriesRejectsInvalidMetadata(t *testing.T) {
	for _, listing := range []string{
		"Path = game.gba\nSize = -1",
		"Path = game.gba\nSize = 18446744073709551616",
		"Path = game.gba\nSize = abc",
		"Path = game.gba\nAttributes = A",
		"Path = @files.txt\nSize = 4",
		"Path = game.bin\nSize = 4\nEncrypted = +",
		"Path = linked.bin\nSize = 4\nAttributes = A_ lrwxrwxrwx",
		"Path = junction\nSize = 0\nAttributes = DL\nFolder = +",
		"Path = pipe\nSize = 0\nAttributes = p---rw----",
		"Path = linked.bin\nSize = 0\nHard Link = game.bin",
		"Path = old.bin\nSize = 0\nAnti = +",
		"Path = tab\tname.bin\nSize = 4",
		"Path = game.gba\nSize = 4\nPath = other.gba",
		"Path = game.gba\nSize = 4\nSize = 0",
	} {
		if _, err := parse7zEntries(listing); err == nil {
			t.Errorf("accepted invalid metadata %q", listing)
		}
	}
}

func TestSevenZipRequiresConfirmedExactOutput(t *testing.T) {
	for _, test := range []struct {
		name, log, member string
		valid             bool
	}{
		{"test success", "Everything is Ok\r\n", "", true},
		{"no test confirmation", "Size: 0\n", "", false},
		{"no extracted files", "No files to process\nEverything is Ok\n", "empty.bin", false},
		{"missing extraction status", "Everything is Ok\n", "empty.bin", false},
		{"exact member", "- folder/Ã©.bin\r\nEverything is Ok\r\n", "folder/Ã©.bin", true},
		{"wrong case", "- Game.bin\nEverything is Ok\n", "game.bin", false},
		{"extraneous empty member", "- game.bin\n- empty.bin\nEverything is Ok\n", "game.bin", false},
		{"premature success", "Everything is Ok\n- empty.bin\n", "empty.bin", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := check7zResult(test.log, test.member); (err == nil) != test.valid {
				t.Fatalf("status validation returned %v, want valid=%t", err, test.valid)
			}
		})
	}
}

func TestSevenZipExpansionChecksCombinedSize(t *testing.T) {
	entries := []Entry{{Name: "first", Size: 5}, {Name: "second", Size: 6}}
	if err := check7zExpansion(entries, 11); err != nil {
		t.Fatal(err)
	}
	if err := check7zExpansion(entries, 10); err == nil {
		t.Fatal("7z accepted an aggregate expansion exceeding its limit")
	}
	entries[1].Size = ^uint64(0)
	if err := check7zExpansion(entries, kind.DefaultMaxOutput); err == nil {
		t.Fatal("7z accepted an unrepresentable member size")
	}
}

func TestExternalBoundWriterRejectsOverflow(t *testing.T) {
	var output bytes.Buffer
	writer := externalBoundWriter{writer: &output, remaining: 4}
	if n, err := writer.Write([]byte("ab")); n != 2 || err != nil {
		t.Fatalf("write: %d %v", n, err)
	}
	if n, err := writer.Write([]byte("cde")); n != 0 || err == nil {
		t.Fatalf("overflow: %d %v", n, err)
	}
	if output.String() != "ab" || writer.remaining != 2 {
		t.Fatal("overflow changed output")
	}
}
