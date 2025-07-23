package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FFmpegManager manages FFmpeg operations
type FFmpegManager struct {
	ffmpegPath  string
	ffprobePath string
	config      *FFmpegConfig
	mu          sync.RWMutex

	probeCache   map[string]*ffprobeCacheEntry
	probeCacheMu sync.Mutex
}

type ffprobeCacheEntry struct {
	FileSize int64
	Result   *ProbeResponse
	Err      error
	Time     time.Time
}

// FFmpegConfig holds FFmpeg configuration
type FFmpegConfig struct {
	HardwareAcceleration bool
	TranscodeHorsepower  float64
	TranscodeMaxBitRate  int
	TranscodeConcurrency int
	TranscodeMaxWidth    int
	TranscodeProfile     string
	Debug                bool
}

// VideoInfo contains video metadata
type VideoInfo struct {
	Duration  float64      `json:"duration"`
	Width     int          `json:"width"`
	Height    int          `json:"height"`
	BitRate   int          `json:"bit_rate"`
	CodecName string       `json:"codec_name"`
	CodecType string       `json:"codec_type"`
	Streams   []StreamInfo `json:"streams"`
	Format    FormatInfo   `json:"format"`
}

// StreamInfo contains stream information
type StreamInfo struct {
	Index      int     `json:"index"`
	CodecName  string  `json:"codec_name"`
	CodecType  string  `json:"codec_type"`
	Width      int     `json:"width,omitempty"`
	Height     int     `json:"height,omitempty"`
	BitRate    int     `json:"bit_rate,omitempty"`
	Duration   float64 `json:"duration,omitempty"`
	SampleRate int     `json:"sample_rate,omitempty"`
	Channels   int     `json:"channels,omitempty"`
	Language   string  `json:"language,omitempty"`
	Title      string  `json:"title,omitempty"`
}

// FormatInfo contains format information
type FormatInfo struct {
	Duration float64 `json:"duration"`
	BitRate  int     `json:"bit_rate"`
	Size     int64   `json:"size"`
}

// TranscodeJob represents a transcoding job
type TranscodeJob struct {
	ID         string
	InputPath  string
	OutputPath string
	Options    TranscodeOptions
	Status     string
	Progress   float64
	Error      error
	StartTime  time.Time
	EndTime    time.Time
	mu         sync.RWMutex
}

// TranscodeOptions holds transcoding parameters
type TranscodeOptions struct {
	VideoCodec      string
	AudioCodec      string
	VideoBitRate    int
	AudioBitRate    int
	Width           int
	Height          int
	FrameRate       int
	Quality         int
	HardwareAccel   bool
	SegmentDuration int
	OutputFormat    string
	SubtitleStream  int
}

// ProbeResponse represents the expected JSON response format for /probe
type ProbeResponse struct {
	Format  ProbeFormat            `json:"format"`
	Streams []ProbeStream          `json:"streams"`
	Samples map[string]interface{} `json:"samples"`
}

// ProbeFormat represents format information in the probe response
type ProbeFormat struct {
	Name     string  `json:"name"`
	Duration float64 `json:"duration"`
}

// ProbeStream represents stream information in the probe response
type ProbeStream struct {
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
	NumberOfBytes    int64   `json:"numberOfBytes,omitempty"`
	FormatDuration   float64 `json:"formatDuration,omitempty"`
	SampleRate       int     `json:"sampleRate,omitempty"`
	Channels         int     `json:"channels,omitempty"`
	ChannelLayout    string  `json:"channelLayout,omitempty"`
	Title            *string `json:"title,omitempty"`
	Language         string  `json:"language,omitempty"`
}

// NewFFmpegManager creates a new FFmpeg manager (matching Node.js server.js behavior)
func NewFFmpegManager(config *FFmpegConfig) (*FFmpegManager, error) {
	ffmpegPath, err := findFFmpeg()
	if err != nil {
		log.Printf("[NewFFmpegManager] Warning: FFmpeg not found: %v", err)
		// Return a manager with empty paths - server can still run without FFmpeg
		return &FFmpegManager{
			ffmpegPath:  "",
			ffprobePath: "",
			config:      config,
			probeCache:  make(map[string]*ffprobeCacheEntry),
		}, nil
	}

	ffprobePath, err := findFFprobe()
	if err != nil {
		log.Printf("[NewFFmpegManager] Warning: FFprobe not found: %v", err)
		// Return a manager with just ffmpeg - some operations will still work
		return &FFmpegManager{
			ffmpegPath:  ffmpegPath,
			ffprobePath: "",
			config:      config,
			probeCache:  make(map[string]*ffprobeCacheEntry),
		}, nil
	}

	manager := &FFmpegManager{
		ffmpegPath:  ffmpegPath,
		ffprobePath: ffprobePath,
		config:      config,
		probeCache:  make(map[string]*ffprobeCacheEntry),
	}

	// Test FFmpeg installation (but don't fail if it doesn't work)
	if err := manager.testInstallation(); err != nil {
		log.Printf("[NewFFmpegManager] Warning: FFmpeg test failed: %v", err)
		// Still return the manager - server can run without working FFmpeg
	}

	log.Printf("[NewFFmpegManager] FFmpeg initialized: %s, FFprobe: %s", ffmpegPath, ffprobePath)
	return manager, nil
}

// findFFmpeg locates the FFmpeg executable (matching Node.js server.js logic)
func findFFmpeg() (string, error) {
	// Get executable directory
	execPath, err := os.Executable()
	execDir := ""
	if err == nil {
		execDir = filepath.Dir(execPath)
	}

	// Paths matching Node.js server.js exactly
	paths := []string{
		os.Getenv("FFMPEG_BIN"),                     // Environment variable
		filepath.Join(execDir, "ffmpeg"),            // Same directory as executable
		filepath.Join(execDir, "ffmpeg.exe"),        // Windows executable
		filepath.Join(execDir, "bin", "ffmpeg.exe"), // Windows bin directory
		"/usr/lib/jellyfin-ffmpeg/ffmpeg",           // Jellyfin FFmpeg
		"/usr/bin/ffmpeg",                           // System FFmpeg
		"/usr/local/bin/ffmpeg",                     // Local FFmpeg
		"ffmpeg",                                    // Try PATH
	}

	for _, path := range paths {
		if path == "" {
			continue
		}

		// Check if file exists and is executable
		if stat, err := os.Stat(path); err == nil {
			// On Unix systems, check if it's executable
			if runtime.GOOS != "windows" {
				if stat.Mode()&0111 == 0 {
					continue // Not executable
				}
			}
			return path, nil
		}
	}

	return "", fmt.Errorf("ffmpeg not found in any of the expected locations")
}

