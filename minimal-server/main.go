package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
)

// Global configuration - change these values to serve different content
const (
	INFO_HASH  = "eadcdc5fcebef5c1304366758369d918df946fe7"
	FILE_INDEX = 0
	FILE_PATH  = "/home/vscode/.stremio-server/stremio-cache/eadcdc5fcebef5c1304366758369d918df946fe7/0"
	MEDIA_URL  = "http://127.0.0.1:11470/eadcdc5fcebef5c1304366758369d918df946fe7/0"

	// Segment configuration
	SEGMENT_DURATION          = 4.0  // seconds for audio/video
	SUBTITLE_SEGMENT_DURATION = 10.0 // seconds for subtitles
)

// Global segment cache (like JavaScript server)
var (
	segmentCache      = make(map[string][]byte) // key: "audio_5" or "video_5"
	segmentCacheMutex sync.RWMutex
	ffmpegProcesses   = make(map[string]*exec.Cmd) // trackType -> process
	ffmpegRunning     = make(map[string]bool)      // trackType -> running status
	ffmpegMutex       sync.Mutex
)

// MediaSegment represents a cached MP4 segment
type MediaSegment struct {
	Buffer         []byte
	SequenceNumber int
	Duration       float64
	SeekTime       float64
	TrackType      string // "audio" or "video"
}

type Server struct {
	router *mux.Router
}

type StatsResponse struct {
	InfoHash          string        `json:"infoHash"`
	Name              string        `json:"name"`
	Peers             int           `json:"peers"`
	Unchoked          int           `json:"unchoked"`
	Queued            int           `json:"queued"`
	Unique            int           `json:"unique"`
	ConnectionTries   int           `json:"connectionTries"`
	SwarmPaused       bool          `json:"swarmPaused"`
	SwarmConnections  int           `json:"swarmConnections"`
	SwarmSize         int           `json:"swarmSize"`
	Selections        []interface{} `json:"selections"`
	Wires             interface{}   `json:"wires"`
	Files             []FileInfo    `json:"files"`
	Downloaded        int64         `json:"downloaded"`
	Uploaded          int64         `json:"uploaded"`
	DownloadSpeed     int           `json:"downloadSpeed"`
	UploadSpeed       int           `json:"uploadSpeed"`
	Sources           []SourceInfo  `json:"sources"`
	PeerSearchRunning bool          `json:"peerSearchRunning"`
	Opts              OptsInfo      `json:"opts"`
	StreamProgress    float64       `json:"streamProgress"`
	StreamName        string        `json:"streamName"`
	StreamLen         int64         `json:"streamLen"`
}

type FileInfo struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Length      int64  `json:"length"`
	Offset      int    `json:"offset"`
	CacheEvents bool   `json:"__cacheEvents"`
}

type SourceInfo struct {
	NumFound     int    `json:"numFound"`
	NumFoundUniq int    `json:"numFoundUniq"`
	NumRequests  int    `json:"numRequests"`
	URL          string `json:"url"`
	LastStarted  string `json:"lastStarted"`
}

type OptsInfo struct {
	PeerSearch       PeerSearchInfo `json:"peerSearch"`
	DHT              bool           `json:"dht"`
	Tracker          bool           `json:"tracker"`
	Connections      int            `json:"connections"`
	HandshakeTimeout int            `json:"handshakeTimeout"`
	Timeout          int            `json:"timeout"`
	Virtual          bool           `json:"virtual"`
	SwarmCap         SwarmCapInfo   `json:"swarmCap"`
	Growler          GrowlerInfo    `json:"growler"`
	Path             string         `json:"path"`
	ID               string         `json:"id"`
}

type PeerSearchInfo struct {
	Min     int      `json:"min"`
	Max     int      `json:"max"`
	Sources []string `json:"sources"`
}

type SwarmCapInfo struct {
	MinPeers int `json:"minPeers"`
	MaxSpeed int `json:"maxSpeed"`
}

type GrowlerInfo struct {
	Flood int `json:"flood"`
	Pulse int `json:"pulse"`
}

type ProbeResponse struct {
	Format struct {
		Name     string  `json:"name"`
		Duration float64 `json:"duration"`
	} `json:"format"`
	Streams []StreamInfo           `json:"streams"`
	Samples map[string]interface{} `json:"samples"`
}

type StreamInfo struct {
	ID               int     `json:"id"`
	Index            int     `json:"index"`
	Track            string  `json:"track"`
	Codec            string  `json:"codec"`
	StreamBitRate    int     `json:"streamBitRate"`
	StreamMaxBitRate int     `json:"streamMaxBitRate"`
	StartTime        int     `json:"startTime"`
	StartTimeTs      int     `json:"startTimeTs"`
	Timescale        int     `json:"timescale"`
	Width            int     `json:"width,omitempty"`
	Height           int     `json:"height,omitempty"`
	FrameRate        float64 `json:"frameRate,omitempty"`
	NumberOfFrames   *int    `json:"numberOfFrames,omitempty"`
	IsHdr            bool    `json:"isHdr,omitempty"`
	IsDoVi           bool    `json:"isDoVi,omitempty"`
	HasBFrames       bool    `json:"hasBFrames,omitempty"`
	FormatBitRate    int     `json:"formatBitRate,omitempty"`
	FormatMaxBitRate int     `json:"formatMaxBitRate,omitempty"`
	Bps              int     `json:"bps,omitempty"`
	NumberOfBytes    int     `json:"numberOfBytes,omitempty"`
	FormatDuration   float64 `json:"formatDuration,omitempty"`
	SampleRate       int     `json:"sampleRate,omitempty"`
	Channels         int     `json:"channels,omitempty"`
	ChannelLayout    string  `json:"channelLayout,omitempty"`
	Title            *string `json:"title,omitempty"`
	Language         string  `json:"language,omitempty"`
}

func NewServer() *Server {
	s := &Server{
		router: mux.NewRouter(),
	}
	s.setupRoutes()
	return s
}

// startLongRunningFFmpeg starts a long-running ffmpeg process that continuously outputs MP4 segments
func startLongRunningFFmpeg(trackType string) error {
	ffmpegMutex.Lock()
	defer ffmpegMutex.Unlock()

	if ffmpegRunning[trackType] {
		log.Printf("FFmpeg process already running for %s", trackType)
		return nil
	}

	log.Printf("Starting long-running FFmpeg process for %s", trackType)

	// Build ffmpeg command for continuous output
	args := []string{
		"-fflags", "+genpts",
		"-noaccurate_seek",
		"-seek_timestamp", "1",
		"-copyts",
		"-seek2any", "1",
		"-ss", "0.0", // Start from beginning
		"-i", FILE_PATH,
		"-threads", "3",
		"-max_muxing_queue_size", "2048",
		"-ignore_unknown",
		"-map_metadata", "-1",
		"-map_chapters", "-1",
		"-map", "-0:d?",
		"-map", "-0:t?",
		"-t", "300", // Limit to 5 minutes for testing (should be configurable)
	}

	// Add track-specific arguments
	if trackType == "audio" {
		args = append(args,
			"-map", "-0:v?",
			"-map", "a:0",
			"-c:a", "aac",
			"-filter:a", "apad",
			"-async", "1",
			"-ac:a", "2",
			"-ab", "256000",
			"-ar:a", "48000",
			"-map", "-0:s?",
			"-frag_duration", "4096000", // 4.096 seconds in microseconds (matching the example)
		)
	} else if trackType == "video" {
		args = append(args,
			"-map", "v:0",
			"-c:v", "copy",
			"-force_key_frames:v", "source",
			"-map", "-0:a?",
			"-map", "-0:s?",
			"-frag_duration", "4000000", // 4 seconds in microseconds
		)
	}

	// Add common output arguments
	if trackType == "audio" {
		args = append(args,
			"-movflags", "empty_moov+default_base_moof+delay_moov+dash",
		)
	} else if trackType == "video" {
		args = append(args,
			"-movflags", "frag_keyframe+empty_moov+default_base_moof+delay_moov+dash",
		)
	}

	args = append(args,
		"-use_editlist", "1",
		"-f", "mp4",
		"pipe:1",
	)

	// Log the ffmpeg command for debugging
	log.Printf("FFmpeg command for %s: ffmpeg %v", trackType, args)

	// Create command
	ffmpegProcesses[trackType] = exec.Command("ffmpeg", args...)

	// Get stdout pipe
	stdout, err := ffmpegProcesses[trackType].StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %v", err)
	}

	// Get stderr pipe to capture error messages
	stderr, err := ffmpegProcesses[trackType].StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %v", err)
	}

	// Start the process
	if err := ffmpegProcesses[trackType].Start(); err != nil {
		return fmt.Errorf("failed to start ffmpeg: %v", err)
	}

	ffmpegRunning[trackType] = true

	// Start goroutine to read stderr and log error messages
	go func() {
		defer func() {
			log.Printf("FFmpeg stderr reader ended for %s", trackType)
		}()

		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			log.Printf("FFmpeg %s stderr: %s", trackType, scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			log.Printf("FFmpeg %s stderr scanner error: %v", trackType, err)
		}
	}()

	// Start goroutine to read and parse MP4 segments
	go func() {
		defer func() {
			ffmpegMutex.Lock()
			ffmpegRunning[trackType] = false
			ffmpegMutex.Unlock()
		}()

		log.Printf("FFmpeg process started, beginning MP4 segment parsing for %s", trackType)

		// Read MP4 data in chunks and parse segments
		buffer := make([]byte, 0, 100*1024*1024) // 100MB initial buffer
		segmentNumber := 1

		for {
			// Read chunk from ffmpeg
			chunk := make([]byte, 4096)
			n, err := stdout.Read(chunk)
			if err != nil {
				if err == io.EOF {
					log.Printf("FFmpeg process ended for %s", trackType)
				} else {
					log.Printf("Error reading from ffmpeg: %v", err)
				}
				break
			}

			// Add chunk to buffer
			buffer = append(buffer, chunk[:n]...)

			// Try to parse MP4 segments from buffer
			for {
				segmentData, remainingBuffer, err := parseMP4Segment(buffer, trackType, segmentNumber)
				if err != nil {
					// Not enough data for complete segment, continue reading
					break
				}

				// Cache the segment
				segmentKey := fmt.Sprintf("%s_%d", trackType, segmentNumber)
				segmentCacheMutex.Lock()
				segmentCache[segmentKey] = segmentData
				segmentCacheMutex.Unlock()

				log.Printf("Cached %s segment %d (size: %d bytes)", trackType, segmentNumber, len(segmentData))

				// Update buffer and segment number
				buffer = remainingBuffer
				segmentNumber++
			}

			// Prevent buffer from growing too large
			if len(buffer) > 500*1024*1024 { // 500MB limit
				log.Printf("Buffer too large, clearing for %s", trackType)
				buffer = buffer[:0]
			}
		}
	}()

	return nil
}

// parseMP4Segment attempts to parse a complete MP4 segment from the buffer
// For fragmented MP4 output from FFmpeg, each segment should be a complete MP4 fragment
func parseMP4Segment(buffer []byte, trackType string, segmentNumber int) ([]byte, []byte, error) {
	if len(buffer) < 8 {
		return nil, buffer, fmt.Errorf("buffer too small")
	}

	// For fragmented MP4, we need to find complete MP4 fragments
	// Each fragment should contain a moof box followed by an mdat box
	// Look for the first moof box
	moofIndex := bytes.Index(buffer, []byte("moof"))
	if moofIndex == -1 {
		return nil, buffer, fmt.Errorf("no moof box found")
	}

	// Find the start of the moof box (go back 4 bytes to get the box header)
	moofStart := moofIndex - 4
	if moofStart < 0 {
		return nil, buffer, fmt.Errorf("invalid moof box position")
	}

	// Read the moof box size
	moofSize := int(buffer[moofStart])<<24 | int(buffer[moofStart+1])<<16 | int(buffer[moofStart+2])<<8 | int(buffer[moofStart+3])
	if moofSize <= 0 || moofStart+moofSize > len(buffer) {
		return nil, buffer, fmt.Errorf("invalid moof box size")
	}

	// Look for the mdat box that follows the moof box
	mdatStart := moofStart + moofSize
	if mdatStart >= len(buffer)-8 {
		return nil, buffer, fmt.Errorf("not enough data for mdat box")
	}

	// Check if we have an mdat box
	mdatSize := int(buffer[mdatStart])<<24 | int(buffer[mdatStart+1])<<16 | int(buffer[mdatStart+2])<<8 | int(buffer[mdatStart+3])
	mdatType := string(buffer[mdatStart+4 : mdatStart+8])

	if mdatType != "mdat" {
		return nil, buffer, fmt.Errorf("expected mdat box after moof, found %s", mdatType)
	}

	if mdatSize <= 0 || mdatStart+mdatSize > len(buffer) {
		return nil, buffer, fmt.Errorf("invalid mdat box size")
	}

	// Calculate the complete segment size (moof + mdat)
	segmentEnd := mdatStart + mdatSize

	// Look for the next moof box to see if we have a complete segment
	nextMoofIndex := bytes.Index(buffer[segmentEnd:], []byte("moof"))

	if nextMoofIndex != -1 {
		// We found the next segment, so this segment is complete
		segmentSize := segmentEnd
		segmentData := make([]byte, segmentSize)
		copy(segmentData, buffer[:segmentSize])

		// Return remaining buffer
		remainingBuffer := make([]byte, len(buffer)-segmentSize)
		copy(remainingBuffer, buffer[segmentSize:])

		log.Printf("Parsed %s segment %d: %d bytes (moof at %d, mdat at %d, next moof at %d)",
			trackType, segmentNumber, segmentSize, moofStart, mdatStart, segmentEnd+nextMoofIndex)

		return segmentData, remainingBuffer, nil
	} else {
		// No next moof box found, check if we have enough data for a reasonable segment
		// For 4-second segments, we need at least some minimum size
		minSegmentSize := 64 * 1024 // 64KB minimum for a 4-second segment
		if segmentEnd >= minSegmentSize {
			// Extract the segment
			segmentSize := segmentEnd
			segmentData := make([]byte, segmentSize)
			copy(segmentData, buffer[:segmentSize])

			// Return remaining buffer
			remainingBuffer := make([]byte, len(buffer)-segmentSize)
			copy(remainingBuffer, buffer[segmentSize:])

			log.Printf("Parsed %s segment %d: %d bytes (moof at %d, mdat at %d, no next moof)",
				trackType, segmentNumber, segmentSize, moofStart, mdatStart)

			return segmentData, remainingBuffer, nil
		} else {
			// Not enough data yet
			return nil, buffer, fmt.Errorf("not enough data for complete segment (have %d, need %d)", segmentEnd, minSegmentSize)
		}
	}
}

