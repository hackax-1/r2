// Package config defines the runtime configuration structures and loader
// for the Free CDN Live Relay system.
//
// Architecture Overview:
// The system decouples video chunk distribution from playlist delivery:
//   1. Heavy video chunks (MPEG-TS disguised as .js) are uploaded to Cloudflare R2.
//   2. Lightweight playlists (.m3u8 manifests) are served directly from the Go origin server's RAM.
package config

import (
	"encoding/json"
	"os"
)

// S3Config holds the AWS S3 / Cloudflare R2 credentials and endpoint information.
// Compatible with any S3-compliant storage (Cloudflare R2, Backblaze B2, AWS S3, MinIO).
type S3Config struct {
	Endpoint           string `json:"endpoint"`              // S3 endpoint URL (e.g. https://<account_id>.r2.cloudflarestorage.com)
	Region             string `json:"region"`                // S3 region (typically "auto" for Cloudflare R2)
	Bucket             string `json:"bucket"`                // Target bucket name (e.g. "oreo")
	KeyID              string `json:"key_id"`                // S3 Access Key ID
	KeySecret          string `json:"key_secret"`            // S3 Secret Access Key
	PublicURL          string `json:"public_url"`            // Public CDN edge domain (e.g. https://pub-xxx.r2.dev)
	IncludeBucketInURL *bool  `json:"include_bucket_in_url,omitempty"` // Omit bucket name in public CDN path if true
}

// IPTVConfig holds upstream Xtream Codes / MPEG-TS IPTV stream provider credentials.
type IPTVConfig struct {
	Host     string `json:"host"`     // IPTV server base URL (e.g. http://iptv-provider.example:8080)
	Username string `json:"username"` // Single-line subscriber username
	Password string `json:"password"` // Subscriber password
}

// Config represents the top-level configuration tree.
type Config struct {
	S3     S3Config     `json:"s3"`
	IPTV   IPTVConfig   `json:"iptv"`
	Port   int          `json:"port"`   // Local HTTP web server port (default: 8888)
	Stream StreamConfig `json:"stream"` // Video ingestion and packaging settings
}

// StreamConfig controls HLS slicing parameters and CDN edge buffering.
type StreamConfig struct {
	DefaultChannel    string `json:"default_channel"`     // Initial channel to ingest on startup
	ChunkDuration     int    `json:"chunk_duration"`      // Target chunk duration in seconds (standard: 6)
	PlaylistSize      int    `json:"playlist_size"`       // Number of segments in the active sliding window (standard: 15)
	BufferDelayChunks int    `json:"buffer_delay_chunks"` // Safety runway on R2 before serving to viewers (10 chunks = 60s)
	WorkDir           string `json:"work_dir"`            // Local temporary scratch directory for FFmpeg
}

// LoadConfig reads config.json from disk with sensible production defaults.
// If the configuration file is missing, it falls back to hardcoded defaults.
func LoadConfig(configPath string) (*Config, error) {
	cfg := &Config{
		S3: S3Config{
			Endpoint:  "https://<account_id>.r2.cloudflarestorage.com",
			Region:    "auto",
			Bucket:    "your-bucket-name",
			KeyID:     "your-key-id",
			KeySecret: "your-key-secret",
			PublicURL: "https://pub-xxx.r2.dev",
		},
		IPTV: IPTVConfig{
			Host:     "http://your-iptv-host:8080",
			Username: "your-username",
			Password: "your-password",
		},
		Port: 8888,
		Stream: StreamConfig{
			DefaultChannel:    "389601", // Family Guy FHD (1080p) default
			ChunkDuration:     6,        // 6.0 second standard HLS chunks
			PlaylistSize:      15,       // 15 segments sliding window (90s runway)
			BufferDelayChunks: 10,       // 10 chunks * 6s = 60s warm CF edge buffer
			WorkDir:           "/tmp/free_cdn_relay",
		},
	}

	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err == nil {
			_ = json.Unmarshal(data, cfg)
		}
	}

	return cfg, nil
}
//might in future want to add more accounts to rotate from cuz yk who knows