// findFFprobe locates the FFprobe executable (matching Node.js server.js logic)
func findFFprobe() (string, error) {
	// Get executable directory
	execPath, err := os.Executable()
	execDir := ""
	if err == nil {
		execDir = filepath.Dir(execPath)
	}

	// Paths matching Node.js server.js exactly
	paths := []string{
		os.Getenv("FFPROBE_BIN"),                     // Environment variable
		filepath.Join(execDir, "ffprobe"),            // Same directory as executable
		filepath.Join(execDir, "ffprobe.exe"),        // Windows executable
		filepath.Join(execDir, "bin", "ffprobe.exe"), // Windows bin directory
		"/usr/lib/jellyfin-ffmpeg/ffprobe",           // Jellyfin FFprobe
		"/usr/bin/ffprobe",                           // System FFprobe
		"/usr/local/bin/ffprobe",                     // Local FFprobe
		"ffprobe",                                    // Try PATH
	}

	for _, path := range paths {
		if path == "" {
			continue
		}

		// Check if file exists and is executable
		if stat, err := os.Stat(path); err == nil {
			// On Unix systems, check if it's executable
			if runtime.GOOS != "windows" {
				if stat.Mode()&0111 == 0 {
					continue // Not executable
				}
			}
			return path, nil
		}
	}

	return "", fmt.Errorf("ffprobe not found in any of the expected locations")
}

// testInstallation tests if FFmpeg is working correctly
func (fm *FFmpegManager) testInstallation() error {
	cmd := exec.Command(fm.ffmpegPath, "-version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg test failed: %v", err)
	}

	cmd = exec.Command(fm.ffprobePath, "-version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffprobe test failed: %v", err)
	}

	return nil
}

// GetVideoInfo gets video information using ffprobe
func (fm *FFmpegManager) GetVideoInfo(inputPath string) (*VideoInfo, error) {
	args := []string{
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		inputPath,
	}

	cmd := exec.Command(fm.ffprobePath, args...)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe failed: %v", err)
	}

	var videoInfo VideoInfo
	if err := json.Unmarshal(output, &videoInfo); err != nil {
		return nil, fmt.Errorf("failed to parse ffprobe output: %v", err)
	}

	return &videoInfo, nil
}

// TranscodeVideo transcodes a video file
func (fm *FFmpegManager) TranscodeVideo(inputPath, outputPath string, options TranscodeOptions) error {
	args := fm.buildTranscodeArgs(inputPath, outputPath, options)

	if fm.config.Debug {
		log.Printf("[TranscodeVideo] FFmpeg command: %s %s", fm.ffmpegPath, strings.Join(args, " "))
	}

	cmd := exec.Command(fm.ffmpegPath, args...)
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// TranscodeVideoToHLS creates HLS segments from a video file
func (fm *FFmpegManager) TranscodeVideoToHLS(inputPath, outputDir, playlistName string, options TranscodeOptions) error {
	// Ensure output directory exists
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %v", err)
	}

	segmentDuration := options.SegmentDuration
	if segmentDuration == 0 {
		segmentDuration = 10
	}

	args := []string{
		"-i", inputPath,
		"-c:v", options.VideoCodec,
		"-c:a", options.AudioCodec,
		"-b:v", strconv.Itoa(options.VideoBitRate) + "k",
		"-b:a", strconv.Itoa(options.AudioBitRate) + "k",
		"-map", "0:v:0",
		"-map", "0:a:0",
		"-f", "hls",
		"-hls_time", strconv.Itoa(segmentDuration),
		"-hls_list_size", "0",
		"-hls_segment_filename", filepath.Join(outputDir, "segment_%03d.ts"),
		filepath.Join(outputDir, playlistName),
	}

	// Add hardware acceleration if enabled
	if options.HardwareAccel && fm.config.HardwareAcceleration {
		args = append([]string{"-hwaccel", "vaapi", "-hwaccel_device", "/dev/dri/renderD128"}, args...)
	}

	// Add quality scaling if specified
	if options.Width > 0 && options.Height > 0 {
		args = append(args, "-vf", fmt.Sprintf("scale=%d:%d", options.Width, options.Height))
	}

	if fm.config.Debug {
		log.Printf("[TranscodeVideoToHLS] FFmpeg HLS command: %s %s", fm.ffmpegPath, strings.Join(args, " "))
	}

	cmd := exec.Command(fm.ffmpegPath, args...)
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// StreamTranscode transcodes a video stream in real-time
func (fm *FFmpegManager) StreamTranscode(inputPath string, w http.ResponseWriter, r *http.Request, options TranscodeOptions) error {
	args := fm.buildStreamArgs(inputPath, options)

	if fm.config.Debug {
		log.Printf("[StreamTranscode] FFmpeg stream command: %s %s", fm.ffmpegPath, strings.Join(args, " "))
	}

	cmd := exec.Command(fm.ffmpegPath, args...)

	// Set up pipes
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %v", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %v", err)
	}

	// Start the command
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start ffmpeg: %v", err)
	}

	// Set response headers
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "no-cache")

	// Stream the output
	go func() {
		defer cmd.Wait()
		io.Copy(w, stdout)
	}()

	// Log stderr if debug is enabled
	if fm.config.Debug {
		go func() {
			scanner := bufio.NewScanner(stderr)
			for scanner.Scan() {
				log.Printf("[StreamTranscode] FFmpeg: %s", scanner.Text())
			}
		}()
	}

	return nil
}

