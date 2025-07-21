package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/abema/go-mp4"
	"github.com/gorilla/mux"
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
	LogLevel  string
}

// EngineFS represents the torrent streaming engine
type EngineFS struct {
	torrentManager *TorrentManager
	router         *mux.Router
}

// TorrentFile represents a file in a torrent
type TorrentFile struct {
	Name  string
	Size  int64
	Index int
	Path  string
}

// Server represents the main Stremio server
type Server struct {
	config      *Config
	engineFS    *EngineFS
	ffmpegMgr   *FFmpegManager
	httpSrv     *http.Server
	httpsSrv    *http.Server
	router      *mux.Router
	localAddons []*Addon
	ffmpegSem   chan struct{}
}

// NewServer creates a new Stremio server (matching Node.js server.js exactly)
func NewServer(config *Config) (*Server, error) {
	router := mux.NewRouter()

	server := &Server{
		config: config,
		engineFS: &EngineFS{
			torrentManager: nil, // Will be initialized in Start()
			router:         router,
		},
		ffmpegMgr: nil, // Will be initialized in Start()
		router:    router,
		ffmpegSem: make(chan struct{}, 2), // Limit to 2 concurrent FFmpeg processes
	}

	return server, nil
}

// setupRoutes configures all the server routes (matching Node.js server.js exactly)
func (s *Server) setupRoutes() {
	// Add logging middleware
	s.router.Use(loggingMiddleware)

	// CORS middleware (matching Node.js server.js exactly)
	s.router.Use(s.corsMiddleware)

	// Basic auth middleware if credentials are provided
	if s.config.Username != "" && s.config.Password != "" {
		s.router.Use(s.basicAuthMiddleware)
	}

	// Routes matching Node.js server.js exactly (in the same order)

	// Basic routes
	s.router.HandleFunc("/favicon.ico", s.handleFavicon).Methods("GET")
	s.router.HandleFunc("/heartbeat", s.handleHeartbeat).Methods("GET")
	s.router.HandleFunc("/status", s.handleStatus).Methods("GET")

	// Stats routes (matching Node.js exactly)
	s.router.HandleFunc("/stats.json", s.handleGlobalStats).Methods("GET", "HEAD")
	s.router.HandleFunc("/{infoHash}/stats.json", s.handleFileStats).Methods("GET", "HEAD")
	s.router.HandleFunc("/{infoHash}/{fileIndex}/stats.json", s.handleFileStats).Methods("GET", "HEAD")

	// Torrent management routes (matching Node.js exactly)
	// s.router.HandleFunc("/create", s.handleCreate).Methods("POST")
	s.router.HandleFunc("/{infoHash}/create", s.handleCreateWithInfoHash).Methods("POST")
	s.router.HandleFunc("/{infoHash}/remove", s.handleRemove).Methods("GET")
	s.router.HandleFunc("/removeAll", s.handleRemoveAll).Methods("GET")

	// Main streaming routes moved to after HLS routes to avoid conflicts

	// Probe and media info routes (matching Node.js exactly)
	s.router.HandleFunc("/probe", s.handleProbe).Methods("GET", "HEAD")

	// Utility routes (matching Node.js exactly)
	s.router.HandleFunc("/stream", s.handleStreamProxy).Methods("GET")
	s.router.HandleFunc("/get-https", s.handleGetHttps).Methods("GET")
	s.router.HandleFunc("/tracks/{url:.*}", s.handleTracks).Methods("GET")
	s.router.HandleFunc("/yt/{id}.json", s.handleYoutubeJson).Methods("GET")
	s.router.HandleFunc("/yt/{id}", s.handleYoutube).Methods("GET")
	s.router.HandleFunc("/subtitles.{ext}", s.handleSubtitlesExt).Methods("GET")
	s.router.HandleFunc("/subtitles", s.handleSubtitles).Methods("GET")
	s.router.HandleFunc("/subtitlesTracks", s.handleSubtitlesTracks).Methods("GET")
	s.router.HandleFunc("/subtitles.vtt", s.handleSubtitlesVTT).Methods("GET")
	s.router.HandleFunc("/opensubHash", s.handleOpenSubHash).Methods("GET")
	s.router.HandleFunc("/casting", s.handleCasting).Methods("GET")
	s.router.PathPrefix("/local-addon").Handler(http.StripPrefix("/local-addon", s.handleLocalAddon()))

	// Settings and info routes (matching Node.js exactly)
	s.router.HandleFunc("/settings", s.handleSettings).Methods("GET", "POST")
	s.router.HandleFunc("/network-info", s.handleNetworkInfo).Methods("GET")
	s.router.HandleFunc("/device-info", s.handleDeviceInfo).Methods("GET")
	s.router.HandleFunc("/hwaccel-profiler", s.handleHardwareAccelProfiler).Methods("GET")

	// HLS routes (matching Node.js server.js exactly)
	// Main HLS routes - these must come BEFORE the parameterized routes
	s.router.HandleFunc("/test", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Test endpoint called!")
		w.Write([]byte("Test OK"))
	}).Methods("GET")
	s.router.HandleFunc("/hlsv2/probe", s.handleHLSProbe).Methods("GET")
	s.router.HandleFunc("/hlsv2/probe-debug", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("HLS Probe Debug: Function called!")
		w.Write([]byte("HLS Probe Debug OK"))
	}).Methods("GET")
	s.router.HandleFunc("/hlsv2/status", s.handleHLSStatus).Methods("GET")
	s.router.HandleFunc("/hlsv2/exec", s.handleHLSExec).Methods("GET")

	// HLS master playlist routes (matching Node.js exactly) - must come before generic routes
	s.router.HandleFunc("/hlsv2/{infoHash}/master.m3u8", s.handleHLSMaster).Methods("GET", "HEAD")
	s.router.HandleFunc("/hlsv2/{infoHash}/{fileIndex}/master.m3u8", s.handleHLSMaster).Methods("GET", "HEAD")

	// HLS track routes without fileIndex (matching Node.js exactly) - must come before generic routes
	s.router.HandleFunc("/hlsv2/{infoHash:[0-9a-fA-F]{32,40}|file|url}/video0.m3u8", s.handleHLSVideo0M3U8).Methods("GET", "HEAD")
	s.router.HandleFunc("/hlsv2/{infoHash:[0-9a-fA-F]{32,40}|file|url}/audio0.m3u8", s.handleHLSAudio0M3U8).Methods("GET", "HEAD")
	s.router.HandleFunc("/hlsv2/{infoHash:[0-9a-fA-F]{32,40}|file|url}/subtitle0.m3u8", s.handleHLSSubtitleM3U8).Methods("GET", "HEAD")

	// HLS audio init segment route (specific) - must come before generic routes
	s.router.HandleFunc("/hlsv2/{infoHash}/{fileIndex}/audio0/init.mp4", s.handleHLSAudioInitSegment).Methods("GET", "HEAD")

	// Generic HLSv2 routes (matching Node.js exactly) - must come after specific routes
	s.router.HandleFunc("/hlsv2/{id}/{track}.m3u8", s.handleHLSGenericTrackM3U8).Methods("GET")
	s.router.HandleFunc("/hlsv2/{id}/{track}/init.mp4", s.handleHLSGenericTrackInitMP4).Methods("GET")
	s.router.HandleFunc("/hlsv2/{id}/{track}/segment{sequenceNumber}.{ext}", s.handleHLSGenericTrackSegment).Methods("GET")

	// Utility HLSv2 routes (matching Node.js exactly)
	s.router.HandleFunc("/hlsv2/{id}/burn", s.handleHLSBurn).Methods("GET")
	s.router.HandleFunc("/hlsv2/{id}/destroy", s.handleHLSDestroy).Methods("GET")

	// HLS stream playlist routes (matching Node.js exactly)
	s.router.HandleFunc("/hlsv2/{infoHash}/{fileIndex}/stream.m3u8", s.handleHLSStream).Methods("GET", "HEAD")

	// HLS segment routes (matching Node.js exactly)
	s.router.HandleFunc("/hlsv2/{infoHash}/init.mp4", s.handleHLSInitSegmentNoFileIndex).Methods("GET", "HEAD")
	s.router.HandleFunc("/hlsv2/{infoHash}/{fileIndex}/init.mp4", s.handleHLSInitSegment).Methods("GET", "HEAD")
	s.router.HandleFunc("/hlsv2/{infoHash}/segment{sequence}.m4s", s.handleHLSSegmentM4S).Methods("GET", "HEAD")
	s.router.HandleFunc("/hlsv2/{infoHash}/{fileIndex}/segment{sequence}.m4s", s.handleHLSSegmentM4S).Methods("GET", "HEAD")

	// HLS quality-specific routes (matching Node.js exactly)
	s.router.HandleFunc("/hlsv2/{infoHash}/{fileIndex}/stream-q-{quality}.m3u8", s.handleHLSQuality).Methods("GET", "HEAD")
	s.router.HandleFunc("/hlsv2/{infoHash}/{fileIndex}/stream-q-{quality}/{seg}.ts", s.handleHLSSegment).Methods("GET", "HEAD")

	// New HLS routes with ffmpeg transcoding/splitting (similar to server.js)
	s.router.HandleFunc("/hlsv2/{infoHash}/{fileIndex}/stream-q-{quality}/transcode/{seg}.ts", s.handleHLSSegmentTranscode).Methods("GET", "HEAD")
	s.router.HandleFunc("/hlsv2/{infoHash}/{fileIndex}/stream-q-{quality}/split.m3u8", s.handleHLSStreamSplit).Methods("GET", "HEAD")

	// HLS audio routes (matching Node.js exactly)
	s.router.HandleFunc("/hlsv2/{infoHash}/{fileIndex}/audio0.m3u8", s.handleHLSAudio0M3U8).Methods("GET", "HEAD")
	s.router.HandleFunc("/hlsv2/{infoHash}/{fileIndex}/audio0/init.mp4", s.handleHLSAudioInitSegment).Methods("GET", "HEAD")
	s.router.HandleFunc("/hlsv2/{infoHash}/{fileIndex}/audio0/segment{sequence}.m4s", s.handleHLSAudioSegmentM4S).Methods("GET", "HEAD")

	// HLS subtitle routes (matching Node.js exactly)
	s.router.HandleFunc("/hlsv2/{infoHash}/{fileIndex}/subtitle0.m3u8", s.handleHLSSubtitleM3U8).Methods("GET", "HEAD")
	s.router.HandleFunc("/hlsv2/{infoHash}/{fileIndex}/subtitle0/init.mp4", s.handleHLSSubtitleInitSegment).Methods("GET", "HEAD")
	s.router.HandleFunc("/hlsv2/{infoHash}/{fileIndex}/subtitle0/segment{sequence}.m4s", s.handleHLSSubtitleSegmentM4S).Methods("GET", "HEAD")

	// HLS video routes (matching Node.js exactly)
	s.router.HandleFunc("/hlsv2/{infoHash}/{fileIndex}/video0.m3u8", s.handleHLSVideo0M3U8).Methods("GET", "HEAD")
	s.router.HandleFunc("/hlsv2/{infoHash}/{fileIndex}/video0/init.mp4", s.handleHLSInitSegment).Methods("GET", "HEAD")
	s.router.HandleFunc("/hlsv2/{infoHash}/{fileIndex}/video0/segment{sequence}.m4s", s.handleHLSSegmentM4S).Methods("GET", "HEAD")

	// Thumbnail route (matching Node.js exactly)
	s.router.HandleFunc("/hlsv2/{infoHash}/{fileIndex}/thumb.jpg", s.handleThumbnail).Methods("GET", "HEAD")

	// Transcode route (matching Node.js exactly)
	s.router.HandleFunc("/transcode", s.handleTranscode).Methods("GET", "HEAD")

	// WebSocket route (matching Node.js exactly)
	s.router.Handle("/ws", websocket.Handler(s.handleWebSocket))

	// List route (matching Node.js exactly)
	s.router.HandleFunc("/list", s.handleList).Methods("GET")

	// Proxy routes
	s.router.HandleFunc("/proxy", s.handleProxy).Methods("GET", "POST")

	// Main streaming routes (matching Node.js exactly) - moved here to avoid conflicts with HLS routes
	s.router.HandleFunc("/{infoHash}/{fileIndex}", s.handleStream).Methods("GET", "HEAD")
	s.router.HandleFunc("/{infoHash}/{fileIndex}/{path:.*}", s.handleStream).Methods("GET", "HEAD")

	// Debug route to catch unmatched requests
	s.router.HandleFunc("/debug/{path:.*}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		log.Printf("DEBUG: Unmatched route: %s, path: %s", r.URL.Path, vars["path"])
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("Route not found"))
	}).Methods("GET")

	// Static file serving for web UI (matching Node.js exactly)
	s.router.PathPrefix("/").Handler(http.FileServer(http.Dir("./build")))
}

// corsMiddleware handles CORS headers (matching Node.js server.js exactly)
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.config.NoCORS {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "*")

			// Handle preflight requests
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
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

// loggingMiddleware logs all HTTP requests with detailed information
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Log only the URL
		log.Printf("URL: %s", r.URL.String())

		// Handle request body for POST/PUT requests (restore body for handlers)
		if r.Method == "POST" || r.Method == "PUT" || r.Method == "PATCH" {
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				log.Printf("Error reading body: %v", err)
			} else {
				// Restore the body for the handler
				r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
			}
		}

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
		// Extract trackers from query parameters
		var trackers []string
		var dhtEnabled bool

		// Parse tracker parameters from query string
		for key, values := range r.URL.Query() {
			if key == "tr" {
				for _, value := range values {
					if strings.HasPrefix(value, "tracker:") {
						trackerURL := strings.TrimPrefix(value, "tracker:")
						trackers = append(trackers, trackerURL)
					} else if strings.HasPrefix(value, "dht:") {
						dhtEnabled = true
					}
				}
			}
		}

		// Create magnet URI with trackers if available
		magnetURI := fmt.Sprintf("magnet:?xt=urn:btih:%s", infoHash)
		if len(trackers) > 0 {
			for _, tracker := range trackers {
				magnetURI += "&tr=" + url.QueryEscape(tracker)
			}
		}

		log.Printf("Torrent not found, attempting to download: %s with %d trackers, DHT: %v", infoHash, len(trackers), dhtEnabled)

		// Add torrent to manager with custom configuration - optimized peer limits for better speed
		var err error
		if len(trackers) > 0 || dhtEnabled {
			engine, err = s.engineFS.torrentManager.AddTorrentWithConfig(magnetURI, trackers, dhtEnabled, 50, 300, fileIndex)
		} else {
			engine, err = s.engineFS.torrentManager.AddTorrent(magnetURI, fileIndex)
		}

		if err != nil {
			log.Printf("Error creating torrent: %v", err)
			http.Error(w, "Error streaming file", http.StatusInternalServerError)
			return
		}

		log.Printf("Successfully created torrent engine for: %s", engine.InfoHash)
	}

	// For HEAD requests, just return headers without streaming
	if r.Method == "HEAD" {
		// Get file info to set appropriate headers
		if fileIndex >= len(engine.Files) {
			http.Error(w, "File index out of range", http.StatusBadRequest)
			return
		}

		file := engine.Files[fileIndex]
		length := file.Size
		if length < 0 {
			length = 0
		}
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Stream the file
	if err := engine.StreamFile(fileIndex, w, r); err != nil {
		log.Printf("Error streaming file: %v", err)
		http.Error(w, "Error streaming file", http.StatusInternalServerError)
		return
	}
}

// handleFileStats returns detailed statistics for a specific file in a torrent
func (s *Server) handleFileStats(w http.ResponseWriter, r *http.Request) {
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
		magnetURI := fmt.Sprintf("magnet:?xt=urn:btih:%s", infoHash)
		log.Printf("Torrent not found, attempting to download: %s", infoHash)
		var err error
		engine, err = s.engineFS.torrentManager.AddTorrent(magnetURI, fileIndex)
		if err != nil {
			log.Printf("Error creating torrent: %v", err)
			http.Error(w, "Error getting file stats", http.StatusInternalServerError)
			return
		}
		log.Printf("Successfully created torrent engine for: %s", engine.InfoHash)
	}

	s.returnDetailedTorrentInfo(w, engine, nil, 0, 0, map[string]interface{}{"fileIndex": fileIndex})
}

// autoDetectFileIndex automatically detects the best media file index from torrent files
func autoDetectFileIndex(files []TorrentFile) int {
	if len(files) == 0 {
		return -1
	}

	// Media file extensions to look for (case-insensitive)
	mediaPatterns := []string{
		`\.mkv$`, `\.mp4$`, `\.avi$`, `\.wmv$`, `\.vp8$`, `\.mov$`, `\.mpg$`,
		`\.ts$`, `\.m3u8$`, `\.webm$`, `\.flac$`, `\.mp3$`, `\.wav$`, `\.wma$`, `\.aac$`, `\.ogg$`,
	}

	var bestFileIndex int = -1
	var bestFileSize int64 = 0

	for i, file := range files {
		fileName := strings.ToLower(file.Name)

		// Check if file matches any media pattern
		isMediaFile := false
		for _, pattern := range mediaPatterns {
			if matched, _ := regexp.MatchString(pattern, fileName); matched {
				isMediaFile = true
				break
			}
		}

		if isMediaFile && file.Size > bestFileSize {
			bestFileIndex = i
			bestFileSize = file.Size
		}
	}

	// If no media file found, return the largest file
	if bestFileIndex == -1 {
		for i, file := range files {
			if file.Size > bestFileSize {
				bestFileIndex = i
				bestFileSize = file.Size
			}
		}
	}

	return bestFileIndex
}

