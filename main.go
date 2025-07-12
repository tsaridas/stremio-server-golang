package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/rs/cors"
	"golang.org/x/net/websocket"
)

// Config holds server configuration
type Config struct {
	HTTPPort  int
	HTTPSPort int
	AppPath   string
	NoCORS    bool
	Username  string
	Password  string
	FFmpeg    *FFmpegConfig
}

// EngineFS represents the torrent streaming engine
type EngineFS struct {
	torrentManager *TorrentManager
	router         *mux.Router
}

// TorrentFile represents a file in a torrent
type TorrentFile struct {
	Name   string
	Size   int64
	Index  int
	Path   string
}

// Server represents the main Stremio server
type Server struct {
	config      *Config
	engineFS    *EngineFS
	ffmpegMgr   *FFmpegManager
	httpSrv     *http.Server
	httpsSrv    *http.Server
	router      *mux.Router
}

// NewServer creates a new Stremio server
func NewServer(config *Config) (*Server, error) {
	router := mux.NewRouter()
	
	// Create torrent manager
	torrentManager, err := NewTorrentManager(filepath.Join(config.AppPath, "cache"))
	if err != nil {
		return nil, fmt.Errorf("failed to create torrent manager: %v", err)
	}
	
	// Create FFmpeg manager
	var ffmpegMgr *FFmpegManager
	if config.FFmpeg != nil {
		ffmpegMgr, err = NewFFmpegManager(config.FFmpeg)
		if err != nil {
			log.Printf("Warning: FFmpeg initialization failed: %v", err)
		}
	}
	
	server := &Server{
		config: config,
		engineFS: &EngineFS{
			torrentManager: torrentManager,
			router:         router,
		},
		ffmpegMgr: ffmpegMgr,
		router:    router,
	}

	server.setupRoutes()
	return server, nil
}

// setupRoutes configures all the server routes
func (s *Server) setupRoutes() {
	// Add logging middleware
	s.router.Use(loggingMiddleware)

	// CORS middleware
	corsMiddleware := cors.New(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{"*"},
	})

	if !s.config.NoCORS {
		s.router.Use(corsMiddleware.Handler)
	}

	// Basic auth middleware if credentials are provided
	if s.config.Username != "" && s.config.Password != "" {
		s.router.Use(s.basicAuthMiddleware)
	}

	// API routes
	api := s.router.PathPrefix("/api").Subrouter()
	
	// EngineFS routes
	api.HandleFunc("/stream/{infoHash}/{fileIndex}", s.handleStream).Methods("GET")
	api.HandleFunc("/stats/{infoHash}", s.handleStats).Methods("GET")
	api.HandleFunc("/create", s.handleCreate).Methods("POST")
	api.HandleFunc("/remove/{infoHash}", s.handleRemove).Methods("DELETE")
	api.HandleFunc("/list", s.handleList).Methods("GET")
	
	// Proxy routes
	api.HandleFunc("/proxy", s.handleProxy).Methods("GET", "POST")
	
	// Settings routes
	api.HandleFunc("/settings", s.handleSettings).Methods("GET", "POST")
	api.HandleFunc("/network-info", s.handleNetworkInfo).Methods("GET")
	api.HandleFunc("/device-info", s.handleDeviceInfo).Methods("GET")
	
	// Subtitles routes
	api.HandleFunc("/subtitles", s.handleSubtitles).Methods("GET")
	api.HandleFunc("/subtitlesTracks", s.handleSubtitlesTracks).Methods("GET")
	api.HandleFunc("/opensubHash", s.handleOpenSubHash).Methods("GET")
	
	// Casting routes
	api.HandleFunc("/casting", s.handleCasting).Methods("GET", "POST")
	
	// Local addon routes
	api.HandleFunc("/local-addon", s.handleLocalAddon).Methods("GET", "POST")
	
	// Status and health check
	api.HandleFunc("/status", s.handleStatus).Methods("GET")
	api.HandleFunc("/heartbeat", s.handleHeartbeat).Methods("GET")
	
	// HLS streaming routes
	api.HandleFunc("/hlsv2/{infoHash}/{fileIndex}/master.m3u8", s.handleHLSMaster).Methods("GET")
	api.HandleFunc("/hlsv2/{infoHash}/{fileIndex}/stream.m3u8", s.handleHLSStream).Methods("GET")
	api.HandleFunc("/hlsv2/{infoHash}/{fileIndex}/stream-{quality}.m3u8", s.handleHLSQuality).Methods("GET")
	api.HandleFunc("/hlsv2/{infoHash}/{fileIndex}/stream-{quality}/{segment}.ts", s.handleHLSSegment).Methods("GET")
	
	// FFmpeg routes
	api.HandleFunc("/hwaccel-profiler", s.handleHardwareAccelProfiler).Methods("GET")
	api.HandleFunc("/transcode", s.handleTranscode).Methods("POST")
	api.HandleFunc("/probe", s.handleProbe).Methods("GET")
	
	// Thumbnail routes
	api.HandleFunc("/thumb.jpg", s.handleThumbnail).Methods("GET")
	
	// WebSocket support for real-time updates
	api.Handle("/ws", websocket.Handler(s.handleWebSocket))
	
	// Static file serving for web UI
	s.router.PathPrefix("/").Handler(http.FileServer(http.Dir("./build")))
}