// GenerateThumbnail generates a thumbnail from a video
func (fm *FFmpegManager) GenerateThumbnail(inputPath, outputPath string, timeOffset float64) error {
	args := []string{
		"-i", inputPath,
		"-ss", fmt.Sprintf("%.2f", timeOffset),
		"-vframes", "1",
		"-q:v", "2",
		"-y", // Overwrite output file
		outputPath,
	}

	if fm.config.Debug {
		log.Printf("[GenerateThumbnail] FFmpeg thumbnail command: %s %s", fm.ffmpegPath, strings.Join(args, " "))
	}

	cmd := exec.Command(fm.ffmpegPath, args...)
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// GetHardwareAccelerationInfo gets information about available hardware acceleration (matching Node.js server.js)
func (fm *FFmpegManager) GetHardwareAccelerationInfo() map[string]interface{} {
	info := make(map[string]interface{})
	var profiles []string

	// Get hardware profiles with codec mappings
	hardwareProfiles := fm.GetHardwareProfiles()

	for profileName, profile := range hardwareProfiles {
		if profile.Available {
			info[profileName] = map[string]interface{}{
				"available":     true,
				"displayName":   profile.DisplayName,
				"platform":      profile.Platform,
				"videoEncoders": profile.VideoEncoders,
				"videoDecoders": profile.VideoDecoders,
				"audioEncoders": profile.AudioEncoders,
				"audioDecoders": profile.AudioDecoders,
				"supportsHEVC":  profile.VideoEncoders["hevc"] != "",
			}
			profiles = append(profiles, profileName)
		}
	}

	// Add profiles array (matching Node.js behavior)
	info["profiles"] = profiles

	// Add default profile if none found
	if len(profiles) == 0 {
		info["defaultProfile"] = "software"
	} else {
		info["defaultProfile"] = profiles[0]
	}

	// Add codec support summary
	info["codecSupport"] = map[string]interface{}{
		"hevc":             len(profiles) > 0,
		"h264":             len(profiles) > 0,
		"hardwareProfiles": len(profiles),
	}

	return info
}

// testHardwareAccel tests if a specific hardware acceleration is available (matching Node.js server.js)
func (fm *FFmpegManager) testHardwareAccel(accelType string) bool {
	var args []string

	switch accelType {
	case "vaapi":
		args = []string{"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=1", "-c:v", "h264_vaapi", "-f", "null", "-"}
	case "cuda":
		args = []string{"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=1", "-c:v", "h264_nvenc", "-f", "null", "-"}
	case "videotoolbox":
		args = []string{"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=1", "-c:v", "h264_videotoolbox", "-f", "null", "-"}
	case "qsv":
		args = []string{"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=1", "-c:v", "h264_qsv", "-f", "null", "-"}
	case "nvenc":
		args = []string{"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=1", "-c:v", "h264_nvenc", "-f", "null", "-"}
	default:
		return false
	}

	cmd := exec.Command(fm.ffmpegPath, args...)
	cmd.Stderr = io.Discard
	cmd.Stdout = io.Discard

	return cmd.Run() == nil
}

// buildTranscodeArgs builds FFmpeg arguments for transcoding
func (fm *FFmpegManager) buildTranscodeArgs(inputPath, outputPath string, options TranscodeOptions) []string {
	args := []string{"-i", inputPath}

	// Add hardware acceleration if enabled
	if options.HardwareAccel && fm.config.HardwareAcceleration {
		args = append(args, "-hwaccel", "vaapi", "-hwaccel_device", "/dev/dri/renderD128")
	}

	// Video codec
	if options.VideoCodec != "" {
		args = append(args, "-c:v", options.VideoCodec)
	}

	// Audio codec
	if options.AudioCodec != "" {
		args = append(args, "-c:a", options.AudioCodec)
	}

	// Video bitrate
	if options.VideoBitRate > 0 {
		args = append(args, "-b:v", strconv.Itoa(options.VideoBitRate)+"k")
	}

	// Audio bitrate
	if options.AudioBitRate > 0 {
		args = append(args, "-b:a", strconv.Itoa(options.AudioBitRate)+"k")
	}

	// Resolution
	if options.Width > 0 && options.Height > 0 {
		args = append(args, "-vf", fmt.Sprintf("scale=%d:%d", options.Width, options.Height))
	}

	// Frame rate
	if options.FrameRate > 0 {
		args = append(args, "-r", strconv.Itoa(options.FrameRate))
	}

	// Quality
	if options.Quality > 0 {
		args = append(args, "-crf", strconv.Itoa(options.Quality))
	}

	// Output format
	if options.OutputFormat != "" {
		args = append(args, "-f", options.OutputFormat)
	}

	// Subtitle stream
	if options.SubtitleStream >= 0 {
		args = append(args, "-map", fmt.Sprintf("0:s:%d", options.SubtitleStream), "-c:s", "mov_text")
	}

	// Overwrite output
	args = append(args, "-y", outputPath)

	return args
}

// buildStreamArgs builds FFmpeg arguments for streaming
func (fm *FFmpegManager) buildStreamArgs(inputPath string, options TranscodeOptions) []string {
	args := []string{"-i", inputPath}

	// Add hardware acceleration if enabled
	if options.HardwareAccel && fm.config.HardwareAcceleration {
		args = append(args, "-hwaccel", "vaapi", "-hwaccel_device", "/dev/dri/renderD128")
	}

	// Video codec
	if options.VideoCodec != "" {
		args = append(args, "-c:v", options.VideoCodec)
	}

	// Audio codec
	if options.AudioCodec != "" {
		args = append(args, "-c:a", options.AudioCodec)
	}

	// Video bitrate
	if options.VideoBitRate > 0 {
		args = append(args, "-b:v", strconv.Itoa(options.VideoBitRate)+"k")
	}

	// Audio bitrate
	if options.AudioBitRate > 0 {
		args = append(args, "-b:a", strconv.Itoa(options.AudioBitRate)+"k")
	}

	// Resolution
	if options.Width > 0 && options.Height > 0 {
		args = append(args, "-vf", fmt.Sprintf("scale=%d:%d", options.Width, options.Height))
	}

	// Output to stdout
	args = append(args, "-f", "mp4", "-movflags", "frag_keyframe+empty_moov", "-")

	return args
}

// ExtractAudio extracts audio from a video file
func (fm *FFmpegManager) ExtractAudio(inputPath, outputPath string, format string) error {
	args := []string{
		"-i", inputPath,
		"-vn", // No video
		"-acodec", "copy",
		"-y",
		outputPath,
	}

	if format != "" {
		args = append(args, "-f", format)
	}

	if fm.config.Debug {
		log.Printf("[ExtractAudio] FFmpeg audio extraction command: %s %s", fm.ffmpegPath, strings.Join(args, " "))
	}

	cmd := exec.Command(fm.ffmpegPath, args...)
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// GetStreamInfo gets detailed stream information
func (fm *FFmpegManager) GetStreamInfo(inputPath string) ([]StreamInfo, error) {
	args := []string{
		"-v", "quiet",
		"-print_format", "json",
		"-show_streams",
		inputPath,
	}

	cmd := exec.Command(fm.ffprobePath, args...)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe failed: %v", err)
	}

	var result struct {
		Streams []StreamInfo `json:"streams"`
	}

	if err := json.Unmarshal(output, &result); err != nil {
		return nil, fmt.Errorf("failed to parse ffprobe output: %v", err)
	}

	return result.Streams, nil
}

// GetDuration gets the duration of a media file
func (fm *FFmpegManager) GetDuration(inputPath string) (float64, error) {
	args := []string{
		"-v", "quiet",
		"-show_entries", "format=duration",
		"-of", "csv=p=0",
		inputPath,
	}

	cmd := exec.Command(fm.ffprobePath, args...)
	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe failed: %v", err)
	}

	duration, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse duration: %v", err)
	}

	return duration, nil
}