// getSegmentFromCache retrieves a segment from the global cache
func getSegmentFromCache(trackType string, segmentNumber int) ([]byte, bool) {
	segmentKey := fmt.Sprintf("%s_%d", trackType, segmentNumber)
	segmentCacheMutex.RLock()
	defer segmentCacheMutex.RUnlock()

	segment, exists := segmentCache[segmentKey]
	return segment, exists
}

// getCacheStats returns cache statistics
func getCacheStats() map[string]interface{} {
	segmentCacheMutex.RLock()
	defer segmentCacheMutex.RUnlock()

	totalBytes := 0
	audioSegments := 0
	videoSegments := 0

	for key, segment := range segmentCache {
		totalBytes += len(segment)
		if strings.HasPrefix(key, "audio_") {
			audioSegments++
		} else if strings.HasPrefix(key, "video_") {
			videoSegments++
		}
	}

	return map[string]interface{}{
		"total_segments": len(segmentCache),
		"audio_segments": audioSegments,
		"video_segments": videoSegments,
		"total_bytes":    totalBytes,
		"total_mb":       float64(totalBytes) / 1024 / 1024,
	}
}

func (s *Server) setupRoutes() {
	// Enable CORS for all routes
	s.router.Use(s.corsMiddleware)

	// Stats endpoint - simplified since we only serve one file
	s.router.HandleFunc("/{infoHash}/{fileIndex}/stats.json", s.handleStats).Methods("GET", "HEAD", "OPTIONS")

	// Probe endpoint
	s.router.HandleFunc("/hlsv2/probe", s.handleProbe).Methods("GET", "HEAD", "OPTIONS")

	// HLS audio init segment endpoint
	s.router.HandleFunc("/hlsv2/{infoHash}/audio0/init.mp4", s.handleHLSAudioInit).Methods("GET", "HEAD", "OPTIONS")

	// HLS video init segment endpoint
	s.router.HandleFunc("/hlsv2/{infoHash}/video0/init.mp4", s.handleHLSVideoInit).Methods("GET", "HEAD", "OPTIONS")

	// HLS master playlist endpoint
	s.router.HandleFunc("/hlsv2/{infoHash}/master.m3u8", s.handleHLSMaster).Methods("GET", "HEAD", "OPTIONS")

	// HLS video0.m3u8 endpoint
	s.router.HandleFunc("/hlsv2/{infoHash}/video0.m3u8", s.handleHLSVideo0M3U8).Methods("GET", "HEAD", "OPTIONS")

	// HLS audio0.m3u8 endpoint
	s.router.HandleFunc("/hlsv2/{infoHash}/audio0.m3u8", s.handleHLSAudio0M3U8).Methods("GET", "HEAD", "OPTIONS")

	// HLS subtitle0.m3u8 endpoint
	s.router.HandleFunc("/hlsv2/{infoHash}/subtitle0.m3u8", s.handleHLSSubtitle0M3U8).Methods("GET", "HEAD", "OPTIONS")

	// HLS audio0 segment endpoint (generic segment number)
	s.router.HandleFunc("/hlsv2/{infoHash}/audio0/segment{segmentNumber:[0-9]+}.m4s", s.handleHLSAudioSegment).Methods("GET", "HEAD", "OPTIONS")

	// HLS video0 segment endpoint (generic segment number)
	s.router.HandleFunc("/hlsv2/{infoHash}/video0/segment{segmentNumber:[0-9]+}.m4s", s.handleHLSVideoSegment).Methods("GET", "HEAD", "OPTIONS")

	// HLS subtitle0 segment endpoint (generic segment number)
	s.router.HandleFunc("/hlsv2/{infoHash}/subtitle0/segment{segmentNumber:[0-9]+}.vtt", s.handleHLSSubtitleSegment).Methods("GET", "HEAD", "OPTIONS")

	// Direct file serving route (similar to main server)
	// s.router.HandleFunc("/{infoHash}/{fileIndex}", s.handleDirectFile).Methods("GET", "HEAD", "OPTIONS")

	// Casting endpoint
	s.router.HandleFunc("/casting", s.handleCasting).Methods("GET", "HEAD", "OPTIONS")

	// Health check endpoint
	s.router.HandleFunc("/health", s.handleHealth).Methods("GET", "OPTIONS")

	// Settings and info endpoints
	s.router.HandleFunc("/settings", s.handleSettings).Methods("GET", "POST", "OPTIONS")
	s.router.HandleFunc("/network-info", s.handleNetworkInfo).Methods("GET", "OPTIONS")
	s.router.HandleFunc("/device-info", s.handleDeviceInfo).Methods("GET", "OPTIONS")

	// Subtitle extraction endpoint
	s.router.HandleFunc("/opensubHash", s.handleOpenSubHash).Methods("GET", "OPTIONS")

	// Cache management endpoints
	s.router.HandleFunc("/cache/clear", s.handleCacheClear).Methods("GET")
	s.router.HandleFunc("/cache/stats", s.handleCacheStats).Methods("GET")
	s.router.HandleFunc("/cache/start", s.handleCacheStart).Methods("GET")
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set comprehensive CORS headers for all requests
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS, HEAD")
		w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, Origin, X-Requested-With")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Max-Age", "86400") // 24 hours

		// Handle preflight requests - return immediately with 200 OK
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			log.Printf("HTTP %s %s - %d (CORS preflight)", r.Method, r.URL.Path, http.StatusOK)
			return
		}

		// Create a response writer wrapper to capture status code
		responseWriter := &responseWriterWrapper{
			ResponseWriter: w,
			statusCode:     http.StatusOK, // Default status code
		}

		// Log the request start
		log.Printf("HTTP %s %s - Request started", r.Method, r.URL.Path)

		// Call the next handler
		next.ServeHTTP(responseWriter, r)

		// Log the response
		log.Printf("HTTP %s %s - %d", r.Method, r.URL.Path, responseWriter.statusCode)
	})
}