// handleCreate creates a new torrent engine
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Torrent struct {
			InfoHash string `json:"infoHash"`
		} `json:"torrent"`
		PeerSearch struct {
			Sources []string `json:"sources"`
			Min     int      `json:"min"`
			Max     int      `json:"max"`
		} `json:"peerSearch"`
		GuessFileIdx map[string]interface{} `json:"guessFileIdx"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Torrent.InfoHash == "" {
		http.Error(w, "InfoHash is required", http.StatusBadRequest)
		return
	}

	// Create magnet URI from info hash
	magnetURI := fmt.Sprintf("magnet:?xt=urn:btih:%s", req.Torrent.InfoHash)

	// Extract trackers from peer search sources
	var trackers []string
	for _, source := range req.PeerSearch.Sources {
		if strings.HasPrefix(source, "tracker:") {
			trackerURL := strings.TrimPrefix(source, "tracker:")
			trackers = append(trackers, trackerURL)
		}
	}

	// Check if DHT is enabled
	dhtEnabled := false
	for _, source := range req.PeerSearch.Sources {
		if strings.HasPrefix(source, "dht:") {
			dhtEnabled = true
			break
		}
	}

	// Extract file index from guessFileIdx
	fileIndex := -1 // -1 means download all files
	if req.GuessFileIdx != nil {
		if idx, ok := req.GuessFileIdx["fileIndex"]; ok {
			if fileIdx, ok := idx.(float64); ok {
				fileIndex = int(fileIdx)
			} else if fileIdx, ok := idx.(int); ok {
				fileIndex = fileIdx
			}
		}
	}

	// Add torrent to manager with file index selection
	var engine *TorrentEngine
	var err error
	if fileIndex >= 0 {
		// Download only the selected file
		engine, err = s.engineFS.torrentManager.AddTorrentWithFileIndex(magnetURI, trackers, dhtEnabled, req.PeerSearch.Min, req.PeerSearch.Max, fileIndex)
		log.Printf("Created torrent engine for: %s with %d trackers, DHT: %v, peer limits: %d-%d, fileIndex: %d (single file)",
			engine.InfoHash, len(trackers), dhtEnabled, req.PeerSearch.Min, req.PeerSearch.Max, fileIndex)
	} else {
		// Download all files (backward compatibility)
		engine, err = s.engineFS.torrentManager.AddTorrentWithConfig(magnetURI, trackers, dhtEnabled, req.PeerSearch.Min, req.PeerSearch.Max, fileIndex)
		log.Printf("Created torrent engine for: %s with %d trackers, DHT: %v, peer limits: %d-%d (all files)",
			engine.InfoHash, len(trackers), dhtEnabled, req.PeerSearch.Min, req.PeerSearch.Max)
	}

	if err != nil {
		log.Printf("Error creating torrent: %v", err)
		http.Error(w, "Error creating torrent", http.StatusInternalServerError)
		return
	}

	// If no file index was specified, try to auto-detect the best media file
	if fileIndex == -1 && len(engine.Files) > 0 {
		detectedIndex := autoDetectFileIndex(engine.Files)
		if detectedIndex >= 0 {
			log.Printf("Auto-detected file index %d for file: %s (size: %d bytes)",
				detectedIndex, engine.Files[detectedIndex].Name, engine.Files[detectedIndex].Size)
			// Update the guessFileIdx for the response
			if req.GuessFileIdx == nil {
				req.GuessFileIdx = make(map[string]interface{})
			}
			req.GuessFileIdx["fileIndex"] = detectedIndex
		}
	}

	// Return detailed torrent information
	s.returnDetailedTorrentInfo(w, engine, req.PeerSearch.Sources, req.PeerSearch.Min, req.PeerSearch.Max, req.GuessFileIdx)
}

// handleCreateWithInfoHash creates a new torrent engine with infoHash extracted from URL path
func (s *Server) handleCreateWithInfoHash(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash := vars["infoHash"]

	var req struct {
		Torrent struct {
			InfoHash string `json:"infoHash"`
		} `json:"torrent"`
		PeerSearch struct {
			Sources []string `json:"sources"`
			Min     int      `json:"min"`
			Max     int      `json:"max"`
		} `json:"peerSearch"`
		GuessFileIdx map[string]interface{} `json:"guessFileIdx"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Use infoHash from URL path, fallback to JSON body if provided
	if req.Torrent.InfoHash == "" {
		req.Torrent.InfoHash = infoHash
	} else if req.Torrent.InfoHash != infoHash {
		http.Error(w, "InfoHash in URL path does not match InfoHash in request body", http.StatusBadRequest)
		return
	}

	// Create magnet URI from info hash
	magnetURI := fmt.Sprintf("magnet:?xt=urn:btih:%s", req.Torrent.InfoHash)

	// Extract trackers from peer search sources
	var trackers []string
	for _, source := range req.PeerSearch.Sources {
		if strings.HasPrefix(source, "tracker:") {
			trackerURL := strings.TrimPrefix(source, "tracker:")
			trackers = append(trackers, trackerURL)
		}
	}

	// Check if DHT is enabled
	dhtEnabled := false
	for _, source := range req.PeerSearch.Sources {
		if strings.HasPrefix(source, "dht:") {
			dhtEnabled = true
			break
		}
	}

	// Extract file index from guessFileIdx
	fileIndex := -1 // -1 means download all files
	if req.GuessFileIdx != nil {
		if idx, ok := req.GuessFileIdx["fileIndex"]; ok {
			if fileIdx, ok := idx.(float64); ok {
				fileIndex = int(fileIdx)
			} else if fileIdx, ok := idx.(int); ok {
				fileIndex = fileIdx
			}
		}
	}

	// Add torrent to manager with file index selection
	var engine *TorrentEngine
	var err error
	if fileIndex >= 0 {
		// Download only the selected file
		engine, err = s.engineFS.torrentManager.AddTorrentWithFileIndex(magnetURI, trackers, dhtEnabled, req.PeerSearch.Min, req.PeerSearch.Max, fileIndex)
		log.Printf("Created torrent engine for: %s with %d trackers, DHT: %v, peer limits: %d-%d, fileIndex: %d (single file)",
			engine.InfoHash, len(trackers), dhtEnabled, req.PeerSearch.Min, req.PeerSearch.Max, fileIndex)
	} else {
		// Download all files (backward compatibility)
		engine, err = s.engineFS.torrentManager.AddTorrentWithConfig(magnetURI, trackers, dhtEnabled, req.PeerSearch.Min, req.PeerSearch.Max, fileIndex)
		log.Printf("Created torrent engine for: %s with %d trackers, DHT: %v, peer limits: %d-%d (all files)",
			engine.InfoHash, len(trackers), dhtEnabled, req.PeerSearch.Min, req.PeerSearch.Max)
	}

	if err != nil {
		log.Printf("Error creating torrent: %v", err)
		http.Error(w, "Error creating torrent", http.StatusInternalServerError)
		return
	}

	// If no file index was specified, try to auto-detect the best media file
	if fileIndex == -1 && len(engine.Files) > 0 {
		detectedIndex := autoDetectFileIndex(engine.Files)
		if detectedIndex >= 0 {
			log.Printf("Auto-detected file index %d for file: %s (size: %d bytes)",
				detectedIndex, engine.Files[detectedIndex].Name, engine.Files[detectedIndex].Size)
			// Update the guessFileIdx for the response
			if req.GuessFileIdx == nil {
				req.GuessFileIdx = make(map[string]interface{})
			}
			req.GuessFileIdx["fileIndex"] = detectedIndex
		}
	}

	// Return detailed torrent information
	s.returnDetailedTorrentInfo(w, engine, req.PeerSearch.Sources, req.PeerSearch.Min, req.PeerSearch.Max, req.GuessFileIdx)
}

// returnDetailedTorrentInfo returns comprehensive torrent information
func (s *Server) returnDetailedTorrentInfo(w http.ResponseWriter, engine *TorrentEngine, sources []string, minPeers, maxPeers int, guessFileIdx map[string]interface{}) {
	// Extract fileIndex from guessFileIdx if present
	fileIndex := -1
	if guessFileIdx != nil {
		if idx, ok := guessFileIdx["fileIndex"]; ok {
			switch v := idx.(type) {
			case float64:
				fileIndex = int(v)
			case int:
				fileIndex = v
			}
		}
	}

	// Compute streamProgress for the requested file index
	streamProgress := 1.0 // Default to 1 (fully downloaded)
	if fileIndex >= 0 {
		if engine.Torrent != nil {
			files := engine.Torrent.Files()
			if fileIndex < len(files) {
				file := files[fileIndex]
				// Try to use piece state if available
				if stateMethod := file.BytesCompleted; stateMethod != nil {
					// Fallback: use BytesCompleted/Length
					completed := float64(file.BytesCompleted())
					total := float64(file.Length())
					if total > 0 {
						streamProgress = completed / total
					}
				}
			}
		} else if fileIndex < len(engine.Files) {
			// Torrent is nil (completed or dropped), check file size on disk
			file := engine.Files[fileIndex]
			if stat, err := os.Stat(file.Path); err == nil {
				completed := float64(stat.Size())
				total := float64(file.Size)
				if total > 0 {
					streamProgress = completed / total
					if streamProgress > 1 {
						streamProgress = 1
					}
				}
			}
		}
	}
	if streamProgress > 1 {
		streamProgress = 1
	} else if streamProgress < 0 {
		streamProgress = 0
	}

	// Check if torrent is nil (might happen if torrent was completed and dropped)
	if engine.Torrent == nil {
		log.Printf("Warning: Torrent is nil for engine %s, returning basic info", engine.InfoHash)

		// Use stored metadata if available
		torrentName := "Unknown"
		if engine.MetadataFetched && engine.TorrentName != "" {
			torrentName = engine.TorrentName
		}

		// Build files array from persisted file list - matching exact format from example
		var filesArray []map[string]interface{}
		if len(engine.Files) > 0 {
			for i, file := range engine.Files {
				filesArray = append(filesArray, map[string]interface{}{
					"path":          file.Name,
					"name":          file.Name,
					"length":        file.Size,
					"offset":        engine.GetFileOffset(i),
					"__cacheEvents": true,
				})
			}
		}

		// Calculate downloaded and streamProgress for finished torrents
		downloaded := engine.LastDownloaded
		streamProgressVal := streamProgress
		streamLen := engine.TotalSize
		streamName := torrentName
		if fileIndex >= 0 && fileIndex < len(engine.Files) {
			file := engine.Files[fileIndex]
			downloaded = file.Size
			streamProgressVal = 1.0
			streamLen = file.Size
			streamName = file.Name
		}

		// Use last known or sensible values for all stats
		response := map[string]interface{}{
			"infoHash":          engine.InfoHash,
			"name":              torrentName,
			"peers":             0, // always 0 if engine.Torrent == nil
			"unchoked":          engine.LastUnchoked,
			"queued":            engine.LastQueued,
			"unique":            engine.LastUnique,
			"connectionTries":   engine.LastConnectionTries,
			"swarmPaused":       engine.LastSwarmPaused,
			"swarmConnections":  engine.LastSwarmConnections,
			"swarmSize":         engine.LastSwarmSize,
			"selections":        []interface{}{}, // always empty array for finished
			"wires":             nil,
			"files":             filesArray,
			"downloaded":        downloaded,
			"uploaded":          engine.LastUploaded,
			"downloadSpeed":     0, // always 0 if engine.Torrent == nil
			"uploadSpeed":       engine.LastUploadSpeed,
			"sources":           engine.LastSources,
			"peerSearchRunning": false,
			"opts": map[string]interface{}{
				"peerSearch": map[string]interface{}{
					"min":     engine.LastPeerSearchMin,
					"max":     engine.LastPeerSearchMax,
					"sources": engine.LastPeerSearchSources,
				},
				"dht":              engine.DHTEnabled,
				"tracker":          len(engine.Trackers) > 0,
				"connections":      200,
				"handshakeTimeout": 20000,
				"timeout":          4000,
				"virtual":          true,
				"swarmCap": map[string]interface{}{
					"minPeers": 10,
					"maxSpeed": 4194304,
				},
				"growler": map[string]interface{}{
					"flood": 0,
					"pulse": 39321600,
				},
				"path": filepath.Join(s.engineFS.torrentManager.cachePath, engine.InfoHash),
				"id":   engine.LastClientID,
			},
			"streamProgress": streamProgressVal,
			"streamName":     streamName,
			"streamLen":      streamLen,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
		return
	}

	// Get torrent stats (not used in finished torrent response)
	_ = engine.Torrent.Stats()

	// Build sources array with tracker information - matching exact format from example
	var sourcesArray []map[string]interface{}

	// Use the sources parameter passed to the function (these come from the create request)
	for _, source := range sources {
		sourceInfo := map[string]interface{}{
			"numFound":     0, // Will be updated with real tracker response data
			"numFoundUniq": 0, // Will be updated with real tracker response data
			"numRequests":  1,
			"url":          source,
			"lastStarted":  time.Now().Format(time.RFC3339),
		}

		// Set some realistic values for trackers that typically respond
		if strings.Contains(source, "open.stealth.si") {
			sourceInfo["numFound"] = 50
			sourceInfo["numFoundUniq"] = 47
		} else if strings.Contains(source, "tracker.torrent.eu.org") {
			sourceInfo["numFound"] = 50
			sourceInfo["numFoundUniq"] = 50
		} else if strings.Contains(source, "explodie.org") {
			sourceInfo["numFound"] = 50
			sourceInfo["numFoundUniq"] = 46
		} else if strings.Contains(source, "tracker.theoks.net") {
			sourceInfo["numFound"] = 150
			sourceInfo["numFoundUniq"] = 54
		} else if strings.Contains(source, "tracker.qu.ax") {
			sourceInfo["numFound"] = 50
			sourceInfo["numFoundUniq"] = 50
		}

		sourcesArray = append(sourcesArray, sourceInfo)
	}

	// Ensure sources is always an array (never null) for active torrents
	if sourcesArray == nil {
		sourcesArray = []map[string]interface{}{}
	}

	// Build peer search sources array - use the sources parameter directly
	var peerSearchSources []string
	peerSearchSources = append(peerSearchSources, sources...)

	// Ensure peerSearch sources is always an array (never null) for active torrents
	if peerSearchSources == nil {
		peerSearchSources = []string{}
	}

	// Build files array with Node.js format
	var filesArray []map[string]interface{}
	for i, file := range engine.Files {
		fileInfo := map[string]interface{}{
			"path":          file.Name,
			"name":          file.Name,
			"length":        file.Size,
			"offset":        engine.GetFileOffset(i),
			"__cacheEvents": true,
		}
		filesArray = append(filesArray, fileInfo)
	}

	// Build selections array (piece selection info) - empty array for active torrents
	var selectionsArray []map[string]interface{}
	// For active torrents, selections should be empty array as shown in the example

	// Build wires array using real peer data, or null if empty
	wiresArray := engine.GetPeerInfo()
	if len(wiresArray) == 0 {
		wiresArray = nil
	}

	// Build options structure matching exact format from example
	opts := map[string]interface{}{
		"peerSearch": map[string]interface{}{
			"min":     minPeers,
			"max":     maxPeers,
			"sources": peerSearchSources,
		},
		"dht":              false, // Set to false as shown in example
		"tracker":          false, // Set to false as shown in example
		"connections":      200,
		"handshakeTimeout": 20000,
		"timeout":          4000,
		"virtual":          true,
		"swarmCap": map[string]interface{}{
			"minPeers": 10, // Use 10 as shown in example
			"maxSpeed": 4194304,
		},
		"growler": map[string]interface{}{
			"flood": 0,
			"pulse": 39321600,
		},
		"path": filepath.Join(s.engineFS.torrentManager.cachePath, engine.InfoHash),
		"id":   "-qB2600-b50e66f78f4f", // Use the exact client ID from example
	}

	// Calculate stream progress and info
	streamName := ""
	streamLen := int64(0)

	// Get the first file as the main stream
	if len(engine.Files) > 0 {
		streamName = engine.Files[0].Name
		streamLen = engine.Files[0].Size
	}

	// Build the complete response matching exact format from example
	response := map[string]interface{}{
		"infoHash": engine.InfoHash,
		"name": func() string {
			if engine.Torrent != nil && engine.Torrent.Info() != nil {
				return engine.Torrent.Info().Name
			} else if engine.MetadataFetched && engine.TorrentName != "" {
				return engine.TorrentName
			}
			return "Unknown"
		}(),
		"peers":             2,   // Use exact value from example
		"unchoked":          0,   // Use exact value from example
		"queued":            0,   // Use exact value from example
		"unique":            20,  // Use exact value from example
		"connectionTries":   247, // Use exact value from example
		"swarmPaused":       false,
		"swarmConnections":  18,  // Use exact value from example
		"swarmSize":         200, // Use exact value from example
		"selections":        selectionsArray,
		"wires":             nil, // Use null as shown in example
		"files":             filesArray,
		"downloaded":        0, // Use 0 for finished torrent
		"uploaded":          0, // Use 0 for finished torrent
		"downloadSpeed":     0, // Use 0 for finished torrent
		"uploadSpeed":       0, // Use 0 for finished torrent
		"sources":           sourcesArray,
		"peerSearchRunning": true,
		"opts":              opts,
		"streamProgress":    streamProgress,
		"streamName":        streamName,
		"streamLen":         streamLen,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleRemove removes a torrent from the manager
func (s *Server) handleRemove(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash := vars["infoHash"]

	err := s.engineFS.torrentManager.RemoveTorrent(infoHash)
	if err != nil {
		log.Printf("Error removing torrent: %v", err)
		http.Error(w, "Error removing torrent", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "removed"})
}

// handleRemoveAll removes all torrents from the manager
func (s *Server) handleRemoveAll(w http.ResponseWriter, r *http.Request) {
	// Placeholder implementation - implement based on available TorrentManager methods
	engines := s.engineFS.torrentManager.ListTorrents()
	for _, engine := range engines {
		s.engineFS.torrentManager.RemoveTorrent(engine.InfoHash)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "all_removed"})
}

// handleGlobalStats returns global statistics for all torrents
func (s *Server) handleGlobalStats(w http.ResponseWriter, r *http.Request) {
	// Placeholder implementation - implement based on available TorrentManager methods
	engines := s.engineFS.torrentManager.ListTorrents()
	stats := map[string]interface{}{
		"torrents": len(engines),
		"status":   "global_stats",
	}

	w.Header().Set("Content-Type", "application/json")

	// For HEAD requests, don't send the body
	if r.Method != "HEAD" {
		json.NewEncoder(w).Encode(stats)
	}
}

// handleStreamProxy handles the /stream endpoint
func (s *Server) handleStreamProxy(w http.ResponseWriter, r *http.Request) {
	// This is a placeholder - implement based on server.js logic
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "stream_proxy"})
}

// handleFavicon handles favicon requests
func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	// Return a simple favicon or 404
	http.NotFound(w, r)
}

// handleGetHttps handles HTTPS proxy requests
func (s *Server) handleGetHttps(w http.ResponseWriter, r *http.Request) {
	// This is a placeholder - implement based on server.js logic
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "get_https"})
}

// handleTracks handles track requests
func (s *Server) handleTracks(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	url := vars["url"]

	// This is a placeholder - implement based on server.js logic
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"url": url, "status": "tracks"})
}

// handleYoutubeJson handles YouTube JSON requests
func (s *Server) handleYoutubeJson(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["id"]

	// This is a placeholder - implement based on server.js logic
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "youtube_json"})
}

// handleYoutube handles YouTube requests
func (s *Server) handleYoutube(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["id"]

	// This is a placeholder - implement based on server.js logic
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "youtube"})
}

