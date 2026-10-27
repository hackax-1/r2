package uploader

import (
	"strings"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"free-cdn-relay/config"
)

func TestR2UploadAndFetch(t *testing.T) {
	cfg, err := config.LoadConfig("../config.json")
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if strings.Contains(cfg.S3.Endpoint, "<account_id>") || cfg.S3.Bucket == "your-bucket-name" {
		t.Skip("Skipping live R2 test: placeholder credentials")
	}

	u := NewR2Uploader(cfg.S3)
	testFileName := fmt.Sprintf("test_go_sigv4_%d.txt", time.Now().Unix())
	testData := []byte("hello from go sigv4 uploader test")

	publicURL, err := u.Upload(testFileName, testData, "text/plain", "public, max-age=60")
	if err != nil {
		t.Fatalf("Upload failed: %v", err)
	}

	t.Logf("Uploaded to: %s", publicURL)

	// Verify via public HTTP GET
	resp, err := http.Get(publicURL)
	if err != nil {
		t.Fatalf("Failed to fetch uploaded file: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", resp.StatusCode)
	}

	fetchedData, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read body: %v", err)
	}

	if string(fetchedData) != string(testData) {
		t.Fatalf("Fetched data mismatch: %s vs %s", string(fetchedData), string(testData))
	}

	cacheHeader := resp.Header.Get("Cache-Control")
	t.Logf("Cache-Control on R2: %s", cacheHeader)
}