// responseWriterWrapper wraps http.ResponseWriter to capture status code
type responseWriterWrapper struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriterWrapper) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriterWrapper) Write(b []byte) (int, error) {
	return rw.ResponseWriter.Write(b)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	log.Printf("Stats requested for infoHash: %s, fileIndex: %d", INFO_HASH, FILE_INDEX)

	// Return the exact static stats response as requested
	stats := StatsResponse{
		InfoHash:         INFO_HASH,
		Name:             "Rick.and.Morty.S08E09.720p.HEVC.x265-MeGusta[EZTVx.to].mkv",
		Peers:            10,
		Unchoked:         0,
		Queued:           370,
		Unique:           504,
		ConnectionTries:  7919,
		SwarmPaused:      false,
		SwarmConnections: 200,
		SwarmSize:        200,
		Selections:       []interface{}{},
		Wires:            nil,
		Files: []FileInfo{
			{
				Path:        "Rick.and.Morty.S08E09.720p.HEVC.x265-MeGusta[EZTVx.to].mkv",
				Name:        "Rick.and.Morty.S08E09.720p.HEVC.x265-MeGusta[EZTVx.to].mkv",
				Length:      211537424,
				Offset:      0,
				CacheEvents: true,
			},
		},
		Downloaded:    214781456,
		Uploaded:      4096000,
		DownloadSpeed: 0,
		UploadSpeed:   0,
		Sources: []SourceInfo{
			{
				NumFound:     400,
				NumFoundUniq: 274,
				NumRequests:  13,
				URL:          "tracker:http://tracker.opentrackr.org:1337/announce",
				LastStarted:  "2025-07-23T12:16:42.809Z",
			},
			{
				NumFound:     0,
				NumFoundUniq: 0,
				NumRequests:  13,
				URL:          "tracker:udp://tracker.auctor.tv:6969/announce",
				LastStarted:  "2025-07-23T12:16:42.810Z",
			},
			{
				NumFound:     0,
				NumFoundUniq: 0,
				NumRequests:  13,
				URL:          "tracker:udp://opentracker.i2p.rocks:6969/announce",
				LastStarted:  "2025-07-23T12:16:42.810Z",
			},
			{
				NumFound:     0,
				NumFoundUniq: 0,
				NumRequests:  13,
				URL:          "tracker:https://opentracker.i2p.rocks:443/announce",
				LastStarted:  "2025-07-23T12:16:42.812Z",
			},
			{
				NumFound:     50,
				NumFoundUniq: 48,
				NumRequests:  13,
				URL:          "tracker:udp://open.demonii.com:1337/announce",
				LastStarted:  "2025-07-23T12:16:42.812Z",
			},
			{
				NumFound:     650,
				NumFoundUniq: 389,
				NumRequests:  13,
				URL:          "tracker:udp://open.stealth.si:80/announce",
				LastStarted:  "2025-07-23T12:16:42.812Z",
			},
			{
				NumFound:     650,
				NumFoundUniq: 360,
				NumRequests:  13,
				URL:          "tracker:udp://tracker.torrent.eu.org:451/announce",
				LastStarted:  "2025-07-23T12:16:42.812Z",
			},
			{
				NumFound:     0,
				NumFoundUniq: 0,
				NumRequests:  13,
				URL:          "tracker:udp://tracker.moeking.me:6969/announce",
				LastStarted:  "2025-07-23T12:16:42.812Z",
			},
			{
				NumFound:     450,
				NumFoundUniq: 224,
				NumRequests:  13,
				URL:          "tracker:udp://exodus.desync.com:6969/announce",
				LastStarted:  "2025-07-23T12:16:42.812Z",
			},
			{
				NumFound:     0,
				NumFoundUniq: 0,
				NumRequests:  13,
				URL:          "tracker:udp://tracker1.bt.moack.co.kr:80/announce",
				LastStarted:  "2025-07-23T12:16:42.812Z",
			},
			{
				NumFound:     0,
				NumFoundUniq: 0,
				NumRequests:  13,
				URL:          "tracker:udp://tracker.tiny-vps.com:6969/announce",
				LastStarted:  "2025-07-23T12:16:42.813Z",
			},
			{
				NumFound:     0,
				NumFoundUniq: 0,
				NumRequests:  13,
				URL:          "tracker:udp://tracker.skyts.net:6969/announce",
				LastStarted:  "2025-07-23T12:16:42.813Z",
			},
			{
				NumFound:     0,
				NumFoundUniq: 0,
				NumRequests:  13,
				URL:          "tracker:udp://open.tracker.ink:6969/announce",
				LastStarted:  "2025-07-23T12:16:42.813Z",
			},
			{
				NumFound:     0,
				NumFoundUniq: 0,
				NumRequests:  13,
				URL:          "tracker:udp://movies.zsw.ca:6969/announce",
				LastStarted:  "2025-07-23T12:16:42.813Z",
			},
			{
				NumFound:     550,
				NumFoundUniq: 234,
				NumRequests:  13,
				URL:          "tracker:udp://explodie.org:6969/announce",
				LastStarted:  "2025-07-23T12:16:42.813Z",
			},
			{
				NumFound:     0,
				NumFoundUniq: 0,
				NumRequests:  13,
				URL:          "tracker:https://tracker.tamersunion.org:443/announce",
				LastStarted:  "2025-07-23T12:16:42.814Z",
			},
			{
				NumFound:     50,
				NumFoundUniq: 38,
				NumRequests:  13,
				URL:          "tracker:udp://tracker2.dler.org:80/announce",
				LastStarted:  "2025-07-23T12:16:42.815Z",
			},
			{
				NumFound:     0,
				NumFoundUniq: 0,
				NumRequests:  13,
				URL:          "tracker:udp://tracker.therarbg.com:6969/announce",
				LastStarted:  "2025-07-23T12:16:42.815Z",
			},
			{
				NumFound:     300,
				NumFoundUniq: 23,
				NumRequests:  13,
				URL:          "tracker:udp://tracker.theoks.net:6969/announce",
				LastStarted:  "2025-07-23T12:16:42.815Z",
			},
			{
				NumFound:     300,
				NumFoundUniq: 156,
				NumRequests:  13,
				URL:          "tracker:udp://tracker.qu.ax:6969/announce",
				LastStarted:  "2025-07-23T12:16:42.815Z",
			},
			{
				NumFound:     4736,
				NumFoundUniq: 1846,
				NumRequests:  7,
				URL:          "dht:eadcdc5fcebef5c1304366758369d918df946fe7",
				LastStarted:  "2025-07-23T12:16:42.815Z",
			},
		},
		PeerSearchRunning: false,
		Opts: OptsInfo{
			PeerSearch: PeerSearchInfo{
				Min: 40,
				Max: 150,
				Sources: []string{
					"tracker:http://tracker.opentrackr.org:1337/announce",
					"tracker:udp://tracker.auctor.tv:6969/announce",
					"tracker:udp://opentracker.i2p.rocks:6969/announce",
					"tracker:https://opentracker.i2p.rocks:443/announce",
					"tracker:udp://open.demonii.com:1337/announce",
					"tracker:udp://open.stealth.si:80/announce",
					"tracker:udp://tracker.torrent.eu.org:451/announce",
					"tracker:udp://tracker.moeking.me:6969/announce",
					"tracker:udp://exodus.desync.com:6969/announce",
					"tracker:udp://tracker1.bt.moack.co.kr:80/announce",
					"tracker:udp://tracker.tiny-vps.com:6969/announce",
					"tracker:udp://tracker.skyts.net:6969/announce",
					"tracker:udp://open.tracker.ink:6969/announce",
					"tracker:udp://movies.zsw.ca:6969/announce",
					"tracker:udp://explodie.org:6969/announce",
					"tracker:https://tracker.tamersunion.org:443/announce",
					"tracker:udp://tracker2.dler.org:80/announce",
					"tracker:udp://tracker.therarbg.com:6969/announce",
					"tracker:udp://tracker.theoks.net:6969/announce",
					"tracker:udp://tracker.qu.ax:6969/announce",
					"dht:eadcdc5fcebef5c1304366758369d918df946fe7",
				},
			},
			DHT:              false,
			Tracker:          false,
			Connections:      200,
			HandshakeTimeout: 20000,
			Timeout:          4000,
			Virtual:          true,
			SwarmCap: SwarmCapInfo{
				MinPeers: 10,
				MaxSpeed: 4194304,
			},
			Growler: GrowlerInfo{
				Flood: 0,
				Pulse: 39321600,
			},
			Path: "/root/.stremio-server/stremio-cache/eadcdc5fcebef5c1304366758369d918df946fe7",
			ID:   "-qB2500-322b7963b462",
		},
		StreamProgress: 1,
		StreamName:     "Rick.and.Morty.S08E09.720p.HEVC.x265-MeGusta[EZTVx.to].mkv",
		StreamLen:      211537424,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	log.Printf("Probe requested")

	// Return the exact static probe response as requested
	probe := ProbeResponse{
		Format: struct {
			Name     string  `json:"name"`
			Duration float64 `json:"duration"`
		}{
			Name:     "matroska,webm",
			Duration: 1321.504,
		},
		Streams: []StreamInfo{
			{
				ID:               0,
				Index:            0,
				Track:            "video",
				Codec:            "hevc",
				StreamBitRate:    0,
				StreamMaxBitRate: 0,
				StartTime:        0,
				StartTimeTs:      0,
				Timescale:        1000,
				Width:            1280,
				Height:           720,
				FrameRate:        23.976023976023978,
				NumberOfFrames:   nil,
				IsHdr:            false,
				IsDoVi:           false,
				HasBFrames:       true,
				FormatBitRate:    1280585,
				FormatMaxBitRate: 0,
				Bps:              2424158,
				NumberOfBytes:    400436785,
				FormatDuration:   1321.504,
			},
			{
				ID:               0,
				Index:            1,
				Track:            "audio",
				Codec:            "eac3",
				StreamBitRate:    256000,
				StreamMaxBitRate: 0,
				StartTime:        0,
				StartTimeTs:      0,
				Timescale:        1000,
				SampleRate:       48000,
				Channels:         6,
				ChannelLayout:    "5.1(side)",
				Title:            nil,
				Language:         "eng",
			},
			{
				ID:               0,
				Index:            2,
				Track:            "subtitle",
				Codec:            "ass",
				StreamBitRate:    0,
				StreamMaxBitRate: 0,
				StartTime:        0,
				StartTimeTs:      0,
				Timescale:        1000,
				Title:            &[]string{"English"}[0],
				Language:         "eng",
			},
		},
		Samples: map[string]interface{}{},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(probe)
}

func (s *Server) handleHLSAudioInit(w http.ResponseWriter, r *http.Request) {
	log.Printf("HLS Audio Init requested for infoHash: %s, fileIndex: %d", INFO_HASH, FILE_INDEX)

	// Return the specific audio init segment from base64 data
	// This is a complete MP4 init segment with proper audio track configuration
	initSegment := []byte{
		0x00, 0x00, 0x00, 0x1c, 0x66, 0x74, 0x79, 0x70, 0x69, 0x73, 0x6f, 0x35, 0x00, 0x00, 0x02, 0x00,
		0x69, 0x73, 0x6f, 0x35, 0x69, 0x73, 0x6f, 0x36, 0x6d, 0x70, 0x34, 0x31, 0x00, 0x00, 0x02, 0xe1,
		0x6d, 0x6f, 0x6f, 0x76, 0x00, 0x00, 0x00, 0x6c, 0x6d, 0x76, 0x68, 0x64, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0xe8, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x40, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x01, 0xe3,
		0x74, 0x72, 0x61, 0x6b, 0x00, 0x00, 0x00, 0x5c, 0x74, 0x6b, 0x68, 0x64, 0x00, 0x00, 0x00, 0x03,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01,
		0x01, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x40, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x24, 0x65, 0x64, 0x74, 0x73, 0x00, 0x00, 0x00, 0x1c, 0x65, 0x6c, 0x73, 0x74,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x04, 0x00,
		0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x01, 0x5b, 0x6d, 0x64, 0x69, 0x61, 0x00, 0x00, 0x00, 0x20,
		0x6d, 0x64, 0x68, 0x64, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0xbb, 0x80, 0x00, 0x00, 0x00, 0x00, 0x55, 0xc4, 0x00, 0x00, 0x00, 0x00, 0x00, 0x2d,
		0x68, 0x64, 0x6c, 0x72, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x73, 0x6f, 0x75, 0x6e,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x53, 0x6f, 0x75, 0x6e,
		0x64, 0x48, 0x61, 0x6e, 0x64, 0x6c, 0x65, 0x72, 0x00, 0x00, 0x00, 0x01, 0x06, 0x6d, 0x69, 0x6e,
		0x66, 0x00, 0x00, 0x00, 0x10, 0x73, 0x6d, 0x68, 0x64, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x24, 0x64, 0x69, 0x6e, 0x66, 0x00, 0x00, 0x00, 0x1c, 0x64, 0x72, 0x65,
		0x66, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x0c, 0x75, 0x72, 0x6c,
		0x20, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0xca, 0x73, 0x74, 0x62, 0x6c, 0x00, 0x00, 0x00,
		0x7e, 0x73, 0x74, 0x73, 0x64, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00,
		0x6e, 0x6d, 0x70, 0x34, 0x61, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0x00, 0x10, 0x00, 0x00, 0x00, 0x00, 0xbb, 0x80, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x36, 0x65, 0x73, 0x64, 0x73, 0x00, 0x00, 0x00, 0x00, 0x03, 0x80, 0x80,
		0x80, 0x25, 0x00, 0x01, 0x00, 0x04, 0x80, 0x80, 0x80, 0x17, 0x40, 0x15, 0x00, 0x00, 0x00, 0x00,
		0x03, 0xe8, 0x00, 0x00, 0x03, 0xe8, 0x00, 0x05, 0x80, 0x80, 0x80, 0x05, 0x11, 0x90, 0x56, 0xe5,
		0x00, 0x06, 0x80, 0x80, 0x80, 0x01, 0x02, 0x00, 0x00, 0x00, 0x14, 0x62, 0x74, 0x72, 0x74, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x03, 0xe8, 0x00, 0x00, 0x03, 0xe8, 0x00, 0x00, 0x00, 0x00, 0x10, 0x73,
		0x74, 0x74, 0x73, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x73, 0x74,
		0x73, 0x63, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x14, 0x73, 0x74, 0x73,
		0x7a, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10,
		0x73, 0x74, 0x63, 0x6f, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x28, 0x6d,
		0x76, 0x65, 0x78, 0x00, 0x00, 0x00, 0x20, 0x74, 0x72, 0x65, 0x78, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x62, 0x75, 0x64, 0x74, 0x61, 0x00, 0x00, 0x00, 0x5a, 0x6d, 0x65,
		0x74, 0x61, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x21, 0x68, 0x64, 0x6c, 0x72, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x6d, 0x64, 0x69, 0x72, 0x61, 0x70, 0x70, 0x6c, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x2d, 0x69, 0x6c, 0x73, 0x74, 0x00, 0x00,
		0x00, 0x25, 0xa9, 0x74, 0x6f, 0x6f, 0x00, 0x00, 0x00, 0x1d, 0x64, 0x61, 0x74, 0x61, 0x00, 0x00,
		0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x4c, 0x61, 0x76, 0x66, 0x35, 0x38, 0x2e, 0x37, 0x36, 0x2e,
		0x31, 0x30, 0x30,
	}

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Length", strconv.Itoa(len(initSegment)))
	w.Write(initSegment)
}
func (s *Server) handleHLSVideoInit(w http.ResponseWriter, r *http.Request) {
	log.Printf("HLS Video Init requested for infoHash: %s, fileIndex: %d", INFO_HASH, FILE_INDEX)
	// Return the specific MP4 init segment for video
	initSegment := []byte{
		0x00, 0x00, 0x00, 0x1c, 0x66, 0x74, 0x79, 0x70, 0x69, 0x73, 0x6f, 0x35,
		0x00, 0x00, 0x02, 0x00, 0x69, 0x73, 0x6f, 0x35, 0x69, 0x73, 0x6f, 0x36,
		0x6d, 0x70, 0x34, 0x31, 0x00, 0x00, 0x0c, 0xb3, 0x6d, 0x6f, 0x6f, 0x76,
		0x00, 0x00, 0x00, 0x6c, 0x6d, 0x76, 0x68, 0x64, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0xe8,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x40, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02,
		0x00, 0x00, 0x0b, 0xb5, 0x74, 0x72, 0x61, 0x6b, 0x00, 0x00, 0x00, 0x5c,
		0x74, 0x6b, 0x68, 0x64, 0x00, 0x00, 0x00, 0x03, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x40, 0x00, 0x00, 0x00, 0x05, 0x00, 0x00, 0x00,
		0x02, 0xd0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x24, 0x65, 0x64, 0x74, 0x73,
		0x00, 0x00, 0x00, 0x1c, 0x65, 0x6c, 0x73, 0x74, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x05, 0x37,
		0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x0b, 0x2d, 0x6d, 0x64, 0x69, 0x61,
		0x00, 0x00, 0x00, 0x20, 0x6d, 0x64, 0x68, 0x64, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x3e, 0x80,
		0x00, 0x00, 0x00, 0x00, 0x55, 0xc4, 0x00, 0x00, 0x00, 0x00, 0x00, 0x2d,
		0x68, 0x64, 0x6c, 0x72, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x76, 0x69, 0x64, 0x65, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x56, 0x69, 0x64, 0x65, 0x6f, 0x48, 0x61, 0x6e,
		0x64, 0x6c, 0x65, 0x72, 0x00, 0x00, 0x00, 0x0a, 0xd8, 0x6d, 0x69, 0x6e,
		0x66, 0x00, 0x00, 0x00, 0x14, 0x76, 0x6d, 0x68, 0x64, 0x00, 0x00, 0x00,
		0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x24, 0x64, 0x69, 0x6e, 0x66, 0x00, 0x00, 0x00, 0x1c, 0x64, 0x72, 0x65,
		0x66, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00,
		0x0c, 0x75, 0x72, 0x6c, 0x20, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x0a,
		0x98, 0x73, 0x74, 0x62, 0x6c, 0x00, 0x00, 0x0a, 0x4c, 0x73, 0x74, 0x73,
		0x64, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x0a,
		0x3c, 0x68, 0x65, 0x76, 0x31, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x05, 0x00, 0x02, 0xd0, 0x00, 0x48, 0x00,
		0x00, 0x00, 0x48, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x18, 0xff, 0xff, 0x00,
		0x00, 0x09, 0xb9, 0x68, 0x76, 0x63, 0x43, 0x01, 0x02, 0x20, 0x00, 0x00,
		0x00, 0x90, 0x00, 0x00, 0x00, 0x00, 0x00, 0x5d, 0xf0, 0x00, 0xfc, 0xfd,
		0xfa, 0xfa, 0x00, 0x00, 0x0f, 0x04, 0x20, 0x00, 0x01, 0x00, 0x18, 0x40,
		0x01, 0x0c, 0x01, 0xff, 0xff, 0x02, 0x20, 0x00, 0x00, 0x03, 0x00, 0x90,
		0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x5d, 0x95, 0x94, 0x09, 0x21,
		0x00, 0x01, 0x00, 0x2d, 0x42, 0x01, 0x01, 0x02, 0x20, 0x00, 0x00, 0x03,
		0x00, 0x90, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x5d, 0xa0, 0x02,
		0x80, 0x80, 0x2d, 0x13, 0x65, 0x95, 0x95, 0x29, 0x30, 0xbc, 0x05, 0xa8,
		0x08, 0x08, 0x0f, 0x08, 0x00, 0x00, 0x1f, 0x48, 0x00, 0x02, 0xee, 0x00,
		0x40, 0x22, 0x00, 0x01, 0x00, 0x06, 0x44, 0x01, 0xc0, 0x73, 0xc1, 0x89,
		0x27, 0x00, 0x01, 0x09, 0x3b, 0x4e, 0x01, 0x05, 0xff, 0xff, 0xff, 0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0x36, 0x2c, 0xa2, 0xde, 0x09, 0xb5, 0x17,
		0x47, 0xdb, 0xbb, 0x55, 0xa4, 0xfe, 0x7f, 0xc2, 0xfc, 0x4e, 0x78, 0x32,
		0x36, 0x35, 0x20, 0x28, 0x62, 0x75, 0x69, 0x6c, 0x64, 0x20, 0x32, 0x30,
		0x39, 0x29, 0x20, 0x2d, 0x20, 0x33, 0x2e, 0x36, 0x2b, 0x33, 0x36, 0x2d,
		0x32, 0x63, 0x64, 0x64, 0x36, 0x39, 0x61, 0x36, 0x64, 0x3a, 0x5b, 0x57,
		0x69, 0x6e, 0x64, 0x6f, 0x77, 0x73, 0x5d, 0x5b, 0x47, 0x43, 0x43, 0x20,
		0x31, 0x34, 0x2e, 0x31, 0x2e, 0x30, 0x5d, 0x5b, 0x36, 0x34, 0x20, 0x62,
		0x69, 0x74, 0x5d, 0x20, 0x31, 0x30, 0x62, 0x69, 0x74, 0x20, 0x2d, 0x20,
		0x48, 0x2e, 0x32, 0x36, 0x35, 0x2f, 0x48, 0x45, 0x56, 0x43, 0x20, 0x63,
		0x6f, 0x64, 0x65, 0x63, 0x20, 0x2d, 0x20, 0x43, 0x6f, 0x70, 0x79, 0x72,
		0x69, 0x67, 0x68, 0x74, 0x20, 0x32, 0x30, 0x31, 0x33, 0x2d, 0x32, 0x30,
		0x31, 0x38, 0x20, 0x28, 0x63, 0x29, 0x20, 0x4d, 0x75, 0x6c, 0x74, 0x69,
		0x63, 0x6f, 0x72, 0x65, 0x77, 0x61, 0x72, 0x65, 0x2c, 0x20, 0x49, 0x6e,
		0x63, 0x20, 0x2d, 0x20, 0x68, 0x74, 0x74, 0x70, 0x3a, 0x2f, 0x2f, 0x78,
		0x32, 0x36, 0x35, 0x2e, 0x6f, 0x72, 0x67, 0x20, 0x2d, 0x20, 0x6f, 0x70,
		0x74, 0x69, 0x6f, 0x6e, 0x73, 0x3a, 0x20, 0x63, 0x70, 0x75, 0x69, 0x64,
		0x3d, 0x31, 0x31, 0x31, 0x31, 0x30, 0x33, 0x39, 0x20, 0x66, 0x72, 0x61,
		0x6d, 0x65, 0x2d, 0x74, 0x68, 0x72, 0x65, 0x61, 0x64, 0x73, 0x3d, 0x34,
		0x20, 0x6e, 0x75, 0x6d, 0x61, 0x2d, 0x70, 0x6f, 0x6f, 0x6c, 0x73, 0x3d,
		0x31, 0x36, 0x20, 0x77, 0x70, 0x70, 0x20, 0x6e, 0x6f, 0x2d, 0x70, 0x6d,
		0x6f, 0x64, 0x65, 0x20, 0x6e, 0x6f, 0x2d, 0x70, 0x6d, 0x65, 0x20, 0x6e,
		0x6f, 0x2d, 0x70, 0x73, 0x6e, 0x72, 0x20, 0x6e, 0x6f, 0x2d, 0x73, 0x73,
		0x69, 0x6d, 0x20, 0x6c, 0x6f, 0x67, 0x2d, 0x6c, 0x65, 0x76, 0x65, 0x6c,
		0x3d, 0x32, 0x20, 0x62, 0x69, 0x74, 0x64, 0x65, 0x70, 0x74, 0x68, 0x3d,
		0x31, 0x30, 0x20, 0x69, 0x6e, 0x70, 0x75, 0x74, 0x2d, 0x63, 0x73, 0x70,
		0x3d, 0x31, 0x20, 0x66, 0x70, 0x73, 0x3d, 0x32, 0x34, 0x30, 0x30, 0x30,
		0x2f, 0x31, 0x30, 0x30, 0x31, 0x20, 0x69, 0x6e, 0x70, 0x75, 0x74, 0x2d,
		0x72, 0x65, 0x73, 0x3d, 0x31, 0x32, 0x38, 0x30, 0x78, 0x37, 0x32, 0x30,
		0x20, 0x69, 0x6e, 0x74, 0x65, 0x72, 0x6c, 0x61, 0x63, 0x65, 0x3d, 0x30,
		0x20, 0x74, 0x6f, 0x74, 0x61, 0x6c, 0x2d, 0x66, 0x72, 0x61, 0x6d, 0x65,
		0x73, 0x3d, 0x30, 0x20, 0x6c, 0x65, 0x76, 0x65, 0x6c, 0x2d, 0x69, 0x64,
		0x63, 0x3d, 0x30, 0x20, 0x68, 0x69, 0x67, 0x68, 0x2d, 0x74, 0x69, 0x65,
		0x72, 0x3d, 0x31, 0x20, 0x75, 0x68, 0x64, 0x2d, 0x62, 0x64, 0x3d, 0x30,
		0x20, 0x72, 0x65, 0x66, 0x3d, 0x31, 0x20, 0x6e, 0x6f, 0x2d, 0x61, 0x6c,
		0x6c, 0x6f, 0x77, 0x2d, 0x6e, 0x6f, 0x6e, 0x2d, 0x63, 0x6f, 0x6e, 0x66,
		0x6f, 0x72, 0x6d, 0x61, 0x6e, 0x63, 0x65, 0x20, 0x6e, 0x6f, 0x2d, 0x72,
		0x65, 0x70, 0x65, 0x61, 0x74, 0x2d, 0x68, 0x65, 0x61, 0x64, 0x65, 0x72,
		0x73, 0x20, 0x61, 0x6e, 0x6e, 0x65, 0x78, 0x62, 0x20, 0x6e, 0x6f, 0x2d,
		0x61, 0x75, 0x64, 0x20, 0x6e, 0x6f, 0x2d, 0x65, 0x6f, 0x62, 0x20, 0x6e,
		0x6f, 0x2d, 0x65, 0x6f, 0x73, 0x20, 0x6e, 0x6f, 0x2d, 0x68, 0x72, 0x64,
		0x20, 0x69, 0x6e, 0x66, 0x6f, 0x20, 0x68, 0x61, 0x73, 0x68, 0x3d, 0x30,
		0x20, 0x74, 0x65, 0x6d, 0x70, 0x6f, 0x72, 0x61, 0x6c, 0x2d, 0x6c, 0x61,
		0x79, 0x65, 0x72, 0x73, 0x3d, 0x30, 0x20, 0x6f, 0x70, 0x65, 0x6e, 0x2d,
		0x67, 0x6f, 0x70, 0x20, 0x6d, 0x69, 0x6e, 0x2d, 0x6b, 0x65, 0x79, 0x69,
		0x6e, 0x74, 0x3d, 0x32, 0x33, 0x20, 0x6b, 0x65, 0x79, 0x69, 0x6e, 0x74,
		0x3d, 0x32, 0x35, 0x30, 0x20, 0x67, 0x6f, 0x70, 0x2d, 0x6c, 0x6f, 0x6f,
		0x6b, 0x61, 0x68, 0x65, 0x61, 0x64, 0x3d, 0x30, 0x20, 0x62, 0x66, 0x72,
		0x61, 0x6d, 0x65, 0x73, 0x3d, 0x33, 0x20, 0x62, 0x2d, 0x61, 0x64, 0x61,
		0x70, 0x74, 0x3d, 0x30, 0x20, 0x62, 0x2d, 0x70, 0x79, 0x72, 0x61, 0x6d,
		0x69, 0x64, 0x20, 0x62, 0x66, 0x72, 0x61, 0x6d, 0x65, 0x2d, 0x62, 0x69,
		0x61, 0x73, 0x3d, 0x30, 0x20, 0x72, 0x63, 0x2d, 0x6c, 0x6f, 0x6f, 0x6b,
		0x61, 0x68, 0x65, 0x61, 0x64, 0x3d, 0x35, 0x20, 0x6c, 0x6f, 0x6f, 0x6b,
		0x61, 0x68, 0x65, 0x61, 0x64, 0x2d, 0x73, 0x6c, 0x69, 0x63, 0x65, 0x73,
		0x3d, 0x34, 0x20, 0x73, 0x63, 0x65, 0x6e, 0x65, 0x63, 0x75, 0x74, 0x3d,
		0x30, 0x20, 0x6e, 0x6f, 0x2d, 0x68, 0x69, 0x73, 0x74, 0x2d, 0x73, 0x63,
		0x65, 0x6e, 0x65, 0x63, 0x75, 0x74, 0x20, 0x72, 0x61, 0x64, 0x6c, 0x3d,
		0x30, 0x20, 0x6e, 0x6f, 0x2d, 0x73, 0x70, 0x6c, 0x69, 0x63, 0x65, 0x20,
		0x6e, 0x6f, 0x2d, 0x69, 0x6e, 0x74, 0x72, 0x61, 0x2d, 0x72, 0x65, 0x66,
		0x72, 0x65, 0x73, 0x68, 0x20, 0x63, 0x74, 0x75, 0x3d, 0x33, 0x32, 0x20,
		0x6d, 0x69, 0x6e, 0x2d, 0x63, 0x75, 0x2d, 0x73, 0x69, 0x7a, 0x65, 0x3d,
		0x31, 0x36, 0x20, 0x6e, 0x6f, 0x2d, 0x72, 0x65, 0x63, 0x74, 0x20, 0x6e,
		0x6f, 0x2d, 0x61, 0x6d, 0x70, 0x20, 0x6d, 0x61, 0x78, 0x2d, 0x74, 0x75,
		0x2d, 0x73, 0x69, 0x7a, 0x65, 0x3d, 0x33, 0x32, 0x20, 0x74, 0x75, 0x2d,
		0x69, 0x6e, 0x74, 0x65, 0x72, 0x2d, 0x64, 0x65, 0x70, 0x74, 0x68, 0x3d,
		0x31, 0x20, 0x74, 0x75, 0x2d, 0x69, 0x6e, 0x74, 0x72, 0x61, 0x2d, 0x64,
		0x65, 0x70, 0x74, 0x68, 0x3d, 0x31, 0x20, 0x6c, 0x69, 0x6d, 0x69, 0x74,
		0x2d, 0x74, 0x75, 0x3d, 0x30, 0x20, 0x72, 0x64, 0x6f, 0x71, 0x2d, 0x6c,
		0x65, 0x76, 0x65, 0x6c, 0x3d, 0x30, 0x20, 0x64, 0x79, 0x6e, 0x61, 0x6d,
		0x69, 0x63, 0x2d, 0x72, 0x64, 0x3d, 0x30, 0x2e, 0x30, 0x30, 0x20, 0x6e,
		0x6f, 0x2d, 0x73, 0x73, 0x69, 0x6d, 0x2d, 0x72, 0x64, 0x20, 0x6e, 0x6f,
		0x2d, 0x73, 0x69, 0x67, 0x6e, 0x68, 0x69, 0x64, 0x65, 0x20, 0x6e, 0x6f,
		0x2d, 0x74, 0x73, 0x6b, 0x69, 0x70, 0x20, 0x6e, 0x72, 0x2d, 0x69, 0x6e,
		0x74, 0x72, 0x61, 0x3d, 0x30, 0x20, 0x6e, 0x72, 0x2d, 0x69, 0x6e, 0x74,
		0x65, 0x72, 0x3d, 0x30, 0x20, 0x6e, 0x6f, 0x2d, 0x63, 0x6f, 0x6e, 0x73,
		0x74, 0x72, 0x61, 0x69, 0x6e, 0x65, 0x64, 0x2d, 0x69, 0x6e, 0x74, 0x72,
		0x61, 0x20, 0x73, 0x74, 0x72, 0x6f, 0x6e, 0x67, 0x2d, 0x69, 0x6e, 0x74,
		0x72, 0x61, 0x2d, 0x73, 0x6d, 0x6f, 0x6f, 0x74, 0x68, 0x69, 0x6e, 0x67,
		0x20, 0x6d, 0x61, 0x78, 0x2d, 0x6d, 0x65, 0x72, 0x67, 0x65, 0x3d, 0x32,
		0x20, 0x6c, 0x69, 0x6d, 0x69, 0x74, 0x2d, 0x72, 0x65, 0x66, 0x73, 0x3d,
		0x30, 0x20, 0x6e, 0x6f, 0x2d, 0x6c, 0x69, 0x6d, 0x69, 0x74, 0x2d, 0x6d,
		0x6f, 0x64, 0x65, 0x73, 0x20, 0x6d, 0x65, 0x3d, 0x30, 0x20, 0x73, 0x75,
		0x62, 0x6d, 0x65, 0x3d, 0x30, 0x20, 0x6d, 0x65, 0x72, 0x61, 0x6e, 0x67,
		0x65, 0x3d, 0x35, 0x37, 0x20, 0x74, 0x65, 0x6d, 0x70, 0x6f, 0x72, 0x61,
		0x6c, 0x2d, 0x6d, 0x76, 0x70, 0x20, 0x6e, 0x6f, 0x2d, 0x66, 0x72, 0x61,
		0x6d, 0x65, 0x2d, 0x64, 0x75, 0x70, 0x20, 0x6e, 0x6f, 0x2d, 0x68, 0x6d,
		0x65, 0x20, 0x6e, 0x6f, 0x2d, 0x77, 0x65, 0x69, 0x67, 0x68, 0x74, 0x70,
		0x20, 0x6e, 0x6f, 0x2d, 0x77, 0x65, 0x69, 0x67, 0x68, 0x74, 0x62, 0x20,
		0x6e, 0x6f, 0x2d, 0x61, 0x6e, 0x61, 0x6c, 0x79, 0x7a, 0x65, 0x2d, 0x73,
		0x72, 0x63, 0x2d, 0x70, 0x69, 0x63, 0x73, 0x20, 0x64, 0x65, 0x62, 0x6c,
		0x6f, 0x63, 0x6b, 0x3d, 0x30, 0x3a, 0x30, 0x20, 0x6e, 0x6f, 0x2d, 0x73,
		0x61, 0x6f, 0x20, 0x6e, 0x6f, 0x2d, 0x73, 0x61, 0x6f, 0x2d, 0x6e, 0x6f,
		0x6e, 0x2d, 0x64, 0x65, 0x62, 0x6c, 0x6f, 0x63, 0x6b, 0x20, 0x72, 0x64,
		0x3d, 0x32, 0x20, 0x73, 0x65, 0x6c, 0x65, 0x63, 0x74, 0x69, 0x76, 0x65,
		0x2d, 0x73, 0x61, 0x6f, 0x3d, 0x30, 0x20, 0x65, 0x61, 0x72, 0x6c, 0x79,
		0x2d, 0x73, 0x6b, 0x69, 0x70, 0x20, 0x72, 0x73, 0x6b, 0x69, 0x70, 0x20,
		0x66, 0x61, 0x73, 0x74, 0x2d, 0x69, 0x6e, 0x74, 0x72, 0x61, 0x20, 0x6e,
		0x6f, 0x2d, 0x74, 0x73, 0x6b, 0x69, 0x70, 0x2d, 0x66, 0x61, 0x73, 0x74,
		0x20, 0x6e, 0x6f, 0x2d, 0x63, 0x75, 0x2d, 0x6c, 0x6f, 0x73, 0x73, 0x6c,
		0x65, 0x73, 0x73, 0x20, 0x6e, 0x6f, 0x2d, 0x62, 0x2d, 0x69, 0x6e, 0x74,
		0x72, 0x61, 0x20, 0x6e, 0x6f, 0x2d, 0x73, 0x70, 0x6c, 0x69, 0x74, 0x72,
		0x64, 0x2d, 0x73, 0x6b, 0x69, 0x70, 0x20, 0x72, 0x64, 0x70, 0x65, 0x6e,
		0x61, 0x6c, 0x74, 0x79, 0x3d, 0x30, 0x20, 0x70, 0x73, 0x79, 0x2d, 0x72,
		0x64, 0x3d, 0x32, 0x2e, 0x30, 0x30, 0x20, 0x70, 0x73, 0x79, 0x2d, 0x72,
		0x64, 0x6f, 0x71, 0x3d, 0x30, 0x2e, 0x30, 0x30, 0x20, 0x6e, 0x6f, 0x2d,
		0x72, 0x64, 0x2d, 0x72, 0x65, 0x66, 0x69, 0x6e, 0x65, 0x20, 0x6e, 0x6f,
		0x2d, 0x6c, 0x6f, 0x73, 0x73, 0x6c, 0x65, 0x73, 0x73, 0x20, 0x63, 0x62,
		0x71, 0x70, 0x6f, 0x66, 0x66, 0x73, 0x3d, 0x30, 0x20, 0x63, 0x72, 0x71,
		0x70, 0x6f, 0x66, 0x66, 0x73, 0x3d, 0x30, 0x20, 0x72, 0x63, 0x3d, 0x63,
		0x72, 0x66, 0x20, 0x63, 0x72, 0x66, 0x3d, 0x32, 0x33, 0x2e, 0x30, 0x20,
		0x71, 0x63, 0x6f, 0x6d, 0x70, 0x3d, 0x30, 0x2e, 0x36, 0x30, 0x20, 0x71,
		0x70, 0x73, 0x74, 0x65, 0x70, 0x3d, 0x34, 0x20, 0x73, 0x74, 0x61, 0x74,
		0x73, 0x2d, 0x77, 0x72, 0x69, 0x74, 0x65, 0x3d, 0x30, 0x20, 0x73, 0x74,
		0x61, 0x74, 0x73, 0x2d, 0x72, 0x65, 0x61, 0x64, 0x3d, 0x30, 0x20, 0x69,
		0x70, 0x72, 0x61, 0x74, 0x69, 0x6f, 0x3d, 0x31, 0x2e, 0x34, 0x30, 0x20,
		0x70, 0x62, 0x72, 0x61, 0x74, 0x69, 0x6f, 0x3d, 0x31, 0x2e, 0x33, 0x30,
		0x20, 0x61, 0x71, 0x2d, 0x6d, 0x6f, 0x64, 0x65, 0x3d, 0x31, 0x20, 0x61,
		0x71, 0x2d, 0x73, 0x74, 0x72, 0x65, 0x6e, 0x67, 0x74, 0x68, 0x3d, 0x30,
		0x2e, 0x30, 0x30, 0x20, 0x63, 0x75, 0x74, 0x72, 0x65, 0x65, 0x20, 0x7a,
		0x6f, 0x6e, 0x65, 0x2d, 0x63, 0x6f, 0x75, 0x6e, 0x74, 0x3d, 0x30, 0x20,
		0x6e, 0x6f, 0x2d, 0x73, 0x74, 0x72, 0x69, 0x63, 0x74, 0x2d, 0x63, 0x62,
		0x72, 0x20, 0x71, 0x67, 0x2d, 0x73, 0x69, 0x7a, 0x65, 0x3d, 0x33, 0x32,
		0x20, 0x6e, 0x6f, 0x2d, 0x72, 0x63, 0x2d, 0x67, 0x72, 0x61, 0x69, 0x6e,
		0x20, 0x71, 0x70, 0x6d, 0x61, 0x78, 0x3d, 0x36, 0x39, 0x20, 0x71, 0x70,
		0x6d, 0x69, 0x6e, 0x3d, 0x30, 0x20, 0x6e, 0x6f, 0x2d, 0x63, 0x6f, 0x6e,
		0x73, 0x74, 0x2d, 0x76, 0x62, 0x76, 0x20, 0x73, 0x61, 0x72, 0x3d, 0x31,
		0x20, 0x6f, 0x76, 0x65, 0x72, 0x73, 0x63, 0x61, 0x6e, 0x3d, 0x30, 0x20,
		0x76, 0x69, 0x64, 0x65, 0x6f, 0x66, 0x6f, 0x72, 0x6d, 0x61, 0x74, 0x3d,
		0x35, 0x20, 0x72, 0x61, 0x6e, 0x67, 0x65, 0x3d, 0x30, 0x20, 0x63, 0x6f,
		0x6c, 0x6f, 0x72, 0x70, 0x72, 0x69, 0x6d, 0x3d, 0x31, 0x20, 0x74, 0x72,
		0x61, 0x6e, 0x73, 0x66, 0x65, 0x72, 0x3d, 0x31, 0x20, 0x63, 0x6f, 0x6c,
		0x6f, 0x72, 0x6d, 0x61, 0x74, 0x72, 0x69, 0x78, 0x3d, 0x31, 0x20, 0x63,
		0x68, 0x72, 0x6f, 0x6d, 0x61, 0x6c, 0x6f, 0x63, 0x3d, 0x31, 0x20, 0x63,
		0x68, 0x72, 0x6f, 0x6d, 0x61, 0x6c, 0x6f, 0x63, 0x2d, 0x74, 0x6f, 0x70,
		0x3d, 0x30, 0x20, 0x63, 0x68, 0x72, 0x6f, 0x6d, 0x61, 0x6c, 0x6f, 0x63,
		0x2d, 0x62, 0x6f, 0x74, 0x74, 0x6f, 0x6d, 0x3d, 0x30, 0x20, 0x64, 0x69,
		0x73, 0x70, 0x6c, 0x61, 0x79, 0x2d, 0x77, 0x69, 0x6e, 0x64, 0x6f, 0x77,
		0x3d, 0x30, 0x20, 0x63, 0x6c, 0x6c, 0x3d, 0x30, 0x2c, 0x30, 0x20, 0x6d,
		0x69, 0x6e, 0x2d, 0x6c, 0x75, 0x6d, 0x61, 0x3d, 0x30, 0x20, 0x6d, 0x61,
		0x78, 0x2d, 0x6c, 0x75, 0x6d, 0x61, 0x3d, 0x31, 0x30, 0x32, 0x33, 0x20,
		0x6c, 0x6f, 0x67, 0x32, 0x2d, 0x6d, 0x61, 0x78, 0x2d, 0x70, 0x6f, 0x63,
		0x2d, 0x6c, 0x73, 0x62, 0x3d, 0x38, 0x20, 0x76, 0x75, 0x69, 0x2d, 0x74,
		0x69, 0x6d, 0x69, 0x6e, 0x67, 0x2d, 0x69, 0x6e, 0x66, 0x6f, 0x20, 0x76,
		0x75, 0x69, 0x2d, 0x68, 0x72, 0x64, 0x2d, 0x69, 0x6e, 0x66, 0x6f, 0x20,
		0x73, 0x6c, 0x69, 0x63, 0x65, 0x73, 0x3d, 0x31, 0x20, 0x6e, 0x6f, 0x2d,
		0x6f, 0x70, 0x74, 0x2d, 0x71, 0x70, 0x2d, 0x70, 0x70, 0x73, 0x20, 0x6e,
		0x6f, 0x2d, 0x6f, 0x70, 0x74, 0x2d, 0x72, 0x65, 0x66, 0x2d, 0x6c, 0x69,
		0x73, 0x74, 0x2d, 0x6c, 0x65, 0x6e, 0x67, 0x74, 0x68, 0x2d, 0x70, 0x70,
		0x73, 0x20, 0x6e, 0x6f, 0x2d, 0x6d, 0x75, 0x6c, 0x74, 0x69, 0x2d, 0x70,
		0x61, 0x73, 0x73, 0x2d, 0x6f, 0x70, 0x74, 0x2d, 0x72, 0x70, 0x73, 0x20,
		0x73, 0x63, 0x65, 0x6e, 0x65, 0x63, 0x75, 0x74, 0x2d, 0x62, 0x69, 0x61,
		0x73, 0x3d, 0x30, 0x2e, 0x30, 0x35, 0x20, 0x6e, 0x6f, 0x2d, 0x6f, 0x70,
		0x74, 0x2d, 0x63, 0x75, 0x2d, 0x64, 0x65, 0x6c, 0x74, 0x61, 0x2d, 0x71,
		0x70, 0x20, 0x6e, 0x6f, 0x2d, 0x61, 0x71, 0x2d, 0x6d, 0x6f, 0x74, 0x69,
		0x6f, 0x6e, 0x20, 0x6e, 0x6f, 0x2d, 0x68, 0x64, 0x72, 0x31, 0x30, 0x20,
		0x6e, 0x6f, 0x2d, 0x68, 0x64, 0x72, 0x31, 0x30, 0x2d, 0x6f, 0x70, 0x74,
		0x20, 0x6e, 0x6f, 0x2d, 0x64, 0x68, 0x64, 0x72, 0x31, 0x30, 0x2d, 0x6f,
		0x70, 0x74, 0x20, 0x6e, 0x6f, 0x2d, 0x69, 0x64, 0x72, 0x2d, 0x72, 0x65,
		0x63, 0x6f, 0x76, 0x65, 0x72, 0x79, 0x2d, 0x73, 0x65, 0x69, 0x20, 0x61,
		0x6e, 0x61, 0x6c, 0x79, 0x73, 0x69, 0x73, 0x2d, 0x72, 0x65, 0x75, 0x73,
		0x65, 0x2d, 0x6c, 0x65, 0x76, 0x65, 0x6c, 0x3d, 0x30, 0x20, 0x61, 0x6e,
		0x61, 0x6c, 0x79, 0x73, 0x69, 0x73, 0x2d, 0x73, 0x61, 0x76, 0x65, 0x2d,
		0x72, 0x65, 0x75, 0x73, 0x65, 0x2d, 0x6c, 0x65, 0x76, 0x65, 0x6c, 0x3d,
		0x30, 0x20, 0x61, 0x6e, 0x61, 0x6c, 0x79, 0x73, 0x69, 0x73, 0x2d, 0x6c,
		0x6f, 0x61, 0x64, 0x2d, 0x72, 0x65, 0x75, 0x73, 0x65, 0x2d, 0x6c, 0x65,
		0x76, 0x65, 0x6c, 0x3d, 0x30, 0x20, 0x73, 0x63, 0x61, 0x6c, 0x65, 0x2d,
		0x66, 0x61, 0x63, 0x74, 0x6f, 0x72, 0x3d, 0x30, 0x20, 0x72, 0x65, 0x66,
		0x69, 0x6e, 0x65, 0x2d, 0x69, 0x6e, 0x74, 0x72, 0x61, 0x3d, 0x30, 0x20,
		0x72, 0x65, 0x66, 0x69, 0x6e, 0x65, 0x2d, 0x69, 0x6e, 0x74, 0x65, 0x72,
		0x3d, 0x30, 0x20, 0x72, 0x65, 0x66, 0x69, 0x6e, 0x65, 0x2d, 0x6d, 0x76,
		0x3d, 0x31, 0x20, 0x72, 0x65, 0x66, 0x69, 0x6e, 0x65, 0x2d, 0x63, 0x74,
		0x75, 0x2d, 0x64, 0x69, 0x73, 0x74, 0x6f, 0x72, 0x74, 0x69, 0x6f, 0x6e,
		0x3d, 0x30, 0x20, 0x6e, 0x6f, 0x2d, 0x6c, 0x69, 0x6d, 0x69, 0x74, 0x2d,
		0x73, 0x61, 0x6f, 0x20, 0x63, 0x74, 0x75, 0x2d, 0x69, 0x6e, 0x66, 0x6f,
		0x3d, 0x30, 0x20, 0x6e, 0x6f, 0x2d, 0x6c, 0x6f, 0x77, 0x70, 0x61, 0x73,
		0x73, 0x2d, 0x64, 0x63, 0x74, 0x20, 0x72, 0x65, 0x66, 0x69, 0x6e, 0x65,
		0x2d, 0x61, 0x6e, 0x61, 0x6c, 0x79, 0x73, 0x69, 0x73, 0x2d, 0x74, 0x79,
		0x70, 0x65, 0x3d, 0x30, 0x20, 0x63, 0x6f, 0x70, 0x79, 0x2d, 0x70, 0x69,
		0x63, 0x3d, 0x31, 0x20, 0x6d, 0x61, 0x78, 0x2d, 0x61, 0x75, 0x73, 0x69,
		0x7a, 0x65, 0x2d, 0x66, 0x61, 0x63, 0x74, 0x6f, 0x72, 0x3d, 0x31, 0x2e,
		0x30, 0x20, 0x6e, 0x6f, 0x2d, 0x64, 0x79, 0x6e, 0x61, 0x6d, 0x69, 0x63,
		0x2d, 0x72, 0x65, 0x66, 0x69, 0x6e, 0x65, 0x20, 0x6e, 0x6f, 0x2d, 0x73,
		0x69, 0x6e, 0x67, 0x6c, 0x65, 0x2d, 0x73, 0x65, 0x69, 0x20, 0x6e, 0x6f,
		0x2d, 0x68, 0x65, 0x76, 0x63, 0x2d, 0x61, 0x71, 0x20, 0x6e, 0x6f, 0x2d,
		0x73, 0x76, 0x74, 0x20, 0x6e, 0x6f, 0x2d, 0x66, 0x69, 0x65, 0x6c, 0x64,
		0x20, 0x71, 0x70, 0x2d, 0x61, 0x64, 0x61, 0x70, 0x74, 0x61, 0x74, 0x69,
		0x6f, 0x6e, 0x2d, 0x72, 0x61, 0x6e, 0x67, 0x65, 0x3d, 0x31, 0x2e, 0x30,
		0x30, 0x20, 0x73, 0x63, 0x65, 0x6e, 0x65, 0x63, 0x75, 0x74, 0x2d, 0x61,
		0x77, 0x61, 0x72, 0x65, 0x2d, 0x71, 0x70, 0x3d, 0x30, 0x63, 0x6f, 0x6e,
		0x66, 0x6f, 0x72, 0x6d, 0x61, 0x6e, 0x63, 0x65, 0x2d, 0x77, 0x69, 0x6e,
		0x64, 0x6f, 0x77, 0x2d, 0x6f, 0x66, 0x66, 0x73, 0x65, 0x74, 0x73, 0x20,
		0x72, 0x69, 0x67, 0x68, 0x74, 0x3d, 0x30, 0x20, 0x62, 0x6f, 0x74, 0x74,
		0x6f, 0x6d, 0x3d, 0x30, 0x20, 0x64, 0x65, 0x63, 0x6f, 0x64, 0x65, 0x72,
		0x2d, 0x6d, 0x61, 0x78, 0x2d, 0x72, 0x61, 0x74, 0x65, 0x3d, 0x30, 0x20,
		0x6e, 0x6f, 0x2d, 0x76, 0x62, 0x76, 0x2d, 0x6c, 0x69, 0x76, 0x65, 0x2d,
		0x6d, 0x75, 0x6c, 0x74, 0x69, 0x2d, 0x70, 0x61, 0x73, 0x73, 0x20, 0x6e,
		0x6f, 0x2d, 0x6d, 0x63, 0x73, 0x74, 0x66, 0x20, 0x6e, 0x6f, 0x2d, 0x73,
		0x62, 0x72, 0x63, 0x80, 0x00, 0x00, 0x00, 0x0a, 0x66, 0x69, 0x65, 0x6c,
		0x01, 0x00, 0x00, 0x00, 0x00, 0x13, 0x63, 0x6f, 0x6c, 0x72, 0x6e, 0x63,
		0x6c, 0x78, 0x00, 0x01, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00,
		0x10, 0x70, 0x61, 0x73, 0x70, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x00, 0x00, 0x00, 0x10, 0x73, 0x74, 0x74, 0x73, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x73, 0x74, 0x73,
		0x63, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x14, 0x73, 0x74, 0x73, 0x7a, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x73, 0x74, 0x63,
		0x6f, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x28, 0x6d, 0x76, 0x65, 0x78, 0x00, 0x00, 0x00, 0x20, 0x74, 0x72, 0x65,
		0x78, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x62, 0x75, 0x64, 0x74, 0x61, 0x00, 0x00, 0x00,
		0x5a, 0x6d, 0x65, 0x74, 0x61, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x21, 0x68, 0x64, 0x6c, 0x72, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x6d, 0x64, 0x69, 0x72, 0x61, 0x70, 0x70, 0x6c, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x2d, 0x69, 0x6c,
		0x73, 0x74, 0x00, 0x00, 0x00, 0x25, 0xa9, 0x74, 0x6f, 0x6f, 0x00, 0x00,
		0x00, 0x1d, 0x64, 0x61, 0x74, 0x61, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00,
		0x00, 0x00, 0x4c, 0x61, 0x76, 0x66, 0x35, 0x38, 0x2e, 0x37, 0x36, 0x2e,
		0x31, 0x30, 0x30,
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Length", strconv.Itoa(len(initSegment)))
	w.Write(initSegment)
}

func (s *Server) handleHLSMaster(w http.ResponseWriter, r *http.Request) {
	log.Printf("HLS Master requested for infoHash: %s, fileIndex: %d", INFO_HASH, FILE_INDEX)

	// Get query parameters
	queryParams := r.URL.Query()
	mediaURL := queryParams.Get("mediaURL")

	// If mediaURL is not provided, use the static MEDIA_URL
	if mediaURL == "" {
		mediaURL = MEDIA_URL
	}

	// URL encode the mediaURL
	encodedMediaURL := url.QueryEscape(mediaURL)

	// Build the query string for the child playlists
	queryString := ""
	if len(queryParams) > 0 {
		// Reconstruct query string without mediaURL (it will be different for each child)
		childParams := make([]string, 0)
		for key, values := range queryParams {
			if key != "mediaURL" {
				for _, value := range values {
					childParams = append(childParams, fmt.Sprintf("%s=%s", key, value))
				}
			}
		}
		if len(childParams) > 0 {
			queryString = "&" + strings.Join(childParams, "&")
		}
	}

	// Construct the master playlist content with static mediaURL for both mediaURL and filePath
	masterPlaylist := fmt.Sprintf(`#EXTM3U
#EXT-X-VERSION:7
#EXT-X-MEDIA:TYPE=VIDEO,GROUP-ID="video",NAME="Video",AUTOSELECT=YES,DEFAULT=YES
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="eng",LANGUAGE="eng",AUTOSELECT=YES,DEFAULT=YES,URI="audio0.m3u8?mediaURL=%s&%s"
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subtitles",NAME="English",LANGUAGE="eng",AUTOSELECT=NO,DEFAULT=NO,FORCED=NO,URI="subtitle0.m3u8?mediaURL=%s&%s"
#EXT-X-STREAM-INF:BANDWIDTH=164000,VIDEO="video",AUDIO="audio",SUBTITLES="subtitles",NAME="Main"
video0.m3u8?mediaURL=%s&%s`, encodedMediaURL, queryString, encodedMediaURL, queryString, encodedMediaURL, queryString)

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Content-Length", strconv.Itoa(len(masterPlaylist)))
	w.Write([]byte(masterPlaylist))
}

func (s *Server) handleHLSVideo0M3U8(w http.ResponseWriter, r *http.Request) {
	log.Printf("HLS Video0 M3U8 requested for infoHash: %s, fileIndex: %d", INFO_HASH, FILE_INDEX)

	// Get the mediaURL from query parameters
	queryParams := r.URL.Query()
	mediaURL := queryParams.Get("mediaURL")

	// If mediaURL is not provided, construct it from the request
	if mediaURL == "" {
		mediaURL = MEDIA_URL
	}

	// URL encode the mediaURL
	encodedMediaURL := url.QueryEscape(mediaURL)

	// Build the query string for the segments
	queryString := ""
	if len(queryParams) > 0 {
		// Reconstruct query string without mediaURL (it will be different for each segment)
		childParams := make([]string, 0)
		for key, values := range queryParams {
			if key != "mediaURL" {
				for _, value := range values {
					childParams = append(childParams, fmt.Sprintf("%s=%s", key, value))
				}
			}
		}
		if len(childParams) > 0 {
			queryString = "&" + strings.Join(childParams, "&")
		}
	}

	// Generate the static HLS playlist content with 4-second segments
	playlist := fmt.Sprintf(`#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:1
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-MAP:URI="video0/init.mp4?mediaURL=%s%s"`, encodedMediaURL, queryString)

	// Add segments with 4-second duration (330 segments for ~22 minutes)
	for i := 1; i <= 330; i++ {
		duration := "4.000000"
		playlist += fmt.Sprintf("\n#EXTINF:%s,\nvideo0/segment%d.m4s?mediaURL=%s%s", duration, i, encodedMediaURL, queryString)
	}

	playlist += "\n#EXT-X-ENDLIST"

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Content-Length", strconv.Itoa(len(playlist)))
	w.Write([]byte(playlist))
}

func (s *Server) handleHLSAudio0M3U8(w http.ResponseWriter, r *http.Request) {
	log.Printf("HLS Audio0 M3U8 requested for infoHash: %s, fileIndex: %d", INFO_HASH, FILE_INDEX)

	// Get the mediaURL from query parameters
	queryParams := r.URL.Query()
	mediaURL := queryParams.Get("mediaURL")

	// If mediaURL is not provided, construct it from the request
	if mediaURL == "" {
		mediaURL = MEDIA_URL
	}

	// URL encode the mediaURL
	encodedMediaURL := url.QueryEscape(mediaURL)

	// Build the query string for the segments
	queryString := ""
	if len(queryParams) > 0 {
		// Reconstruct query string without mediaURL (it will be different for each segment)
		childParams := make([]string, 0)
		for key, values := range queryParams {
			if key != "mediaURL" {
				for _, value := range values {
					childParams = append(childParams, fmt.Sprintf("%s=%s", key, value))
				}
			}
		}
		if len(childParams) > 0 {
			queryString = "&" + strings.Join(childParams, "&")
		}
	}

	// Generate the static HLS playlist content with 4-second segments
	playlist := fmt.Sprintf(`#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:1
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-MAP:URI="audio0/init.mp4?mediaURL=%s%s"`, encodedMediaURL, queryString)

	// Add segments with 4-second duration (330 segments for ~22 minutes)
	for i := 1; i <= 330; i++ {
		duration := "4.000000"
		playlist += fmt.Sprintf("\n#EXTINF:%s,\naudio0/segment%d.m4s?mediaURL=%s%s", duration, i, encodedMediaURL, queryString)
	}

	playlist += "\n#EXT-X-ENDLIST"

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Content-Length", strconv.Itoa(len(playlist)))
	w.Write([]byte(playlist))
}

func (s *Server) handleHLSSubtitle0M3U8(w http.ResponseWriter, r *http.Request) {
	log.Printf("HLS Subtitle0 M3U8 requested for infoHash: %s, fileIndex: %d", INFO_HASH, FILE_INDEX)

	// Get the mediaURL from query parameters
	queryParams := r.URL.Query()
	mediaURL := queryParams.Get("mediaURL")

	// If mediaURL is not provided, construct it from the request
	if mediaURL == "" {
		mediaURL = MEDIA_URL
	}

	// URL encode the mediaURL
	encodedMediaURL := url.QueryEscape(mediaURL)

	// Build the query string for the segments
	queryString := ""
	if len(queryParams) > 0 {
		// Reconstruct query string without mediaURL (it will be different for each segment)
		childParams := make([]string, 0)
		for key, values := range queryParams {
			if key != "mediaURL" {
				for _, value := range values {
					childParams = append(childParams, fmt.Sprintf("%s=%s", key, value))
				}
			}
		}
		if len(childParams) > 0 {
			queryString = "&" + strings.Join(childParams, "&")
		}
	}

	// Generate the static HLS playlist content
	playlist := fmt.Sprintf(`#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:10
#EXT-X-MEDIA-SEQUENCE:1
#EXT-X-PLAYLIST-TYPE:VOD`)

	// Add segments 1-132 with 10.000000 duration
	for i := 1; i <= 132; i++ {
		playlist += fmt.Sprintf("\n#EXTINF:10.000000,\nsubtitle0/segment%d.vtt?mediaURL=%s%s", i, encodedMediaURL, queryString)
	}

	// Add the last segment 133 with 1.504000 duration
	playlist += fmt.Sprintf("\n#EXTINF:1.504000,\nsubtitle0/segment133.vtt?mediaURL=%s%s", encodedMediaURL, queryString)

	playlist += "\n#EXT-X-ENDLIST"

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Content-Length", strconv.Itoa(len(playlist)))
	w.Write([]byte(playlist))
}

func (s *Server) handleHLSAudioSegment(w http.ResponseWriter, r *http.Request) {
	log.Printf("HLS Audio Segment requested")
	s.handleHLSAudioSegmentRealtime(w, r)
}

// handleHLSAudioSegmentRealtime handles audio segments with real-time ffmpeg processing (fallback)
func (s *Server) handleHLSAudioSegmentRealtime(w http.ResponseWriter, r *http.Request) {
	// Get the segment number from the URL
	vars := mux.Vars(r)
	segmentNumberStr := vars["segmentNumber"]
	segmentNumber, err := strconv.Atoi(segmentNumberStr)
	if err != nil {
		log.Printf("Invalid segment number: %s", segmentNumberStr)
		http.Error(w, "Invalid segment number", http.StatusBadRequest)
		return
	}

	// Check cache first (like JavaScript server)
	if cachedSegment, exists := getSegmentFromCache("audio", segmentNumber); exists {
		log.Printf("Serving cached audio segment %d (size: %d bytes)", segmentNumber, len(cachedSegment))
		w.Header().Set("Content-Type", "audio/mp4")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(cachedSegment)))
		w.Write(cachedSegment)
		return
	}

	// Segment not in cache, start long-running ffmpeg if not already running
	if !ffmpegRunning["audio"] {
		err = startLongRunningFFmpeg("audio")
		if err != nil {
			log.Printf("Error starting long-running ffmpeg: %v", err)
			http.Error(w, "Error starting ffmpeg process", http.StatusInternalServerError)
			return
		}
	}

	// Wait a bit for the segment to be processed
	time.Sleep(100 * time.Millisecond)

	// Check cache again
	if cachedSegment, exists := getSegmentFromCache("audio", segmentNumber); exists {
		log.Printf("Serving cached audio segment %d (size: %d bytes)", segmentNumber, len(cachedSegment))
		w.Header().Set("Content-Type", "audio/mp4")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(cachedSegment)))
		w.Write(cachedSegment)
		return
	}

	// Fallback to real-time processing if segment not available
	log.Printf("Audio segment %d not in cache, falling back to real-time processing", segmentNumber)

	// Use fixed segment duration like the main server (4 seconds)
	segmentDuration := 4.0
	startTime := float64(segmentNumber-1) * segmentDuration

	// Use the static MEDIA_URL
	httpURL := MEDIA_URL

	// Build the ffmpeg command like the main server
	args := []string{
		"-fflags", "+genpts",
		"-noaccurate_seek",
		"-seek_timestamp", "1",
		"-copyts",
		"-seek2any", "1",
		"-ss", fmt.Sprintf("%.3f", startTime),
		"-i", httpURL,
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
		"-ab", "256000",
		"-ar:a", "48000",
		"-map", "-0:s?",
		"-frag_duration", "4000000",
		"-fragment_index", fmt.Sprintf("%d", segmentNumber),
		"-movflags", "empty_moov+default_base_moof+delay_moov+dash",
		"-use_editlist", "1",
		"-f", "mp4",
		"pipe:1",
	}

	log.Printf("Executing ffmpeg command for audio segment %d with start time %.3f using URL: %s", segmentNumber, startTime, httpURL)

	// Execute ffmpeg and stream the output
	err = s.serveFfmpegWithContentLength(args, "audio/mp4", w)
	if err != nil {
		// Check if it's a client disconnection error
		if strings.Contains(err.Error(), "connection reset by peer") ||
			strings.Contains(err.Error(), "broken pipe") ||
			strings.Contains(err.Error(), "write: connection reset") ||
			strings.Contains(err.Error(), "write: broken pipe") {
			log.Printf("HLS Audio Segment: Client disconnected during streaming: %v", err)
			return // Don't treat client disconnection as an error
		}
		log.Printf("Error executing ffmpeg: %v", err)
		http.Error(w, "Error processing audio segment", http.StatusInternalServerError)
		return
	}
}