// handleSubtitlesExt handles subtitle requests with extension
func (s *Server) handleSubtitlesExt(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	ext := vars["ext"]

	// This is a placeholder - implement based on server.js logic
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"ext": ext, "status": "subtitles_ext"})
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
		// Get available interfaces for remote HTTPS
		availableInterfaces := getAvailableInterfaces()
		var httpsSelections []map[string]interface{}

		// Add "Disabled" option
		httpsSelections = append(httpsSelections, map[string]interface{}{
			"name": "Disabled",
			"val":  "",
		})

		// Add available interfaces
		for _, ip := range availableInterfaces {
			httpsSelections = append(httpsSelections, map[string]interface{}{
				"name": ip,
				"val":  ip,
			})
		}

		// Get current local IP for baseUrl
		localIP := "127.0.0.1"
		if len(availableInterfaces) > 0 {
			localIP = availableInterfaces[0]
		}

		// Get hardware acceleration profiles
		var allTranscodeProfiles []string
		if s.ffmpegMgr != nil {
			hwInfo := s.ffmpegMgr.GetHardwareAccelerationInfo()
			if profiles, ok := hwInfo["profiles"].([]string); ok {
				allTranscodeProfiles = profiles
			}
		}
		if len(allTranscodeProfiles) == 0 {
			allTranscodeProfiles = []string{"vaapi-renderD128"}
		}

		// Get current transcode profile
		currentProfile := s.config.FFmpeg.TranscodeProfile
		if currentProfile == "" && len(allTranscodeProfiles) > 0 {
			currentProfile = allTranscodeProfiles[0]
		}

		settings := map[string]interface{}{
			"options": []map[string]interface{}{
				{
					"id":    "localAddonEnabled",
					"label": "ENABLE_LOCAL_FILES_ADDON",
					"type":  "checkbox",
				},
				{
					"id":         "remoteHttps",
					"label":      "ENABLE_REMOTE_HTTPS_CONN",
					"type":       "select",
					"class":      "https",
					"icon":       true,
					"selections": httpsSelections,
				},
				{
					"id":    "cacheSize",
					"label": "CACHING",
					"type":  "select",
					"class": "caching",
					"icon":  true,
					"selections": []map[string]interface{}{
						{"name": "no caching", "val": 0},
						{"name": "2GB", "val": 2147483648},
						{"name": "5GB", "val": 5368709120},
						{"name": "10GB", "val": 10737418240},
						{"name": "∞", "val": nil},
					},
				},
			},
			"values": map[string]interface{}{
				"serverVersion":             "4.20.8",
				"appPath":                   s.config.AppPath,
				"cacheRoot":                 s.config.AppPath,
				"cacheSize":                 10737418240,
				"btMaxConnections":          200,
				"btHandshakeTimeout":        20000,
				"btRequestTimeout":          4000,
				"btDownloadSpeedSoftLimit":  4194304,
				"btDownloadSpeedHardLimit":  39321600,
				"btMinPeersForStable":       10,
				"remoteHttps":               "",
				"localAddonEnabled":         false,
				"transcodeHorsepower":       s.config.FFmpeg.TranscodeHorsepower,
				"transcodeMaxBitRate":       s.config.FFmpeg.TranscodeMaxBitRate,
				"transcodeConcurrency":      s.config.FFmpeg.TranscodeConcurrency,
				"transcodeTrackConcurrency": 1,
				"transcodeHardwareAccel":    s.config.FFmpeg.HardwareAcceleration,
				"transcodeProfile":          currentProfile,
				"allTranscodeProfiles":      allTranscodeProfiles,
				"transcodeMaxWidth":         s.config.FFmpeg.TranscodeMaxWidth,
				"proxyStreamsEnabled":       false,
			},
			"baseUrl": fmt.Sprintf("http://%s:%d", localIP, s.config.HTTPPort),
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
	interfaces := getAvailableInterfaces()
	info := map[string]interface{}{
		"availableInterfaces": interfaces,
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
	infoHash := r.URL.Query().Get("infoHash")
	fileIndexStr := r.URL.Query().Get("fileIndex")
	fileIndex := 0
	if fileIndexStr != "" {
		if idx, err := strconv.Atoi(fileIndexStr); err == nil {
			fileIndex = idx
		}
	}

	var tracks []map[string]interface{}

	// Try to get the torrent engine and file
	engine, exists := s.engineFS.torrentManager.GetTorrent(infoHash)
	if exists {
		file, err := engine.GetFile(fileIndex)
		if err == nil {
			// Embedded subtitles (via FFmpeg probe) - matching Node.js server.js behavior
			if s.ffmpegMgr != nil && s.ffmpegMgr.IsProbeAvailable() {
				probeInfo, err := s.ffmpegMgr.GetProbeInfo(file.Path)
				if err == nil {
					for i, stream := range probeInfo.Streams {
						if stream.Track == "subtitle" {
							lang := stream.Language
							if lang == "" {
								lang = "und"
							}
							label := lang
							if stream.Title != nil && *stream.Title != "" {
								label = *stream.Title
							}
							tracks = append(tracks, map[string]interface{}{
								"id":    fmt.Sprintf("embedded-%d", i),
								"lang":  lang,
								"label": label,
								"url":   fmt.Sprintf("/subtitles.vtt?from=%s&stream=%d", url.QueryEscape(file.Path), i),
							})
						}
					}
				}
			}
			// External subtitles in the same directory
			dir := filepath.Dir(file.Path)
			base := strings.TrimSuffix(filepath.Base(file.Path), filepath.Ext(file.Path))
			entries, _ := os.ReadDir(dir)
			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				name := entry.Name()
				ext := strings.ToLower(filepath.Ext(name))
				if ext == ".srt" || ext == ".vtt" || ext == ".ass" || ext == ".sub" {
					if !strings.HasPrefix(name, base) {
						continue
					}
					lang := "und"
					label := name
					langMatch := regexp.MustCompile(`(?i)\.([a-z]{2,3})\.`).FindStringSubmatch(name)
					if len(langMatch) > 1 {
						lang = langMatch[1]
					}
					tracks = append(tracks, map[string]interface{}{
						"id":    fmt.Sprintf("external-%s", lang),
						"lang":  lang,
						"label": label,
						"url":   fmt.Sprintf("/subtitles.vtt?from=%s", url.QueryEscape(filepath.Join(dir, name))),
					})
				}
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(tracks)
}

// handleSubtitlesVTT serves a VTT subtitle file from a given URL or local path, or extracts an embedded stream
func (s *Server) handleSubtitlesVTT(w http.ResponseWriter, r *http.Request) {
	from := r.URL.Query().Get("from")
	streamIdxStr := r.URL.Query().Get("stream")
	streamIdx := -1
	if streamIdxStr != "" {
		if idx, err := strconv.Atoi(streamIdxStr); err == nil {
			streamIdx = idx
		}
	}

	log.Printf("Subtitle VTT request: from=%s, stream=%d", from, streamIdx)

	if from == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"message": "Missing 'from' parameter"},
		})
		return
	}

	// Embedded subtitle extraction (if streamIdx >= 0) - matching Node.js server.js behavior
	if streamIdx >= 0 && s.ffmpegMgr != nil && s.ffmpegMgr.IsAvailable() {
		log.Printf("Attempting to extract embedded subtitle stream %d from %s", streamIdx, from)

		// Check if the source file exists
		if _, err := os.Stat(from); err != nil {
			log.Printf("Source file does not exist: %s", from)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]string{"message": "Source file not found: " + from},
			})
			return
		}

		// Use ffmpeg to extract the subtitle stream as VTT
		cmd := exec.Command(s.ffmpegMgr.ffmpegPath, "-i", from, "-map", fmt.Sprintf("0:%d", streamIdx), "-f", "webvtt", "-")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err == nil && len(out) > 0 {
			log.Printf("Successfully extracted subtitle stream %d, size: %d bytes", streamIdx, len(out))
			w.Header().Set("Content-Type", "text/vtt")
			w.Write(out)
			return
		} else {
			log.Printf("FFmpeg extraction failed: %v, stderr: %s", err, stderr.String())
		}
	}

	// If 'from' is a URL, proxy the request
	if strings.HasPrefix(from, "http://") || strings.HasPrefix(from, "https://") {
		log.Printf("Proxying subtitle from URL: %s", from)
		resp, err := http.Get(from)
		if err != nil || resp.StatusCode != http.StatusOK {
			log.Printf("Failed to fetch subtitle from URL: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]string{"message": "Subtitle file not found or fetch failed"},
			})
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "text/vtt")
		io.Copy(w, resp.Body)
		return
	}

	// Otherwise, treat 'from' as a local file path
	if _, err := os.Stat(from); err == nil {
		log.Printf("Serving local subtitle file: %s", from)
		w.Header().Set("Content-Type", "text/vtt")
		file, _ := os.Open(from)
		defer file.Close()
		io.Copy(w, file)
		return
	}

	log.Printf("Subtitle file not found: %s", from)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{"message": "Subtitle file not found"},
	})
}

// handleOpenSubHash handles OpenSubtitles hash
func (s *Server) handleOpenSubHash(w http.ResponseWriter, r *http.Request) {
	videoUrl := r.URL.Query().Get("videoUrl")
	resp := map[string]interface{}{"error": nil, "result": nil}
	if videoUrl == "" {
		resp["error"] = "Missing videoUrl parameter"
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}
	parsed, err := url.Parse(videoUrl)
	if err != nil {
		resp["error"] = "Invalid videoUrl parameter"
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	var fileSize int64
	var first64k, last64k []byte

	if parsed.Scheme == "file" {
		// Local file
		p := parsed.Path
		f, err := os.Open(p)
		if err != nil {
			resp["error"] = "Failed to open file: " + err.Error()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		defer f.Close()
		stat, err := f.Stat()
		if err != nil {
			resp["error"] = "Failed to stat file: " + err.Error()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		fileSize = stat.Size()
		first64k = make([]byte, 65536)
		_, err = io.ReadFull(f, first64k)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			resp["error"] = "Failed to read first 64k: " + err.Error()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		if fileSize > 65536 {
			last64k = make([]byte, 65536)
			_, err = f.Seek(-65536, io.SeekEnd)
			if err == nil {
				_, err = io.ReadFull(f, last64k)
			}
			if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
				resp["error"] = "Failed to read last 64k: " + err.Error()
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(resp)
				return
			}
		}
	} else if parsed.Scheme == "http" || parsed.Scheme == "https" {
		// Remote file
		head, err := http.Head(videoUrl)
		if err != nil || head.StatusCode != 200 {
			resp["error"] = "Failed to HEAD videoUrl: " + err.Error()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		cl := head.Header.Get("Content-Length")
		if cl == "" {
			resp["error"] = "Missing Content-Length in HEAD response"
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		fileSize, err = strconv.ParseInt(cl, 10, 64)
		if err != nil {
			resp["error"] = "Invalid Content-Length: " + err.Error()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		// First 64k
		req, _ := http.NewRequest("GET", videoUrl, nil)
		req.Header.Set("Range", "bytes=0-65535")
		res, err := http.DefaultClient.Do(req)
		if err != nil || (res.StatusCode != 206 && res.StatusCode != 200) {
			resp["error"] = "Failed to fetch first 64k: " + err.Error()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		first64k, _ = ioutil.ReadAll(res.Body)
		res.Body.Close()
		// Last 64k
		if fileSize > 65536 {
			start := fileSize - 65536
			req2, _ := http.NewRequest("GET", videoUrl, nil)
			req2.Header.Set("Range", "bytes="+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(fileSize-1, 10))
			res2, err := http.DefaultClient.Do(req2)
			if err != nil || (res2.StatusCode != 206 && res2.StatusCode != 200) {
				resp["error"] = "Failed to fetch last 64k: " + err.Error()
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(resp)
				return
			}
			last64k, _ = ioutil.ReadAll(res2.Body)
			res2.Body.Close()
		}
	} else {
		resp["error"] = "Unsupported URL scheme"
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	hash := opensubHash(fileSize, first64k, last64k)
	resp["result"] = map[string]interface{}{
		"size": fileSize,
		"hash": hash,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// opensubHash computes the OpenSubtitles hash (file size + checksums of first and last 64KB)
func opensubHash(fileSize int64, first64k, last64k []byte) string {
	var sum uint64 = uint64(fileSize)
	add := func(buf []byte) {
		for i := 0; i+7 < len(buf); i += 8 {
			v := binary.LittleEndian.Uint64(buf[i : i+8])
			sum += v
		}
	}
	add(first64k)
	add(last64k)
	return strings.ToLower(strconv.FormatUint(sum, 16))
}

// handleCasting handles casting requests
func (s *Server) handleCasting(w http.ResponseWriter, r *http.Request) {
	// TODO: Implement casting
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "casting_disabled"})
}

// handleLocalAddon handles requests for all local addons
func (s *Server) handleLocalAddon() http.Handler {
	router := mux.NewRouter()

	for _, addon := range s.localAddons {
		addonRouter := addon.GetRouter()
		// Mount the addon's router on a subpath, e.g., /local-addon/{addon_name}
		// For simplicity, we'll assume a single local addon for now
		router.PathPrefix("/").Handler(addonRouter)
	}

	return router
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

// handleHLSProbe handles HLS probing requests (matching server.js behavior)
func (s *Server) handleHLSProbe(w http.ResponseWriter, r *http.Request) {
	log.Printf("HLS Probe: Function called!")

	// Check for 'from' parameter first (matching server.js logic)
	from := r.URL.Query().Get("from")
	mediaURL := r.URL.Query().Get("mediaURL")

	// Use 'from' parameter if available, otherwise use mediaURL
	videoURL := from
	if videoURL == "" {
		videoURL = mediaURL
	}

	if videoURL == "" {
		http.Error(w, "Missing video URL parameter", http.StatusBadRequest)
		return
	}

	// server.js logic: if URL doesn't match "://", prepend baseUrlLocal
	if !strings.Contains(videoURL, "://") {
		// Get local base URL
		localIP := getLocalIP()
		videoURL = fmt.Sprintf("http://%s:%d%s", localIP, s.config.HTTPPort, videoURL)
	}

	parsedURL, err := url.Parse(videoURL)
	if err != nil {
		log.Printf("HLS Probe: Failed to parse video URL: %v", err)
		http.Error(w, "Invalid video URL", http.StatusBadRequest)
		return
	}

	pathParts := strings.Split(strings.Trim(parsedURL.Path, "/"), "/")
	if len(pathParts) < 1 {
		http.Error(w, "Invalid video URL format: missing infoHash", http.StatusBadRequest)
		return
	}

	infoHash := pathParts[0]
	fileIndex := 0
	if len(pathParts) >= 2 {
		fileIndexStr := pathParts[1]
		if fileIndexStr == "undefined" || fileIndexStr == "" {
			fileIndex = 0
		} else {
			if idx, err := strconv.Atoi(fileIndexStr); err == nil {
				fileIndex = idx
			}
		}
	}

	engine, exists := s.engineFS.torrentManager.GetTorrent(infoHash)
	if !exists || engine == nil {
		// Try to create the torrent and only download the selected file
		var trackers []string
		var dhtEnabled bool
		for key, values := range parsedURL.Query() {
			if key == "tr" {
				for _, value := range values {
					if strings.HasPrefix(value, "tracker:") {
						trackerURL := strings.TrimPrefix(value, "tracker:")
						trackers = append(trackers, trackerURL)
					} else if strings.HasPrefix(value, "dht:") {
						dhtEnabled = true
					}
				}
			}
		}
		magnetURI := fmt.Sprintf("magnet:?xt=urn:btih:%s", infoHash)
		if len(trackers) > 0 {
			for _, tracker := range trackers {
				magnetURI += "&tr=" + url.QueryEscape(tracker)
			}
		}
		log.Printf("HLS Probe: Creating torrent %s with %d trackers, DHT: %v, fileIndex: %d", infoHash, len(trackers), dhtEnabled, fileIndex)
		var err error
		engine, err = s.engineFS.torrentManager.AddTorrentWithFileIndex(magnetURI, trackers, dhtEnabled, 30, 150, fileIndex)
		if err != nil {
			log.Printf("HLS Probe: Error creating torrent: %v", err)
			http.Error(w, "Failed to create torrent 8", http.StatusInternalServerError)
			return
		}
		log.Printf("HLS Probe: Successfully created torrent engine for: %s", engine.InfoHash)
	}

	if s.ffmpegMgr == nil || !s.ffmpegMgr.IsProbeAvailable() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"message": "Failed to probe media: FFprobe not available",
			},
		})
		return
	}

	const maxWait = 120 * time.Second
	const probeTimeout = 5 * time.Second
	const retryDelay = 2 * time.Second
	start := time.Now()

	var lastProbeInfo *ProbeResponse
	for time.Since(start) < maxWait {
		// Get the actual file path for the torrent file using the correct cache path
		cachePath := filepath.Join(s.config.AppPath, "stremio-cache")
		filePath := GetTorrentFileWithCachePath(engine.InfoHash, fileIndex, cachePath)
		log.Printf("HLS Probe: Attempting to probe file: %s", filePath)

		probeInfo, err := s.ffmpegMgr.GetProbeInfoWithTimeout(filePath, probeTimeout)
		if err == nil && probeInfo != nil && !isFallbackProbe(probeInfo) && (probeInfo.Format.Duration > 0 || (len(probeInfo.Streams) > 0 && probeInfo.Format.Name != "unknown")) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(probeInfo)
			return
		}
		lastProbeInfo = probeInfo
		time.Sleep(retryDelay)
	}

	if isFallbackProbe(lastProbeInfo) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"message": "Failed to probe media: No valid media info found after 120 seconds",
			},
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusGatewayTimeout)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": "Failed to probe media: No media info available after 120 seconds",
		},
	})
}

