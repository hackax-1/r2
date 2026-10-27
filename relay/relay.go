// Package relay contains the core streaming engine for the Free CDN Live Relay.
//
// Operational Principles:
//
// 1. Single-Stream IPTV Account Enforcement:  [THIS IS BETA BUILD IN UPCOMING DEVELOPMENT I PLAN TO INTRODUCE MULTI THREAD STREAMING BUT WANT TO KEEP IT PRETTY SIMPLE]
//    The IPTV provider permits exactly 1 active stream connection per account.
//    RelayEngine acts as a singleton supervisor: starting any channel stops any
//    previous channel before establishing the new upstream connection.
//
// 2. The "Roxiestreams" Disguise Trick: [Dosent really metter but still just added here]
//    Video chunks are standard raw MPEG-TS data starting with the standard 0x47 sync byte.
//    They are named with a .js extension (e.g. chunk_ch389601_1792996328_000001.js) and
//    uploaded to Cloudflare R2 with Content-Type: text/javascript and 24h immutable edge cache.
//    Hls.js, Clappr, and VLC natively probe the binary sync bytes and demux the video directly
//    with ZERO transcoding CPU overhead and ZERO Service Worker decoding latency.
//
// 3. Monotonic Presentation Timestamps (PTS) & Keyframe Alignment: [NO TIME STAMP JUMP AND PRETTY BASIC LOGIC]
//    To prevent playback stutters, FFmpeg cuts segments at GOP keyframe boundaries (I-frames)
//    using `-bsf:v dump_extra` and `-hls_flags delete_segments+temp_file+independent_segments`.
//    Whenever an upstream reconnection occurs, `#EXT-X-DISCONTINUITY` is automatically tagged.
//
// 4. Decoupled Playlist & 5-Second In-Memory Micro-Cache: [IN MEMORY MICRO RN BUT SOON I WANT TO USE REDIS BUT YEAH WORKS KIND OF SAME YEAH]
//    The playlist (.m3u8) is NOT pushed to Cloudflare R2. Instead, it is pre-rendered in RAM
//    as a byte slice and served directly from the Go origin HTTP server with 5s micro-caching.
//    Only the video chunks fly to Cloudflare R2 ($0 egress).
package relay

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"free-cdn-relay/config"
	"free-cdn-relay/uploader"
)

// AGENT DID PRETTY GOOD COMMENTING IN THIS FILE

// Segment represents an individual 6.0s video chunk available on Cloudflare R2.
type Segment struct {
	URL           string  `json:"url"`           // Public Cloudflare R2 URL
	Duration      float64 `json:"duration"`      // Segment duration in seconds (from #EXTINF)
	Filename      string  `json:"filename"`      // Remote filename (e.g. chunk_ch389601_*.js)
	Discontinuity bool    `json:"discontinuity"` // Set true when PTS resets or upstream rotates [Presentation Time Stamp]
}

// StreamStatus is returned by the /api/status endpoint to drive web dashboards.
type StreamStatus struct {
	IsRunning         bool      `json:"is_running"`
	ActiveChannel     string    `json:"active_channel"`
	StreamID          string    `json:"stream_id"`
	UptimeSeconds     int64     `json:"uptime_seconds"`
	TotalChunks       int64     `json:"total_chunks"`
	SequenceNumber    int64     `json:"sequence_number"`
	LastChunkTime     time.Time `json:"last_chunk_time"`
	LastChunkUpload   float64   `json:"last_chunk_upload_ms"`
	LastError         string    `json:"last_error"`
	Logs              []string  `json:"logs"`
	BufferDelayChunks int       `json:"buffer_delay_chunks"` // e.g. 10 chunks = 60s
	BufferedChunks    int       `json:"buffered_chunks"`
	IsBufferReady     bool      `json:"is_buffer_ready"`
}