// GetFrameRate gets the frame rate of a video
func (fm *FFmpegManager) GetFrameRate(inputPath string) (float64, error) {
	args := []string{
		"-v", "quiet",
		"-select_streams", "v:0",
		"-show_entries", "stream=r_frame_rate",
		"-of", "csv=p=0",
		inputPath,
	}

	cmd := exec.Command(fm.ffprobePath, args...)
	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe failed: %v", err)
	}

	frameRateStr := strings.TrimSpace(string(output))
	if frameRateStr == "" {
		return 0, fmt.Errorf("no video stream found")
	}

	// Parse frame rate (e.g., "30/1" or "29.97")
	parts := strings.Split(frameRateStr, "/")
	if len(parts) == 2 {
		num, err := strconv.ParseFloat(parts[0], 64)
		if err != nil {
			return 0, fmt.Errorf("failed to parse frame rate numerator: %v", err)
		}
		den, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return 0, fmt.Errorf("failed to parse frame rate denominator: %v", err)
		}
		return num / den, nil
	}

	// Try parsing as decimal
	frameRate, err := strconv.ParseFloat(frameRateStr, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse frame rate: %v", err)
	}

	return frameRate, nil
}

// GetProbeInfo gets video information and transforms it to the expected probe format
func (fm *FFmpegManager) GetProbeInfo(inputPath string) (*ProbeResponse, error) {
	// Use the timeout version with a reasonable default timeout
	return fm.GetProbeInfoWithTimeout(inputPath, 30*time.Second)
}

// GetProbeInfoWithTimeout gets video information with a specific timeout (matching Node.js server.js behavior)
func (fm *FFmpegManager) GetProbeInfoWithTimeout(inputPath string, timeout time.Duration) (*ProbeResponse, error) {
	// Create a context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Check if ffprobe is available
	if fm.ffprobePath == "" {
		log.Printf("[GetProbeInfoWithTimeout] FFprobe not available, using fallback for %s", inputPath)
		return fm.getFallbackProbeInfo(inputPath)
	}

	// Check if input file exists
	stat, err := os.Stat(inputPath)
	if err != nil {
		log.Printf("[GetProbeInfoWithTimeout] Input file does not exist: %s, error: %v", inputPath, err)
		return fm.getFallbackProbeInfo(inputPath)
	}
	fileSize := stat.Size()

	// Check cache
	fm.probeCacheMu.Lock()
	cacheEntry, ok := fm.probeCache[inputPath]
	if ok && cacheEntry.FileSize == fileSize && time.Since(cacheEntry.Time) < 10*time.Minute {
		fm.probeCacheMu.Unlock()
		log.Printf("[GetProbeInfoWithTimeout] FFprobe cache hit for %s (size=%d)", inputPath, fileSize)
		return cacheEntry.Result, cacheEntry.Err
	}
	fm.probeCacheMu.Unlock()

	// Get raw ffprobe output with more detailed information
	args := []string{
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		inputPath,
	}

	cmd := exec.CommandContext(ctx, fm.ffprobePath, args...)
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			log.Printf("[GetProbeInfoWithTimeout] FFprobe timeout after %v for %s", timeout, inputPath)
			return nil, fmt.Errorf("ffprobe timeout after %v", timeout)
		}
		log.Printf("[GetProbeInfoWithTimeout] FFprobe failed for %s: %v, attempting fallback", inputPath, err)
		return fm.getFallbackProbeInfo(inputPath)
	}

	// Parse the raw JSON output
	var rawData map[string]interface{}
	if err := json.Unmarshal(output, &rawData); err != nil {
		log.Printf("[GetProbeInfoWithTimeout] Failed to parse ffprobe output for %s: %v, attempting fallback", inputPath, err)
		return fm.getFallbackProbeInfo(inputPath)
	}

	if len(rawData) == 0 {
		log.Printf("[GetProbeInfoWithTimeout] FFprobe returned empty data for %s, attempting fallback", inputPath)
		return fm.getFallbackProbeInfo(inputPath)
	}

	// Transform to expected format
	response := &ProbeResponse{
		Samples: make(map[string]interface{}),
	}

	// Extract format information
	if formatData, ok := rawData["format"].(map[string]interface{}); ok {
		response.Format.Name = fm.extractFormatName(formatData)
		duration := fm.extractDuration(formatData)
		if duration > 0 {
			response.Format.Duration = duration
		} else {
			if streamsData, ok := rawData["streams"].([]interface{}); ok {
				for _, streamData := range streamsData {
					if stream, ok := streamData.(map[string]interface{}); ok {
						if streamDuration := fm.extractStreamDuration(stream); streamDuration > duration {
							duration = streamDuration
						}
					}
				}
				response.Format.Duration = duration
			}
		}
	}

	// Extract streams information
	streamTypes := make(map[string]int)
	if streamsData, ok := rawData["streams"].([]interface{}); ok {
		for i, streamData := range streamsData {
			if stream, ok := streamData.(map[string]interface{}); ok {
				probeStream := fm.transformStream(stream, i)
				response.Streams = append(response.Streams, probeStream)
				streamTypes[probeStream.Track]++
			}
		}
	}

	// If we still don't have duration, try to get it from the file size and bitrate
	if response.Format.Duration == 0 {
		if formatData, ok := rawData["format"].(map[string]interface{}); ok {
			if size, ok := formatData["size"].(string); ok {
				if fileSize, err := strconv.ParseInt(size, 10, 64); err == nil {
					if bitRate, ok := formatData["bit_rate"].(string); ok {
						if br, err := strconv.ParseInt(bitRate, 10, 64); err == nil && br > 0 {
							response.Format.Duration = float64(fileSize*8) / float64(br)
						}
					}
				}
			}
		}
	}

	if response.Format.Duration == 0 && len(response.Streams) == 0 {
		log.Printf("[GetProbeInfoWithTimeout] No meaningful data extracted from ffprobe for %s, using fallback", inputPath)
		return fm.getFallbackProbeInfo(inputPath)
	}

	// Log a concise summary
	log.Printf("[GetProbeInfoWithTimeout] FFprobe: %s size=%d duration=%.2fs streams=%d [video=%d audio=%d subtitle=%d] format=%s", inputPath, fileSize, response.Format.Duration, len(response.Streams), streamTypes["video"], streamTypes["audio"], streamTypes["subtitle"], response.Format.Name)

	// Store in cache
	fm.probeCacheMu.Lock()
	fm.probeCache[inputPath] = &ffprobeCacheEntry{
		FileSize: fileSize,
		Result:   response,
		Err:      nil,
		Time:     time.Now(),
	}
	fm.probeCacheMu.Unlock()

	return response, nil
}