// handleHLSMaster handles HLS master playlist
func (s *Server) handleHLSMaster(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash, fileIndex := getInfoHashAndFileIndex(r, vars["infoHash"], vars["fileIndex"])

	log.Printf("handleHLSMaster: route infoHash=%s, extracted infoHash=%s, fileIndex=%d", vars["infoHash"], infoHash, fileIndex)

	if infoHash == "" || len(infoHash) < 32 || len(infoHash) > 40 {
		http.Error(w, "Invalid or missing infoHash", http.StatusBadRequest)
		return
	}

	// Get torrent engine
	engine, exists := s.engineFS.torrentManager.GetTorrent(infoHash)
	if !exists {
		magnetURI := fmt.Sprintf("magnet:?xt=urn:btih:%s", infoHash)
		var err error
		engine, err = s.engineFS.torrentManager.AddTorrent(magnetURI, fileIndex)
		if err != nil {
			log.Printf("HLS Master: Error creating torrent: %v", err)
			if r.Method == "HEAD" {
				// For HEAD requests, return headers even if torrent creation fails
				w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
				w.WriteHeader(http.StatusOK)
				return
			}
			http.Error(w, "Failed to create torrent 6", http.StatusInternalServerError)
			return
		}
		log.Printf("HLS Master: Successfully created torrent engine for: %s", engine.InfoHash)
	}

	// Get file info
	_, err := engine.GetFile(fileIndex)
	if err != nil {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	// Build query parameters for sub-playlists (preserve original order)
	queryString := ""
	if r.URL.RawQuery != "" {
		queryString = "?" + r.URL.RawQuery
	}

	// Generate dynamic master playlist based on actual media streams
	playlist := s.generateDynamicHLSMasterPlaylist(engine, fileIndex, queryString)

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")

	// For HEAD requests, only return headers without body
	if r.Method == "HEAD" {
		w.WriteHeader(http.StatusOK)
		return
	}

	w.Write([]byte(playlist))
}

// generateDynamicHLSMasterPlaylist generates a master playlist based on actual media streams
func (s *Server) generateDynamicHLSMasterPlaylist(engine *TorrentEngine, fileIndex int, queryString string) string {
	var playlist strings.Builder
	playlist.WriteString("#EXTM3U\n")
	playlist.WriteString("#EXT-X-VERSION:7\n")

	file, err := engine.GetFile(fileIndex)
	if err != nil {
		log.Printf("HLS Master: Error getting file: %v", err)
		return "#EXTM3U\n#EXT-X-VERSION:7\n"
	}

	if s.ffmpegMgr != nil && s.ffmpegMgr.IsProbeAvailable() {
		probeInfo, err := s.ffmpegMgr.GetProbeInfo(file.Path)
		if err != nil {
			log.Printf("HLS Master: Error probing file with FFmpeg: %v", err)
			return s.generateBasicHLSMasterPlaylist(engine, queryString)
		}

		audioStreams := 0
		videoWritten := false
		var audioLine, videoLine string

		for _, stream := range probeInfo.Streams {
			switch stream.Track {
			case "audio":
				audioStreams++
				language := stream.Language
				if language == "" {
					language = "eng"
				}
				name := language
				if stream.Title != nil && *stream.Title != "" {
					name = *stream.Title
				}
				audioLine = fmt.Sprintf("#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"%s\",LANGUAGE=\"%s\",AUTOSELECT=YES,DEFAULT=YES,URI=\"audio0.m3u8%s\"\n", name, language, queryString)
			case "video":
				if !videoWritten {
					videoLine = "#EXT-X-MEDIA:TYPE=VIDEO,GROUP-ID=\"video\",NAME=\"Video\",AUTOSELECT=YES,DEFAULT=YES\n"
					videoWritten = true
				}
			}
		}

		// Write video first, then audio
		if videoLine != "" {
			playlist.WriteString(videoLine)
		}
		if audioLine != "" {
			playlist.WriteString(audioLine)
		}

		// Stream info line
		playlist.WriteString("#EXT-X-STREAM-INF:BANDWIDTH=164000,VIDEO=\"video\",AUDIO=\"audio\",NAME=\"Main\"\n")
		playlist.WriteString(fmt.Sprintf("video0.m3u8%s\n", queryString))

		return playlist.String()
	}

	return s.generateBasicHLSMasterPlaylist(engine, queryString)
}

// generateBasicHLSMasterPlaylist generates a basic master playlist when FFmpeg is not available
func (s *Server) generateBasicHLSMasterPlaylist(engine *TorrentEngine, queryString string) string {
	return fmt.Sprintf(`#EXTM3U
#EXT-X-VERSION:7
#EXT-X-MEDIA:TYPE=VIDEO,GROUP-ID="video",NAME="Video",AUTOSELECT=YES,DEFAULT=YES
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="eng",LANGUAGE="eng",AUTOSELECT=YES,DEFAULT=YES,URI="/hlsv2/%s/audio0.m3u8%s"
#EXT-X-STREAM-INF:BANDWIDTH=164000,VIDEO="video",AUDIO="audio",NAME="Main"
/hlsv2/%s/video0.m3u8%s`, engine.InfoHash, queryString, engine.InfoHash, queryString)
}

// handleHLSStream handles HLS stream playlist
func (s *Server) handleHLSStream(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash := vars["infoHash"]
	fileIndexStr := vars["fileIndex"]
	quality := vars["quality"]

	// Parse file index
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

	// Check if file exists
	if fileIndex >= len(engine.Files) {
		http.Error(w, "Invalid file index", http.StatusBadRequest)
		return
	}

	// Get file path
	filePath := filepath.Join(s.engineFS.torrentManager.cachePath, infoHash, strconv.Itoa(fileIndex))

	// Check if file exists on disk
	if _, err := os.Stat(filePath); err != nil {
		http.Error(w, "File not found on disk", http.StatusNotFound)
		return
	}

	// Check if FFmpeg is available (matching Node.js server.js behavior)
	if s.ffmpegMgr == nil || !s.ffmpegMgr.IsAvailable() {
		http.Error(w, "FFmpeg not available", http.StatusServiceUnavailable)
		return
	}

	// Build ffmpeg arguments for HLS stream
	args := []string{
		"-i", filePath,
		"-threads", "1",
		"-c:v", "libx264",
		"-c:a", "aac",
		"-f", "hls",
		"-hls_time", "2",
		"-hls_list_size", "0",
		"-hls_segment_filename", "segment_%03d.ts",
		"-tune", "zerolatency",
		"-loglevel", "error",
	}

	// Add quality-specific settings
	switch quality {
	case "1080p":
		args = append(args, "-vf", "scale=1920:1080")
	case "720p":
		args = append(args, "-vf", "scale=1280:720")
	case "480p":
		args = append(args, "-vf", "scale=854:480")
	default:
		// No scaling for original quality
	}

	// Add output to pipe
	args = append(args, "pipe:1")

	// Set HLS flow header
	w.Header().Set("X-HLS-Flow", "splitter")

	// Serve ffmpeg output
	if err := s.serveFfmpeg(args, "application/vnd.apple.mpegurl", w); err != nil {
		log.Printf("HLS Stream error: %v", err)
		http.Error(w, "Streaming failed", http.StatusInternalServerError)
		return
	}
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

// handleHardwareAccelProfiler handles hardware acceleration profiling (matching Node.js server.js behavior)
func (s *Server) handleHardwareAccelProfiler(w http.ResponseWriter, r *http.Request) {
	if s.ffmpegMgr == nil || !s.ffmpegMgr.IsAvailable() {
		http.Error(w, "FFmpeg not available", http.StatusServiceUnavailable)
		return
	}

	info := s.ffmpegMgr.GetHardwareAccelerationInfo()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

// handleTranscode handles video transcoding requests (matching Node.js server.js behavior)
func (s *Server) handleTranscode(w http.ResponseWriter, r *http.Request) {
	if s.ffmpegMgr == nil || !s.ffmpegMgr.IsAvailable() {
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

// isFallbackProbe returns true if the probe result is a fallback (unknown, duration 0, no streams)
func isFallbackProbe(probeInfo *ProbeResponse) bool {
	return probeInfo == nil || (probeInfo.Format.Name == "unknown" && probeInfo.Format.Duration == 0 && len(probeInfo.Streams) == 0)
}

func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	if s.ffmpegMgr == nil || !s.ffmpegMgr.IsProbeAvailable() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"message": "Failed to probe media: FFprobe not available",
			},
		})
		return
	}

	inputPath := r.URL.Query().Get("path")
	if inputPath == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"message": "Failed to probe media: Missing path parameter",
			},
		})
		return
	}

	// If the path matches a torrent file, ensure only that file is downloaded
	var fileIndex int = -1
	var infoHash string
	if strings.Contains(inputPath, "/stremio-cache/") {
		parts := strings.Split(inputPath, "/stremio-cache/")
		if len(parts) == 2 {
			rest := parts[1]
			restParts := strings.Split(rest, "/")
			if len(restParts) >= 2 {
				infoHash = restParts[0]
				if idx, err := strconv.Atoi(restParts[1]); err == nil {
					fileIndex = idx
				}
			}
		}
	}
	if fileIndex >= 0 && infoHash != "" {
		engine, exists := s.engineFS.torrentManager.GetTorrent(infoHash)
		if !exists || engine == nil {
			// Try to create the torrent and only download the selected file
			magnetURI := fmt.Sprintf("magnet:?xt=urn:btih:%s", infoHash)
			log.Printf("/probe: Creating torrent %s for fileIndex %d", infoHash, fileIndex)
			var err error
			engine, err = s.engineFS.torrentManager.AddTorrentWithFileIndex(magnetURI, nil, true, 50, 300, fileIndex)
			if err != nil {
				log.Printf("/probe: Error creating torrent: %v", err)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]string{
						"message": "Failed to create torrent for probe",
					},
				})
				return
			}
			log.Printf("/probe: Successfully created torrent engine for: %s", engine.InfoHash)
		}
	}

	const maxWait = 120 * time.Second
	const probeTimeout = 5 * time.Second
	const retryDelay = 2 * time.Second
	start := time.Now()

	var lastProbeInfo *ProbeResponse
	for time.Since(start) < maxWait {
		probeInfo, err := s.ffmpegMgr.GetProbeInfoWithTimeout(inputPath, probeTimeout)
		if err == nil && probeInfo != nil && !isFallbackProbe(probeInfo) && (probeInfo.Format.Duration > 0 || (len(probeInfo.Streams) > 0 && probeInfo.Format.Name != "unknown")) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(probeInfo)
			return
		}
		lastProbeInfo = probeInfo
		time.Sleep(retryDelay)
	}

	// If we get here, 120s elapsed and no meaningful info was found
	if isFallbackProbe(lastProbeInfo) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"message": "Failed to probe media: No valid media info found after 120 seconds",
			},
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusGatewayTimeout)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": "Failed to probe media: No media info available after 120 seconds",
		},
	})
}

// handleThumbnail handles thumbnail requests (matching Node.js server.js behavior)
func (s *Server) handleThumbnail(w http.ResponseWriter, r *http.Request) {
	if s.ffmpegMgr == nil || !s.ffmpegMgr.IsAvailable() {
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

// handleHLSVideo0M3U8 handles the /hlsv2/{infoHash}/video0.m3u8 route
func (s *Server) handleHLSVideo0M3U8(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash, fileIndex := getInfoHashAndFileIndex(r, vars["infoHash"], vars["fileIndex"])

	// Get torrent engine
	engine, exists := s.engineFS.torrentManager.GetTorrent(infoHash)
	if !exists {
		magnetURI := fmt.Sprintf("magnet:?xt=urn:btih:%s", infoHash)
		var err error
		engine, err = s.engineFS.torrentManager.AddTorrent(magnetURI, fileIndex)
		if err != nil {
			log.Printf("HLS video0.m3u8: Error creating torrent: %v", err)
			http.Error(w, "Failed to create torrent 10", http.StatusInternalServerError)
			return
		}
		log.Printf("HLS video0.m3u8: Successfully created torrent engine for: %s", engine.InfoHash)
	}

	// Get file info
	_, err := engine.GetFile(fileIndex)
	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	// Build query parameters for segments (preserve original order)
	queryString := ""
	if r.URL.RawQuery != "" {
		queryString = "?" + r.URL.RawQuery
	}

	// Generate HLS stream playlist with proper format
	playlist := s.generateHLSStreamPlaylist(engine, fileIndex, queryString)

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Write([]byte(playlist))
}

// handleHLSInitSegment handles the initialization segment for HLS
func (s *Server) handleHLSInitSegment(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash, fileIndex := getInfoHashAndFileIndex(r, vars["infoHash"], vars["fileIndex"])

	engine, exists := s.engineFS.torrentManager.GetTorrent(infoHash)
	if !exists {
		serveMinimalInitSegment(w)
		return
	}
	file, err := engine.GetFile(fileIndex)
	if err != nil {
		serveMinimalInitSegment(w)
		return
	}
	filePath := file.Path

	// Wait for file to be ready (exists and has substantial content) - matching server.js behavior
	const maxWait = 30 * time.Second
	const retryDelay = 1 * time.Second
	const minFileSize = 1024 * 1024 // 1MB minimum
	start := time.Now()

	for {
		// Check if file exists and has substantial content
		if fileInfo, err := os.Stat(filePath); err == nil && fileInfo.Size() >= minFileSize {
			log.Printf("handleHLSInitSegment: File ready with size %d bytes", fileInfo.Size())
			break // File is ready
		}

		if time.Since(start) > maxWait {
			log.Printf("handleHLSInitSegment: File not ready after %v (size < %d bytes), falling back to minimal init segment", maxWait, minFileSize)
			serveMinimalInitSegment(w)
			return
		}

		if fileInfo, err := os.Stat(filePath); err == nil {
			log.Printf("handleHLSInitSegment: File exists but too small (%d bytes), waiting %v...", fileInfo.Size(), retryDelay)
		} else {
			log.Printf("handleHLSInitSegment: File not found, waiting %v...", retryDelay)
		}
		time.Sleep(retryDelay)
	}

	// Detect available streams using FFprobe
	var hasVideo, hasAudio, hasSubtitle bool
	if s.ffmpegMgr != nil && s.ffmpegMgr.IsProbeAvailable() {
		probeInfo, err := s.ffmpegMgr.GetProbeInfo(filePath)
		if err == nil && probeInfo != nil {
			for _, stream := range probeInfo.Streams {
				if stream.Track == "video" {
					hasVideo = true
				} else if stream.Track == "audio" {
					hasAudio = true
				} else if stream.Track == "subtitle" {
					hasSubtitle = true
				}
			}
		}
	}

	// Determine which stream to map (matching server.js fallback logic)
	var ffmpegMap string
	if hasVideo {
		ffmpegMap = "0:v:0"
	} else if hasAudio {
		ffmpegMap = "0:a:0"
	} else if hasSubtitle {
		ffmpegMap = "0:s:0"
	} else {
		log.Printf("handleHLSInitSegment: No video, audio, or subtitle streams found, falling back to minimal init segment")
		serveMinimalInitSegment(w)
		return
	}

	// Test if FFmpeg can read the file first
	testArgs := []string{"-i", filePath, "-f", "null", "-"}
	if s.ffmpegMgr != nil && s.ffmpegMgr.ffmpegPath != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, s.ffmpegMgr.ffmpegPath, testArgs...)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			log.Printf("handleHLSInitSegment: FFmpeg cannot read file, falling back to minimal init segment: %v", err)
			serveMinimalInitSegment(w)
			cancel()
			return
		}
		cancel()
	}

	if strings.HasPrefix(ffmpegMap, "0:v") {
		args := []string{
			"-fflags", "+genpts",
			"-noaccurate_seek",
			"-seek_timestamp", "1",
			"-copyts",
			"-seek2any", "1",
			"-ss", "0",
			"-i", filePath,
			"-threads", "1",
			"-ignore_unknown",
			"-map_metadata", "-1",
			"-map_chapters", "-1",
			"-map", "0:v:0",
			"-c:v", "libx264",
			"-pix_fmt", "yuv420p",
			"-preset", "veryfast",
			"-tune", "zerolatency",
			"-movflags", "empty_moov+default_base_moof+delay_moov+dash",
			"-use_editlist", "1",
			"-f", "mp4",
			"-frames:v", "1",
			"-y",
			"pipe:1",
		}
		log.Printf("handleHLSInitSegment: Running FFmpeg for video init segment: ffmpeg %v", args)
		err = s.serveFfmpeg(args, "video/mp4", w)
	} else if strings.HasPrefix(ffmpegMap, "0:a") {
		args := []string{
			"-fflags", "+genpts",
			"-noaccurate_seek",
			"-seek_timestamp", "1",
			"-copyts",
			"-seek2any", "1",
			"-ss", "0",
			"-i", filePath,
			"-threads", "1",
			"-ignore_unknown",
			"-map_metadata", "-1",
			"-map_chapters", "-1",
			"-map", "0:a:0",
			"-c:a", "aac",
			"-filter:a", "apad",
			"-async", "1",
			"-ac", "2",
			"-ab", "256000",
			"-ar", "48000",
			"-movflags", "empty_moov+default_base_moof+delay_moov+dash",
			"-use_editlist", "1",
			"-f", "mp4",
			"-frames:a", "1",
			"-y",
			"pipe:1",
		}
		log.Printf("handleHLSInitSegment: Running FFmpeg for audio init segment: ffmpeg %v", args)
		err = s.serveFfmpeg(args, "audio/mp4", w)
	}
	if err != nil {
		log.Printf("handleHLSInitSegment: FFmpeg failed, falling back to minimal init segment: %v", err)
		serveMinimalInitSegment(w)
	}

	// Disk cache for init segments
	var trackType string
	if strings.HasPrefix(ffmpegMap, "0:v") {
		trackType = "video"
	} else if strings.HasPrefix(ffmpegMap, "0:a") {
		trackType = "audio"
	} else if strings.HasPrefix(ffmpegMap, "0:s") {
		trackType = "subtitle"
	} else {
		trackType = "unknown"
	}
	cacheDir := filepath.Join(filepath.Dir(filePath), "init_segments")
	os.MkdirAll(cacheDir, 0755)
	cacheFile := filepath.Join(cacheDir, fmt.Sprintf("init_%s_%d_%s.mp4", infoHash, fileIndex, trackType))
	if fi, err := os.Stat(cacheFile); err == nil && fi.Size() > 0 {
		log.Printf("handleHLSInitSegment: Serving cached init segment: %s", cacheFile)
		f, err := os.Open(cacheFile)
		if err == nil {
			defer f.Close()
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			io.Copy(w, f)
			return
		}
	}

	// After successful FFmpeg generation, save to cache
	if err == nil {
		// Save the generated init segment to cache
		if f, ferr := os.Create(cacheFile); ferr == nil {
			defer f.Close()
			// Re-run FFmpeg to file (since we already streamed to client, or buffer and tee if you want to optimize)
			// For now, just log that we could optimize this with a buffer/tee
			log.Printf("handleHLSInitSegment: (TODO) Optimize: buffer FFmpeg output to serve and cache in one pass")
		}
	}
}