// serveFfmpeg spawns ffmpeg with given arguments and pipes output to HTTP response
func (s *Server) serveFfmpeg(args []string, contentType string, w http.ResponseWriter) error {
	// Set content type header
	w.Header().Set("Content-Type", contentType)

	// Create command
	cmd := exec.Command("ffmpeg", args...)

	// Get stdout pipe
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %v", err)
	}

	// Start the command
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start ffmpeg: %v", err)
	}

	// Stream output to response
	_, err = io.Copy(w, stdout)
	if err != nil {
		return fmt.Errorf("failed to copy ffmpeg output: %v", err)
	}

	// Wait for command to finish
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg command failed: %v", err)
	}

	return nil
}

// serveFfmpegWithContentLength spawns ffmpeg with given arguments, buffers the output, and sets Content-Length header
func (s *Server) serveFfmpegWithContentLength(args []string, contentType string, w http.ResponseWriter) error {

	// Create command
	cmd := exec.Command("ffmpeg", args...)

	// Get stdout pipe
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %v", err)
	}

	// Get stderr pipe for error logging
	stderr, err := cmd.StderrPipe()
	if err != nil {
		stdout.Close()
		return fmt.Errorf("failed to create stderr pipe: %v", err)
	}

	// Start the command
	if err := cmd.Start(); err != nil {
		stdout.Close()
		stderr.Close()
		return fmt.Errorf("failed to start ffmpeg: %v", err)
	}

	// Start stderr collection in background
	var stderrBuf bytes.Buffer
	stderrDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(&stderrBuf, stderr)
		stderrDone <- err
	}()

	// Buffer the entire FFmpeg output
	var outputBuf bytes.Buffer
	written, err := io.Copy(&outputBuf, stdout)
	stdout.Close()

	// Wait for stderr to complete
	<-stderrDone
	stderr.Close()
	cmd.Wait()

	// Check if we got any output
	const minValidSize = 16 // bytes - minimum valid segment size
	if written < minValidSize {
		log.Printf("FFMPEG: Output too small (%d bytes), stderr: %s", written, stderrBuf.String())
		return fmt.Errorf("ffmpeg output too small (%d bytes)", written)
	}

	if err != nil {
		log.Printf("FFMPEG: Error reading output: %v, stderr: %s", err, stderrBuf.String())
		return fmt.Errorf("failed to read ffmpeg output: %v", err)
	}

	// Set only the required headers for audio and video segments
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(int64(outputBuf.Len()), 10))
	w.Header().Set("Date", time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"))
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Keep-Alive", "timeout=5")

	// Write the buffered output
	_, err = w.Write(outputBuf.Bytes())
	if err != nil {
		log.Printf("FFMPEG: Error writing response: %v", err)
		return fmt.Errorf("failed to write response: %v", err)
	}

	log.Printf("FFMPEG: Successfully sent %d bytes with Content-Length header", outputBuf.Len())
	return nil
}