// getFallbackProbeInfo provides basic file information when ffprobe fails
func (fm *FFmpegManager) getFallbackProbeInfo(inputPath string) (*ProbeResponse, error) {
	response := &ProbeResponse{
		Format: ProbeFormat{
			Name:     "unknown",
			Duration: 0,
		},
		Streams: []ProbeStream{},
		Samples: make(map[string]interface{}),
	}

	// Try to get basic file information
	if stat, err := os.Stat(inputPath); err == nil {
		// Try to determine format from file extension
		ext := strings.ToLower(filepath.Ext(inputPath))
		switch ext {
		case ".mp4", ".m4v":
			response.Format.Name = "mp4"
		case ".mkv":
			response.Format.Name = "matroska"
		case ".avi":
			response.Format.Name = "avi"
		case ".mov":
			response.Format.Name = "mov"
		case ".webm":
			response.Format.Name = "webm"
		case ".flv":
			response.Format.Name = "flv"
		case ".wmv":
			response.Format.Name = "wmv"
		case ".ts", ".mts":
			response.Format.Name = "mpegts"
		default:
			response.Format.Name = "unknown"
		}

		// For incomplete files, we can't determine duration accurately
		// But we can provide a basic video stream entry
		response.Streams = append(response.Streams, ProbeStream{
			ID:               0,
			Index:            0,
			Track:            "video",
			Codec:            "unknown",
			StreamBitRate:    0,
			StreamMaxBitRate: 0,
			StartTime:        0,
			StartTimeTs:      0,
			Timescale:        1000,
			Width:            0,
			Height:           0,
			FrameRate:        0,
			NumberOfFrames:   nil,
			IsHdr:            false,
			IsDoVi:           false,
			HasBFrames:       false,
			FormatBitRate:    0,
			FormatMaxBitRate: 0,
			Bps:              0,
			NumberOfBytes:    stat.Size(),
			FormatDuration:   0,
			Language:         "eng",
		})

		// Add a basic audio stream entry
		response.Streams = append(response.Streams, ProbeStream{
			ID:               0,
			Index:            1,
			Track:            "audio",
			Codec:            "unknown",
			StreamBitRate:    0,
			StreamMaxBitRate: 0,
			StartTime:        0,
			StartTimeTs:      0,
			Timescale:        1000,
			SampleRate:       0,
			Channels:         0,
			ChannelLayout:    "",
			Title:            nil,
			Language:         "eng",
		})
	}

	return response, nil
}

// extractDuration extracts duration from format data using multiple methods
func (fm *FFmpegManager) extractDuration(formatData map[string]interface{}) float64 {
	// Try duration field first
	if duration, ok := formatData["duration"].(string); ok {
		if dur, err := strconv.ParseFloat(duration, 64); err == nil && dur > 0 {
			return dur
		}
	}

	// Try duration as float
	if duration, ok := formatData["duration"].(float64); ok && duration > 0 {
		return duration
	}

	// Try start_time and end_time calculation
	if startTime, ok := formatData["start_time"].(string); ok {
		if endTime, ok := formatData["end_time"].(string); ok {
			if start, err := strconv.ParseFloat(startTime, 64); err == nil {
				if end, err := strconv.ParseFloat(endTime, 64); err == nil {
					return end - start
				}
			}
		}
	}

	return 0
}

// extractStreamDuration extracts duration from a stream
func (fm *FFmpegManager) extractStreamDuration(stream map[string]interface{}) float64 {
	// Try duration field
	if duration, ok := stream["duration"].(string); ok {
		if dur, err := strconv.ParseFloat(duration, 64); err == nil && dur > 0 {
			return dur
		}
	}

	// Try duration as float
	if duration, ok := stream["duration"].(float64); ok && duration > 0 {
		return duration
	}

	// Try tags.duration
	if tags, ok := stream["tags"].(map[string]interface{}); ok {
		if duration, ok := tags["duration"].(string); ok {
			if dur, err := strconv.ParseFloat(duration, 64); err == nil && dur > 0 {
				return dur
			}
		}
	}

	return 0
}

// extractFormatName extracts the format name from ffprobe output
func (fm *FFmpegManager) extractFormatName(formatData map[string]interface{}) string {
	// Try to get format name from various possible fields
	if formatName, ok := formatData["format_name"].(string); ok {
		return formatName
	}
	if formatLongName, ok := formatData["format_long_name"].(string); ok {
		return formatLongName
	}
	return "unknown"
}