// basicAuthMiddleware handles HTTP basic authentication
func (s *Server) basicAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != s.config.Username || password != s.config.Password {
			w.Header().Set("WWW-Authenticate", `Basic realm="Stremio Server"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// loggingMiddleware logs all HTTP requests
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s %s", r.RemoteAddr, r.Method, r.URL)
		next.ServeHTTP(w, r)
	})
}

// handleStream handles torrent file streaming
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash := vars["infoHash"]
	fileIndexStr := vars["fileIndex"]
	
	fileIndex, err := strconv.Atoi(fileIndexStr)
	if err != nil {
		http.Error(w, "Invalid file index", http.StatusBadRequest)
		return
	}
	
	// Get torrent engine
	engine, exists := s.engineFS.torrentManager.GetTorrent(infoHash)
	if !exists {
		http.Error(w, "Torrent not found", http.StatusNotFound)
		return
	}
	
	// Stream the file
	if err := engine.StreamFile(fileIndex, w, r); err != nil {
		log.Printf("Error streaming file: %v", err)
		http.Error(w, "Error streaming file", http.StatusInternalServerError)
		return
	}
}

// handleStats returns torrent statistics
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash := vars["infoHash"]
	
	engine, exists := s.engineFS.torrentManager.GetTorrent(infoHash)
	if !exists {
		http.Error(w, "Torrent not found", http.StatusNotFound)
		return
	}
	
	stats := engine.GetStats()
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// handleCreate creates a new torrent engine
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MagnetURI string `json:"magnetURI"`
		InfoHash  string `json:"infoHash"`
	}
	
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	
	if req.MagnetURI == "" {
		http.Error(w, "Magnet URI is required", http.StatusBadRequest)
		return
	}
	
	// Add torrent to manager
	engine, err := s.engineFS.torrentManager.AddTorrent(req.MagnetURI)
	if err != nil {
		log.Printf("Error creating torrent: %v", err)
		http.Error(w, "Error creating torrent", http.StatusInternalServerError)
		return
	}
	
	log.Printf("Created torrent engine for: %s", engine.InfoHash)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":   "created",
		"infoHash": engine.InfoHash,
	})
}

// handleRemove removes a torrent engine
func (s *Server) handleRemove(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash := vars["infoHash"]
	
	if err := s.engineFS.torrentManager.RemoveTorrent(infoHash); err != nil {
		log.Printf("Error removing torrent: %v", err)
		http.Error(w, "Error removing torrent", http.StatusInternalServerError)
		return
	}
	
	log.Printf("Removed torrent engine: %s", infoHash)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "removed"})
}

// handleList returns all active torrent engines
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	engines := s.engineFS.torrentManager.ListTorrents()
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(engines)
}

// handleProxy handles proxy requests
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	targetURL := r.URL.Query().Get("url")
	if targetURL == "" {
		http.Error(w, "Missing target URL", http.StatusBadRequest)
		return
	}
	
	parsedURL, err := url.Parse(targetURL)
	if err != nil {
		http.Error(w, "Invalid target URL", http.StatusBadRequest)
		return
	}
	
	// Create reverse proxy
	proxy := httputil.NewSingleHostReverseProxy(parsedURL)
	
	// Modify request
	r.URL.Host = parsedURL.Host
	r.URL.Scheme = parsedURL.Scheme
	r.Header.Set("X-Forwarded-Host", r.Header.Get("Host"))
	r.Host = parsedURL.Host
	
	proxy.ServeHTTP(w, r)
}

// handleSettings handles server settings
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		settings := map[string]interface{}{
			"localAddonEnabled":    true,
			"proxyStreamsEnabled":  true,
			"streamingServerUrl":   fmt.Sprintf("http://127.0.0.1:%d", s.config.HTTPPort),
			"hardwareAcceleration": true,
		}
		
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(settings)
	} else if r.Method == "POST" {
		// TODO: Implement settings update
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
	}
}

// handleNetworkInfo returns network information
func (s *Server) handleNetworkInfo(w http.ResponseWriter, r *http.Request) {
	info := map[string]interface{}{
		"localIP":   getLocalIP(),
		"publicIP":  getPublicIP(),
		"serverPort": s.config.HTTPPort,
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

// handleDeviceInfo returns device information
func (s *Server) handleDeviceInfo(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()
	info := map[string]interface{}{
		"hostname": hostname,
		"platform": "linux",
		"arch":     "amd64",
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

// handleSubtitles handles subtitle requests
func (s *Server) handleSubtitles(w http.ResponseWriter, r *http.Request) {
	// TODO: Implement subtitle handling
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("Subtitles placeholder"))
}

// handleSubtitlesTracks handles subtitle tracks
func (s *Server) handleSubtitlesTracks(w http.ResponseWriter, r *http.Request) {
	// TODO: Implement subtitle tracks
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode([]interface{}{})
}

// handleOpenSubHash handles OpenSubtitles hash
func (s *Server) handleOpenSubHash(w http.ResponseWriter, r *http.Request) {
	// TODO: Implement OpenSubtitles hash
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"hash": ""})
}

// handleCasting handles casting requests
func (s *Server) handleCasting(w http.ResponseWriter, r *http.Request) {
	// TODO: Implement casting
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "casting_disabled"})
}

// handleLocalAddon handles local addon requests
func (s *Server) handleLocalAddon(w http.ResponseWriter, r *http.Request) {
	// TODO: Implement local addon handling
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "local_addon_disabled"})
}

// handleStatus returns server status
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	engines := s.engineFS.torrentManager.ListTorrents()
	status := map[string]interface{}{
		"status":    "running",
		"uptime":    time.Since(startTime).String(),
		"engines":   len(engines),
		"httpPort":  s.config.HTTPPort,
		"httpsPort": s.config.HTTPSPort,
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// handleHeartbeat handles heartbeat requests
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "alive"})
}

// handleHLSMaster handles HLS master playlist
func (s *Server) handleHLSMaster(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash := vars["infoHash"]
	fileIndexStr := vars["fileIndex"]
	
	fileIndex, err := strconv.Atoi(fileIndexStr)
	if err != nil {
		http.Error(w, "Invalid file index", http.StatusBadRequest)
		return
	}
	
	// Get torrent engine
	engine, exists := s.engineFS.torrentManager.GetTorrent(infoHash)
	if !exists {
		http.Error(w, "Torrent not found", http.StatusNotFound)
		return
	}
	
	// Get file info
	_, err = engine.GetFile(fileIndex)
	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	
	// Generate master playlist
	playlist := fmt.Sprintf(`#EXTM3U
#EXT-X-VERSION:3
#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1920x1080
stream-1080p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1000000,RESOLUTION=1280x720
stream-720p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=500000,RESOLUTION=854x480
stream-480p.m3u8`)
	
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Write([]byte(playlist))
}

// handleHLSStream handles HLS stream playlist
func (s *Server) handleHLSStream(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash := vars["infoHash"]
	fileIndexStr := vars["fileIndex"]
	
	fileIndex, err := strconv.Atoi(fileIndexStr)
	if err != nil {
		http.Error(w, "Invalid file index", http.StatusBadRequest)
		return
	}
	
	// Get torrent engine
	engine, exists := s.engineFS.torrentManager.GetTorrent(infoHash)
	if !exists {
		http.Error(w, "Torrent not found", http.StatusNotFound)
		return
	}
	
	// Get file info
	_, err = engine.GetFile(fileIndex)
	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	
	// Generate stream playlist
	playlist := fmt.Sprintf(`#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:10
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:10.0,
segment_000.ts
#EXTINF:10.0,
segment_001.ts
#EXT-X-ENDLIST`)
	
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Write([]byte(playlist))
}

// handleHLSQuality handles HLS quality-specific playlists
func (s *Server) handleHLSQuality(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash := vars["infoHash"]
	fileIndexStr := vars["fileIndex"]
	quality := vars["quality"]
	
	fileIndex, err := strconv.Atoi(fileIndexStr)
	if err != nil {
		http.Error(w, "Invalid file index", http.StatusBadRequest)
		return
	}
	
	// Get torrent engine
	engine, exists := s.engineFS.torrentManager.GetTorrent(infoHash)
	if !exists {
		http.Error(w, "Torrent not found", http.StatusNotFound)
		return
	}
	
	// Get file info
	_, err = engine.GetFile(fileIndex)
	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	
	// Generate quality-specific playlist
	playlist := fmt.Sprintf(`#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:10
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:10.0,
%s/segment_000.ts
#EXTINF:10.0,
%s/segment_001.ts
#EXT-X-ENDLIST`, quality, quality)
	
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Write([]byte(playlist))
}

// handleHLSSegment handles HLS segment serving
func (s *Server) handleHLSSegment(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash := vars["infoHash"]
	fileIndexStr := vars["fileIndex"]
	quality := vars["quality"]
	segment := vars["segment"]
	
	fileIndex, err := strconv.Atoi(fileIndexStr)
	if err != nil {
		http.Error(w, "Invalid file index", http.StatusBadRequest)
		return
	}
	
	// Get torrent engine
	engine, exists := s.engineFS.torrentManager.GetTorrent(infoHash)
	if !exists {
		http.Error(w, "Torrent not found", http.StatusNotFound)
		return
	}
	
	// Get file info
	_, err = engine.GetFile(fileIndex)
	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	
	// Check if segment exists in cache
	segmentPath := filepath.Join(s.config.AppPath, "hls", infoHash, fileIndexStr, quality, segment)
	if _, err := os.Stat(segmentPath); os.IsNotExist(err) {
		// Generate segment using FFmpeg if available
		if s.ffmpegMgr != nil {
			// TODO: Implement real-time segment generation
			http.Error(w, "Segment not found", http.StatusNotFound)
			return
		}
		http.Error(w, "Segment not found", http.StatusNotFound)
		return
	}
	
	// Serve the segment
	http.ServeFile(w, r, segmentPath)
}

// handleHardwareAccelProfiler handles hardware acceleration profiling
func (s *Server) handleHardwareAccelProfiler(w http.ResponseWriter, r *http.Request) {
	if s.ffmpegMgr == nil {
		http.Error(w, "FFmpeg not available", http.StatusServiceUnavailable)
		return
	}
	
	info := s.ffmpegMgr.GetHardwareAccelerationInfo()
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

// handleTranscode handles video transcoding requests
func (s *Server) handleTranscode(w http.ResponseWriter, r *http.Request) {
	if s.ffmpegMgr == nil {
		http.Error(w, "FFmpeg not available", http.StatusServiceUnavailable)
		return
	}
	
	var req struct {
		InputPath  string           `json:"inputPath"`
		OutputPath string           `json:"outputPath"`
		Options    TranscodeOptions `json:"options"`
	}
	
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	
	// Validate input file exists
	if _, err := os.Stat(req.InputPath); os.IsNotExist(err) {
		http.Error(w, "Input file not found", http.StatusNotFound)
		return
	}
	
	// Ensure output directory exists
	outputDir := filepath.Dir(req.OutputPath)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		http.Error(w, "Failed to create output directory", http.StatusInternalServerError)
		return
	}
	
	// Start transcoding in a goroutine
	go func() {
		if err := s.ffmpegMgr.TranscodeVideo(req.InputPath, req.OutputPath, req.Options); err != nil {
			log.Printf("Transcoding failed: %v", err)
		} else {
			log.Printf("Transcoding completed: %s -> %s", req.InputPath, req.OutputPath)
		}
	}()
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "transcoding_started"})
}

