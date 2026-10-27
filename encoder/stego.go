
// I DONT USE IT NO MORE BUT IF YOU WANT TIKTOK CDN STEGO STYLE SEGMENTS THEN YOU CAN USE THIS


package encoder

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
)

var (
	pngSignature = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	magicHeader  = []byte("TIKTIKPX")
)

const (
	pngWidth = 512
	rowBytes = pngWidth * 3 // 1536 bytes per RGB scanline
)

// buildChunk wraps chunk type and payload with 4-byte big-endian length and CRC32
func buildChunk(chunkType string, data []byte) []byte {
	length := uint32(len(data))
	typeBytes := []byte(chunkType)

	buf := make([]byte, 4+4+len(data)+4)
	binary.BigEndian.PutUint32(buf[0:4], length)
	copy(buf[4:8], typeBytes)
	copy(buf[8:8+len(data)], data)

	crc := crc32.ChecksumIEEE(buf[4 : 8+len(data)])
	binary.BigEndian.PutUint32(buf[8+len(data):], crc)

	return buf
}

// EmbedTsToPng converts MPEG-TS data into a valid PNG image carrying the TIKTIKPX payload
func EmbedTsToPng(tsData []byte) ([]byte, error) {
	// 1. Gzip compress TS data (BestSpeed gives high compression with lowest CPU latency)
	var gzBuf bytes.Buffer
	gzWriter, err := gzip.NewWriterLevel(&gzBuf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := gzWriter.Write(tsData); err != nil {
		return nil, err
	}
	if err := gzWriter.Close(); err != nil {
		return nil, err
	}
	gzData := gzBuf.Bytes()

	// 2. Assemble payload: MAGIC (8B) + uint32 length (4B) + gzData
	payloadLen := 8 + 4 + len(gzData)
	payload := make([]byte, payloadLen)
	copy(payload[0:8], magicHeader)
	binary.BigEndian.PutUint32(payload[8:12], uint32(len(gzData)))
	copy(payload[12:], gzData)

	// 3. Calculate image dimensions (512px RGB)
	height := (payloadLen + rowBytes - 1) / rowBytes
	totalPadded := height * rowBytes

	paddedPayload := make([]byte, totalPadded)
	copy(paddedPayload, payload)

	// 4. Build PNG scanlines with filter byte 0x00 (None)
	rawScanlines := make([]byte, height*(1+rowBytes))
	for y := 0; y < height; y++ {
		offset := y * (1 + rowBytes)
		rawScanlines[offset] = 0 // Filter 0: None
		copy(rawScanlines[offset+1:offset+1+rowBytes], paddedPayload[y*rowBytes:(y+1)*rowBytes])
	}

	// 5. Deflate scanlines into IDAT
	var idatBuf bytes.Buffer
	zWriter, err := zlib.NewWriterLevel(&idatBuf, zlib.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := zWriter.Write(rawScanlines); err != nil {
		return nil, err
	}
	if err := zWriter.Close(); err != nil {
		return nil, err
	}

	// 6. IHDR Chunk (13 bytes)
	ihdrData := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdrData[0:4], uint32(pngWidth))
	binary.BigEndian.PutUint32(ihdrData[4:8], uint32(height))
	ihdrData[8] = 8 // 8-bit depth
	ihdrData[9] = 2 // Color type 2 (RGB)
	ihdrData[10] = 0 // Compression method: deflate
	ihdrData[11] = 0 // Filter method: standard
	ihdrData[12] = 0 // Interlace method: none

	// 7. Assemble complete PNG
	ihdrChunk := buildChunk("IHDR", ihdrData)
	idatChunk := buildChunk("IDAT", idatBuf.Bytes())
	iendChunk := buildChunk("IEND", nil)

	totalSize := len(pngSignature) + len(ihdrChunk) + len(idatChunk) + len(iendChunk)
	pngOut := make([]byte, 0, totalSize)
	pngOut = append(pngOut, pngSignature...)
	pngOut = append(pngOut, ihdrChunk...)
	pngOut = append(pngOut, idatChunk...)
	pngOut = append(pngOut, iendChunk...)

	return pngOut, nil
}

// ExtractTsFromPng extracts the original MPEG-TS data from a TIKTIKPX PNG (used for verification)
func ExtractTsFromPng(pngData []byte) ([]byte, error) {
	if len(pngData) < 8 || !bytes.Equal(pngData[:8], pngSignature) {
		return nil, errors.New("invalid PNG signature")
	}

	offset := 8
	var idatBytes []byte
	var width, height uint32
	var bitDepth, colorType byte

	for offset+8 <= len(pngData) {
		chunkLen := binary.BigEndian.Uint32(pngData[offset : offset+4])
		chunkType := string(pngData[offset+4 : offset+8])
		dataStart := offset + 8
		dataEnd := dataStart + int(chunkLen)
		if dataEnd+4 > len(pngData) {
			break
		}

		if chunkType == "IHDR" && chunkLen >= 13 {
			width = binary.BigEndian.Uint32(pngData[dataStart : dataStart+4])
			height = binary.BigEndian.Uint32(pngData[dataStart+4 : dataStart+8])
			bitDepth = pngData[dataStart+8]
			colorType = pngData[dataStart+9]
		} else if chunkType == "IDAT" {
			idatBytes = append(idatBytes, pngData[dataStart:dataEnd]...)
		} else if chunkType == "IEND" {
			break
		}

		offset = dataEnd + 4
	}

	if width != pngWidth || bitDepth != 8 || colorType != 2 {
		return nil, errors.New("unsupported PNG structure")
	}

	// Decompress IDAT
	zReader, err := zlib.NewReader(bytes.NewReader(idatBytes))
	if err != nil {
		return nil, err
	}
	defer zReader.Close()

	rawScanlines, err := io.ReadAll(zReader)
	if err != nil {
		return nil, err
	}

	expectedScanlineLen := int(height) * (1 + rowBytes)
	if len(rawScanlines) < expectedScanlineLen {
		return nil, errors.New("incomplete scanline data")
	}

	// Reconstruct RGB payload
	payload := make([]byte, int(height)*rowBytes)
	for y := 0; y < int(height); y++ {
		srcOff := y*(1+rowBytes) + 1
		copy(payload[y*rowBytes:(y+1)*rowBytes], rawScanlines[srcOff:srcOff+rowBytes])
	}

	// Verify TIKTIKPX header
	if len(payload) < 12 || !bytes.Equal(payload[:8], magicHeader) {
		return nil, errors.New("TIKTIKPX header not found")
	}

	gzLen := binary.BigEndian.Uint32(payload[8:12])
	if int(12+gzLen) > len(payload) {
		return nil, errors.New("payload length mismatch")
	}

	gzReader, err := gzip.NewReader(bytes.NewReader(payload[12 : 12+gzLen]))
	if err != nil {
		return nil, err
	}
	defer gzReader.Close()

	return io.ReadAll(gzReader)
}