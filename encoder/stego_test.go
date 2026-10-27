package encoder

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestStegoRoundtrip(t *testing.T) {
	// Generate mock MPEG-TS packet stream (188 bytes per packet, sync byte 0x47)
	const numPackets = 100
	mockTs := make([]byte, numPackets*188)
	for i := 0; i < numPackets; i++ {
		mockTs[i*188] = 0x47
		_, _ = rand.Read(mockTs[i*188+1 : (i+1)*188])
	}

	pngBytes, err := EmbedTsToPng(mockTs)
	if err != nil {
		t.Fatalf("EmbedTsToPng failed: %v", err)
	}

	if len(pngBytes) == 0 {
		t.Fatal("Output PNG is empty")
	}

	// Verify PNG signature
	if !bytes.Equal(pngBytes[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}) {
		t.Fatal("Invalid PNG header")
	}

	extracted, err := ExtractTsFromPng(pngBytes)
	if err != nil {
		t.Fatalf("ExtractTsFromPng failed: %v", err)
	}

	if !bytes.Equal(mockTs, extracted) {
		t.Fatalf("Extracted TS data does not match original (len %d vs %d)", len(extracted), len(mockTs))
	}
}