// RelayEngine manages the FFmpeg ingest lifecycle, chunk uploading, and live playlist generation.
type RelayEngine struct {
	cfg        *config.Config
	uploader   *uploader.R2Uploader

	mu     sync.RWMutex
	logMu  sync.Mutex
	m3u8Mu sync.Mutex

	cmd            *exec.Cmd
	cancelFn       context.CancelFunc
	desiredRunning bool
	isRunning      bool
	channelID      string
	userAgent      string
	referer        string
	customHeaders  string
	streamID       string
	startTime      time.Time
	seqNumber      int64
	totalChunks    int64
	currentEpoch   uint64 // Incremented on every channel change to invalidate in-flight uploads

	lastChunkTime     time.Time
	lastChunkUploadMs float64
	lastError         string
	logs              []string

	uploadedPool        []Segment       // Circular buffer of chunks uploaded to R2 (~100 chunks)
	baseIndex           int64           // Global sequence offset as pool slides
	activeSegments      []Segment       // The active sliding window served to players (15 segments)
	processedChunks     map[string]bool // Deduplication map for FFmpeg TS chunks
	currentPlaylist     string          // Rendered M3U8 string
	cachedPlaylistBytes []byte          // Pre-rendered byte slice served by origin HTTP server in 0 allocations
}

// NewRelayEngine constructs a new engine instance.
func NewRelayEngine(cfg *config.Config) *RelayEngine {
	return &RelayEngine{
		cfg:                 cfg,
		uploader:            uploader.NewR2Uploader(cfg.S3),
		processedChunks:     make(map[string]bool),
		uploadedPool:        make([]Segment, 0, 100),
		logs:                make([]string, 0, 50),
		streamID:            "live",
		cachedPlaylistBytes: []byte("#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:0\n"),
	}
}

// appendLog stores recent log messages in memory for terminal dashboard rendering.
func (r *RelayEngine) appendLog(format string, v ...interface{}) {
	raw := fmt.Sprintf(format, v...)
	if r.cfg.IPTV.Username != "" {
		raw = strings.ReplaceAll(raw, r.cfg.IPTV.Username, "******")
	}
	if r.cfg.IPTV.Password != "" {
		raw = strings.ReplaceAll(raw, r.cfg.IPTV.Password, "******")
	}
	msg := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), raw)
	log.Println(msg)

	r.logMu.Lock()
	defer r.logMu.Unlock()
	if len(r.logs) >= 50 {
		r.logs = r.logs[1:]
	}
	r.logs = append(r.logs, msg)
}

// StartChannel switches the active stream to the requested channel ID with standard defaults.
func (r *RelayEngine) StartChannel(channelID string) error {
	return r.StartChannelAdvanced(channelID, "", "", "")
}

