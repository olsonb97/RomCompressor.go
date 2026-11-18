package integration_test

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	rc "github.com/olsonb97/romcompressor.go"
	"github.com/olsonb97/romcompressor.go/internal/codec"
)

// Generate complete disc sizes for codecs that require them. The ordinary
// test suite uses smaller fixtures and never needs this much temporary space.
func buildMatrixSample(t *testing.T, dir string, format rc.Format, opts rc.Options) string {
	t.Helper()
	source := matrixSource(t, dir, format, "")
	if format == rc.Raw {
		return source
	}
	if format == rc.NSZ || format == rc.XCZ {
		opts.Keys = os.Getenv("ROMCOMPRESSOR_NSZ_KEYS")
	}
	opts.Format = format
	output := filepath.Join(dir, "sample"+matrixExtension(format))
	result, err := rc.Convert(t.Context(), source, output, opts)
	if err != nil || !result.Verified {
		t.Fatalf("prepare %s sample: %+v, %v", format, result, err)
	}
	return output
}

func matrixSource(t *testing.T, dir string, format rc.Format, media string) string {
	t.Helper()
	write := func(name string, data []byte) string {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	size := int64(0)
	source := ""
	switch format {
	case rc.NSZ, rc.XCZ:
		source = os.Getenv("ROMCOMPRESSOR_NSZ_NSP")
		if format == rc.XCZ {
			source = os.Getenv("ROMCOMPRESSOR_NSZ_XCI")
		}
	case rc.ZCCI, rc.ZCIA, rc.ZCXI, rc.Z3DSX:
		source = write("source"+matrixRawExtension(format, ""), generatedAzaharInput(format))
	case rc.CCI:
		source = write("source.xiso", testXISO())
	case rc.CHD, rc.PBP:
		if media == "hdd" || media == "dvd" {
			data := make([]byte, 257*2048)
			for i := range data {
				data[i] = byte(i*19 + i/179)
			}
			return write("disc"+matrixRawExtension(rc.CHD, media), data)
		}
		write("track.bin", syntheticPSXDisc())
		source = write("disc.cue", []byte("FILE track.bin BINARY\n TRACK 01 MODE2/2352\n INDEX 01 00:00:00\n"))
	case rc.WUA:
		write("title/code/app.xml", []byte("<app><title_id>000500001234abcd</title_id><title_version>0010</title_version></app>"))
		write("title/code/cos.xml", []byte("<app><argstr>sample.rpx</argstr></app>"))
		write("title/meta/meta.xml", []byte("<menu><longname_en>Sample</longname_en></menu>"))
		write("title/content/data", []byte("generated data"))
		source = filepath.Join(dir, "title")
	case rc.WUX:
		source = write("disc.wud", []byte("generated WUD"))
		size = codec.WUDImageBytes
	default:
		image := make([]byte, 256*1024)
		copy(image, "GTEST1")
		binary.BigEndian.PutUint32(image[28:], 0xc2339f3d)
		for i := 65536; i < len(image); i++ {
			image[i] = byte(i*17 + i/251)
		}
		switch format {
		case rc.Raw:
			return write("sample.iso", image)
		case rc.WBFS:
			clear(image[28:32])
			binary.BigEndian.PutUint32(image[24:], 0x5d1c9ea3)
			size = 4699979776
		case rc.CISO:
			size = 1459978240
		}
		source = write("disc.iso", image)
	}
	if size > 0 {
		if err := os.Truncate(source, size); err != nil {
			t.Fatal(err)
		}
	}
	return source
}