// handleProbe handles video probing requests
func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	if s.ffmpegMgr == nil {
		http.Error(w, "FFmpeg not available", http.StatusServiceUnavailable)
		return
	}
	
	inputPath := r.URL.Query().Get("path")
	if inputPath == "" {
		http.Error(w, "Missing path parameter", http.StatusBadRequest)
		return
	}
	
	// Validate input file exists
	if _, err := os.Stat(inputPath); os.IsNotExist(err) {
		http.Error(w, "Input file not found", http.StatusNotFound)
		return
	}
	
	videoInfo, err := s.ffmpegMgr.GetVideoInfo(inputPath)
	if err != nil {
		log.Printf("Video probing failed: %v", err)
		http.Error(w, "Failed to probe video", http.StatusInternalServerError)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(videoInfo)
}

// handleThumbnail handles thumbnail requests
func (s *Server) handleThumbnail(w http.ResponseWriter, r *http.Request) {
	if s.ffmpegMgr == nil {
		http.Error(w, "FFmpeg not available", http.StatusServiceUnavailable)
		return
	}
	
	inputPath := r.URL.Query().Get("path")
	timeOffsetStr := r.URL.Query().Get("time")
	
	if inputPath == "" {
		http.Error(w, "Missing path parameter", http.StatusBadRequest)
		return
	}
	
	// Validate input file exists
	if _, err := os.Stat(inputPath); os.IsNotExist(err) {
		http.Error(w, "Input file not found", http.StatusNotFound)
		return
	}
	
	timeOffset := 10.0 // Default to 10 seconds
	if timeOffsetStr != "" {
		if offset, err := strconv.ParseFloat(timeOffsetStr, 64); err == nil {
			timeOffset = offset
		}
	}
	
	// Create thumbnail in cache directory
	thumbnailPath := filepath.Join(s.config.AppPath, "thumbnails", filepath.Base(inputPath)+".jpg")
	if err := os.MkdirAll(filepath.Dir(thumbnailPath), 0755); err != nil {
		http.Error(w, "Failed to create thumbnail directory", http.StatusInternalServerError)
		return
	}
	
	// Generate thumbnail
	if err := s.ffmpegMgr.GenerateThumbnail(inputPath, thumbnailPath, timeOffset); err != nil {
		log.Printf("Thumbnail generation failed: %v", err)
		http.Error(w, "Failed to generate thumbnail", http.StatusInternalServerError)
		return
	}
	
	// Serve the thumbnail
	http.ServeFile(w, r, thumbnailPath)
}

