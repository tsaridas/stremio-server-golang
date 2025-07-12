package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	Duration    float64            `json:"duration"`
	Width       int                `json:"width"`
	Height      int                `json:"height"`
	BitRate     int                `json:"bit_rate"`
	CodecName   string             `json:"codec_name"`
	CodecType   string             `json:"codec_type"`
	Streams     []StreamInfo       `json:"streams"`
	Format      FormatInfo         `json:"format"`
}

// StreamInfo contains stream information
type StreamInfo struct {
	Index       int     `json:"index"`
	CodecName   string  `json:"codec_name"`
	CodecType   string  `json:"codec_type"`
	Width       int     `json:"width,omitempty"`
	Height      int     `json:"height,omitempty"`
	BitRate     int     `json:"bit_rate,omitempty"`
	Duration    float64 `json:"duration,omitempty"`
	SampleRate  int     `json:"sample_rate,omitempty"`
	Channels    int     `json:"channels,omitempty"`
	Language    string  `json:"language,omitempty"`
	Title       string  `json:"title,omitempty"`
}

// FormatInfo contains format information
type FormatInfo struct {
	Duration    float64 `json:"duration"`
	BitRate     int     `json:"bit_rate"`
	Size        int64   `json:"size"`
}

// TranscodeJob represents a transcoding job
type TranscodeJob struct {
	ID          string
	InputPath   string
	OutputPath  string
	Options     TranscodeOptions
	Status      string
	Progress    float64
	Error       error
	StartTime   time.Time
	EndTime     time.Time
	mu          sync.RWMutex
}

// TranscodeOptions holds transcoding parameters
type TranscodeOptions struct {
	VideoCodec     string
	AudioCodec     string
	VideoBitRate   int
	AudioBitRate   int
	Width          int
	Height         int
	FrameRate      int
	Quality        int
	HardwareAccel  bool
	SegmentDuration int
	OutputFormat   string
}

// NewFFmpegManager creates a new FFmpeg manager
func NewFFmpegManager(config *FFmpegConfig) (*FFmpegManager, error) {
	ffmpegPath, err := findFFmpeg()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg not found: %v", err)
	}
	
	ffprobePath, err := findFFprobe()
	if err != nil {
		return nil, fmt.Errorf("ffprobe not found: %v", err)
	}
	
	manager := &FFmpegManager{
		ffmpegPath:  ffmpegPath,
		ffprobePath: ffprobePath,
		config:      config,
	}
	
	// Test FFmpeg installation
	if err := manager.testInstallation(); err != nil {
		return nil, fmt.Errorf("ffmpeg test failed: %v", err)
	}
	
	log.Printf("FFmpeg initialized: %s, FFprobe: %s", ffmpegPath, ffprobePath)
	return manager, nil
}

// findFFmpeg locates the FFmpeg executable
func findFFmpeg() (string, error) {
	paths := []string{
		os.Getenv("FFMPEG_BIN") + "/ffmpeg",
		"/usr/lib/jellyfin-ffmpeg/ffmpeg",
		"/usr/bin/ffmpeg",
		"/usr/local/bin/ffmpeg",
		"ffmpeg", // Try PATH
	}
	
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	
	return "", fmt.Errorf("ffmpeg not found in any of the expected locations")
}

// findFFprobe locates the FFprobe executable
func findFFprobe() (string, error) {
	paths := []string{
		os.Getenv("FFPROBE_BIN") + "/ffprobe",
		"/usr/lib/jellyfin-ffmpeg/ffprobe",
		"/usr/bin/ffprobe",
		"/usr/local/bin/ffprobe",
		"ffprobe", // Try PATH
	}
	
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err == nil {
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
		log.Printf("FFmpeg command: %s %s", fm.ffmpegPath, strings.Join(args, " "))
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
		log.Printf("FFmpeg HLS command: %s %s", fm.ffmpegPath, strings.Join(args, " "))
	}
	
	cmd := exec.Command(fm.ffmpegPath, args...)
	cmd.Stderr = os.Stderr
	
	return cmd.Run()
}

// StreamTranscode transcodes a video stream in real-time
func (fm *FFmpegManager) StreamTranscode(inputPath string, w http.ResponseWriter, r *http.Request, options TranscodeOptions) error {
	args := fm.buildStreamArgs(inputPath, options)
	
	if fm.config.Debug {
		log.Printf("FFmpeg stream command: %s %s", fm.ffmpegPath, strings.Join(args, " "))
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
				log.Printf("FFmpeg: %s", scanner.Text())
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
		log.Printf("FFmpeg thumbnail command: %s %s", fm.ffmpegPath, strings.Join(args, " "))
	}
	
	cmd := exec.Command(fm.ffmpegPath, args...)
	cmd.Stderr = os.Stderr
	
	return cmd.Run()
}

// GetHardwareAccelerationInfo gets information about available hardware acceleration
func (fm *FFmpegManager) GetHardwareAccelerationInfo() map[string]interface{} {
	info := make(map[string]interface{})
	
	// Test VAAPI
	if fm.testHardwareAccel("vaapi") {
		info["vaapi"] = map[string]interface{}{
			"available": true,
			"device":    "/dev/dri/renderD128",
		}
	}
	
	// Test CUDA
	if fm.testHardwareAccel("cuda") {
		info["cuda"] = map[string]interface{}{
			"available": true,
			"device":    "0",
		}
	}
	
	// Test VideoToolbox (macOS)
	if fm.testHardwareAccel("videotoolbox") {
		info["videotoolbox"] = map[string]interface{}{
			"available": true,
		}
	}
	
	return info
}

// testHardwareAccel tests if a specific hardware acceleration is available
func (fm *FFmpegManager) testHardwareAccel(accelType string) bool {
	var args []string
	
	switch accelType {
	case "vaapi":
		args = []string{"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=1", "-c:v", "h264_vaapi", "-f", "null", "-"}
	case "cuda":
		args = []string{"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=1", "-c:v", "h264_nvenc", "-f", "null", "-"}
	case "videotoolbox":
		args = []string{"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=1", "-c:v", "h264_videotoolbox", "-f", "null", "-"}
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
		log.Printf("FFmpeg audio extraction command: %s %s", fm.ffmpegPath, strings.Join(args, " "))
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