// transformStream transforms a raw stream object to ProbeStream format
func (fm *FFmpegManager) transformStream(stream map[string]interface{}, index int) ProbeStream {
	probeStream := ProbeStream{
		ID:               0,
		Index:            index,
		StreamBitRate:    0,
		StreamMaxBitRate: 0,
		StartTime:        0,
		StartTimeTs:      0,
		Timescale:        1000,
		IsHdr:            false,
		IsDoVi:           false,
		HasBFrames:       false,
		FormatBitRate:    0,
		FormatMaxBitRate: 0,
		Bps:              0,
		NumberOfBytes:    0,
		FormatDuration:   0,
		Language:         "eng", // Default language
	}

	// Extract codec information
	if codecName, ok := stream["codec_name"].(string); ok {
		probeStream.Codec = codecName
	}

	// Determine track type
	if codecType, ok := stream["codec_type"].(string); ok {
		switch codecType {
		case "video":
			probeStream.Track = "video"
			probeStream = fm.extractVideoInfo(stream, probeStream)
		case "audio":
			probeStream.Track = "audio"
			probeStream = fm.extractAudioInfo(stream, probeStream)
		case "subtitle":
			probeStream.Track = "subtitle"
			probeStream = fm.extractSubtitleInfo(stream, probeStream)
		}
	}

	// Extract common stream information
	if bitRate, ok := stream["bit_rate"].(string); ok {
		if br, err := strconv.Atoi(bitRate); err == nil {
			probeStream.StreamBitRate = br
			probeStream.Bps = br
		}
	} else if bitRate, ok := stream["bit_rate"].(float64); ok {
		probeStream.StreamBitRate = int(bitRate)
		probeStream.Bps = int(bitRate)
	}

	if startTime, ok := stream["start_time"].(string); ok {
		if st, err := strconv.ParseFloat(startTime, 64); err == nil {
			probeStream.StartTime = int(st * 1000) // Convert to milliseconds
		}
	} else if startTime, ok := stream["start_time"].(float64); ok {
		probeStream.StartTime = int(startTime * 1000) // Convert to milliseconds
	}

	if startTimeTs, ok := stream["start_pts"].(float64); ok {
		probeStream.StartTimeTs = int(startTimeTs)
	}

	// Extract duration
	if duration, ok := stream["duration"].(string); ok {
		if dur, err := strconv.ParseFloat(duration, 64); err == nil {
			probeStream.FormatDuration = dur
		}
	} else if duration, ok := stream["duration"].(float64); ok {
		probeStream.FormatDuration = duration
	}

	// Extract language and title from tags
	if tags, ok := stream["tags"].(map[string]interface{}); ok {
		if lang, ok := tags["language"].(string); ok && lang != "" {
			probeStream.Language = lang
		}
		if title, ok := tags["title"].(string); ok && title != "" {
			probeStream.Title = &title
		}
		// Also check for language in other common fields
		if lang, ok := tags["lang"].(string); ok && lang != "" && probeStream.Language == "eng" {
			probeStream.Language = lang
		}
	}

	// Extract format bitrate information
	if formatBitRate, ok := stream["bit_rate"].(string); ok {
		if fbr, err := strconv.Atoi(formatBitRate); err == nil {
			probeStream.FormatBitRate = fbr
		}
	} else if formatBitRate, ok := stream["bit_rate"].(float64); ok {
		probeStream.FormatBitRate = int(formatBitRate)
	}

	return probeStream
}

// extractVideoInfo extracts video-specific information
func (fm *FFmpegManager) extractVideoInfo(stream map[string]interface{}, probeStream ProbeStream) ProbeStream {
	if width, ok := stream["width"].(float64); ok {
		probeStream.Width = int(width)
	}

	if height, ok := stream["height"].(float64); ok {
		probeStream.Height = int(height)
	}

	// Extract frame rate
	if rFrameRate, ok := stream["r_frame_rate"].(string); ok {
		if frameRate := fm.parseFrameRate(rFrameRate); frameRate > 0 {
			probeStream.FrameRate = frameRate
		}
	} else if avgFrameRate, ok := stream["avg_frame_rate"].(string); ok {
		if frameRate := fm.parseFrameRate(avgFrameRate); frameRate > 0 {
			probeStream.FrameRate = frameRate
		}
	}

	// Extract bitrate information
	if bitRate, ok := stream["bit_rate"].(string); ok {
		if br, err := strconv.Atoi(bitRate); err == nil {
			probeStream.Bps = br
		}
	} else if bitRate, ok := stream["bit_rate"].(float64); ok {
		probeStream.Bps = int(bitRate)
	}

	// Check for HDR/DoVi (enhanced detection)
	if codecName, ok := stream["codec_name"].(string); ok {
		codecNameLower := strings.ToLower(codecName)
		if strings.Contains(codecNameLower, "hevc") || strings.Contains(codecNameLower, "h265") {
			probeStream.IsHdr = true
		}
	}

	// Check for DoVi (Dolby Vision)
	if profile, ok := stream["profile"].(string); ok {
		if strings.Contains(strings.ToLower(profile), "dolby") || strings.Contains(strings.ToLower(profile), "dovi") {
			probeStream.IsDoVi = true
		}
	}

	// Check for B-frames
	if hasBFrames, ok := stream["has_b_frames"].(float64); ok {
		probeStream.HasBFrames = hasBFrames > 0
	} else if hasBFrames, ok := stream["has_b_frames"].(string); ok {
		if hasBFrames == "1" || strings.ToLower(hasBFrames) == "true" {
			probeStream.HasBFrames = true
		}
	}

	// Extract number of frames if available
	if nbFrames, ok := stream["nb_frames"].(string); ok {
		if frames, err := strconv.Atoi(nbFrames); err == nil && frames > 0 {
			probeStream.NumberOfFrames = &frames
		}
	} else if nbFrames, ok := stream["nb_frames"].(float64); ok && nbFrames > 0 {
		frames := int(nbFrames)
		probeStream.NumberOfFrames = &frames
	}

	// Extract format bitrate for video streams
	if bitRate, ok := stream["bit_rate"].(string); ok {
		if br, err := strconv.Atoi(bitRate); err == nil {
			probeStream.FormatBitRate = br
		}
	} else if bitRate, ok := stream["bit_rate"].(float64); ok {
		probeStream.FormatBitRate = int(bitRate)
	}

	return probeStream
}