// handleWebSocket handles WebSocket connections
func (s *Server) handleWebSocket(ws *websocket.Conn) {
	defer ws.Close()
	
	// TODO: Implement WebSocket handling for real-time updates
	log.Printf("WebSocket connection established")
	
	// Keep connection alive
	for {
		var msg string
		if err := websocket.Message.Receive(ws, &msg); err != nil {
			break
		}
		
		// Echo message back
		websocket.Message.Send(ws, "echo: "+msg)
	}
}

// Start starts the server
func (s *Server) Start() error {
	// Start HTTP server
	s.httpSrv = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.config.HTTPPort),
		Handler: s.router,
	}
	
	go func() {
		log.Printf("Starting HTTP server on port %d", s.config.HTTPPort)
		if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server error: %v", err)
		}
	}()
	
	// Start HTTPS server (only if certificates are available)
	certFile := getEnv("SSL_CERT_FILE", "")
	keyFile := getEnv("SSL_KEY_FILE", "")
	
	if certFile != "" && keyFile != "" {
		s.httpsSrv = &http.Server{
			Addr:    fmt.Sprintf(":%d", s.config.HTTPSPort),
			Handler: s.router,
			TLSConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
		}
		
		go func() {
			log.Printf("Starting HTTPS server on port %d", s.config.HTTPSPort)
			if err := s.httpsSrv.ListenAndServeTLS(certFile, keyFile); err != nil && err != http.ErrServerClosed {
				log.Printf("HTTPS server error: %v", err)
			}
		}()
	} else {
		log.Printf("HTTPS server disabled - SSL certificates not provided")
	}
	
	return nil
}