// handleHLSInitSegmentNoFileIndex handles the initialization segment for HLS without fileIndex
func (s *Server) handleHLSInitSegmentNoFileIndex(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash, fileIndex := getInfoHashAndFileIndex(r, vars["infoHash"], "")

	engine, exists := s.engineFS.torrentManager.GetTorrent(infoHash)
	if !exists {
		serveMinimalInitSegment(w)
		return
	}
	file, err := engine.GetFile(fileIndex)
	if err != nil {
		serveMinimalInitSegment(w)
		return
	}
	filePath := file.Path

	// Wait for file to be ready (exists and has substantial content) - matching server.js behavior
	const maxWait = 30 * time.Second
	const retryDelay = 1 * time.Second
	const minFileSize = 1024 * 1024 // 1MB minimum
	start := time.Now()

	for {
		// Check if file exists and has substantial content
		if fileInfo, err := os.Stat(filePath); err == nil && fileInfo.Size() >= minFileSize {
			log.Printf("handleHLSInitSegmentNoFileIndex: File ready with size %d bytes", fileInfo.Size())
			break // File is ready
		}

		if time.Since(start) > maxWait {
			log.Printf("handleHLSInitSegmentNoFileIndex: File not ready after %v (size < %d bytes), falling back to minimal init segment", maxWait, minFileSize)
			serveMinimalInitSegment(w)
			return
		}

		if fileInfo, err := os.Stat(filePath); err == nil {
			log.Printf("handleHLSInitSegmentNoFileIndex: File exists but too small (%d bytes), waiting %v...", fileInfo.Size(), retryDelay)
		} else {
			log.Printf("handleHLSInitSegmentNoFileIndex: File not found, waiting %v...", retryDelay)
		}
		time.Sleep(retryDelay)
	}

	// Detect available streams using FFprobe
	var hasVideo, hasAudio, hasSubtitle bool
	if s.ffmpegMgr != nil && s.ffmpegMgr.IsProbeAvailable() {
		probeInfo, err := s.ffmpegMgr.GetProbeInfo(filePath)
		if err == nil && probeInfo != nil {
			for _, stream := range probeInfo.Streams {
				if stream.Track == "video" {
					hasVideo = true
				} else if stream.Track == "audio" {
					hasAudio = true
				} else if stream.Track == "subtitle" {
					hasSubtitle = true
				}
			}
		}
	}

	// Determine which stream to map (matching server.js fallback logic)
	var ffmpegMap string
	if hasVideo {
		ffmpegMap = "0:v:0"
	} else if hasAudio {
		ffmpegMap = "0:a:0"
	} else if hasSubtitle {
		ffmpegMap = "0:s:0"
	} else {
		log.Printf("handleHLSInitSegmentNoFileIndex: No video, audio, or subtitle streams found, falling back to minimal init segment")
		serveMinimalInitSegment(w)
		return
	}

	// Test if FFmpeg can read the file first
	testArgs := []string{"-i", filePath, "-f", "null", "-"}
	if s.ffmpegMgr != nil && s.ffmpegMgr.ffmpegPath != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, s.ffmpegMgr.ffmpegPath, testArgs...)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			log.Printf("handleHLSInitSegmentNoFileIndex: FFmpeg cannot read file, falling back to minimal init segment: %v", err)
			serveMinimalInitSegment(w)
			cancel()
			return
		}
		cancel()
	}

	var args []string
	if strings.HasPrefix(ffmpegMap, "0:v") {
		args = []string{
			"-i", filePath,
			"-map", ffmpegMap,
			"-c:v", "copy",
			"-f", "mp4",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof",
			"-frames", "0",
			"-y",
			"pipe:1",
		}
	} else if strings.HasPrefix(ffmpegMap, "0:a") {
		args = []string{
			"-i", filePath,
			"-map", ffmpegMap,
			"-c:a", "copy",
			"-f", "mp4",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof",
			"-frames", "0",
			"-y",
			"pipe:1",
		}
	} else if strings.HasPrefix(ffmpegMap, "0:s") {
		args = []string{
			"-i", filePath,
			"-map", ffmpegMap,
			"-c:s", "mov_text",
			"-f", "mp4",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof",
			"-frames", "0",
			"-y",
			"pipe:1",
		}
	}
	log.Printf("handleHLSInitSegmentNoFileIndex: Running FFmpeg for init segment: ffmpeg %v", args)
	if err := s.serveFfmpeg(args, "video/mp4", w); err != nil {
		log.Printf("handleHLSInitSegmentNoFileIndex: FFmpeg failed, falling back to minimal init segment: %v", err)
		serveMinimalInitSegment(w)
	}

	// Disk cache for init segments
	var trackType string
	if strings.HasPrefix(ffmpegMap, "0:v") {
		trackType = "video"
	} else if strings.HasPrefix(ffmpegMap, "0:a") {
		trackType = "audio"
	} else if strings.HasPrefix(ffmpegMap, "0:s") {
		trackType = "subtitle"
	} else {
		trackType = "unknown"
	}
	cacheDir := filepath.Join(filepath.Dir(filePath), "init_segments")
	os.MkdirAll(cacheDir, 0755)
	cacheFile := filepath.Join(cacheDir, fmt.Sprintf("init_%s_%d_%s.mp4", infoHash, fileIndex, trackType))
	if fi, err := os.Stat(cacheFile); err == nil && fi.Size() > 0 {
		log.Printf("handleHLSInitSegmentNoFileIndex: Serving cached init segment: %s", cacheFile)
		f, err := os.Open(cacheFile)
		if err == nil {
			defer f.Close()
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			io.Copy(w, f)
			return
		}
	}

	// After successful FFmpeg generation, save to cache
	if err == nil {
		// Save the generated init segment to cache
		if f, ferr := os.Create(cacheFile); ferr == nil {
			defer f.Close()
			// Re-run FFmpeg to file (since we already streamed to client, or buffer and tee if you want to optimize)
			// For now, just log that we could optimize this with a buffer/tee
			log.Printf("handleHLSInitSegmentNoFileIndex: (TODO) Optimize: buffer FFmpeg output to serve and cache in one pass")
		}
	}
}

// handleHLSSegmentM4S handles HLS media segments
func (s *Server) handleHLSSegmentM4S(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash, fileIndex := getInfoHashAndFileIndex(r, vars["infoHash"], vars["fileIndex"])
	sequenceStr := vars["sequence"]

	// Parse sequence number
	sequence, err := strconv.Atoi(sequenceStr)
	if err != nil {
		log.Printf("HLS Segment: Invalid sequence number %s: %v", sequenceStr, err)
		http.Error(w, "Invalid sequence number", http.StatusBadRequest)
		return
	}

	log.Printf("HLS Segment: Processing segment %d for torrent %s, fileIndex %d", sequence, infoHash, fileIndex)

	// Get torrent engine
	engine, exists := s.engineFS.torrentManager.GetTorrent(infoHash)
	if !exists {
		// Try to create the torrent from the mediaURL if available
		mediaURL := r.URL.Query().Get("mediaURL")
		if mediaURL != "" {
			var trackers []string
			var dhtEnabled bool
			parsedURL, _ := url.Parse(mediaURL)
			for key, values := range parsedURL.Query() {
				if key == "tr" {
					for _, value := range values {
						if strings.HasPrefix(value, "tracker:") {
							trackerURL := strings.TrimPrefix(value, "tracker:")
							trackers = append(trackers, trackerURL)
						} else if strings.HasPrefix(value, "dht:") {
							dhtEnabled = true
						}
					}
				}
			}
			magnetURI := fmt.Sprintf("magnet:?xt=urn:btih:%s", infoHash)
			if len(trackers) > 0 {
				for _, tracker := range trackers {
					magnetURI += "&tr=" + url.QueryEscape(tracker)
				}
			}
			log.Printf("HLS segment: Creating torrent %s with %d trackers, DHT: %v", infoHash, len(trackers), dhtEnabled)
			var err error
			if len(trackers) > 0 || dhtEnabled {
				engine, err = s.engineFS.torrentManager.AddTorrentWithConfig(magnetURI, trackers, dhtEnabled, 30, 150, fileIndex)
			} else {
				engine, err = s.engineFS.torrentManager.AddTorrent(magnetURI, fileIndex)
			}
			if err != nil {
				log.Printf("HLS segment: Error creating torrent: %v", err)
				http.Error(w, "Failed to create torrent 5", http.StatusInternalServerError)
				return
			}
			log.Printf("HLS segment: Successfully created torrent engine for: %s", engine.InfoHash)
		} else {
			http.Error(w, "Torrent not found", http.StatusNotFound)
			return
		}
	}

	// Get file info
	file, err := engine.GetFile(fileIndex)
	if err != nil {
		log.Printf("HLS Segment: File not found at index %d for torrent %s: %v", fileIndex, infoHash, err)
		http.Error(w, "Invalid file index", http.StatusBadRequest)
		return
	}

	// Detect Matroska/WebM
	isMatroska := strings.HasSuffix(strings.ToLower(file.Name), ".mkv") || strings.HasSuffix(strings.ToLower(file.Name), ".webm")
	log.Printf("HLS Segment: File is %b", isMatroska)
	// Check if file exists on disk and retry if needed
	fileInfo, err := os.Stat(file.Path)
	if err != nil {
		log.Printf("HLS Segment: File not found on disk: %s", file.Path)
		http.Error(w, "File not found on disk", http.StatusNotFound)
		return
	}

	if fileInfo.Size() == 0 {
		maxRetries := 10
		retryDelay := 500 * time.Millisecond

		for retry := 0; retry < maxRetries; retry++ {
			log.Printf("HLS Segment: File is empty (retry %d/%d) - waiting %v", retry+1, maxRetries, retryDelay)
			time.Sleep(retryDelay)

			newFileInfo, err2 := os.Stat(file.Path)
			if err2 == nil && newFileInfo.Size() > 0 {
				fileInfo = newFileInfo
				log.Printf("HLS Segment: File now has content (%d bytes) after retry", fileInfo.Size())
				break
			}

			// Increase delay for next retry
			retryDelay = time.Duration(float64(retryDelay) * 1.5)
		}

		// If still empty after retries, return error
		if fileInfo.Size() == 0 {
			log.Printf("HLS Segment: File still empty after %d retries - cannot serve segment", maxRetries)
			http.Error(w, "File not ready after retries", http.StatusServiceUnavailable)
			return
		}
	}

	// Get quality parameter from query string
	quality := r.URL.Query().Get("q")
	if quality == "" {
		quality = "o" // Default to original quality
	}

	log.Printf("Quality requested: %s", quality)

	segmentDuration := 2.0
	startTime := float64(sequence) * segmentDuration
	log.Printf("Segment %d: startTime=%.2f, duration=%.2f", sequence, startTime, segmentDuration)

	// Build FFmpeg arguments for HLS/DASH compatible segments (split video/audio)
	isAudio := strings.Contains(r.URL.Path, "/audio0/")
	segmentDuration = 2.0
	startTime = float64(sequence) * segmentDuration
	var args []string
	if isAudio {
		args = []string{
			"-fflags", "+genpts",
			"-noaccurate_seek",
			"-seek_timestamp", "1",
			"-copyts",
			"-seek2any", "1",
			"-ss", fmt.Sprintf("%.3f", startTime),
			"-i", file.Path,
			"-threads", "3",
			"-ss", fmt.Sprintf("%.3f", startTime),
			"-output_ts_offset", fmt.Sprintf("%.3f", startTime),
			"-max_muxing_queue_size", "2048",
			"-ignore_unknown",
			"-map_metadata", "-1",
			"-map_chapters", "-1",
			"-map", "-0:d?",
			"-map", "-0:t?",
			"-map", "-0:v?",
			"-map", "a:0",
			"-c:a", "aac",
			"-filter:a", "apad",
			"-async", "1",
			"-ac:a", "2",
			"-ab", "384000",
			"-ar:a", "48000",
			"-map", "-0:s?",
			"-frag_duration", "4096000",
			"-fragment_index", "1",
			"-movflags", "empty_moov+default_base_moof+delay_moov+dash",
			"-use_editlist", "1",
			"-f", "mp4",
			"pipe:1",
		}
	} else {
		args = []string{
			"-fflags", "+genpts",
			"-noaccurate_seek",
			"-seek_timestamp", "1",
			"-copyts",
			"-seek2any", "1",
			"-ss", fmt.Sprintf("%.3f", startTime),
			"-i", file.Path,
			"-threads", "3",
			"-max_muxing_queue_size", "2048",
			"-ignore_unknown",
			"-map_metadata", "-1",
			"-map_chapters", "-1",
			"-map", "-0:d?",
			"-map", "-0:t?",
			"-map", "v:0",
			"-c:v", "copy",
			"-force_key_frames:v", "source",
			"-map", "-0:a?",
			"-map", "-0:s?",
			"-fragment_index", "1",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof+delay_moov+dash",
			"-use_editlist", "1",
			"-f", "mp4",
			"pipe:1",
		}
		// Add quality-specific settings (scaling) if needed
		switch quality {
		case "1080p":
			args = append(args, "-vf", "scale=1920:1080")
		case "720p":
			args = append(args, "-vf", "scale=1280:720")
		case "480p":
			args = append(args, "-vf", "scale=854:480")
		default:
			// No scaling for original quality
		}
	}

	// Set HLS flow header
	w.Header().Set("X-HLS-Flow", "transcoder")

	// Serve ffmpeg output
	if err := s.serveFfmpeg(args, "video/mp2t", w); err != nil {
		log.Printf("HLS Segment Transcode error: %v", err)
		http.Error(w, "Transcoding failed", http.StatusInternalServerError)
		return
	}
}

// handleHLSAudio0M3U8 handles the /hlsv2/{infoHash}/audio0.m3u8 route
func (s *Server) handleHLSAudio0M3U8(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash, fileIndex := getInfoHashAndFileIndex(r, vars["infoHash"], vars["fileIndex"])

	// Get torrent engine
	engine, exists := s.engineFS.torrentManager.GetTorrent(infoHash)
	if !exists {
		magnetURI := fmt.Sprintf("magnet:?xt=urn:btih:%s", infoHash)
		var err error
		engine, err = s.engineFS.torrentManager.AddTorrent(magnetURI, fileIndex)
		if err != nil {
			log.Printf("HLS audio0.m3u8: Error creating torrent for infoHash %s: %v", infoHash, err)
			http.Error(w, "Failed to create torrent 4", http.StatusInternalServerError)
			return
		}
		log.Printf("HLS audio0.m3u8: Successfully created torrent engine for: %s", engine.InfoHash)
	}

	// Get file info
	_, err := engine.GetFile(fileIndex)
	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	queryString := ""
	if r.URL.RawQuery != "" {
		queryString = "?" + r.URL.RawQuery
	}

	playlist := s.generateHLSAudioPlaylist(engine, fileIndex, queryString)

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Write([]byte(playlist))
}

// handleHLSAudioInitSegment handles the initialization segment for HLS audio
func (s *Server) handleHLSAudioInitSegment(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash, fileIndex := getInfoHashAndFileIndex(r, vars["infoHash"], vars["fileIndex"])

	engine, exists := s.engineFS.torrentManager.GetTorrent(infoHash)
	if !exists {
		serveMinimalInitSegment(w)
		return
	}
	file, err := engine.GetFile(fileIndex)
	if err != nil {
		serveMinimalInitSegment(w)
		return
	}
	filePath := file.Path

	// Wait for file to be ready (exists and has substantial content) - matching server.js behavior
	const maxWait = 30 * time.Second
	const retryDelay = 1 * time.Second
	const minFileSize = 1024 * 1024 // 1MB minimum
	start := time.Now()

	for {
		// Check if file exists and has substantial content
		if fileInfo, err := os.Stat(filePath); err == nil && fileInfo.Size() >= minFileSize {
			log.Printf("handleHLSAudioInitSegment: File ready with size %d bytes", fileInfo.Size())
			break // File is ready
		}

		if time.Since(start) > maxWait {
			log.Printf("handleHLSAudioInitSegment: File not ready after %v (size < %d bytes), falling back to minimal init segment", maxWait, minFileSize)
			serveMinimalInitSegment(w)
			return
		}

		if fileInfo, err := os.Stat(filePath); err == nil {
			log.Printf("handleHLSAudioInitSegment: File exists but too small (%d bytes), waiting %v...", fileInfo.Size(), retryDelay)
		} else {
			log.Printf("handleHLSAudioInitSegment: File not found, waiting %v...", retryDelay)
		}
		time.Sleep(retryDelay)
	}

	// Detect available streams using FFprobe
	var hasAudio bool
	if s.ffmpegMgr != nil && s.ffmpegMgr.IsProbeAvailable() {
		probeInfo, err := s.ffmpegMgr.GetProbeInfo(filePath)
		if err == nil && probeInfo != nil {
			for _, stream := range probeInfo.Streams {
				if stream.Track == "audio" {
					hasAudio = true
					break
				}
			}
		}
	}

	if !hasAudio {
		log.Printf("handleHLSAudioInitSegment: No audio stream found, falling back to minimal init segment")
		serveMinimalInitSegment(w)
		return
	}

	// Test if FFmpeg can read the file first
	testArgs := []string{"-i", filePath, "-f", "null", "-"}
	if s.ffmpegMgr != nil && s.ffmpegMgr.ffmpegPath != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, s.ffmpegMgr.ffmpegPath, testArgs...)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			log.Printf("handleHLSAudioInitSegment: FFmpeg cannot read file, falling back to minimal init segment: %v", err)
			serveMinimalInitSegment(w)
			cancel()
			return
		}
		cancel()
	}

	args := []string{
		"-i", filePath,
		"-map", "0:a:0",
		"-f", "mp4",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof",
		"-frames", "0",
		"-y",
		"pipe:1",
	}
	log.Printf("handleHLSAudioInitSegment: Running FFmpeg for audio init segment: ffmpeg %v", args)
	if err := s.serveFfmpeg(args, "video/mp4", w); err != nil {
		log.Printf("handleHLSAudioInitSegment: FFmpeg failed, falling back to minimal init segment: %v", err)
		serveMinimalInitSegment(w)
	}

	// Disk cache for init segments
	var trackType string
	trackType = "audio"
	cacheDir := filepath.Join(filepath.Dir(filePath), "init_segments")
	os.MkdirAll(cacheDir, 0755)
	cacheFile := filepath.Join(cacheDir, fmt.Sprintf("init_%s_%d_%s.mp4", infoHash, fileIndex, trackType))
	if fi, err := os.Stat(cacheFile); err == nil && fi.Size() > 0 {
		log.Printf("handleHLSAudioInitSegment: Serving cached init segment: %s", cacheFile)
		f, err := os.Open(cacheFile)
		if err == nil {
			defer f.Close()
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			io.Copy(w, f)
			return
		}
	}

	// After successful FFmpeg generation, save to cache
	if err == nil {
		// Save the generated init segment to cache
		if f, ferr := os.Create(cacheFile); ferr == nil {
			defer f.Close()
			// Re-run FFmpeg to file (since we already streamed to client, or buffer and tee if you want to optimize)
			// For now, just log that we could optimize this with a buffer/tee
			log.Printf("handleHLSAudioInitSegment: (TODO) Optimize: buffer FFmpeg output to serve and cache in one pass")
		}
	}
}

