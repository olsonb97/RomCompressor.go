package codec

import (
	"context"
	"io"
)

// Options controls bounded streaming compression and decompression.
type Options struct {
	Entry     string
	Name      string
	MaxOutput int64
	Progress  func(int64, int64)
}

// Encode writes a container to a seekable staging file.
func Encode(ctx context.Context, format string, src io.ReaderAt, size int64, dst io.WriteSeeker, opts Options) error {
	return encode(ctx, format, src, size, dst, opts)
}

// Decode writes the expanded payload to dst. It must reject malformed/truncated input and enforce MaxOutput.
func Decode(ctx context.Context, format string, src io.ReaderAt, size int64, dst io.Writer, opts Options) error {
	return decode(ctx, format, src, size, dst, opts)
}

// Verify checks every ZIP file when Entry is empty, or the selected payload.
// MaxOutput bounds the combined expanded size when verifying an entire ZIP.
func Verify(ctx context.Context, format string, src io.ReaderAt, size int64, opts Options) error {
	if format == "zip" {
		return verifyZIP(ctx, src, size, opts)
	}
	return Decode(ctx, format, src, size, io.Discard, opts)
}