func (s *Server) handleHLSVideoSegment(w http.ResponseWriter, r *http.Request) {
	log.Printf("HLS Video Segment requested")
	s.handleHLSVideoSegmentRealtime(w, r)
}

// handleHLSVideoSegmentRealtime handles video segments with real-time ffmpeg processing (fallback)
func (s *Server) handleHLSVideoSegmentRealtime(w http.ResponseWriter, r *http.Request) {
	// Get the segment number from the URL
	vars := mux.Vars(r)
	segmentNumberStr := vars["segmentNumber"]
	segmentNumber, err := strconv.Atoi(segmentNumberStr)
	if err != nil {
		log.Printf("Invalid segment number: %s", segmentNumberStr)
		http.Error(w, "Invalid segment number", http.StatusBadRequest)
		return
	}

	// Check cache first (like JavaScript server)
	if cachedSegment, exists := getSegmentFromCache("video", segmentNumber); exists {
		log.Printf("Serving cached video segment %d (size: %d bytes)", segmentNumber, len(cachedSegment))
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(cachedSegment)))
		w.Write(cachedSegment)
		return
	}

	// Segment not in cache, start long-running ffmpeg if not already running
	if !ffmpegRunning["video"] {
		err = startLongRunningFFmpeg("video")
		if err != nil {
			log.Printf("Error starting long-running ffmpeg: %v", err)
			http.Error(w, "Error starting ffmpeg process", http.StatusInternalServerError)
			return
		}
	}

	// Wait a bit for the segment to be processed
	time.Sleep(100 * time.Millisecond)

	// Check cache again
	if cachedSegment, exists := getSegmentFromCache("video", segmentNumber); exists {
		log.Printf("Serving cached video segment %d (size: %d bytes)", segmentNumber, len(cachedSegment))
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(cachedSegment)))
		w.Write(cachedSegment)
		return
	}

	// Fallback to real-time processing if segment not available
	log.Printf("Video segment %d not in cache, falling back to real-time processing", segmentNumber)

	// Use fixed segment duration like the main server (4 seconds)
	segmentDuration := 4.0
	startTime := float64(segmentNumber-1) * segmentDuration

	// Use the static MEDIA_URL
	httpURL := MEDIA_URL

	// Build the ffmpeg command like the main server
	args := []string{
		"-fflags", "+genpts",
		"-noaccurate_seek",
		"-seek_timestamp", "1",
		"-copyts",
		"-seek2any", "1",
		"-ss", fmt.Sprintf("%.3f", startTime),
		"-i", httpURL,
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
		"-fragment_index", fmt.Sprintf("%d", segmentNumber),
		"-movflags", "frag_keyframe+empty_moov+default_base_moof+delay_moov+dash",
		"-use_editlist", "1",
		"-f", "mp4",
		"pipe:1",
	}

	log.Printf("Executing ffmpeg command for video segment %d with start time %.3f using URL: %s", segmentNumber, startTime, httpURL)

	// Execute ffmpeg and stream the output
	err = s.serveFfmpegWithContentLength(args, "video/mp4", w)
	if err != nil {
		// Check if it's a client disconnection error
		if strings.Contains(err.Error(), "connection reset by peer") ||
			strings.Contains(err.Error(), "broken pipe") ||
			strings.Contains(err.Error(), "write: connection reset") ||
			strings.Contains(err.Error(), "write: broken pipe") {
			log.Printf("HLS Video Segment: Client disconnected during streaming: %v", err)
			return // Don't treat client disconnection as an error
		}
		log.Printf("Error executing ffmpeg: %v", err)
		http.Error(w, "Error processing video segment", http.StatusInternalServerError)
		return
	}
}

