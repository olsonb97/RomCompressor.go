package chd

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/olsonb97/romcompressor.go/internal/fileops"
	"github.com/olsonb97/romcompressor.go/internal/process"
)

const hddSectorSize = 512

func createHDDCHD(ctx context.Context, tool, source, output string, limit int64) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || info.Size()%hddSectorSize != 0 {
		return fmt.Errorf("raw hard-drive image must contain complete 512-byte sectors")
	}
	if info.Size() > limit {
		return fmt.Errorf("raw hard-drive image exceeds the expanded size limit")
	}
	cylinders, heads, sectors := exactHDDGeometry(info.Size() / hddSectorSize)
	chs := fmt.Sprintf("%d,%d,%d", cylinders, heads, sectors)
	if _, err := process.Run(ctx, tool, "createhd", "-i", source, "-o", output, "-ss", "512", "-chs", chs, "-np", "2"); err != nil {
		return err
	}
	image, err := readHDDCHD(output, limit)
	if err != nil {
		return err
	}
	if image.size != info.Size() || image.sectorSize != hddSectorSize {
		return fmt.Errorf("created hard-drive CHD does not match the source geometry")
	}
	if err := Verify(ctx, tool, output, limit, "hdd"); err != nil {
		return err
	}
	return image.checkRaw(ctx, source)
}

func extractHDDCHD(ctx context.Context, tool, source, output string, limit int64) error {
	image, err := readHDDCHD(source, limit)
	if err != nil {
		return err
	}
	if err := Verify(ctx, tool, source, limit, "hdd"); err != nil {
		return err
	}
	if _, err := process.Run(ctx, tool, "extracthd", "-i", source, "-o", output); err != nil {
		return err
	}
	return image.checkRaw(ctx, output)
}

// chdman 0.251 can round up to a different CHS capacity when guessing from
// byte length. Prefer its usual 16-head/63-sector range, but let single-head
// and single-sector geometries represent sizes with fewer divisors.
func exactHDDGeometry(total int64) (cylinders, heads, sectors int64) {
	for sectors := int64(63); sectors >= 2; sectors-- {
		for heads := int64(16); heads >= 1; heads-- {
			if total%(sectors*heads) == 0 {
				return total / (sectors * heads), heads, sectors
			}
		}
	}
	return total, 1, 1
}

func readHDDCHD(path string, limit int64) (rawCHD, error) {
	file, info, err := fileops.OpenRegular(path)
	if err != nil {
		return rawCHD{}, err
	}
	defer file.Close()
	var header [chdV5HeaderSize]byte
	image, err := readCHDV5Header(file, &header)
	if err != nil {
		return rawCHD{}, err
	}
	if image.size > limit {
		return rawCHD{}, fmt.Errorf("hard-drive CHD exceeds the expanded size limit")
	}
	firstMeta := binary.BigEndian.Uint64(header[48:56])
	return image, checkHDDMetadata(file, uint64(info.Size()), firstMeta, image)
}

func checkHDDMetadata(src io.ReaderAt, size, offset uint64, image rawCHD) error {
	const maxGeometryBytes = 128
	foundGeometry := false
	err := walkCHDMetadata(src, size, chdV5HeaderSize, offset, func(entry chdMetadata) error {
		switch entry.tag {
		case "GDDD":
			if foundGeometry || entry.length < 2 || entry.length > maxGeometryBytes || entry.flags&1 == 0 {
				return fmt.Errorf("invalid or unverified hard-drive CHD geometry metadata")
			}
			var geometry [maxGeometryBytes]byte
			data := geometry[:entry.length]
			if _, err := src.ReadAt(data, entry.offset); err != nil {
				return err
			}
			if err := validateHDDGeometry(data, image); err != nil {
				return err
			}
			foundGeometry = true
		default:
			if chdMetadataMedia(entry.tag) != "" {
				return fmt.Errorf("CHD metadata identifies a different media type")
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !foundGeometry {
		return fmt.Errorf("CHD lacks hard-drive geometry metadata")
	}
	return nil
}

func validateHDDGeometry(data []byte, image rawCHD) error {
	text, ok := strings.CutSuffix(string(data), "\x00")
	fields := strings.Split(text, ",")
	labels := [...]string{"CYLS:", "HEADS:", "SECS:", "BPS:"}
	if !ok || len(fields) != len(labels) {
		return fmt.Errorf("invalid CHD hard-drive geometry")
	}
	capacity := uint64(1)
	for i, field := range fields {
		value, ok := strings.CutPrefix(field, labels[i])
		n, err := strconv.ParseUint(value, 10, 32)
		if !ok || err != nil || n == 0 || capacity > uint64(image.size)/n || (i == 3 && n != uint64(image.sectorSize)) {
			return fmt.Errorf("CHD hard-drive geometry disagrees with the image header")
		}
		capacity *= n
	}
	if capacity != uint64(image.size) {
		return fmt.Errorf("CHD hard-drive geometry disagrees with the image size")
	}
	return nil
}
