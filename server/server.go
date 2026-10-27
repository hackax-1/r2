// Package server implements the embedded HTTP server and REST API for the Free CDN Live Relay.
//
// Endpoints:
//   - GET  /              : Utilitarian Pure HTML Admin Dashboard (ingest controls, presets, terminal logs).
//   - GET  /watch         : Minimalist All-White Clappr Viewer Player.
//   - GET  /live.m3u8     : Real-time origin HLS manifest with 5s micro-cache headers.
//   - GET  /api/status    : Real-time stream metrics (uptime, buffered chunks, logs, latency).
//   - GET  /api/start     : Ingest a specified IPTV channel ID (strictly enforces 1 stream).
//   - GET  /api/stop      : Cleanly terminate active FFmpeg ingest.
//   - GET  /api/channels  : Popular verified channel presets.
package server

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"

	"free-cdn-relay/config"
	"free-cdn-relay/relay"
)

// webFS embeds HTML, CSS, and JS assets directly into the compiled single binary.
//
//go:embed web/*
var webFS embed.FS

// Server hosts the HTTP REST API and UI.
type Server struct {
	cfg    *config.Config
	engine *relay.RelayEngine
	mux    *http.ServeMux
}

// NewServer initializes the HTTP server with configured routing.
func NewServer(cfg *config.Config, engine *relay.RelayEngine) *Server {
	s := &Server{
		cfg:    cfg,
		engine: engine,
		mux:    http.NewServeMux(),
	}
	s.routes()
	return s
}

// routes registers all HTTP handlers.
func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/admin", s.handleIndex)
	s.mux.HandleFunc("/watch", s.handleWatch)
	s.mux.HandleFunc("/player", s.handleWatch)
	s.mux.HandleFunc("/play", s.handleWatch)
	s.mux.HandleFunc("/player.html", s.handleWatch)
	s.mux.HandleFunc("/clappr.min.js", s.handleClapprJS)
	s.mux.HandleFunc("/level-selector.min.js", s.handleLevelSelectorJS)
	s.mux.HandleFunc("/live.m3u8", s.handleLiveM3U8)
	s.mux.HandleFunc("/api/status", s.handleStatus)
	s.mux.HandleFunc("/api/start", s.handleStart)
	s.mux.HandleFunc("/api/stop", s.handleStop)
	s.mux.HandleFunc("/api/channels", s.handleChannels)
}

// ServeHTTP adds global CORS headers and delegates to the internal mux.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// handleLiveM3U8 serves the live sliding-window playlist directly from RAM.
// Employs a 5-second micro-cache (3s max-age + 2s stale-while-revalidate) to protect the origin
// from polling spikes while chunks are offloaded to Cloudflare R2 ($0 egress).
func (s *Server) handleLiveM3U8(w http.ResponseWriter, r *http.Request) {
	data := s.engine.GetLiveM3U8Bytes()
	if len(data) == 0 {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:0\n"))
		return
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "public, max-age=3, stale-while-revalidate=2")
	_, _ = w.Write(data)
}

// handleStatus returns real-time streaming metrics as JSON.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	status := s.engine.GetStatus()
	_ = json.NewEncoder(w).Encode(status)
}

// handleStart switches the active channel. Enforces strictly 1 active upstream IPTV line.
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	channel := r.URL.Query().Get("channel")
	if channel == "" {
		channel = s.cfg.Stream.DefaultChannel
	}
	referer := r.URL.Query().Get("referer")
	if referer == "" {
		referer = r.URL.Query().Get("ref")
	}
	ua := r.URL.Query().Get("ua")
	if ua == "" {
		ua = r.URL.Query().Get("user_agent")
	}
	headers := r.URL.Query().Get("headers")

	err := s.engine.StartChannelAdvanced(channel, ua, referer, headers)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"channel": channel,
		"message": fmt.Sprintf("Started ingest for channel %s (6s chunks -> Cloudflare R2)", channel),
	})
}

// handleStop cleanly terminates the active stream.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	s.engine.Stop()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Stream stopped cleanly",
	})
}

// ChannelPreset represents a pre-configured channel button for the dashboard.
type ChannelPreset struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
}

// handleChannels returns verified channel presets.
func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	presets := []ChannelPreset{
		{ID: "389601", Name: "Family Guy FHD (24/7 Animation)", Category: "Animation"},
		{ID: "352259", Name: "MX: TUDN HD (Liga MX / Sports)", Category: "Sports"},
		{ID: "347276", Name: "US: ESPN HD", Category: "Sports"},
		{ID: "347277", Name: "US: ESPN 2 HD", Category: "Sports"},
		{ID: "347285", Name: "US: FOX Sports 1 HD", Category: "Sports"},
		{ID: "347287", Name: "US: NFL Network HD", Category: "Sports"},
		{ID: "347288", Name: "US: NFL RedZone HD", Category: "Sports"},
		{ID: "347291", Name: "US: CBS Sports Network HD", Category: "Sports"},
		{ID: "347293", Name: "US: NBC Sports HD", Category: "Sports"},
		{ID: "352261", Name: "MX: Fox Sports 1 HD", Category: "Sports"},
		{ID: "352262", Name: "MX: Fox Sports 2 HD", Category: "Sports"},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(presets)
}

// handleIndex serves the Gruvbox Dark Admin Dashboard.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if data, err := webFS.ReadFile("web/index.html"); err == nil {
		_, _ = w.Write(data)
		return
	}
	http.Error(w, "index.html not found", http.StatusNotFound)
}

// handleWatch serves the dedicated All-White Clappr Viewer Player.
func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if data, err := webFS.ReadFile("web/watch.html"); err == nil {
		_, _ = w.Write(data)
		return
	}
	http.Error(w, "watch.html not found", http.StatusNotFound)
}

// handleClapprJS serves the embedded or fallback clappr.min.js
func (s *Server) handleClapprJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if data, err := webFS.ReadFile("web/clappr.min.js"); err == nil {
		_, _ = w.Write(data)
		return
	}
	http.Redirect(w, r, "https://cdn.jsdelivr.net/npm/@clappr/player@latest/dist/clappr.min.js", http.StatusTemporaryRedirect)
}

// handleLevelSelectorJS serves the embedded or fallback level-selector.min.js
func (s *Server) handleLevelSelectorJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if data, err := webFS.ReadFile("web/level-selector.min.js"); err == nil {
		_, _ = w.Write(data)
		return
	}
	http.Redirect(w, r, "https://cdn.jsdelivr.net/gh/clappr/clappr-level-selector-plugin@latest/dist/level-selector.min.js", http.StatusTemporaryRedirect)
}