// Stop gracefully stops the server
func (s *Server) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	
	var errs []error
	
	if err := s.httpSrv.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("HTTP server shutdown error: %v", err))
	}
	
	if err := s.httpsSrv.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("HTTPS server shutdown error: %v", err))
	}
	
	// Close torrent manager
	if err := s.engineFS.torrentManager.Close(); err != nil {
		errs = append(errs, fmt.Errorf("torrent manager shutdown error: %v", err))
	}
	
	if len(errs) > 0 {
		return fmt.Errorf("server shutdown errors: %v", errs)
	}
	
	return nil
}

// Utility functions
func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	
	return "127.0.0.1"
}

func getPublicIP() string {
	// TODO: Implement public IP detection
	return "127.0.0.1"
}

var startTime = time.Now()

func main() {
	// Parse environment variables
	config := &Config{
		HTTPPort:  getEnvInt("HTTP_PORT", 11470),
		HTTPSPort: getEnvInt("HTTPS_PORT", 12470),
		AppPath:   getEnv("APP_PATH", expandHomeDir("~/.stremio-server")),
		NoCORS:    getEnvBool("NO_CORS", false),
		Username:  getEnv("USERNAME", ""),
		Password:  getEnv("PASSWORD", ""),
		FFmpeg: &FFmpegConfig{
			HardwareAcceleration: getEnvBool("FFMPEG_HARDWARE_ACCEL", true),
			TranscodeHorsepower:  getEnvFloat("FFMPEG_HORSEPOWER", 0.75),
			TranscodeMaxBitRate:  getEnvInt("FFMPEG_MAX_BITRATE", 0),
			TranscodeConcurrency: getEnvInt("FFMPEG_CONCURRENCY", 1),
			TranscodeMaxWidth:    getEnvInt("FFMPEG_MAX_WIDTH", 1920),
			TranscodeProfile:     getEnv("FFMPEG_PROFILE", ""),
			Debug:                getEnvBool("FFMPEG_DEBUG", false),
		},
	}
	
	// Create server
	server, err := NewServer(config)
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}
	
	// Start server
	if err := server.Start(); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
	
	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	
	log.Println("Shutting down server...")
	
	// Stop server gracefully
	if err := server.Stop(); err != nil {
		log.Printf("Error stopping server: %v", err)
	}
	
	log.Println("Server stopped")
}

// Environment variable helpers
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if boolValue, err := strconv.ParseBool(value); err == nil {
			return boolValue
		}
	}
	return defaultValue
}

func getEnvFloat(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		if floatValue, err := strconv.ParseFloat(value, 64); err == nil {
			return floatValue
		}
	}
	return defaultValue
}

// expandHomeDir expands ~ to the user's home directory
func expandHomeDir(path string) string {
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[1:])
	}
	return path
} 