// extractAudioInfo extracts audio-specific information
func (fm *FFmpegManager) extractAudioInfo(stream map[string]interface{}, probeStream ProbeStream) ProbeStream {
	if sampleRate, ok := stream["sample_rate"].(string); ok {
		if sr, err := strconv.Atoi(sampleRate); err == nil {
			probeStream.SampleRate = sr
		}
	} else if sampleRate, ok := stream["sample_rate"].(float64); ok {
		probeStream.SampleRate = int(sampleRate)
	}

	if channels, ok := stream["channels"].(float64); ok {
		probeStream.Channels = int(channels)
	} else if channels, ok := stream["channels"].(string); ok {
		if ch, err := strconv.Atoi(channels); err == nil {
			probeStream.Channels = ch
		}
	}

	// Extract channel layout
	if channelLayout, ok := stream["channel_layout"].(string); ok && channelLayout != "" {
		probeStream.ChannelLayout = channelLayout
	}

	// Extract audio bitrate
	if bitRate, ok := stream["bit_rate"].(string); ok {
		if br, err := strconv.Atoi(bitRate); err == nil {
			probeStream.StreamBitRate = br
			probeStream.Bps = br
		}
	} else if bitRate, ok := stream["bit_rate"].(float64); ok {
		probeStream.StreamBitRate = int(bitRate)
		probeStream.Bps = int(bitRate)
	}

	// Extract format bitrate for audio streams
	if bitRate, ok := stream["bit_rate"].(string); ok {
		if br, err := strconv.Atoi(bitRate); err == nil {
			probeStream.FormatBitRate = br
		}
	} else if bitRate, ok := stream["bit_rate"].(float64); ok {
		probeStream.FormatBitRate = int(bitRate)
	}

	// Extract language and title from tags if not already set
	if tags, ok := stream["tags"].(map[string]interface{}); ok {
		if probeStream.Language == "eng" {
			if lang, ok := tags["language"].(string); ok && lang != "" {
				probeStream.Language = lang
			}
		}
		if probeStream.Title == nil {
			if title, ok := tags["title"].(string); ok && title != "" {
				probeStream.Title = &title
			}
		}
	}

	return probeStream
}

// extractSubtitleInfo extracts subtitle-specific information
func (fm *FFmpegManager) extractSubtitleInfo(stream map[string]interface{}, probeStream ProbeStream) ProbeStream {
	// Extract language and title from tags
	if tags, ok := stream["tags"].(map[string]interface{}); ok {
		if lang, ok := tags["language"].(string); ok && lang != "" {
			probeStream.Language = lang
		}
		if title, ok := tags["title"].(string); ok && title != "" {
			probeStream.Title = &title
		}
		// Also check for language in other common fields
		if lang, ok := tags["lang"].(string); ok && lang != "" && probeStream.Language == "eng" {
			probeStream.Language = lang
		}
	}

	// Extract subtitle codec
	if codecName, ok := stream["codec_name"].(string); ok {
		probeStream.Codec = codecName
	}

	// Extract subtitle bitrate (usually 0 for subtitles)
	if bitRate, ok := stream["bit_rate"].(string); ok {
		if br, err := strconv.Atoi(bitRate); err == nil {
			probeStream.StreamBitRate = br
		}
	} else if bitRate, ok := stream["bit_rate"].(float64); ok {
		probeStream.StreamBitRate = int(bitRate)
	}

	return probeStream
}

// parseFrameRate parses frame rate from ffprobe format (e.g., "30/1", "29.97")
func (fm *FFmpegManager) parseFrameRate(frameRateStr string) float64 {
	parts := strings.Split(frameRateStr, "/")
	if len(parts) == 2 {
		num, err := strconv.ParseFloat(parts[0], 64)
		if err != nil {
			return 0
		}
		den, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return 0
		}
		if den != 0 {
			return num / den
		}
	}

	// Try parsing as decimal
	if frameRate, err := strconv.ParseFloat(frameRateStr, 64); err == nil {
		return frameRate
	}

	return 0
}

// IsAvailable checks if FFmpeg is available for use
func (fm *FFmpegManager) IsAvailable() bool {
	return fm.ffmpegPath != ""
}

// RemuxStream remuxes a video, copying video/audio and including a single subtitle track
func (fm *FFmpegManager) RemuxStream(inputPath string, w http.ResponseWriter, r *http.Request, subtitleTrack int) error {
	// Check if input is HEVC and handle accordingly
	isHEVC := false
	if fm.IsProbeAvailable() {
		probeInfo, err := fm.GetProbeInfo(inputPath)
		if err == nil && probeInfo != nil {
			for _, stream := range probeInfo.Streams {
				if stream.Track == "video" {
					codecLower := strings.ToLower(stream.Codec)
					if strings.Contains(codecLower, "hevc") || strings.Contains(codecLower, "h.265") || strings.Contains(codecLower, "h265") {
						isHEVC = true
						break
					}
				}
			}
		}
	}

	args := []string{
		"-i", inputPath,
	}

	if isHEVC {
		args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-crf", "18")
	} else {
		args = append(args, "-c:v", "copy")
	}

	args = append(args, "-c:a", "copy", "-map", "0:v:0", "-map", "0:a:0")

	if subtitleTrack >= 0 {
		args = append(args, "-map", fmt.Sprintf("0:s:%d", subtitleTrack), "-c:s", "mov_text")
	}

	args = append(args, "-f", "mp4", "-movflags", "frag_keyframe+empty_moov", "-")

	if fm.config.Debug {
		log.Printf("[RemuxStream] FFmpeg remux command: %s %s", fm.ffmpegPath, strings.Join(args, " "))
	}

	cmd := exec.Command(fm.ffmpegPath, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %v", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start ffmpeg: %v", err)
	}

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "no-cache")

	go func() {
		defer cmd.Wait()
		io.Copy(w, stdout)
	}()

	if fm.config.Debug {
		go func() {
			scanner := bufio.NewScanner(stderr)
			for scanner.Scan() {
				log.Printf("FFmpeg: %s", scanner.Text())
			}
		}()
	}

	return nil
}

