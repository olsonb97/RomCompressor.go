package codec

import (
	"context"
	"errors"
	"io"
)

// PreflightEncode checks input geometry and the bounded header fields used by
// native encoders. It does not read payloads, estimate compression, or verify data.
func PreflightEncode(ctx context.Context, format string, src io.ReaderAt, size, limit int64) error {
	opts, err := normalize(ctx, size, Options{MaxOutput: limit})
	if err != nil {
		return err
	}
	if size > opts.MaxOutput {
		return errors.New("input exceeds expanded size limit")
	}
	src = contextReaderAt{ReaderAt: src, ctx: ctx}
	switch format {
	case "cso", "zso":
		return validateCSOSize(size)
	case "wbfs", "ciso":
		if err := validateSparseSize(format, size); err != nil {
			return err
		}
		var header [32]byte
		if _, err := src.ReadAt(header[:], 0); err != nil {
			return err
		}
		return validateSparseHeader(header[:], size)
	case "cci":
		if err := validateCCISize(size); err != nil {
			return err
		}
		var sector [cciSectorBytes]byte
		if _, err := src.ReadAt(sector[:], xisoVolume*cciSectorBytes); err != nil {
			return err
		}
		return validateXISOVolume(sector[:], size)
	case "wux":
		return validateWUXSize(size)
	default:
		return nil
	}
}

// PreflightDecode checks bounded native disc metadata and known expanded sizes.
// It returns the known expanded byte count, or -1 for stream formats whose size
// requires decoding. Archive payloads, checksums, and ECM need full decoding.
func PreflightDecode(ctx context.Context, format string, src io.ReaderAt, size, limit int64) (int64, error) {
	opts, err := normalize(ctx, size, Options{MaxOutput: limit})
	if err != nil {
		return -1, err
	}
	src = contextReaderAt{ReaderAt: src, ctx: ctx}
	switch format {
	case "cso", "zso":
		geometry, err := readCSOGeometry(format, src, size, opts.MaxOutput)
		return int64(geometry.total), err
	case "wbfs", "ciso":
		var disc *sparseDisc
		if format == "wbfs" {
			disc, err = parseWBFS(ctx, src, size)
		} else {
			disc, err = parseCISO(ctx, src, size)
		}
		if err != nil {
			return -1, err
		}
		if disc.size > opts.MaxOutput {
			return -1, errors.New("disc exceeds expanded size limit")
		}
		return disc.size, ctx.Err()
	case "cci":
		expanded, _, err := loadCCI(ctx, src, size, opts.MaxOutput)
		return expanded, err
	case "wux":
		if WUDImageBytes > opts.MaxOutput {
			return -1, errors.New("WUX disc exceeds expanded size limit")
		}
		return WUDImageBytes, ValidateWUX(ctx, src, size)
	case "pbp":
		disc, err := ReadPBP(ctx, src, size)
		if err != nil {
			return -1, err
		}
		expanded := int64(disc.Sectors) * PBPBytesPerSector
		if expanded > opts.MaxOutput {
			return -1, errors.New("PBP disc exceeds expanded size limit")
		}
		return expanded, ctx.Err()
	}
	return -1, ctx.Err()
}

func validateCSOSize(size int64) error {
	if size > defaultLimit {
		return errors.New("CSO/ZSO images larger than 1 TiB are unsupported")
	}
	return nil
}

func validateSparseSize(format string, size int64) error {
	if format == "wbfs" {
		if size != wiiSingleLayerSize && size != wiiDualLayerSize {
			return errors.New("WBFS compression requires a full single- or dual-layer Wii ISO")
		}
	} else if size != gameCubeDiscSize && size != wiiSingleLayerSize && size != wiiDualLayerSize {
		return errors.New("CISO compression requires a full GameCube or Wii ISO")
	}
	return nil
}

func validateSparseHeader(header []byte, size int64) error {
	if (size == gameCubeDiscSize && (!hasGameCubeMagic(header) || hasWiiMagic(header))) ||
		(size != gameCubeDiscSize && !hasWiiMagic(header)) {
		return errors.New("disc header does not match its ISO size")
	}
	return nil
}

func validateCCISize(size int64) error {
	if size < (xisoVolume+1)*cciSectorBytes || size > cciMaxImage || size%cciSectorBytes != 0 {
		return errors.New("CCI compression requires a sector-aligned XISO of up to 8 GiB")
	}
	return nil
}

func validateWUXSize(size int64) error {
	if size != WUDImageBytes {
		return errors.New("WUX compression requires a complete 25,025,314,816-byte WUD image")
	}
	return nil
}