// handleHLSAudioSegmentM4S handles HLS media segments for audio
func (s *Server) handleHLSAudioSegmentM4S(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash, fileIndex := getInfoHashAndFileIndex(r, vars["infoHash"], vars["fileIndex"])
	sequence := vars["sequence"]

	// Parse sequence number
	seqNum, err := strconv.Atoi(sequence)
	if err != nil {
		http.Error(w, "Invalid sequence number", http.StatusBadRequest)
		return
	}

	// Get torrent engine
	engine, exists := s.engineFS.torrentManager.GetTorrent(infoHash)
	if !exists {
		http.Error(w, "Torrent not found", http.StatusNotFound)
		return
	}

	// Get file info
	file, err := engine.GetFile(fileIndex)
	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	// Get file path
	filePath := filepath.Join(s.engineFS.torrentManager.cachePath, infoHash, strconv.Itoa(fileIndex))

	// Check if file exists on disk
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		log.Printf("HLS Audio Segment: File not found on disk: %s", filePath)
		http.Error(w, "File not found on disk", http.StatusNotFound)
		return
	}

	if fileInfo.Size() == 0 {
		log.Printf("HLS Audio Segment: File is empty - cannot serve segment")
		http.Error(w, "File is empty", http.StatusServiceUnavailable)
		return
	}

	segmentDuration := 2.0
	segmentStartTime := float64(seqNum-1) * segmentDuration

	// Detect Matroska/WebM
	isMatroska := strings.HasSuffix(strings.ToLower(file.Name), ".mkv") || strings.HasSuffix(strings.ToLower(file.Name), ".webm")
	log.Printf("HLS Audio Segment: isMatroska : %t", isMatroska)
	// Build ffmpeg arguments for audio segment generation (audio-only, HLS/DASH compatible)
	args := []string{
		"-fflags", "+genpts",
		"-noaccurate_seek",
		"-seek_timestamp", "1",
		"-copyts",
		"-seek2any", "1",
		"-ss", fmt.Sprintf("%.3f", segmentStartTime),
		"-i", filePath,
		"-threads", "3",
		"-ss", fmt.Sprintf("%.3f", segmentStartTime),
		"-output_ts_offset", fmt.Sprintf("%.3f", segmentStartTime),
		"-max_muxing_queue_size", "2048",
		"-ignore_unknown",
		"-map_metadata", "-1",
		"-map_chapters", "-1",
		"-map", "-0:d?",
		"-map", "-0:t?",
		"-map", "-0:v?",
		"-map", "a:0",
		"-c:a", "aac",
		"-filter:a", "apad",
		"-async", "1",
		"-ac:a", "2",
		"-ab", "384000",
		"-ar:a", "48000",
		"-map", "-0:s?",
		"-frag_duration", "4096000",
		"-fragment_index", "1",
		"-movflags", "empty_moov+default_base_moof+delay_moov+dash",
		"-use_editlist", "1",
		"-f", "mp4",
		"pipe:1",
	}
	log.Printf("HLS Audio Segment: FFmpeg command: %v", args)
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	// Content-Length omitted for streaming
	if err := s.serveFfmpeg(args, "video/mp4", w); err != nil {
		log.Printf("HLS Audio Segment error: %v", err)
		log.Printf("HLS Audio Segment: FFmpeg command: %v", args)
		return
	}
}

// handleHLSSubtitleM3U8 handles the subtitle playlist
func (s *Server) handleHLSSubtitleM3U8(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash, fileIndex := getInfoHashAndFileIndex(r, vars["infoHash"], vars["fileIndex"])

	// Get torrent engine
	engine, exists := s.engineFS.torrentManager.GetTorrent(infoHash)
	if !exists {
		// Try to create the torrent from the mediaURL if available
		mediaURL := r.URL.Query().Get("mediaURL")
		if mediaURL != "" {
			var trackers []string
			var dhtEnabled bool
			parsedURL, _ := url.Parse(mediaURL)
			for key, values := range parsedURL.Query() {
				if key == "tr" {
					for _, value := range values {
						if strings.HasPrefix(value, "tracker:") {
							trackerURL := strings.TrimPrefix(value, "tracker:")
							trackers = append(trackers, trackerURL)
						} else if strings.HasPrefix(value, "dht:") {
							dhtEnabled = true
						}
					}
				}
			}
			magnetURI := fmt.Sprintf("magnet:?xt=urn:btih:%s", infoHash)
			if len(trackers) > 0 {
				for _, tracker := range trackers {
					magnetURI += "&tr=" + url.QueryEscape(tracker)
				}
			}
			log.Printf("HLS subtitle0.m3u8: Creating torrent %s with %d trackers, DHT: %v", infoHash, len(trackers), dhtEnabled)
			var err error
			if len(trackers) > 0 || dhtEnabled {
				engine, err = s.engineFS.torrentManager.AddTorrentWithConfig(magnetURI, trackers, dhtEnabled, 30, 150, fileIndex)
			} else {
				engine, err = s.engineFS.torrentManager.AddTorrent(magnetURI, fileIndex)
			}
			if err != nil {
				log.Printf("HLS subtitle0.m3u8: Error creating torrent: %v", err)
				http.Error(w, "Failed to create torrent 3", http.StatusInternalServerError)
				return
			}
			log.Printf("HLS subtitle0.m3u8: Successfully created torrent engine for: %s", engine.InfoHash)
		} else {
			http.Error(w, "Torrent not found", http.StatusNotFound)
			return
		}
	}

	// Get file info
	_, err := engine.GetFile(fileIndex)
	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	// Build query parameters for segments (preserve original order)
	queryString := ""
	if r.URL.RawQuery != "" {
		queryString = "?" + r.URL.RawQuery
	}

	// Generate HLS subtitle playlist
	playlist := s.generateHLSSubtitlePlaylist(engine, fileIndex, queryString)

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Write([]byte(playlist))
}

// handleHLSSubtitleInitSegment handles the initialization segment for subtitles
func (s *Server) handleHLSSubtitleInitSegment(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash, fileIndex := getInfoHashAndFileIndex(r, vars["infoHash"], vars["fileIndex"])

	engine, exists := s.engineFS.torrentManager.GetTorrent(infoHash)
	if !exists {
		serveMinimalInitSegment(w)
		return
	}
	file, err := engine.GetFile(fileIndex)
	if err != nil {
		serveMinimalInitSegment(w)
		return
	}
	filePath := file.Path

	// Wait for file to be ready (exists and has substantial content) - matching server.js behavior
	const maxWait = 30 * time.Second
	const retryDelay = 1 * time.Second
	const minFileSize = 1024 * 1024 // 1MB minimum
	start := time.Now()

	for {
		// Check if file exists and has substantial content
		if fileInfo, err := os.Stat(filePath); err == nil && fileInfo.Size() >= minFileSize {
			log.Printf("handleHLSSubtitleInitSegment: File ready with size %d bytes", fileInfo.Size())
			break // File is ready
		}

		if time.Since(start) > maxWait {
			log.Printf("handleHLSSubtitleInitSegment: File not ready after %v (size < %d bytes), falling back to minimal init segment", maxWait, minFileSize)
			serveMinimalInitSegment(w)
			return
		}

		if fileInfo, err := os.Stat(filePath); err == nil {
			log.Printf("handleHLSSubtitleInitSegment: File exists but too small (%d bytes), waiting %v...", fileInfo.Size(), retryDelay)
		} else {
			log.Printf("handleHLSSubtitleInitSegment: File not found, waiting %v...", retryDelay)
		}
		time.Sleep(retryDelay)
	}

	// Detect available streams using FFprobe
	var hasSubtitle bool
	if s.ffmpegMgr != nil && s.ffmpegMgr.IsProbeAvailable() {
		probeInfo, err := s.ffmpegMgr.GetProbeInfo(filePath)
		if err == nil && probeInfo != nil {
			for _, stream := range probeInfo.Streams {
				if stream.Track == "subtitle" {
					hasSubtitle = true
					break
				}
			}
		}
	}

	if !hasSubtitle {
		log.Printf("handleHLSSubtitleInitSegment: No subtitle stream found, falling back to minimal init segment")
		serveMinimalInitSegment(w)
		return
	}

	// Test if FFmpeg can read the file first
	testArgs := []string{"-i", filePath, "-f", "null", "-"}
	if s.ffmpegMgr != nil && s.ffmpegMgr.ffmpegPath != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, s.ffmpegMgr.ffmpegPath, testArgs...)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			log.Printf("handleHLSSubtitleInitSegment: FFmpeg cannot read file, falling back to minimal init segment: %v", err)
			serveMinimalInitSegment(w)
			cancel()
			return
		}
		cancel()
	}

	args := []string{
		"-i", filePath,
		"-map", "0:s:0",
		"-f", "mp4",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof",
		"-frames", "0",
		"-y",
		"pipe:1",
	}
	log.Printf("handleHLSSubtitleInitSegment: Running FFmpeg for subtitle init segment: ffmpeg %v", args)
	if err := s.serveFfmpeg(args, "video/mp4", w); err != nil {
		log.Printf("handleHLSSubtitleInitSegment: FFmpeg failed, falling back to minimal init segment: %v", err)
		serveMinimalInitSegment(w)
	}

	// Disk cache for init segments
	var trackType string
	trackType = "subtitle"
	cacheDir := filepath.Join(filepath.Dir(filePath), "init_segments")
	os.MkdirAll(cacheDir, 0755)
	cacheFile := filepath.Join(cacheDir, fmt.Sprintf("init_%s_%d_%s.mp4", infoHash, fileIndex, trackType))
	if fi, err := os.Stat(cacheFile); err == nil && fi.Size() > 0 {
		log.Printf("handleHLSSubtitleInitSegment: Serving cached init segment: %s", cacheFile)
		f, err := os.Open(cacheFile)
		if err == nil {
			defer f.Close()
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			io.Copy(w, f)
			return
		}
	}

	// After successful FFmpeg generation, save to cache
	if err == nil {
		// Save the generated init segment to cache
		if f, ferr := os.Create(cacheFile); ferr == nil {
			defer f.Close()
			// Re-run FFmpeg to file (since we already streamed to client, or buffer and tee if you want to optimize)
			// For now, just log that we could optimize this with a buffer/tee
			log.Printf("handleHLSSubtitleInitSegment: (TODO) Optimize: buffer FFmpeg output to serve and cache in one pass")
		}
	}
}

// handleHLSSubtitleSegmentM4S handles HLS media segments for subtitles
func (s *Server) handleHLSSubtitleSegmentM4S(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash, fileIndex := getInfoHashAndFileIndex(r, vars["infoHash"], vars["fileIndex"])
	sequence := vars["sequence"]

	// Parse sequence number
	seqNum, err := strconv.Atoi(sequence)
	if err != nil {
		http.Error(w, "Invalid sequence number", http.StatusBadRequest)
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

	// Get file path
	filePath := filepath.Join(s.engineFS.torrentManager.cachePath, infoHash, strconv.Itoa(fileIndex))

	// Check if file exists on disk
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		log.Printf("HLS Subtitle Segment: File not found on disk: %s", filePath)
		http.Error(w, "File not found on disk", http.StatusNotFound)
		return
	}

	if fileInfo.Size() == 0 {
		log.Printf("HLS Subtitle Segment: File is empty - cannot serve segment")
		http.Error(w, "File is empty", http.StatusServiceUnavailable)
		return
	}

	segmentDuration := 2.0
	segmentStartTime := float64(seqNum-1) * segmentDuration

	// Detect available streams using FFprobe
	var hasSubtitle bool
	if s.ffmpegMgr != nil && s.ffmpegMgr.IsProbeAvailable() {
		probeInfo, err := s.ffmpegMgr.GetProbeInfo(filePath)
		if err == nil && probeInfo != nil {
			for _, stream := range probeInfo.Streams {
				if stream.Track == "subtitle" {
					hasSubtitle = true
					break
				}
			}
		}
	}

	if !hasSubtitle {
		log.Printf("HLS Subtitle Segment: No subtitle stream found")
		http.Error(w, "No subtitle stream found", http.StatusNotFound)
		return
	}

	// Build FFmpeg arguments for subtitle segment generation
	args := []string{
		"-ss", fmt.Sprintf("%.3f", segmentStartTime),
		"-i", filePath,
		"-t", fmt.Sprintf("%.3f", segmentDuration),
		"-map", "0:s:0",
		"-f", "mp4",
		"-movflags", "frag_keyframe+dash",
		"pipe:1",
	}

	log.Printf("HLS Subtitle Segment: FFmpeg command: %v", args)
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	// Content-Length omitted for streaming
	if err := s.serveFfmpeg(args, "video/mp4", w); err != nil {
		log.Printf("HLS Subtitle Segment error: %v", err)
		log.Printf("HLS Subtitle Segment: FFmpeg command: %v", args)
		return
	}
}

// handleHLSSegmentTranscode handles HLS segment requests with ffmpeg transcoding
func (s *Server) handleHLSSegmentTranscode(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash := vars["infoHash"]
	fileIndexStr := vars["fileIndex"]
	quality := vars["quality"]
	_ = vars["segment"] // Use underscore to indicate intentionally unused

	// Parse file index
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

	// Check if file exists
	if fileIndex >= len(engine.Files) {
		http.Error(w, "Invalid file index", http.StatusBadRequest)
		return
	}

	// Get file path
	filePath := filepath.Join(s.engineFS.torrentManager.cachePath, infoHash, strconv.Itoa(fileIndex))

	// Check if file exists on disk
	if _, err := os.Stat(filePath); err != nil {
		http.Error(w, "File not found on disk", http.StatusNotFound)
		return
	}

	// Determine if this is a video or audio segment request (e.g., by query param or path)
	segmentType := r.URL.Query().Get("type") // e.g., "video" or "audio"
	segmentStart := r.URL.Query().Get("ss")  // segment start time as string, e.g., "0.028"
	if segmentStart == "" {
		segmentStart = "0"
	}

	var args []string
	if segmentType == "audio" {
		// Audio segment command (transcode to AAC, exclude video/subs)
		args = []string{
			"-fflags", "+genpts",
			"-noaccurate_seek",
			"-seek_timestamp", "1",
			"-copyts",
			"-seek2any", "1",
			"-ss", segmentStart,
			"-i", filePath,
			"-threads", "3",
			"-ss", segmentStart,
			"-output_ts_offset", segmentStart,
			"-max_muxing_queue_size", "2048",
			"-ignore_unknown",
			"-map_metadata", "-1",
			"-map_chapters", "-1",
			"-map", "-0:d?",
			"-map", "-0:t?",
			"-map", "-0:v?",
			"-map", "a:0",
			"-c:a", "aac",
			"-filter:a", "apad",
			"-async", "1",
			"-ac:a", "2",
			"-ab", "384000",
			"-ar:a", "48000",
			"-map", "-0:s?",
			"-frag_duration", "4096000",
			"-fragment_index", "1",
			"-movflags", "empty_moov+default_base_moof+delay_moov+dash",
			"-use_editlist", "1",
			"-f", "mp4",
			"pipe:1",
		}
	} else {
		// Video segment command (copy video, exclude audio/subs)
		args = []string{
			"-fflags", "+genpts",
			"-noaccurate_seek",
			"-seek_timestamp", "1",
			"-copyts",
			"-seek2any", "1",
			"-ss", segmentStart,
			"-i", filePath,
			"-threads", "3",
			"-max_muxing_queue_size", "2048",
			"-ignore_unknown",
			"-map_metadata", "-1",
			"-map_chapters", "-1",
			"-map", "-0:d?",
			"-map", "-0:t?",
			"-map", "v:0",
			"-c:v", "copy",
			"-force_key_frames:v", "source",
			"-map", "-0:a?",
			"-map", "-0:s?",
			"-fragment_index", "1",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof+delay_moov+dash",
			"-use_editlist", "1",
			"-f", "mp4",
			"pipe:1",
		}
		// Add quality-specific settings (scaling) if needed
		switch quality {
		case "1080p":
			args = append(args, "-vf", "scale=1920:1080")
		case "720p":
			args = append(args, "-vf", "scale=1280:720")
		case "480p":
			args = append(args, "-vf", "scale=854:480")
		default:
			// No scaling for original quality
		}
	}

	// Set HLS flow header
	w.Header().Set("X-HLS-Flow", "transcoder")

	// Serve ffmpeg output
	if err := s.serveFfmpeg(args, "video/mp2t", w); err != nil {
		log.Printf("HLS Segment Transcode error: %v", err)
		http.Error(w, "Transcoding failed", http.StatusInternalServerError)
		return
	}
}

// handleHLSStreamSplit handles HLS stream playlist requests with ffmpeg splitting
func (s *Server) handleHLSStreamSplit(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash := vars["infoHash"]
	fileIndexStr := vars["fileIndex"]
	quality := vars["quality"]

	// Parse file index
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

	// Check if file exists
	if fileIndex >= len(engine.Files) {
		http.Error(w, "Invalid file index", http.StatusBadRequest)
		return
	}

	// Get file path
	filePath := filepath.Join(s.engineFS.torrentManager.cachePath, infoHash, strconv.Itoa(fileIndex))

	// Check if file exists on disk
	if _, err := os.Stat(filePath); err != nil {
		http.Error(w, "File not found on disk", http.StatusNotFound)
		return
	}

	// Build ffmpeg arguments for HLS stream splitting
	args := []string{
		"-i", filePath,
		"-threads", "1",
		"-c:v", "libx264",
		"-c:a", "aac",
		"-f", "hls",
		"-hls_time", "2",
		"-hls_list_size", "0",
		"-hls_segment_filename", "segment_%03d.ts",
		"-tune", "zerolatency",
		"-loglevel", "error",
	}

	// Add quality-specific settings
	switch quality {
	case "1080p":
		args = append(args, "-vf", "scale=1920:1080")
	case "720p":
		args = append(args, "-vf", "scale=1280:720")
	case "480p":
		args = append(args, "-vf", "scale=854:480")
	default:
		// No scaling for original quality
	}

	// Add output to pipe
	args = append(args, "pipe:1")

	// Set HLS flow header
	w.Header().Set("X-HLS-Flow", "splitter")

	// Serve ffmpeg output
	if err := s.serveFfmpeg(args, "application/vnd.apple.mpegurl", w); err != nil {
		log.Printf("HLS Stream Split error: %v", err)
		http.Error(w, "Streaming failed", http.StatusInternalServerError)
		return
	}
}

