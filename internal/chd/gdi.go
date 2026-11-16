package chd

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/olsonb97/romcompressor.go/internal/disc"
	"github.com/olsonb97/romcompressor.go/internal/fileops"
)

// ValidateGDI checks track extents before chdman can round down partial sectors
// or read from byte zero despite a descriptor's nonzero offset.
func ValidateGDI(path string, limit int64) error {
	var sizes []int64
	var total int64
	if err := disc.VisitReferences(path, func(_ string, info os.FileInfo) error {
		if info.Size() > limit-total {
			return fmt.Errorf("GDI tracks exceed the expanded size limit")
		}
		sizes = append(sizes, info.Size())
		total += info.Size()
		return nil
	}); err != nil {
		return err
	}
	file, _, err := fileops.OpenRegular(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, disc.MaxDescriptorSize+1))
	scanner.Buffer(make([]byte, 510), 510)
	scanner.Scan() // The reference validation read the track count.
	var previousEnd uint64
	i := 0
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) == 0 {
			if i < len(sizes) {
				return fmt.Errorf("GDI track lines cannot be empty")
			}
			continue
		}
		if strings.ContainsAny(line, "\t\v\f\r\x00") {
			return fmt.Errorf("GDI track lines must use spaces as separators")
		}
		match := disc.MatchGDITrack(line)
		if match == nil || i >= len(sizes) || i >= 99 {
			return fmt.Errorf("invalid GDI track entry")
		}
		// Older chdman versions read quoted names by joining space-delimited
		// tokens. They collapse repeated spaces and consume the offset when
		// the opening and closing quotes appear in the same token.
		quoted, bare := match[1], match[2]
		if strings.ContainsAny(bare, "\"'") || quoted != "" && (!strings.Contains(quoted, " ") || strings.Contains(quoted, "  ")) {
			return fmt.Errorf("ambiguous GDI track filename for chdman")
		}
		number, err := strconv.Atoi(fields[0])
		if err != nil || number != i+1 {
			return fmt.Errorf("GDI tracks must be numbered in order")
		}
		lba, err := strconv.ParseUint(fields[1], 10, 31)
		if err != nil || lba < previousEnd {
			return fmt.Errorf("GDI tracks overlap or have an invalid start sector")
		}
		if i == 0 && lba != 0 {
			return fmt.Errorf("chdman cannot preserve a nonzero first-track LBA in GDI input")
		}
		sector, err := strconv.ParseInt(fields[3], 10, 32)
		kind, kindErr := strconv.ParseInt(fields[2], 10, 32)
		data := kind == 4 && (sector == 2048 || sector == 2352)
		audio := kind == 0 && sector == 2352
		if err != nil || kindErr != nil || !data && !audio {
			return fmt.Errorf("unsupported GDI track type or sector size")
		}
		offset, err := strconv.ParseUint(fields[len(fields)-1], 10, 64)
		if err != nil || offset != 0 {
			return fmt.Errorf("chdman cannot preserve a GDI track with a nonzero file offset")
		}
		if sizes[i] == 0 || sizes[i]%sector != 0 {
			return fmt.Errorf("GDI track must contain complete sectors")
		}
		previousEnd = lba + uint64(sizes[i]/sector)
		if previousEnd > math.MaxInt32 {
			return fmt.Errorf("GDI track exceeds the sector address range")
		}
		i++
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if i != len(sizes) {
		return fmt.Errorf("GDI references changed during validation")
	}
	return nil
}
