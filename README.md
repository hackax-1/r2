# Free CDN Stream Relay (R2 Live Ingest Engine)

A high-performance live video streaming ingest engine written in pure Go. It ingests upstream live MPEG-TS / IPTV feeds, slices them into GOP keyframe-aligned segments using FFmpeg (`-c copy`, ~0% CPU), offloads heavy video chunks to S3-compatible object storage (such as Cloudflare R2 with $0 egress fees) disguised as static `.js` assets, and serves an in-memory sliding-window playlist (`.m3u8`) directly from RAM.

---

## Key Features

- **Zero Bandwidth Egress**: Video chunks are stored on Cloudflare R2 ($0 egress) or S3-compatible storage. Viewers fetch chunks directly from CDN edge caches.
- **Pure Native Go**: Zero Node.js runtime, zero npm dependencies, zero service worker overhead in browsers.
- **PTS Discontinuity Tracking**: Seamlessly handles upstream IPTV stream restarts and timestamp rollovers (`#EXT-X-DISCONTINUITY`) to prevent player freezes.
- **Micro-Cached Manifests**: Serves `live.m3u8` directly from RAM with `Cache-Control: public, max-age=3, stale-while-revalidate=2` to protect origin servers during high-concurrency spikes.
- **Utilitarian Pure HTML Admin Dashboard**: Fast, lightweight management console accessible at `/admin` (zero CSS overhead).
- **Integrated Video Viewer**: Minimalist viewer player available at `/watch`.

---

## Architecture

```
 Upstream IPTV (MPEG-TS)
         │
         ▼
 ┌─────────────────────────────────────────────────────────┐
 │               Free CDN Relay (Pure Go)                  │
 │                                                         │
 │  [FFmpeg Slicer] ──> [Discontinuity Tracker]            │
 │         │                                               │
 │         ▼                                               │
 │  [Pure Go AWS SigV4 Uploader]                           │
 └─────────┬───────────────────────────────────────────────┘
           │
           ├──────────────────────────────┐
           ▼                              ▼
 ┌───────────────────┐          ┌───────────────────┐
 │   Cloudflare R2   │          │    Origin RAM     │
 │  ($0 Egress CDN)  │          │   (Micro-Cache)   │
 │                   │          │                   │
 │   chunk_001.js    │          │     live.m3u8     │
 │   chunk_002.js    │          │                   │
 └─────────┬─────────┘          └─────────┬─────────┘
           │                              │
           └──────────────┬───────────────┘
                          ▼
                  [HLS Video Player]
              (Hls.js / Clappr / iOS Safari)
```

---

## Directory Structure

```
├── config/              # Configuration structs & JSON loader
├── deploy/              # Systemd unit & Caddy reverse proxy examples
├── encoder/             # PNG steganography chunk encoder
├── relay/               # Core FFmpeg segmenter, stream watcher, manifest builder
├── server/              # HTTP API, embedded web UI, and viewer player
├── uploader/            # Pure Go AWS SigV4 S3/R2 chunk uploader
├── config.json.example  # Reference configuration template
├── Dockerfile           # Multi-stage Alpine container build
├── go.mod               # Go module definition
├── main.go              # Application CLI entrypoint
└── README.md
```

---

## Quickstart

### 1. Requirements
- Go 1.22+
- `ffmpeg` installed and in your system PATH

### 2. Configure
Copy the example configuration:
```bash
cp config.json.example config.json
```
Edit `config.json` with your Cloudflare R2 credentials and IPTV source:
```json
{
  "s3": {
    "endpoint": "https://<account_id>.r2.cloudflarestorage.com",
    "region": "auto",
    "bucket": "your-bucket-name",
    "key_id": "your-key-id",
    "key_secret": "your-key-secret",
    "public_url": "https://pub-xxx.r2.dev"
  },
  "iptv": {
    "host": "http://your-iptv-host:8080",
    "username": "your-username",
    "password": "your-password"
  },
  "port": 8888,
  "stream": {
    "default_channel": "389601",
    "chunk_duration": 6,
    "playlist_size": 15,
    "buffer_delay_chunks": 10,
    "work_dir": "/tmp/free_cdn_relay"
  }
}
```

### 3. Build & Run
Run all unit tests:
```bash
go test ./...
```

Compile the single binary:
```bash
go build -o cdn-relay .
```

Run the relay server:
```bash
./cdn-relay -port=8888 -channel=389601 -autostart=true
```

Access the endpoints:
- **Admin Dashboard**: `http://localhost:8888/admin`
- **Viewer Player**: `http://localhost:8888/watch`
- **Live HLS Stream**: `http://localhost:8888/live.m3u8`

---

## Docker Deployment

Build the container:
```bash
docker build -t cdn-relay .
```

Run with configuration mounted:
```bash
docker run -d \
  --name cdn-relay \
  -p 8888:8888 \
  -v $(pwd)/config.json:/app/config.json:ro \
  cdn-relay
```

---

## License
MIT