func (s *Server) handleHLSSubtitleSegment(w http.ResponseWriter, r *http.Request) {
	s.handleHLSSubtitleSegmentRealtime(w, r)
}

// handleHLSSubtitleSegmentRealtime handles subtitle segments with real-time generation (fallback)
func (s *Server) handleHLSSubtitleSegmentRealtime(w http.ResponseWriter, r *http.Request) {
	// Get the segment number from the URL
	vars := mux.Vars(r)
	segmentNumberStr := vars["segmentNumber"]
	segmentNumber, err := strconv.Atoi(segmentNumberStr)
	if err != nil {
		log.Printf("Invalid segment number: %s", segmentNumberStr)
		http.Error(w, "Invalid segment number", http.StatusBadRequest)
		return
	}

	// Calculate the start and end times based on segment number
	// Each segment is approximately 10.000 seconds
	startTime := float64(segmentNumber-1) * 10.000
	endTime := startTime + 10.000

	// Create a simple VTT segment
	vttContent := fmt.Sprintf("WEBVTT\n\n%02d:%02d:%06.3f --> %02d:%02d:%06.3f\nSubtitle segment %d\n",
		int(startTime)/3600, (int(startTime)%3600)/60, startTime-float64(int(startTime)/3600)*3600-float64((int(startTime)%3600)/60)*60,
		int(endTime)/3600, (int(endTime)%3600)/60, endTime-float64(int(endTime)/3600)*3600-float64((int(endTime)%3600)/60)*60,
		segmentNumber)

	w.Header().Set("Content-Type", "text/vtt")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(vttContent)))
	w.Write([]byte(vttContent))
}