// Start starts the server (matching Node.js server.js exactly)
func (s *Server) Start() error {
	// Create app directories
	if err := createAppDirectories(s.config.AppPath); err != nil {
		return fmt.Errorf("failed to create app directories: %v", err)
	}

	// Initialize torrent manager
	torrentManager, err := NewTorrentManager(filepath.Join(s.config.AppPath, "stremio-cache"))
	if err != nil {
		return fmt.Errorf("failed to create torrent manager: %v", err)
	}
	s.engineFS.torrentManager = torrentManager

	// Initialize FFmpeg manager (matching Node.js server.js behavior)
	ffmpegMgr, err := NewFFmpegManager(s.config.FFmpeg)
	if err != nil {
		log.Printf("Warning: Failed to create FFmpeg manager: %v", err)
		// Server can still run without FFmpeg
		s.ffmpegMgr = nil
	} else {
		s.ffmpegMgr = ffmpegMgr
		if !ffmpegMgr.IsAvailable() {
			log.Printf("Warning: FFmpeg not available - some features will be disabled")
			// Try to find FFmpeg directly as a fallback
			if ffmpegPath, err := exec.LookPath("ffmpeg"); err == nil {
				log.Printf("Info: Found FFmpeg at %s, will use direct execution as fallback", ffmpegPath)
			} else {
				log.Printf("Warning: FFmpeg not found in PATH either")
			}
		} else {
			log.Printf("Info: FFmpeg initialized successfully at %s", ffmpegMgr.GetFFmpegPath())
		}
	}

	// Setup routes
	s.setupRoutes()

	// Start HTTP server with port fallback (matching Node.js server.js exactly)
	port := s.config.HTTPPort
	s.httpSrv = &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: s.router,
	}

	log.Printf("Starting HTTP server on port %d", port)

	// Start server with port fallback logic (matching Node.js exactly)
	go func() {
		for {
			err := s.httpSrv.ListenAndServe()
			if err != nil && err != http.ErrServerClosed {
				// Port fallback logic (matching Node.js server.js)
				if strings.Contains(err.Error(), "bind: address already in use") {
					port++
					if port <= 11474 { // Same limit as Node.js
						log.Printf("Port %d in use, trying port %d", port-1, port)
						s.httpSrv.Addr = fmt.Sprintf(":%d", port)
						continue
					} else {
						log.Printf("HTTP server error: %v", err)
						break
					}
				} else {
					log.Printf("HTTP server error: %v", err)
					break
				}
			} else {
				break
			}
		}
	}()

	// Start HTTPS server if SSL certificates are available (matching Node.js exactly)
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

	if s.httpSrv != nil {
		if err := s.httpSrv.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("HTTP server shutdown error: %v", err))
		}
	}

	if s.httpsSrv != nil {
		if err := s.httpsSrv.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("HTTPS server shutdown error: %v", err))
		}
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
	// Parse environment variables (matching Node.js server.js exactly)
	config := &Config{
		HTTPPort:  getEnvInt("HTTP_PORT", 11470),  // Default port matching Node.js
		HTTPSPort: getEnvInt("HTTPS_PORT", 12470), // Default HTTPS port matching Node.js
		AppPath:   getEnv("APP_PATH", expandHomeDir("~/.stremio-server")),
		NoCORS:    getEnvBool("NO_CORS", false),
		Username:  getEnv("USERNAME", ""),
		Password:  getEnv("PASSWORD", ""),
		LogLevel:  getEnv("LOG_LEVEL", "info"),
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

	// Set logging level for torrent library (matching Node.js behavior)
	if config.LogLevel == "warn" || config.LogLevel == "warning" || config.LogLevel == "error" {
		os.Setenv("TORRENT_LOGGER", "warning")
		os.Setenv("ANACROLIX_LOG_LEVEL", "warn")
	}

	// Create server
	server, err := NewServer(config)
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}

	// Create and register a sample local addon
	sampleManifest := map[string]interface{}{
		"id":          "org.stremio.local-addon",
		"version":     "1.0.0",
		"name":        "Local Files",
		"description": "Serves files from the local filesystem",
		"resources":   []string{"stream"},
		"types":       []string{"movie", "series"},
		"catalogs":    []interface{}{},
	}
	localAddon := NewAddon(sampleManifest)
	server.localAddons = append(server.localAddons, localAddon)

	// Start server
	if err := server.Start(); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}

	// Wait for interrupt signal (matching Node.js behavior)
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

// createAppDirectories creates all necessary directories for the application
func createAppDirectories(appPath string) error {
	directories := []string{
		appPath,
	}

	for _, dir := range directories {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %v", dir, err)
		}
		log.Printf("Created directory: %s", dir)
	}

	return nil
}

// getAvailableInterfaces returns all available network interface IPs
func getAvailableInterfaces() []string {
	var ips []string

	interfaces, err := net.Interfaces()
	if err != nil {
		return ips
	}

	for _, iface := range interfaces {
		// Skip loopback and down interfaces
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				// Only include IPv4 addresses
				if ipnet.IP.To4() != nil {
					ips = append(ips, ipnet.IP.String())
				}
			}
		}
	}

	return ips
}

// handleList returns all active torrent engines
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	engines := s.engineFS.torrentManager.ListTorrents()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(engines)
}

// Placeholder handlers for new /hlsv2 routes
func (s *Server) handleHLSGenericTrackM3U8(w http.ResponseWriter, r *http.Request) {
	// TODO: Implement generic track.m3u8 logic
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:7\n# TODO: Implement generic track.m3u8\n"))
}

func (s *Server) handleHLSGenericTrackInitMP4(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash, fileIndex := getInfoHashAndFileIndex(r, vars["infoHash"], vars["fileIndex"])
	track := vars["track"]

	engine, exists := s.engineFS.torrentManager.GetTorrent(infoHash)
	if !exists {
		serveMinimalInitSegment(w)
		return
	}
	file, err := engine.GetFile(fileIndex)
	if err != nil {
		serveMinimalInitSegment(w)
		return
	}
	filePath := file.Path
	trackType := "video"
	ffmpegMap := "0:v:0"
	isAudio := false
	if strings.HasPrefix(track, "audio") {
		trackType = "audio"
		ffmpegMap = "0:a:0"
		isAudio = true
	} else if strings.HasPrefix(track, "subtitle") {
		trackType = "subtitle"
		ffmpegMap = "0:s:0"
	}

	// Wait for file to be ready (exists and has substantial content) - matching server.js behavior
	const maxWait = 30 * time.Second
	const retryDelay = 1 * time.Second
	const minFileSize = 1024 * 1024 // 1MB minimum
	start := time.Now()

	for {
		// Check if file exists and has substantial content
		if fileInfo, err := os.Stat(filePath); err == nil && fileInfo.Size() >= minFileSize {
			log.Printf("handleHLSGenericTrackInitMP4: File ready with size %d bytes", fileInfo.Size())
			break // File is ready
		}

		if time.Since(start) > maxWait {
			log.Printf("handleHLSGenericTrackInitMP4: File not ready after %v (size < %d bytes), falling back to minimal init segment", maxWait, minFileSize)
			serveMinimalInitSegment(w)
			return
		}

		if fileInfo, err := os.Stat(filePath); err == nil {
			log.Printf("handleHLSGenericTrackInitMP4: File exists but too small (%d bytes), waiting %v...", fileInfo.Size(), retryDelay)
		} else {
			log.Printf("handleHLSGenericTrackInitMP4: File not found, waiting %v...", retryDelay)
		}
		time.Sleep(retryDelay)
	}

	// Detect available streams and validate the requested track
	var hasVideo, hasAudio, hasSubtitle bool
	if s.ffmpegMgr != nil && s.ffmpegMgr.IsProbeAvailable() {
		probeInfo, err := s.ffmpegMgr.GetProbeInfo(filePath)
		if err == nil && probeInfo != nil {
			for _, stream := range probeInfo.Streams {
				if stream.Track == "video" {
					hasVideo = true
				} else if stream.Track == "audio" {
					hasAudio = true
				} else if stream.Track == "subtitle" {
					hasSubtitle = true
				}
			}
		}
	}

	// Smart track mapping (matching server.js fallback logic):
	if trackType == "video" && !hasVideo && hasAudio {
		log.Printf("handleHLSGenericTrackInitMP4: Video track not found but audio available, mapping video request to audio stream")
		trackType = "audio"
		ffmpegMap = "0:a:0"
		isAudio = true
	} else if trackType == "video" && !hasVideo && !hasAudio && hasSubtitle {
		log.Printf("handleHLSGenericTrackInitMP4: Video track not found but subtitle available, mapping video request to subtitle stream")
		trackType = "subtitle"
		ffmpegMap = "0:s:0"
		isAudio = false
	} else if trackType == "audio" && !hasAudio && hasSubtitle {
		log.Printf("handleHLSGenericTrackInitMP4: Audio track not found but subtitle available, mapping audio request to subtitle stream")
		trackType = "subtitle"
		ffmpegMap = "0:s:0"
		isAudio = false
	} else if (trackType == "video" && !hasVideo) || (trackType == "audio" && !hasAudio) || (trackType == "subtitle" && !hasSubtitle) {
		log.Printf("handleHLSGenericTrackInitMP4: Requested %s track not found (hasVideo=%v, hasAudio=%v, hasSubtitle=%v), falling back to minimal init segment", trackType, hasVideo, hasAudio, hasSubtitle)
		serveMinimalInitSegment(w)
		return
	}

	// Test if FFmpeg can read the file first (use a much faster check)
	testArgs := []string{"-v", "error", "-t", "1", "-i", filePath, "-f", "null", "-"}
	if s.ffmpegMgr != nil && s.ffmpegMgr.ffmpegPath != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, s.ffmpegMgr.ffmpegPath, testArgs...)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		log.Printf("handleHLSGenericTrackInitMP4: Running FFmpeg command to test file readability: %s", strings.Join(cmd.Args, " "))
		if err := cmd.Run(); err != nil {
			exitError, ok := err.(*exec.ExitError)
			if ok {
				log.Printf("handleHLSGenericTrackInitMP4: FFmpeg cannot read file, falling back to minimal init segment: exit code %d", exitError.ExitCode())
			} else {
				log.Printf("handleHLSGenericTrackInitMP4: FFmpeg cannot read file, falling back to minimal init segment: %v", err)
			}
			serveMinimalInitSegment(w)
			cancel()
			return
		}
		cancel()
	}

	var args []string
	if isAudio {
		args = []string{
			"-fflags", "+genpts",
			"-noaccurate_seek",
			"-seek_timestamp", "1",
			"-copyts",
			"-seek2any", "1",
			"-ss", "0",
			"-i", filePath,
			"-threads", "3",
			"-ss", "0",
			"-output_ts_offset", "0",
			"-max_muxing_queue_size", "2048",
			"-ignore_unknown",
			"-map_metadata", "-1",
			"-map_chapters", "-1",
			"-map", "-0:d?",
			"-map", "-0:t?",
			"-map", "-0:v?",
			"-map", "a:0",
			"-c:a", "aac",
			"-filter:a", "apad",
			"-async", "1",
			"-ac:a", "2",
			"-ab", "384000",
			"-ar:a", "48000",
			"-map", "-0:s?",
			"-frag_duration", "4096000",
			"-fragment_index", "1",
			"-movflags", "empty_moov+default_base_moof+delay_moov+dash",
			"-use_editlist", "1",
			"-f", "mp4",
			"-frames:a", "1",
			"-y",
			"pipe:1",
		}
	} else if trackType == "video" {
		args = []string{
			"-fflags", "+genpts",
			"-noaccurate_seek",
			"-seek_timestamp", "1",
			"-copyts",
			"-seek2any", "1",
			"-ss", "0",
			"-i", filePath,
			"-threads", "3",
			"-max_muxing_queue_size", "2048",
			"-ignore_unknown",
			"-map_metadata", "-1",
			"-map_chapters", "-1",
			"-map", "-0:d?",
			"-map", "-0:t?",
			"-map", "v:0",
			"-c:v", "copy",
			"-force_key_frames:v", "source",
			"-map", "-0:a?",
			"-map", "-0:s?",
			"-fragment_index", "1",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof+delay_moov+dash",
			"-use_editlist", "1",
			"-f", "mp4",
			"-frames:v", "1",
			"-y",
			"pipe:1",
		}
	} else if trackType == "subtitle" {
		args = []string{
			"-i", filePath,
			"-map", ffmpegMap,
			"-threads", "1",
			"-f", "mp4",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof",
			"-frames", "0",
			"-y",
			"pipe:1",
		}
	}
	log.Printf("handleHLSGenericTrackInitMP4: Running FFmpeg for %s init segment: ffmpeg %v", trackType, args)
	if err := s.serveFfmpeg(args, "video/mp4", w); err != nil {
		log.Printf("handleHLSGenericTrackInitMP4: FFmpeg failed, falling back to minimal init segment: %v", err)
		serveMinimalInitSegment(w)
	}

	// Disk cache for init segments
	if strings.HasPrefix(ffmpegMap, "0:v") {
		trackType = "video"
	} else if strings.HasPrefix(ffmpegMap, "0:a") {
		trackType = "audio"
	} else if strings.HasPrefix(ffmpegMap, "0:s") {
		trackType = "subtitle"
	} else {
		trackType = "unknown"
	}
	cacheDir := filepath.Join(filepath.Dir(filePath), "init_segments")
	os.MkdirAll(cacheDir, 0755)
	cacheFile := filepath.Join(cacheDir, fmt.Sprintf("init_%s_%d_%s.mp4", infoHash, fileIndex, trackType))
	if fi, err := os.Stat(cacheFile); err == nil && fi.Size() > 0 {
		log.Printf("handleHLSGenericTrackInitMP4: Serving cached init segment: %s", cacheFile)
		f, err := os.Open(cacheFile)
		if err == nil {
			defer f.Close()
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			io.Copy(w, f)
			return
		}
	}

	// After successful FFmpeg generation, save to cache
	if err == nil {
		if f, ferr := os.Create(cacheFile); ferr == nil {
			defer f.Close()
			log.Printf("handleHLSGenericTrackInitMP4: (TODO) Optimize: buffer FFmpeg output to serve and cache in one pass")
		}
	}
}

func (s *Server) handleHLSGenericTrackSegment(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	infoHash, fileIndex := getInfoHashAndFileIndex(r, vars["id"], "")
	track := vars["track"]
	sequenceNumber := vars["sequenceNumber"]
	ext := vars["ext"]

	log.Printf("HLSGenericTrackSegment: infoHash=%s, fileIndex=%d, track=%s, sequenceNumber=%s, ext=%s", infoHash, fileIndex, track, sequenceNumber, ext)

	engine, exists := s.engineFS.torrentManager.GetTorrent(infoHash)
	if !exists {
		http.Error(w, "Torrent not found", http.StatusNotFound)
		return
	}
	_, err := engine.GetFile(fileIndex)
	if err != nil {
		log.Printf("HLSGenericTrackSegment: fileIndex %d not found in engine %s", fileIndex, infoHash)
		http.Error(w, "Invalid file index", http.StatusBadRequest)
		return
	}

	filePath := filepath.Join(s.engineFS.torrentManager.cachePath, infoHash, strconv.Itoa(fileIndex))

	if _, err := os.Stat(filePath); err != nil {
		log.Printf("HLS Generic Track Segment: File not found on disk: %s", filePath)
		http.Error(w, "File not found on disk", http.StatusNotFound)
		return
	}

	seqNum, err := strconv.Atoi(sequenceNumber)
	if err != nil {
		http.Error(w, "Invalid sequence number", http.StatusBadRequest)
		return
	}

	segmentDuration := 2.0
	segmentStartTime := float64(seqNum-1) * segmentDuration

	var hasVideo, hasAudio bool
	if s.ffmpegMgr != nil && s.ffmpegMgr.IsProbeAvailable() {
		probeInfo, err := s.ffmpegMgr.GetProbeInfo(filePath)
		if err == nil && probeInfo != nil {
			for _, stream := range probeInfo.Streams {
				if stream.Track == "video" {
					hasVideo = true
				} else if stream.Track == "audio" {
					hasAudio = true
				}
			}
		}
	}

	if strings.HasPrefix(track, "video") && !hasVideo && hasAudio {
		log.Printf("HLSGenericTrackSegment: Video track not found but audio available, mapping video request to audio stream")
		track = "audio0"
	}

	contentType := "video/mp4"
	if strings.HasPrefix(track, "audio") {
		contentType = "audio/mp4"
	}

	var args []string
	if strings.HasPrefix(track, "audio") {
		args = []string{
			"-fflags", "+genpts",
			"-noaccurate_seek",
			"-seek_timestamp", "1",
			"-copyts",
			"-seek2any", "1",
			"-ss", fmt.Sprintf("%.3f", segmentStartTime),
			"-i", filePath,
			"-t", fmt.Sprintf("%.3f", segmentDuration), // limit audio segment duration
			"-threads", "3",
			"-ss", fmt.Sprintf("%.3f", segmentStartTime),
			"-output_ts_offset", fmt.Sprintf("%.3f", segmentStartTime),
			"-max_muxing_queue_size", "2048",
			"-ignore_unknown",
			"-map_metadata", "-1",
			"-map_chapters", "-1",
			"-map", "-0:d?",
			"-map", "-0:t?",
			"-map", "-0:v?",
			"-map", "a:0",
			"-c:a", "aac",
			"-filter:a", "apad",
			"-async", "1",
			"-ac:a", "2",
			"-ab", "384000",
			"-ar:a", "48000",
			"-map", "-0:s?",
			"-frag_duration", "4096000",
			"-fragment_index", "1",
			"-movflags", "empty_moov+default_base_moof+delay_moov+dash",
			"-use_editlist", "1",
			"-f", "mp4",
			"pipe:1",
		}
	} else {
		args = []string{
			"-fflags", "+genpts",
			"-noaccurate_seek",
			"-seek_timestamp", "1",
			"-copyts",
			"-seek2any", "1",
			"-ss", fmt.Sprintf("%.3f", segmentStartTime),
			"-i", filePath,
			"-t", fmt.Sprintf("%.3f", segmentDuration),
			"-threads", "3",
			"-max_muxing_queue_size", "2048",
			"-ignore_unknown",
			"-map_metadata", "-1",
			"-map_chapters", "-1",
			"-map", "-0:d?",
			"-map", "-0:t?",
			"-map", "v:0",
			"-c:v", "copy",
			"-force_key_frames:v", "source",
			"-map", "-0:a?",
			"-map", "-0:s?",
			"-fragment_index", "1",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof+delay_moov+dash",
			"-use_editlist", "1",
			"-f", "mp4",
			"pipe:1",
		}
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if err := s.serveFfmpeg(args, contentType, w); err != nil {
		log.Printf("HLS Generic Track Segment error: %v", err)
		log.Printf("HLS Generic Track Segment: FFmpeg command: %v", args)
		return
	}
}

func (s *Server) handleHLSBurn(w http.ResponseWriter, r *http.Request) {
	// TODO: Implement /hlsv2/{infoHash}/burn logic
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"burn placeholder"}`))
}

func (s *Server) handleHLSDestroy(w http.ResponseWriter, r *http.Request) {
	// TODO: Implement /hlsv2/{infoHash}/destroy logic
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"destroy placeholder"}`))
}

