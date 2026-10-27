// Application Entrypoint for Free CDN Live Relay.
//
// Overview:
//   A single-binary Go backend that ingests an upstream IPTV live stream (Xtream Codes / MPEG-TS),
//   slices the stream into standard 6.0-second GOP keyframe-aligned chunks (-c copy, 0% CPU),
//   disguises segments as static .js assets (Roxiestreams trick), offloads all video bandwidth
//   to Cloudflare R2 ($0 egress CDN), and serves a live sliding-window playlist (.m3u8) directly
//   from origin RAM with a 5-second micro-cache.
//
// Usage:
//   ./cdn-relay -port=8888 -channel=389601 -autostart=true
package main
//imports
import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"free-cdn-relay/config"
	"free-cdn-relay/relay"
	"free-cdn-relay/server"
)

func main() {
	portFlag := flag.Int("port", 8888, "HTTP server port")
	configFlag := flag.String("config", "config.json", "Path to config.json")
	channelFlag := flag.String("channel", "", "Default IPTV channel ID")
	autoStartFlag := flag.Bool("autostart", true, "Auto start ingest on boot")
	flag.Parse()

	// Locate config.json with fallback candidates
	configPath := *configFlag
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		candidates := []string{"config.json", "../config.json", "../../config.json"}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				configPath = c
				break
			}
		}
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to initialize configuration: %v", err)
	}

	if *portFlag != 8080 {
		cfg.Port = *portFlag
	}
	if *channelFlag != "" {
		cfg.Stream.DefaultChannel = *channelFlag
	}

	// Ensure work scratch directory exists
	_ = os.MkdirAll(cfg.Stream.WorkDir, 0755)

	log.Printf("==================================================")
	log.Printf("  Free CDN Stream Relay (Pure Go Backend)")
	log.Printf("==================================================")
	log.Printf("  IPTV Host      : %s", cfg.IPTV.Host)
	log.Printf("  IPTV Account   : %s (1 stream max limit)", cfg.IPTV.Username)
	log.Printf("  Default Channel: %s", cfg.Stream.DefaultChannel)
	log.Printf("  Chunk Standard : %d seconds", cfg.Stream.ChunkDuration)
	log.Printf("  Cloudflare R2  : %s", cfg.S3.Bucket)
	log.Printf("  Public CDN URL : %s", cfg.S3.PublicURL)
	log.Printf("  HTTP Port      : %d", cfg.Port)
	log.Printf("==================================================")

	// Initialize modular streaming engine and HTTP server
	engine := relay.NewRelayEngine(cfg)
	srv := server.NewServer(cfg, engine)

	// Graceful termination handling (SIGINT / SIGTERM)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("\nReceived shutdown signal. Stopping FFmpeg ingest cleanly...")
		engine.Stop()
		_ = os.RemoveAll(filepath.Join(cfg.Stream.WorkDir, "stream_"+cfg.Stream.DefaultChannel))
		os.Exit(0)
	}()

	// Auto-start initial channel ingest if requested
	if *autoStartFlag && cfg.Stream.DefaultChannel != "" {
		go func() {
			if err := engine.StartChannel(cfg.Stream.DefaultChannel); err != nil {
				log.Printf("Auto-start warning: %v", err)
			}
		}()
	}

	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("Player & API Server listening at http://localhost:%d", cfg.Port)
	log.Printf("Zero-Bandwidth CDN stream available at: %s/live.m3u8", cfg.S3.PublicURL)

	httpServer := &http.Server{
		Addr:    addr,
		Handler: srv,
	}

	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("HTTP server failed: %v", err)
	}
}
