package testfixture

import "encoding/binary"

// RVZHeader declares a logical disc size for tests that substitute a fake engine.
func RVZHeader(size uint64) []byte {
	header := make([]byte, 0x48)
	copy(header, "RVZ\x01")
	binary.BigEndian.PutUint64(header[36:], size)
	binary.BigEndian.PutUint64(header[44:], uint64(len(header)))
	return header
}

// CHDMetadata constructs a CHD header and metadata chain without compressed data.
// It is suitable for metadata parsing tests, never engine interoperability tests.
func CHDMetadata(version uint32, tags ...string) []byte {
	headerSize, logicalAt, metaAt, hunkAt := 124, 32, 48, 56
	if version == 3 || version == 4 {
		headerSize, logicalAt, metaAt, hunkAt = 120, 28, 36, 76
		if version == 4 {
			headerSize, hunkAt = 108, 44
		}
	}
	data := make([]byte, headerSize+17*len(tags))
	copy(data, "MComprHD")
	binary.BigEndian.PutUint32(data[8:12], uint32(headerSize))
	binary.BigEndian.PutUint32(data[12:16], version)
	binary.BigEndian.PutUint64(data[logicalAt:], 2048)
	binary.BigEndian.PutUint32(data[hunkAt:], 2048)
	if version == 5 {
		binary.BigEndian.PutUint32(data[60:64], 2048)
		data[64] = 1 // Declare a raw checksum without supplying a compressed payload.
	} else {
		binary.BigEndian.PutUint32(data[24:28], 1)
	}
	if len(tags) != 0 {
		binary.BigEndian.PutUint64(data[metaAt:], uint64(headerSize))
	}
	for i, tag := range tags {
		offset := headerSize + 17*i
		entry := data[offset:]
		copy(entry[:4], tag)
		entry[4], entry[7] = 1, 1
		if i+1 < len(tags) {
			binary.BigEndian.PutUint64(entry[8:16], uint64(offset+17))
		}
	}
	return data
}

// CDTrackHeader builds a v5 header with one four-frame span for metadata tests.
func CDTrackHeader(metadata string) []byte {
	data := append(CHDMetadata(5, "CHT2")[:140], metadata...)
	data = append(data, 0)
	binary.BigEndian.PutUint64(data[32:], 4*2448)
	binary.BigEndian.PutUint32(data[56:], 4*2448)
	binary.BigEndian.PutUint32(data[60:], 2448)
	binary.BigEndian.PutUint32(data[128:], 1<<24|uint32(len(metadata)+1))
	return data
}

// CHDTrack extends only generated test data past chdman's tiny-map edge cases.
func CHDTrack(data []byte, sectorBytes int) []byte {
	const frames = 128
	if len(data) >= frames*sectorBytes {
		return data
	}
	extended := make([]byte, frames*sectorBytes)
	copy(extended, data)
	for i := len(data); i < len(extended); i++ {
		extended[i] = byte(i*17 + i/251)
	}
	return extended
}
