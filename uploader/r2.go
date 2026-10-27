// Package uploader implements high-performance, native AWS Signature Version 4 (SigV4)
// uploads directly to Cloudflare R2 object storage without external AWS SDK dependencies.
//
// Key Features:
//   1. Zero external AWS SDK bloat: pure Go standard library (crypto/hmac, crypto/sha256).
//   2. HTTP Connection Pooling: keeps TCP/TLS connections hot to Cloudflare R2 to minimize PUT latency.
//   3. Exponential backoff and retries for network resilience.
//   4. Supports arbitrary HTTP headers like Cache-Control and Content-Type.
package uploader

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"free-cdn-relay/config"
)

// R2Uploader manages authenticated SigV4 HTTP uploads to Cloudflare R2.
type R2Uploader struct {
	cfg        config.S3Config
	httpClient *http.Client
}

// NewR2Uploader initializes an R2Uploader with an optimized HTTP transport pool.
func NewR2Uploader(cfg config.S3Config) *R2Uploader {
	transport := &http.Transport{
		MaxIdleConns:        100,             // Total maximum idle connections across all hosts
		MaxIdleConnsPerHost: 30,              // Keep up to 30 idle connections warm to Cloudflare R2
		IdleConnTimeout:     90 * time.Second, // Reuse connections across 6s chunk upload intervals
		DisableKeepAlives:   false,
	}
	return &R2Uploader{
		cfg: cfg,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
	}
}

// sha256Hex computes the lowercase SHA-256 hex string of a byte slice.
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// hmacSha256 computes HMAC-SHA256 of data using the provided key.
func hmacSha256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// getSigningKey derives the AWS SigV4 composite signing key:
// HMAC(HMAC(HMAC(HMAC("AWS4" + secret, dateStamp), region), service), "aws4_request")
func getSigningKey(secretKey, dateStamp, region, service string) []byte {
	kDate := hmacSha256([]byte("AWS4"+secretKey), dateStamp)
	kRegion := hmacSha256(kDate, region)
	kService := hmacSha256(kRegion, service)
	return hmacSha256(kService, "aws4_request")
}

// Upload uploads raw bytes directly to Cloudflare R2 with automatic retries and exponential backoff.
// Returns the public CDN URL to access the uploaded file.
func (u *R2Uploader) Upload(fileName string, data []byte, contentType, cacheControl string) (string, error) {
	const maxRetries = 3
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		url, err := u.doUpload(fileName, data, contentType, cacheControl)
		if err == nil {
			return url, nil
		}
		lastErr = err

		if attempt < maxRetries {
			// Linear/exponential backoff: 250ms, 500ms
			time.Sleep(time.Duration(attempt*250) * time.Millisecond)
		}
	}

	return "", fmt.Errorf("R2 upload failed after %d attempts: %w", maxRetries, lastErr)
}

// doUpload executes a single AWS SigV4 PUT request.
func (u *R2Uploader) doUpload(fileName string, data []byte, contentType, cacheControl string) (string, error) {
	if u.cfg.Endpoint == "" || u.cfg.KeyID == "" || u.cfg.KeySecret == "" || u.cfg.Bucket == "" {
		return "", fmt.Errorf("R2 configuration is incomplete")
	}

	rawEndpoint := u.cfg.Endpoint
	if !strings.HasPrefix(rawEndpoint, "http://") && !strings.HasPrefix(rawEndpoint, "https://") {
		rawEndpoint = "https://" + rawEndpoint
	}

	endpointURL, err := url.Parse(rawEndpoint)
	if err != nil {
		return "", fmt.Errorf("invalid endpoint URL: %w", err)
	}

	host := endpointURL.Host
	region := u.cfg.Region
	if region == "" {
		if strings.Contains(host, "r2.cloudflarestorage") {
			region = "auto"
		} else {
			region = "us-east-1"
		}
	}
	service := "s3"

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z") // ISO 8601 basic format: YYYYMMDD'T'HHMMSS'Z'
	dateStamp := now.Format("20060102")

	encodedFileName := url.PathEscape(fileName)
	canonicalURI := fmt.Sprintf("/%s/%s", u.cfg.Bucket, encodedFileName)
	payloadHash := sha256Hex(data)

	// Construct headers to sign
	headersToSign := map[string]string{
		"host":                 host,
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           amzDate,
	}

	if contentType != "" {
		headersToSign["content-type"] = contentType
	}
	if cacheControl != "" {
		headersToSign["cache-control"] = cacheControl
	}

	// Canonical headers must be sorted by lowercase name
	var sortedKeys []string
	for k := range headersToSign {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)

	var canonicalHeaders strings.Builder
	for _, k := range sortedKeys {
		canonicalHeaders.WriteString(k)
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(strings.TrimSpace(headersToSign[k]))
		canonicalHeaders.WriteString("\n")
	}

	signedHeaders := strings.Join(sortedKeys, ";")

	// Task 1: Create Canonical Request
	canonicalRequest := strings.Join([]string{
		"PUT",
		canonicalURI,
		"", // query string (empty)
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	// Task 2: Create String to Sign
	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStamp, region, service)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	// Task 3: Calculate the Signature
	signingKey := getSigningKey(u.cfg.KeySecret, dateStamp, region, service)
	signature := hex.EncodeToString(hmacSha256(signingKey, stringToSign))

	// Task 4: Add Signing Information to Authorization Header
	authHeader := fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		u.cfg.KeyID, credentialScope, signedHeaders, signature,
	)

	// Send HTTP PUT to Cloudflare R2 endpoint
	targetURL := fmt.Sprintf("%s://%s%s", endpointURL.Scheme, host, canonicalURI)
	req, err := http.NewRequest("PUT", targetURL, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("failed to create PUT request: %w", err)
	}

	for k, v := range headersToSign {
		if k == "host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	req.Header.Set("Authorization", authHeader)
	req.ContentLength = int64(len(data))

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("R2 upload request error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("R2 upload status %d: %s", resp.StatusCode, string(body))
	}

	// Compute public viewer URL
	if u.cfg.PublicURL != "" {
		baseURL := strings.TrimRight(u.cfg.PublicURL, "/")
		if strings.Contains(baseURL, "r2.dev") || strings.HasSuffix(baseURL, u.cfg.Bucket) || (u.cfg.IncludeBucketInURL != nil && !*u.cfg.IncludeBucketInURL) {
			return fmt.Sprintf("%s/%s", baseURL, encodedFileName), nil
		}
		return fmt.Sprintf("%s/%s/%s", baseURL, u.cfg.Bucket, encodedFileName), nil
	}

	return targetURL, nil
}