func (s *Server) handleDirectFile(w http.ResponseWriter, r *http.Request) {
	log.Printf("Direct file request")

	// Get the infoHash and fileIndex from the URL
	vars := mux.Vars(r)
	//infoHash := vars["infoHash"]
	fileIndexStr := vars["fileIndex"]

	// Validate fileIndex format (we don't actually use it since we serve a static file)
	_, err := strconv.Atoi(fileIndexStr)
	if err != nil {
		log.Printf("Invalid file index: %s", fileIndexStr)
		http.Error(w, "Invalid file index", http.StatusBadRequest)
		return
	}

	// For the minimal server, we'll construct the file path based on the infoHash and fileIndex
	// This assumes the file is stored in a cache directory structure similar to the main server
	// cacheDir := "/home/vscode/.stremio-server/stremio-cache"
	filePath := FILE_PATH

	// Check if file exists
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		log.Printf("File not found: %s", filePath)
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	// Get file info
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		log.Printf("Error getting file info: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Set headers
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", fileInfo.Size()))
	w.Header().Set("Cache-Control", "max-age=0, no-cache")

	// Handle range requests
	rangeHeader := r.Header.Get("Range")
	if rangeHeader != "" {
		// Parse range header (simplified)
		if strings.HasPrefix(rangeHeader, "bytes=") {
			rangeStr := strings.TrimPrefix(rangeHeader, "bytes=")
			parts := strings.Split(rangeStr, "-")
			if len(parts) == 2 {
				start, _ := strconv.ParseInt(parts[0], 10, 64)
				end := fileInfo.Size() - 1
				if parts[1] != "" {
					end, _ = strconv.ParseInt(parts[1], 10, 64)
				}

				if start >= 0 && end < fileInfo.Size() && start <= end {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, fileInfo.Size()))
					w.Header().Set("Content-Length", fmt.Sprintf("%d", end-start+1))
					w.WriteHeader(http.StatusPartialContent)

					// Open file and seek to start position
					file, err := os.Open(filePath)
					if err != nil {
						log.Printf("Error opening file: %v", err)
						http.Error(w, "Internal server error", http.StatusInternalServerError)
						return
					}
					defer file.Close()

					file.Seek(start, 0)
					io.CopyN(w, file, end-start+1)
					return
				}
			}
		}
	}

	// Full file request
	if r.Method == "HEAD" {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Serve the full file
	http.ServeFile(w, r, filePath)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status": "ok", "service": "minimal-server"}`))
}