// RunFFmpeg executes a custom FFmpeg command with the given arguments
func (fm *FFmpegManager) RunFFmpeg(args []string) error {
	if fm.config.Debug {
		log.Printf("[RunFFmpeg] FFmpeg custom command: %s %s", fm.ffmpegPath, strings.Join(args, " "))
	}

	cmd := exec.Command(fm.ffmpegPath, args...)
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// IsProbeAvailable checks if FFprobe is available for use
func (fm *FFmpegManager) IsProbeAvailable() bool {
	return fm.ffprobePath != ""
}

// GetFFmpegPath returns the FFmpeg executable path
func (fm *FFmpegManager) GetFFmpegPath() string {
	return fm.ffmpegPath
}

// GetFFprobePath returns the FFprobe executable path
func (fm *FFmpegManager) GetFFprobePath() string {
	return fm.ffprobePath
}

// HardwareProfile represents a hardware acceleration profile with codec mappings
type HardwareProfile struct {
	Name          string
	DisplayName   string
	Platform      string
	VideoEncoders map[string]string // codec -> encoder mapping
	VideoDecoders map[string]string // codec -> decoder mapping
	AudioEncoders map[string]string
	AudioDecoders map[string]string
	InputArgs     []string
	Available     bool
}

// GetHardwareProfiles returns available hardware acceleration profiles (matching Node.js server.js)
func (fm *FFmpegManager) GetHardwareProfiles() map[string]*HardwareProfile {
	profiles := make(map[string]*HardwareProfile)

	// VAAPI Profile (Linux Intel/AMD)
	vaapiProfile := &HardwareProfile{
		Name:        "vaapi-renderD128",
		DisplayName: "VAAPI (Intel/AMD)",
		Platform:    "linux",
		VideoEncoders: map[string]string{
			"libx264": "h264_vaapi",
			"hevc":    "hevc_vaapi",
			"h264":    "h264_vaapi",
			"h265":    "hevc_vaapi",
		},
		VideoDecoders: map[string]string{
			"h264": "h264",
			"hevc": "hevc",
			"h265": "hevc",
		},
		AudioEncoders: map[string]string{
			"aac": "aac",
		},
		AudioDecoders: map[string]string{
			"aac": "aac",
			"ac3": "ac3",
		},
		InputArgs: []string{"-hwaccel", "vaapi", "-hwaccel_device", "/dev/dri/renderD128"},
		Available: fm.testHardwareAccel("vaapi"),
	}
	profiles["vaapi-renderD128"] = vaapiProfile

	// NVIDIA NVENC Profile
	nvencProfile := &HardwareProfile{
		Name:        "nvenc",
		DisplayName: "NVIDIA NVENC",
		Platform:    "all",
		VideoEncoders: map[string]string{
			"libx264": "h264_nvenc",
			"hevc":    "hevc_nvenc",
			"h264":    "h264_nvenc",
			"h265":    "hevc_nvenc",
		},
		VideoDecoders: map[string]string{
			"h264": "h264_cuvid",
			"hevc": "hevc_cuvid",
			"h265": "hevc_cuvid",
		},
		AudioEncoders: map[string]string{
			"aac": "aac",
		},
		AudioDecoders: map[string]string{
			"aac": "aac",
			"ac3": "ac3",
		},
		InputArgs: []string{"-hwaccel", "cuda", "-hwaccel_device", "0"},
		Available: fm.testHardwareAccel("cuda"),
	}
	profiles["nvenc"] = nvencProfile

	// VideoToolbox Profile (macOS)
	videotoolboxProfile := &HardwareProfile{
		Name:        "videotoolbox",
		DisplayName: "VideoToolbox (macOS)",
		Platform:    "darwin",
		VideoEncoders: map[string]string{
			"libx264": "h264_videotoolbox",
			"hevc":    "hevc_videotoolbox",
			"h264":    "h264_videotoolbox",
			"h265":    "hevc_videotoolbox",
		},
		VideoDecoders: map[string]string{
			"h264": "h264",
			"hevc": "hevc",
			"h265": "hevc",
		},
		AudioEncoders: map[string]string{
			"aac": "aac",
		},
		AudioDecoders: map[string]string{
			"aac": "aac",
			"ac3": "ac3",
		},
		InputArgs: []string{"-hwaccel", "videotoolbox"},
		Available: fm.testHardwareAccel("videotoolbox"),
	}
	profiles["videotoolbox"] = videotoolboxProfile

	// Intel Quick Sync Profile
	qsvProfile := &HardwareProfile{
		Name:        "qsv",
		DisplayName: "Intel Quick Sync",
		Platform:    "all",
		VideoEncoders: map[string]string{
			"libx264": "h264_qsv",
			"hevc":    "hevc_qsv",
			"h264":    "h264_qsv",
			"h265":    "hevc_qsv",
		},
		VideoDecoders: map[string]string{
			"h264": "h264_qsv",
			"hevc": "hevc_qsv",
			"h265": "hevc_qsv",
		},
		AudioEncoders: map[string]string{
			"aac": "aac",
		},
		AudioDecoders: map[string]string{
			"aac": "aac",
			"ac3": "ac3",
		},
		InputArgs: []string{"-hwaccel", "qsv", "-hwaccel_device", "/dev/dri/renderD128"},
		Available: fm.testHardwareAccel("qsv"),
	}
	profiles["qsv"] = qsvProfile

	return profiles
}

// GetOptimalEncoder selects the best encoder for a given codec and client capabilities
func (fm *FFmpegManager) GetOptimalEncoder(inputCodec string, capabilities *ClientCapabilities, preferHardware bool) (string, []string) {
	profiles := fm.GetHardwareProfiles()

	// If client supports HEVC and input is HEVC, try to use HEVC encoder
	codecLower := strings.ToLower(inputCodec)
	isInputHEVC := strings.Contains(codecLower, "hevc") || strings.Contains(codecLower, "h.265") || strings.Contains(codecLower, "h265")

	// Determine target codec based on client capabilities
	var targetCodec string
	if isInputHEVC && capabilities != nil && capabilities.SupportsHEVC {
		targetCodec = "hevc"
	} else {
		targetCodec = "h264" // Default fallback
	}

	// If hardware acceleration is preferred and available
	if preferHardware && fm.config.HardwareAcceleration {
		// Try profiles in order of preference
		profileOrder := []string{"videotoolbox", "nvenc", "vaapi-renderD128", "qsv"}

		for _, profileName := range profileOrder {
			if profile, exists := profiles[profileName]; exists && profile.Available {
				if encoder, hasEncoder := profile.VideoEncoders[targetCodec]; hasEncoder {
					log.Printf("GetOptimalEncoder: Using hardware encoder %s for %s (profile: %s)", encoder, targetCodec, profileName)
					return encoder, profile.InputArgs
				}
			}
		}
	}

	// Fallback to software encoding
	if targetCodec == "hevc" {
		log.Printf("GetOptimalEncoder: Using software encoder libx265 for HEVC")
		return "libx265", []string{}
	} else {
		log.Printf("GetOptimalEncoder: Using software encoder libx264 for H.264")
		return "libx264", []string{}
	}
}