// StartChannelAdvanced switches the active stream with custom User-Agent, Referer, and headers.
func (r *RelayEngine) StartChannelAdvanced(channelID, userAgent, referer, customHeaders string) error {
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		channelID = r.cfg.Stream.DefaultChannel
	}
	userAgent = strings.TrimSpace(userAgent)
	referer = strings.TrimSpace(referer)
	customHeaders = strings.TrimSpace(customHeaders)

	// Guard against duplicate start calls on already active channels with identical params
	r.mu.RLock()
	if r.isRunning && r.channelID == channelID && r.userAgent == userAgent && r.referer == referer && r.customHeaders == customHeaders {
		r.mu.RUnlock()
		r.appendLog("Channel %s is already active; preserving warm buffer", channelID)
		return nil
	}
	r.mu.RUnlock()

	// Stop previous ingest cleanly
	r.Stop()
	time.Sleep(500 * time.Millisecond)

	safeID := regexp.MustCompile(`[^a-zA-Z0-9_-]`).ReplaceAllString(channelID, "_")
	if len(safeID) > 20 {
		safeID = safeID[len(safeID)-20:]
	}

	r.mu.Lock()
	r.currentEpoch++
	epoch := r.currentEpoch
	r.channelID = channelID
	r.userAgent = userAgent
	r.referer = referer
	r.customHeaders = customHeaders
	// Unique stream namespace: embeds channel ID and timestamp to permanently prevent browser cache collisions
	r.streamID = fmt.Sprintf("ch%s_%d", safeID, time.Now().Unix())
	r.startTime = time.Now()
	r.processedChunks = make(map[string]bool)
	r.uploadedPool = make([]Segment, 0, 100)
	r.baseIndex = 0
	r.activeSegments = make([]Segment, 0, r.cfg.Stream.PlaylistSize)
	r.currentPlaylist = ""
	r.cachedPlaylistBytes = []byte("#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:0\n")
	r.seqNumber = 0
	r.lastError = ""
	r.desiredRunning = true

	workDir := filepath.Join(r.cfg.Stream.WorkDir, fmt.Sprintf("stream_%s", safeID))
	_ = os.RemoveAll(workDir)
	if err := os.MkdirAll(workDir, 0755); err != nil {
		r.mu.Unlock()
		return fmt.Errorf("failed to create work dir: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	r.cancelFn = cancel
	r.mu.Unlock()

	r.appendLog("Starting supervisor ingest for channel %s (6s chunks, 60s CF edge buffer, epoch #%d)", channelID, epoch)

	go r.supervisorLoop(ctx, channelID, workDir, epoch)

	return nil
}

// supervisorLoop monitors the FFmpeg child process, automatically reconnecting if IPTV drops.
func (r *RelayEngine) supervisorLoop(ctx context.Context, channelID, workDir string, epoch uint64) {
	var iptvURL string
	if strings.HasPrefix(channelID, "http://") || strings.HasPrefix(channelID, "https://") {
		iptvURL = channelID
	} else {
		iptvURL = fmt.Sprintf("%s/%s/%s/%s",
			strings.TrimRight(r.cfg.IPTV.Host, "/"),
			r.cfg.IPTV.Username,
			r.cfg.IPTV.Password,
			channelID,
		)
	}

	chunkDuration := r.cfg.Stream.ChunkDuration
	if chunkDuration <= 0 {
		chunkDuration = 6
	}

	reconnectCount := 0

	for {
		r.mu.RLock()
		keepGoing := r.desiredRunning && r.currentEpoch == epoch
		ua := r.userAgent
		ref := r.referer
		customH := r.customHeaders
		r.mu.RUnlock()

		if !keepGoing || ctx.Err() != nil {
			return
		}

		sessionID := time.Now().Unix()
		segmentPattern := filepath.Join(workDir, fmt.Sprintf("part_%d_%%05d.ts", sessionID))
		m3u8Output := filepath.Join(workDir, fmt.Sprintf("stream_%d.m3u8", sessionID))

		// FFmpeg Packaging Flags:
		//   -c copy: Zero CPU transcoding overhead. Copies input H.264/AAC packets as-is.
		//   -bsf:v dump_extra: Prepends SPS/PPS headers to every keyframe for decoder stability.
		//   -hls_flags independent_segments: Ensures every segment starts on an IDR keyframe.
		//   -hls_flags temp_file: Writes part_*.ts.tmp first; renames to .ts only when completely finalized.
		args := []string{
			"-y",
		}

		// If local HTTP proxy (Privoxy / WARP) is active on port 8118, route through it to bypass Cloudflare WAF
		if conn, err := net.DialTimeout("tcp", "127.0.0.1:8118", 200*time.Millisecond); err == nil {
			conn.Close()
			args = append(args, "-http_proxy", "http://127.0.0.1:8118")
		}

		if ua == "" {
			ua = "VLC/3.0.18"
		}
		args = append(args, "-user_agent", ua)

		if ref != "" {
			args = append(args, "-referer", ref)
		}

		if customH != "" {
			if !strings.HasSuffix(customH, "\r\n") {
				customH += "\r\n"
			}
			args = append(args, "-headers", customH)
		}

		args = append(args,
			"-reconnect", "1",
			"-reconnect_at_eof", "1",
			"-reconnect_streamed", "1",
			"-reconnect_delay_max", "5",
			"-err_detect", "ignore_err",
			"-i", iptvURL,
			"-c", "copy",
			"-bsf:v", "dump_extra",
			"-f", "hls",
			"-hls_time", strconv.Itoa(chunkDuration),
			"-hls_list_size", strconv.Itoa(r.cfg.Stream.PlaylistSize+2),
			"-hls_flags", "delete_segments+temp_file+independent_segments",
			"-hls_segment_filename", segmentPattern,
			m3u8Output,
		)

		cmdCtx, cmdCancel := context.WithCancel(ctx)
		cmd := exec.CommandContext(cmdCtx, "ffmpeg", args...)

		r.mu.Lock()
		if r.currentEpoch != epoch {
			r.mu.Unlock()
			cmdCancel()
			return
		}
		r.cmd = cmd
		r.isRunning = true
		r.mu.Unlock()

		stderr, err := cmd.StderrPipe()
		if err != nil {
			cmdCancel()
			r.appendLog("Failed to create stderr pipe: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}

		if err := cmd.Start(); err != nil {
			cmdCancel()
			r.appendLog("Failed to spawn FFmpeg: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}

		// Pipe FFmpeg progress logs into dashboard terminal
		go func() {
			scanner := bufio.NewScanner(stderr)
			for scanner.Scan() {
				line := scanner.Text()
				if strings.Contains(line, "Opening") || strings.Contains(line, "speed=") || strings.Contains(line, "Error") || strings.Contains(line, "discontinuity") {
					r.appendLog("[FFmpeg] %s", strings.TrimSpace(line))
				}
			}
		}()

		// Mark first chunk as discontinuity if this is an automatic reconnect within the same channel
		markDiscontinuity := reconnectCount > 0
		pollCtx, pollCancel := context.WithCancel(cmdCtx)
		go r.chunkMonitorLoop(pollCtx, workDir, m3u8Output, epoch, markDiscontinuity)

		// Wait for FFmpeg process exit
		waitErr := cmd.Wait()
		pollCancel()
		cmdCancel()

		reconnectCount++

		r.mu.Lock()
		r.isRunning = false
		if waitErr != nil && ctx.Err() == nil {
			r.lastError = waitErr.Error()
		}
		stillDesired := r.desiredRunning && r.currentEpoch == epoch
		r.mu.Unlock()

		if !stillDesired || ctx.Err() != nil {
			r.appendLog("FFmpeg ingest stopped cleanly")
			return
		}

		r.appendLog("IPTV connection dropped or rotated (%v). Reconnecting in 1s...", waitErr)
		time.Sleep(1 * time.Second)
	}
}

// Stop cleanly terminates the active stream and kills FFmpeg.
func (r *RelayEngine) Stop() {
	r.mu.Lock()
	r.desiredRunning = false
	r.isRunning = false
	if r.cancelFn != nil {
		r.cancelFn()
		r.cancelFn = nil
	}
	if r.cmd != nil && r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
		r.cmd = nil
	}
	r.mu.Unlock()
}

var extInfRegex = regexp.MustCompile(`#EXTINF:([0-9.]+),`)

// chunkMonitorLoop polls the local FFmpeg m3u8 playlist every 400ms for new closed segments.
func (r *RelayEngine) chunkMonitorLoop(ctx context.Context, workDir, m3u8File string, epoch uint64, markFirstDiscontinuity bool) {
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()

	firstChunk := markFirstDiscontinuity

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.checkNewSegments(workDir, m3u8File, epoch, &firstChunk)
		}
	}
}

// checkNewSegments parses the local manifest, verifies finalized chunks, and triggers uploads.
func (r *RelayEngine) checkNewSegments(workDir, m3u8File string, epoch uint64, firstChunk *bool) {
	data, err := os.ReadFile(m3u8File)
	if err != nil {
		return
	}

	lines := strings.Split(string(data), "\n")
	var currentDuration float64 = 6.0
	var pendingDiscontinuity bool

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "#EXT-X-DISCONTINUITY" {
			pendingDiscontinuity = true
		} else if strings.HasPrefix(line, "#EXTINF:") {
			matches := extInfRegex.FindStringSubmatch(line)
			if len(matches) > 1 {
				if d, err := strconv.ParseFloat(matches[1], 64); err == nil {
					currentDuration = d
				}
			}
		} else if strings.HasSuffix(line, ".ts") {
			tsFilename := line
			r.mu.RLock()
			alreadyProcessed := r.processedChunks[tsFilename]
			curEpoch := r.currentEpoch
			r.mu.RUnlock()

			if curEpoch != epoch {
				return
			}

			if !alreadyProcessed {
				isDiscont := pendingDiscontinuity
				if firstChunk != nil && *firstChunk {
					isDiscont = true
					*firstChunk = false
				}
				r.processSingleChunk(workDir, tsFilename, currentDuration, isDiscont, epoch)
			}
			pendingDiscontinuity = false
		}
	}
}

// processSingleChunk disguises the TS segment as a .js asset and uploads it to Cloudflare R2.
func (r *RelayEngine) processSingleChunk(workDir, tsFilename string, duration float64, discontinuity bool, epoch uint64) {
	// Guard against older epoch uploads arriving after a channel switch
	r.mu.RLock()
	if epoch != r.currentEpoch {
		r.mu.RUnlock()
		return
	}
	r.mu.RUnlock()

	tsPath := filepath.Join(workDir, tsFilename)
	tsData, err := os.ReadFile(tsPath)
	if err != nil {
		return
	}

	// Valid MPEG-TS chunks contain at least one 188-byte packet
	if len(tsData) < 188 {
		return
	}

	r.mu.Lock()
	if epoch != r.currentEpoch {
		r.mu.Unlock()
		return
	}
	r.processedChunks[tsFilename] = true
	r.totalChunks++
	chunkIndex := r.totalChunks
	streamID := r.streamID
	r.mu.Unlock()

	startTime := time.Now()

	// Roxiestreams method: raw MPEG-TS chunk disguised as static .js asset with unique namespace
	remoteFilename := fmt.Sprintf("chunk_%s_%06d.js", streamID, chunkIndex)

	// Upload to Cloudflare R2 as text/javascript with 24h immutable edge cache ($0 egress)
	remoteURL, err := r.uploader.Upload(
		remoteFilename,
		tsData,
		"text/javascript",
		"public, max-age=86400, immutable",
	)
	if err != nil {
		r.appendLog("R2 push failed for %s: %v", remoteFilename, err)
		return
	}

	elapsed := float64(time.Since(startTime).Microseconds()) / 1000.0

	r.mu.Lock()
	if epoch != r.currentEpoch {
		r.mu.Unlock()
		_ = os.Remove(tsPath)
		return
	}

	r.lastChunkTime = time.Now()
	r.lastChunkUploadMs = elapsed

	// Add chunk to in-memory sliding pool
	r.uploadedPool = append(r.uploadedPool, Segment{
		URL:           remoteURL,
		Duration:      duration,
		Filename:      remoteFilename,
		Discontinuity: discontinuity,
	})

	// Bound pool memory to ~100 chunks
	if len(r.uploadedPool) > 100 {
		r.baseIndex++
		r.uploadedPool = r.uploadedPool[1:]
	}

	delayChunks := r.cfg.Stream.BufferDelayChunks
	if delayChunks < 0 {
		delayChunks = 0 // 0 = zero delay, publish chunks immediately to live manifest
	}

	playlistSize := r.cfg.Stream.PlaylistSize
	if playlistSize <= 0 {
		playlistSize = 10 // 10 chunks = 40s player sliding window
	}

	totalUploaded := int(r.baseIndex) + len(r.uploadedPool)
	var playlistContent string
	var readyToPublish bool

	// Publish immediately or after minimal delay runway
	if totalUploaded > delayChunks {
		globalEnd := totalUploaded - delayChunks
		if globalEnd > 0 {
			globalStart := globalEnd - playlistSize
			if globalStart < 0 {
				globalStart = 0
			}

			localStart := int(globalStart - int(r.baseIndex))
			localEnd := int(globalEnd - int(r.baseIndex))

			if localStart < 0 {
				localStart = 0
			}
			if localEnd > len(r.uploadedPool) {
				localEnd = len(r.uploadedPool)
			}

			if localStart < localEnd {
				r.activeSegments = r.uploadedPool[localStart:localEnd]
				r.seqNumber = int64(globalStart + 1)
				playlistContent = r.generateM3U8()
				r.currentPlaylist = playlistContent
				r.cachedPlaylistBytes = []byte(playlistContent) // Pre-rendered RAM micro-cache
				readyToPublish = true
			}
		}
	}
	r.mu.Unlock()

	// Clean up scratch file from local disk
	_ = os.Remove(tsPath)

	if readyToPublish {
		discontNotice := ""
		if discontinuity {
			discontNotice = " [DISCONTINUITY TAGGED]"
		}
		r.appendLog("Uploaded %s (%.2fs)%s -> origin hot m3u8 updated (seq #%d)", remoteFilename, duration, discontNotice, r.seqNumber)
	} else {
		r.appendLog("Warming CF edge buffer: %d/%d chunks (%ds/60s on R2)", totalUploaded, delayChunks, totalUploaded*6)
	}
}

// generateM3U8 renders standard HLS version 6 manifest pointing to Cloudflare R2 .js chunks.
func (r *RelayEngine) generateM3U8() string {
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	sb.WriteString("#EXT-X-VERSION:6\n")
	sb.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", r.cfg.Stream.ChunkDuration+2))
	sb.WriteString(fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", r.seqNumber))
	sb.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")

	for _, seg := range r.activeSegments {
		if seg.Discontinuity {
			sb.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		sb.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", seg.Duration))
		sb.WriteString(seg.URL + "\n")
	}

	return sb.String()
}

// GetLiveM3U8 returns the active M3U8 string.
func (r *RelayEngine) GetLiveM3U8() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.currentPlaylist
}

// GetLiveM3U8Bytes returns the pre-rendered byte slice with zero allocations for high-throughput origin delivery.
func (r *RelayEngine) GetLiveM3U8Bytes() []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cachedPlaylistBytes
}

// GetStatus returns the current streaming metrics for the dashboard.
func (r *RelayEngine) GetStatus() StreamStatus {
	r.mu.RLock()
	uptime := int64(0)
	if r.isRunning && !r.startTime.IsZero() {
		uptime = int64(time.Since(r.startTime).Seconds())
	}

	delayChunks := r.cfg.Stream.BufferDelayChunks
	if delayChunks < 0 {
		delayChunks = 0
	}
	totalUploaded := int(r.baseIndex) + len(r.uploadedPool)
	isReady := totalUploaded >= delayChunks

	status := StreamStatus{
		IsRunning:         r.isRunning,
		ActiveChannel:     r.channelID,
		StreamID:          r.streamID,
		UptimeSeconds:     uptime,
		TotalChunks:       r.totalChunks,
		SequenceNumber:    r.seqNumber,
		LastChunkTime:     r.lastChunkTime,
		LastChunkUpload:   r.lastChunkUploadMs,
		LastError:         r.lastError,
		BufferDelayChunks: delayChunks,
		BufferedChunks:    totalUploaded,
		IsBufferReady:     isReady,
	}
	r.mu.RUnlock()

	r.logMu.Lock()
	logsCopy := make([]string, len(r.logs))
	copy(logsCopy, r.logs)
	r.logMu.Unlock()

	status.Logs = logsCopy
	return status
}