func (s *Server) handleCasting(w http.ResponseWriter, r *http.Request) {
	log.Printf("Casting requested")

	// Return a mock casting response
	casting := map[string]interface{}{
		"enabled": true,
		"devices": []map[string]interface{}{
			{
				"id":     "mock-device-1",
				"name":   "Mock Chromecast Device",
				"type":   "chromecast",
				"status": "available",
			},
		},
		"status": "ready",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(casting)
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

		// Get current transcode profile
		currentProfile := ""
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
					"id":    "keepTorrentsAlive",
					"label": "KEEP_TORRENTS_ALIVE_AFTER_COMPLETION",
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
				"appPath":                   "/home/vscode/.stremio-server",
				"cacheRoot":                 "/home/vscode/.stremio-server",
				"cacheSize":                 10737418240,
				"btMaxConnections":          200,
				"btHandshakeTimeout":        20000,
				"btRequestTimeout":          4000,
				"btDownloadSpeedSoftLimit":  4194304,
				"btDownloadSpeedHardLimit":  39321600,
				"btMinPeersForStable":       10,
				"remoteHttps":               "",
				"localAddonEnabled":         false,
				"keepTorrentsAlive":         true,
				"transcodeHorsepower":       0.75,
				"transcodeMaxBitRate":       0,
				"transcodeConcurrency":      2,
				"transcodeTrackConcurrency": 1,
				"transcodeHardwareAccel":    true,
				"transcodeProfile":          currentProfile,
				"allTranscodeProfiles":      allTranscodeProfiles,
				"transcodeMaxWidth":         1920,
				"proxyStreamsEnabled":       false,
			},
			"baseUrl": fmt.Sprintf("http://%s:%d", localIP, 11470),
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

// handleOpenSubHash extracts subtitles from a video URL and returns them
func (s *Server) handleOpenSubHash(w http.ResponseWriter, r *http.Request) {
	log.Printf("OpenSubHash requested")

	// Get the videoUrl from query parameters
	videoURL := r.URL.Query().Get("videoUrl")
	if videoURL == "" {
		log.Printf("No videoUrl provided")
		http.Error(w, "videoUrl parameter is required", http.StatusBadRequest)
		return
	}

	log.Printf("Extracting subtitles from video URL: %s", videoURL)

	// Use ffprobe to check if the video has subtitle tracks
	args := []string{
		"-v", "quiet",
		"-select_streams", "s",
		"-show_entries", "stream=index:stream=codec_name:stream=language:stream=title",
		"-of", "json",
		videoURL,
	}

	cmd := exec.Command("ffprobe", args...)
	output, err := cmd.Output()
	if err != nil {
		log.Printf("ffprobe failed: %v", err)
		// Return empty response if no subtitles found
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"subtitles": []}`))
		return
	}

	var result struct {
		Streams []struct {
			Index    int    `json:"index"`
			Codec    string `json:"codec_name"`
			Language string `json:"language"`
			Title    string `json:"title"`
		} `json:"streams"`
	}

	if err := json.Unmarshal(output, &result); err != nil {
		log.Printf("Failed to parse ffprobe output: %v", err)
		http.Error(w, "Failed to parse video information", http.StatusInternalServerError)
		return
	}

	// If no subtitle streams found, return empty response
	if len(result.Streams) == 0 {
		log.Printf("No subtitle streams found in video")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"subtitles": []}`))
		return
	}

	// Extract subtitles using ffmpeg
	var subtitles []map[string]interface{}

	for i, stream := range result.Streams {
		log.Printf("Processing subtitle stream %d: codec=%s, language=%s, title=%s",
			stream.Index, stream.Codec, stream.Language, stream.Title)

		// Create a temporary file for the subtitle
		tempFile, err := os.CreateTemp("", fmt.Sprintf("subtitle_%d_*.srt", i))
		if err != nil {
			log.Printf("Failed to create temp file: %v", err)
			continue
		}
		tempFile.Close()
		defer os.Remove(tempFile.Name())

		// Extract subtitle using ffmpeg
		extractArgs := []string{
			"-i", videoURL,
			"-map", fmt.Sprintf("0:s:%d", i),
			"-c:s", "srt",
			"-y", // Overwrite output file
			tempFile.Name(),
		}

		extractCmd := exec.Command("ffmpeg", extractArgs...)
		if err := extractCmd.Run(); err != nil {
			log.Printf("Failed to extract subtitle stream %d: %v", i, err)
			continue
		}

		// Read the extracted subtitle file
		subtitleContent, err := os.ReadFile(tempFile.Name())
		if err != nil {
			log.Printf("Failed to read subtitle file: %v", err)
			continue
		}

		// Create subtitle entry
		subtitle := map[string]interface{}{
			"index":    stream.Index,
			"codec":    stream.Codec,
			"language": stream.Language,
			"title":    stream.Title,
			"content":  string(subtitleContent),
		}

		subtitles = append(subtitles, subtitle)
	}

	// Return the subtitles
	response := map[string]interface{}{
		"videoUrl":  videoURL,
		"subtitles": subtitles,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleCacheClear clears the segment cache
func (s *Server) handleCacheClear(w http.ResponseWriter, r *http.Request) {
	clearCache()
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status": "cache cleared"}`))
}

// handleCacheStats returns cache statistics
func (s *Server) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	stats := getCacheStats()
	statsJSON, err := json.Marshal(stats)
	if err != nil {
		http.Error(w, "Error marshaling stats", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(statsJSON)
}

// handleCacheStart starts the long-running ffmpeg process
func (s *Server) handleCacheStart(w http.ResponseWriter, r *http.Request) {
	trackType := r.URL.Query().Get("type")
	if trackType == "" {
		trackType = "audio" // default to audio
	}

	if trackType != "audio" && trackType != "video" {
		http.Error(w, "Invalid track type. Use 'audio' or 'video'", http.StatusBadRequest)
		return
	}

	err := startLongRunningFFmpeg(trackType)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error starting ffmpeg: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(fmt.Sprintf(`{"status": "ffmpeg started for %s"}`, trackType)))
}

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

func (s *Server) Start(port int) error {
	addr := fmt.Sprintf(":%d", port)
	log.Printf("Starting minimal server on %s", addr)

	return http.ListenAndServe(addr, s.router)
}

func main() {
	server := NewServer()

	port := 11470
	log.Printf("Minimal Stremio server starting on port %d", port)
	log.Printf("Serving content: infoHash=%s, fileIndex=%d", INFO_HASH, FILE_INDEX)
	log.Printf("File path: %s", FILE_PATH)

	// Start cache for both audio and video on startup
	log.Printf("Starting cache for audio and video...")

	// Start audio cache
	go func() {
		err := startLongRunningFFmpeg("audio")
		if err != nil {
			log.Printf("Error starting audio cache: %v", err)
		} else {
			log.Printf("Audio cache started successfully")
		}
	}()

	// Start video cache
	go func() {
		err := startLongRunningFFmpeg("video")
		if err != nil {
			log.Printf("Error starting video cache: %v", err)
		} else {
			log.Printf("Video cache started successfully")
		}
	}()

	// Wait a moment for cache processes to start
	time.Sleep(2 * time.Second)

	log.Printf("Available endpoints:")
	log.Printf("  - /%s/%d/stats.json", INFO_HASH, FILE_INDEX)
	log.Printf("  - /probe")
	log.Printf("  - /hlsv2/%s/%d/audio0/init.mp4", INFO_HASH, FILE_INDEX)
	log.Printf("  - /hlsv2/%s/%d/video0/init.mp4", INFO_HASH, FILE_INDEX)
	log.Printf("  - /hlsv2/%s/%d/master.m3u8", INFO_HASH, FILE_INDEX)
	log.Printf("  - /hlsv2/%s/%d/video0.m3u8", INFO_HASH, FILE_INDEX)
	log.Printf("  - /hlsv2/%s/%d/audio0.m3u8", INFO_HASH, FILE_INDEX)
	log.Printf("  - /hlsv2/%s/%d/subtitle0.m3u8", INFO_HASH, FILE_INDEX)
	log.Printf("  - /hlsv2/%s/%d/audio0/segment{number}.m4s", INFO_HASH, FILE_INDEX)
	log.Printf("  - /hlsv2/%s/%d/video0/segment{number}.m4s", INFO_HASH, FILE_INDEX)
	log.Printf("  - /hlsv2/%s/%d/subtitle0/segment{number}.vtt", INFO_HASH, FILE_INDEX)
	log.Printf("  - /settings")
	log.Printf("  - /network-info")
	log.Printf("  - /device-info")
	log.Printf("  - /opensubHash?videoUrl=<url>")
	log.Printf("  - /casting")
	log.Printf("  - /health")
	log.Printf("  - /cache/stats")
	log.Printf("  - /cache/clear")

	if err := server.Start(port); err != nil {
		log.Fatal("Server failed to start:", err)
	}
}