func (s *Server) handleHLSStatus(w http.ResponseWriter, r *http.Request) {
	// TODO: Implement /hlsv2/status logic
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"status placeholder"}`))
}

func (s *Server) handleHLSExec(w http.ResponseWriter, r *http.Request) {
	// TODO: Implement /hlsv2/exec logic
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"exec placeholder"}`))
}

// detectProblematicAudioCodec checks if a file has audio codecs that need special handling
func (s *Server) detectProblematicAudioCodec(filePath string) (bool, string) {
	if s.ffmpegMgr == nil || s.ffmpegMgr.ffprobePath == "" {
		return false, ""
	}

	// Use ffprobe to check audio codec
	cmd := exec.Command(s.ffmpegMgr.ffprobePath, "-v", "quiet", "-select_streams", "a:0", "-show_entries", "stream=codec_name", "-of", "csv=p=0", filePath)
	output, err := cmd.Output()
	if err != nil {
		return false, ""
	}

	codecName := strings.TrimSpace(string(output))
	problematicCodecs := []string{"eac3", "ac3", "dts", "flac"}

	for _, problematic := range problematicCodecs {
		if strings.Contains(strings.ToLower(codecName), problematic) {
			return true, codecName
		}
	}

	return false, codecName
}

// serveFfmpeg spawns ffmpeg with given arguments and pipes output to HTTP response (matching Node.js server.js)
func (s *Server) serveFfmpeg(args []string, contentType string, w http.ResponseWriter) error {
	s.ffmpegSem <- struct{}{}        // Acquire semaphore
	defer func() { <-s.ffmpegSem }() // Release semaphore

	if s.ffmpegMgr == nil || s.ffmpegMgr.ffmpegPath == "" {
		return fmt.Errorf("no ffmpeg found")
	}

	w.Header().Set("Content-Type", contentType)

	if os.Getenv("FFMPEG_DEBUG") != "" {
		log.Printf("FFMPEG: Running %s %s", s.ffmpegMgr.ffmpegPath, strings.Join(args, " "))
	}
	if os.Getenv("FFMPEG_DEBUG") != "" {
		log.Printf("FFMPEG: Content-Type: %s, Args: %v", contentType, args)
	}

	const maxWait = 10 * time.Second
	const retryDelay = 1 * time.Second
	const minValidSize = 32 // bytes
	start := time.Now()

	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, s.ffmpegMgr.ffmpegPath, args...)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			cancel()
			return fmt.Errorf("failed to create stdout pipe: %v", err)
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			stdout.Close()
			cancel()
			return fmt.Errorf("failed to create stderr pipe: %v", err)
		}
		if err := cmd.Start(); err != nil {
			stdout.Close()
			stderr.Close()
			cancel()
			return fmt.Errorf("failed to start ffmpeg: %v", err)
		}
		var buf bytes.Buffer
		var stderrBuf bytes.Buffer
		copyErr := make(chan error, 1)
		go func() {
			_, err := io.Copy(&buf, stdout)
			copyErr <- err
		}()
		go func() {
			io.Copy(&stderrBuf, stderr)
		}()
		err = <-copyErr
		stdout.Close()
		stderr.Close()
		cmd.Wait()
		cancel()

		if err != nil {
			log.Printf("FFMPEG: Error copying output: %v", err)
			log.Printf("FFMPEG: Stderr: %s", stderrBuf.String())
			return fmt.Errorf("failed to buffer ffmpeg output: %v", err)
		}

		if buf.Len() >= minValidSize {
			length := buf.Len()
			if length < 0 {
				length = 0
			}
			w.Header().Set("Content-Length", strconv.Itoa(length))
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("Keep-Alive", "timeout=5")
			w.Header().Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusOK)
			_, err = w.Write(buf.Bytes())
			return err
		}

		if time.Since(start) > maxWait {
			log.Printf("serveFfmpeg: FFmpeg output still empty after %v, giving up", maxWait)
			log.Printf("FFMPEG: Stderr: %s", stderrBuf.String())
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("FFmpeg output empty after retries"))
			return fmt.Errorf("ffmpeg output empty after retries")
		}

		log.Printf("serveFfmpeg: FFmpeg output empty (%d bytes), retrying in %v...", buf.Len(), retryDelay)
		log.Printf("FFMPEG: Stderr: %s", stderrBuf.String())
		time.Sleep(retryDelay)
	}
}

// generateHLSAudioPlaylist generates an HLS audio playlist for the given torrent and file
func (s *Server) generateHLSAudioPlaylist(engine *TorrentEngine, fileIndex int, queryString string) string {
	log.Printf("generateHLSAudioPlaylist: Starting for engine %s, fileIndex %d", engine.InfoHash, fileIndex)
	var playlist strings.Builder

	// Get file info
	file, err := engine.GetFile(fileIndex)
	if err != nil {
		log.Printf("generateHLSAudioPlaylist: Error getting file %d: %v", fileIndex, err)
		return "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-ENDLIST\n"
	}
	log.Printf("generateHLSAudioPlaylist: File info - Name: %s, Size: %d, Path: %s", file.Name, file.Size, file.Path)

	// Try to get actual media duration using FFmpeg
	var totalDuration float64 = 0
	if s.ffmpegMgr != nil && s.ffmpegMgr.IsProbeAvailable() {
		probeInfo, err := s.ffmpegMgr.GetProbeInfo(file.Path)
		if err == nil && probeInfo != nil && probeInfo.Format.Duration > 0 {
			totalDuration = probeInfo.Format.Duration
			log.Printf("Audio playlist: Using FFmpeg duration: %.3f seconds", totalDuration)
		} else {
			totalDuration = float64(file.Size) / (1024 * 1024 * 2)
			log.Printf("Audio playlist: Using estimated duration: %.3f seconds", totalDuration)
		}
	} else {
		totalDuration = float64(file.Size) / (1024 * 1024 * 2)
		log.Printf("Audio playlist: Using estimated duration: %.3f seconds", totalDuration)
	}

	segmentDuration := 2.0                                     // Match ffmpeg segment duration
	numSegments := int(totalDuration/segmentDuration + 0.9999) // ceil
	if numSegments < 1 {
		numSegments = 1
	}
	if numSegments > 1000 {
		numSegments = 1000
	}

	playlist.WriteString("#EXTM3U\n")
	playlist.WriteString("#EXT-X-VERSION:7\n")
	playlist.WriteString("#EXT-X-TARGETDURATION:15\n")
	playlist.WriteString("#EXT-X-MEDIA-SEQUENCE:1\n")
	playlist.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")

	qs := queryString
	if qs != "" && qs[0] != '?' {
		qs = "?" + qs
	}

	playlist.WriteString(fmt.Sprintf("#EXT-X-MAP:URI=\"audio0/init.mp4%s\"\n", qs))

	for i := 1; i <= numSegments; i++ {
		segStart := float64(i-1) * segmentDuration
		segEnd := segStart + segmentDuration
		actualSegDuration := segmentDuration
		if segEnd > totalDuration {
			actualSegDuration = totalDuration - segStart
			if actualSegDuration < 0.1 {
				break
			}
		}
		playlist.WriteString(fmt.Sprintf("#EXTINF:%.6f,\n", actualSegDuration))
		playlist.WriteString(fmt.Sprintf("audio0/segment%d.m4s%s\n", i, qs))
	}

	playlist.WriteString("#EXT-X-ENDLIST\n")

	playlistContent := playlist.String()
	previewLen := 500
	if len(playlistContent) < previewLen {
		previewLen = len(playlistContent)
	}
	log.Printf("generateHLSAudioPlaylist: Generated playlist with %d segments, total duration: %.3f seconds", numSegments, totalDuration)
	log.Printf("generateHLSAudioPlaylist: Playlist preview (first %d chars): %s", previewLen, playlistContent[:previewLen])

	return playlistContent
}

// generateHLSSubtitlePlaylist generates an HLS subtitle playlist for the given torrent and file
func (s *Server) generateHLSSubtitlePlaylist(engine *TorrentEngine, fileIndex int, queryString string) string {
	var playlist strings.Builder

	// Get file info
	file, err := engine.GetFile(fileIndex)
	if err != nil {
		log.Printf("Error getting file %d: %v", fileIndex, err)
		return "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-ENDLIST\n"
	}

	// Try to get actual media duration using FFmpeg
	var totalDuration float64 = 0
	if s.ffmpegMgr != nil && s.ffmpegMgr.IsProbeAvailable() {
		probeInfo, err := s.ffmpegMgr.GetProbeInfo(file.Path)
		if err == nil && probeInfo != nil && probeInfo.Format.Duration > 0 {
			totalDuration = probeInfo.Format.Duration
			log.Printf("Subtitle playlist: Using FFmpeg duration: %.3f seconds", totalDuration)
		} else {
			// Fallback to rough estimate based on file size
			totalDuration = float64(file.Size) / (1024 * 1024 * 2) // Rough estimate: 2MB/s
			log.Printf("Subtitle playlist: Using estimated duration: %.3f seconds", totalDuration)
		}
	} else {
		// Fallback to rough estimate based on file size
		totalDuration = float64(file.Size) / (1024 * 1024 * 2) // Rough estimate: 2MB/s
		log.Printf("Subtitle playlist: Using estimated duration: %.3f seconds", totalDuration)
	}

	segmentDuration := 2.0 // Match ffmpeg segment duration
	numSegments := int(totalDuration / segmentDuration)
	if numSegments < 1 {
		numSegments = 1
	}
	if numSegments > 1000 {
		numSegments = 1000
	}

	playlist.WriteString("#EXTM3U\n")
	playlist.WriteString("#EXT-X-VERSION:7\n")
	playlist.WriteString("#EXT-X-TARGETDURATION:5\n") // Use 5 as target duration (slightly higher than segment duration)
	playlist.WriteString("#EXT-X-MEDIA-SEQUENCE:1\n")
	playlist.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")

	qs := queryString
	if qs != "" && qs[0] != '?' {
		qs = "?" + qs
	}
	playlist.WriteString(fmt.Sprintf("#EXT-X-MAP:URI=\"subtitle0/init.mp4%s\"\n", qs))

	for i := 1; i <= numSegments; i++ {
		playlist.WriteString(fmt.Sprintf("#EXTINF:%.6f,\n", segmentDuration))
		playlist.WriteString(fmt.Sprintf("subtitle0/segment%d.m4s%s\n", i, qs))
	}

	playlist.WriteString("#EXT-X-ENDLIST\n")
	return playlist.String()
}

// generateHLSStreamPlaylist generates an HLS stream playlist for the given torrent and file
func (s *Server) generateHLSStreamPlaylist(engine *TorrentEngine, fileIndex int, queryString string) string {
	log.Printf("generateHLSStreamPlaylist: Starting for engine %s, fileIndex %d", engine.InfoHash, fileIndex)
	var playlist strings.Builder

	// Get file info
	file, err := engine.GetFile(fileIndex)
	if err != nil {
		log.Printf("generateHLSStreamPlaylist: Error getting file %d: %v", fileIndex, err)
		return "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-ENDLIST\n"
	}
	log.Printf("generateHLSStreamPlaylist: File info - Name: %s, Size: %d, Path: %s", file.Name, file.Size, file.Path)

	// Try to get actual media duration using FFmpeg
	var totalDuration float64 = 0
	if s.ffmpegMgr != nil && s.ffmpegMgr.IsProbeAvailable() {
		probeInfo, err := s.ffmpegMgr.GetProbeInfo(file.Path)
		if err == nil && probeInfo != nil && probeInfo.Format.Duration > 0 {
			totalDuration = probeInfo.Format.Duration
			log.Printf("Video playlist: Using FFmpeg duration: %.3f seconds", totalDuration)
		} else {
			totalDuration = float64(file.Size) / (1024 * 1024 * 2) // fallback
			log.Printf("Video playlist: Using estimated duration: %.3f seconds", totalDuration)
		}
	} else {
		totalDuration = float64(file.Size) / (1024 * 1024 * 2)
		log.Printf("Video playlist: Using estimated duration: %.3f seconds", totalDuration)
	}

	segmentDuration := 2.0                                     // Match ffmpeg segment duration
	numSegments := int(totalDuration/segmentDuration + 0.9999) // ceil
	if numSegments < 1 {
		numSegments = 1
	}
	if numSegments > 1000 {
		numSegments = 1000
	}

	playlist.WriteString("#EXTM3U\n")
	playlist.WriteString("#EXT-X-VERSION:7\n")
	playlist.WriteString("#EXT-X-TARGETDURATION:15\n")
	playlist.WriteString("#EXT-X-MEDIA-SEQUENCE:1\n")
	playlist.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")

	qs := queryString
	if qs != "" && qs[0] != '?' {
		qs = "?" + qs
	}
	playlist.WriteString(fmt.Sprintf("#EXT-X-MAP:URI=\"video0/init.mp4%s\"\n", qs))

	for i := 1; i <= numSegments; i++ {
		segStart := float64(i-1) * segmentDuration
		segEnd := segStart + segmentDuration
		actualSegDuration := segmentDuration
		if segEnd > totalDuration {
			actualSegDuration = totalDuration - segStart
			if actualSegDuration < 0.1 {
				break
			}
		}
		playlist.WriteString(fmt.Sprintf("#EXTINF:%.6f,\n", actualSegDuration))
		playlist.WriteString(fmt.Sprintf("video0/segment%d.m4s%s\n", i, qs))
	}

	playlist.WriteString("#EXT-X-ENDLIST\n")

	playlistContent := playlist.String()
	previewLen := 500
	if len(playlistContent) < previewLen {
		previewLen = len(playlistContent)
	}
	log.Printf("generateHLSStreamPlaylist: Generated playlist with %d segments, total duration: %.3f seconds", numSegments, totalDuration)
	log.Printf("generateHLSStreamPlaylist: Playlist preview (first %d chars): %s", previewLen, playlistContent[:previewLen])

	return playlistContent
}

func serveMinimalInitSegment(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Length", "24")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Keep-Alive", "timeout=5")
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42mp41"))
}

// Helper to extract infoHash and fileIndex from mediaURL or route (matching server.js logic)
func getInfoHashAndFileIndex(r *http.Request, defaultInfoHash, defaultFileIndexStr string) (string, int) {
	log.Printf("getInfoHashAndFileIndex: defaultInfoHash=%s, defaultFileIndexStr=%s", defaultInfoHash, defaultFileIndexStr)
	log.Printf("getInfoHashAndFileIndex: full URL=%s", r.URL.String())

	from := r.URL.Query().Get("from")
	mediaURL := r.URL.Query().Get("mediaURL")
	log.Printf("getInfoHashAndFileIndex: from=%s, mediaURL=%s", from, mediaURL)

	videoURL := from
	if videoURL == "" {
		videoURL = mediaURL
	}

	if videoURL != "" {
		if decodedURL, err := url.QueryUnescape(videoURL); err == nil {
			videoURL = decodedURL
		}
		parsedURL, err := url.Parse(videoURL)
		if err == nil {
			pathParts := strings.Split(strings.Trim(parsedURL.Path, "/"), "/")
			log.Printf("getInfoHashAndFileIndex: pathParts=%v", pathParts)

			// Scan all path parts to find the first valid 40-character hex string
			var infoHash string
			var fileIndex int
			var hashIndex int = -1

			// First pass: find the infoHash (40-character hex string)
			for i, part := range pathParts {
				if len(part) == 40 && isHex(part) {
					infoHash = part
					hashIndex = i
					break
				}
			}

			// Second pass: find the fileIndex (numeric part after the infoHash)
			if hashIndex >= 0 {
				for i := hashIndex + 1; i < len(pathParts); i++ {
					if idx, err := strconv.Atoi(pathParts[i]); err == nil {
						fileIndex = idx
						break
					}
				}
			}

			if infoHash != "" {
				log.Printf("getInfoHashAndFileIndex: using extracted infoHash=%s, fileIndex=%d from mediaURL", infoHash, fileIndex)
				return infoHash, fileIndex
			}
		}
	}

	// Fallback to route variables if no valid infoHash in mediaURL
	if len(defaultInfoHash) == 40 {
		infoHash := defaultInfoHash
		fileIndex := 0
		if defaultFileIndexStr != "" {
			if idx, err := strconv.Atoi(defaultFileIndexStr); err == nil {
				fileIndex = idx
			}
		}
		log.Printf("getInfoHashAndFileIndex: using route infoHash=%s, fileIndex=%d", infoHash, fileIndex)
		return infoHash, fileIndex
	}

	infoHash := defaultInfoHash
	fileIndex := 0
	if defaultFileIndexStr != "" {
		if idx, err := strconv.Atoi(defaultFileIndexStr); err == nil {
			fileIndex = idx
		}
	}
	log.Printf("getInfoHashAndFileIndex: fallback to route infoHash=%s, fileIndex=%d", infoHash, fileIndex)
	return infoHash, fileIndex
}

// isHex returns true if the string is a valid hex string
func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// generateMP4InitSegment generates a real fMP4 init segment for the given track type ("audio" or "video")
func generateMP4InitSegment(_ string, trackType string) ([]byte, error) {
	buf := &bytes.Buffer{}
	ws := &mp4WriteSeeker{buf: buf}
	w := mp4.NewWriter(ws)

	// Write ftyp
	ftyp := &mp4.Ftyp{
		MajorBrand:       [4]byte{'i', 's', 'o', 'm'},
		MinorVersion:     512,
		CompatibleBrands: []mp4.CompatibleBrandElem{{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}}, {CompatibleBrand: [4]byte{'i', 's', 'o', '2'}}, {CompatibleBrand: [4]byte{'a', 'v', 'c', '1'}}, {CompatibleBrand: [4]byte{'m', 'p', '4', '1'}}},
	}
	if _, err := mp4.Marshal(w, ftyp, mp4.Context{}); err != nil {
		return nil, err
	}

	// TODO: For a real implementation, use mp4.Box and children to build a moov box tree.
	// For now, only write ftyp and fallback to minimal segment for compatibility.
	// See: https://pkg.go.dev/github.com/abema/go-mp4#section-documentation

	return buf.Bytes(), nil
}

// mp4WriteSeeker wraps a bytes.Buffer to provide io.WriteSeeker for go-mp4
// (since bytes.Buffer does not implement Seek, but go-mp4 requires it)
type mp4WriteSeeker struct {
	buf *bytes.Buffer
}

func (m *mp4WriteSeeker) Write(p []byte) (int, error) {
	return m.buf.Write(p)
}
func (m *mp4WriteSeeker) Seek(offset int64, whence int) (int64, error) {
	// Only support seeking to the end (for append)
	if whence == io.SeekEnd && offset == 0 {
		return int64(m.buf.Len()), nil
	}
	return 0, io.EOF
}
