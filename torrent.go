package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"bytes"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/bencode"
)

// TorrentManager manages all torrent operations
type TorrentManager struct {
	client    *torrent.Client
	engines   map[string]*TorrentEngine
	mu        sync.RWMutex
	cachePath string
}

// NewTorrentManager creates a new torrent manager
func NewTorrentManager(cachePath string) (*TorrentManager, error) {
	config := torrent.NewDefaultClientConfig()
	config.DataDir = cachePath
	config.ListenPort = 0 // Let the system choose a port
	
	client, err := torrent.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create torrent client: %v", err)
	}
	
	return &TorrentManager{
		client:    client,
		engines:   make(map[string]*TorrentEngine),
		cachePath: cachePath,
	}, nil
}

// AddTorrent adds a new torrent to the manager
func (tm *TorrentManager) AddTorrent(magnetURI string) (*TorrentEngine, error) {
	t, err := tm.client.AddMagnet(magnetURI)
	if err != nil {
		return nil, fmt.Errorf("failed to add magnet: %v", err)
	}
	
	// Wait for metadata
	<-t.GotInfo()
	
	infoHash := t.InfoHash().String()
	
	engine := &TorrentEngine{
		InfoHash:    infoHash,
		Torrent:     t,
		Files:       make([]TorrentFile, 0, len(t.Files())),
		Status:      "downloading",
		Peers:       0,
		Downloaded:  0,
		TotalSize:   0,
		CreatedAt:   time.Now(),
		manager:     tm,
	}
	
	// Process files
	for i, file := range t.Files() {
		tf := TorrentFile{
			Name:   file.DisplayPath(),
			Size:   file.Length(),
			Index:  i,
			Path:   file.Path(),
		}
		engine.Files = append(engine.Files, tf)
		engine.TotalSize += file.Length()
	}
	
	tm.mu.Lock()
	tm.engines[infoHash] = engine
	tm.mu.Unlock()
	
	// Start monitoring
	go engine.monitor()
	
	log.Printf("Added torrent: %s (%d files, %d bytes)", 
		t.Info().Name, len(engine.Files), engine.TotalSize)
	
	return engine, nil
}

// GetTorrent retrieves a torrent by info hash
func (tm *TorrentManager) GetTorrent(infoHash string) (*TorrentEngine, bool) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	engine, exists := tm.engines[infoHash]
	return engine, exists
}

// RemoveTorrent removes a torrent from the manager
func (tm *TorrentManager) RemoveTorrent(infoHash string) error {
	tm.mu.Lock()
	engine, exists := tm.engines[infoHash]
	if !exists {
		tm.mu.Unlock()
		return fmt.Errorf("torrent not found: %s", infoHash)
	}
	delete(tm.engines, infoHash)
	tm.mu.Unlock()
	
	// Stop the torrent
	engine.Torrent.Drop()
	
	log.Printf("Removed torrent: %s", infoHash)
	return nil
}

// ListTorrents returns all active torrents
func (tm *TorrentManager) ListTorrents() []*TorrentEngine {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	
	engines := make([]*TorrentEngine, 0, len(tm.engines))
	for _, engine := range tm.engines {
		engines = append(engines, engine)
	}
	return engines
}

// Close closes the torrent manager
func (tm *TorrentManager) Close() error {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	
	// Drop all torrents
	for _, engine := range tm.engines {
		engine.Torrent.Drop()
	}
	
	// Close client
	tm.client.Close()
	return nil
}

// TorrentEngine represents a single torrent with enhanced functionality
type TorrentEngine struct {
	InfoHash    string
	Torrent     *torrent.Torrent
	Files       []TorrentFile
	Status      string
	Peers       int
	Downloaded  int64
	TotalSize   int64
	CreatedAt   time.Time
	manager     *TorrentManager
	mu          sync.RWMutex
}

// monitor continuously monitors the torrent status
func (e *TorrentEngine) monitor() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			e.updateStatus()
		case <-e.Torrent.Closed():
			return
		}
	}
}

// updateStatus updates the torrent status
func (e *TorrentEngine) updateStatus() {
	e.mu.Lock()
	defer e.mu.Unlock()
	
	// Update peer count
	e.Peers = e.Torrent.Stats().TotalPeers
	
	// Update downloaded bytes
	e.Downloaded = e.Torrent.BytesCompleted()
	
	// Update status
	if e.Torrent.Complete.Bool() {
		e.Status = "completed"
	} else if e.Downloaded > 0 {
		e.Status = "downloading"
	} else {
		e.Status = "starting"
	}
}

// GetFile returns a specific file by index
func (e *TorrentEngine) GetFile(index int) (*TorrentFile, error) {
	if index < 0 || index >= len(e.Files) {
		return nil, fmt.Errorf("invalid file index: %d", index)
	}
	return &e.Files[index], nil
}

// StreamFile streams a file with range support
func (e *TorrentEngine) StreamFile(fileIndex int, w http.ResponseWriter, r *http.Request) error {
	if fileIndex < 0 || fileIndex >= len(e.Torrent.Files()) {
		return fmt.Errorf("invalid file index: %d", fileIndex)
	}
	
	file := e.Torrent.Files()[fileIndex]
	
	// Parse range header
	rangeHeader := r.Header.Get("Range")
	var start, end int64
	
	if rangeHeader != "" {
		// Parse "bytes=start-end" format
		rangeStr := strings.TrimPrefix(rangeHeader, "bytes=")
		parts := strings.Split(rangeStr, "-")
		
		if len(parts) == 2 {
			if parts[0] != "" {
				start, _ = strconv.ParseInt(parts[0], 10, 64)
			}
			if parts[1] != "" {
				end, _ = strconv.ParseInt(parts[1], 10, 64)
			} else {
				end = file.Length() - 1
			}
		}
	} else {
		start = 0
		end = file.Length() - 1
	}
	
	// Set headers
	w.Header().Set("Content-Type", getContentType(file.DisplayPath()))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	
	if rangeHeader != "" {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, file.Length()))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	
	// Create reader for the specified range
	reader := file.NewReader()
	reader.SetReadahead(1024 * 1024) // 1MB readahead
	
	if start > 0 {
		reader.Seek(start, io.SeekStart)
	}
	
	// Stream the data
	_, err := io.CopyN(w, reader, end-start+1)
	return err
}

// GetStats returns torrent statistics
func (e *TorrentEngine) GetStats() map[string]interface{} {
	e.mu.RLock()
	defer e.mu.RUnlock()
	
	progress := float64(0)
	if e.TotalSize > 0 {
		progress = float64(e.Downloaded) / float64(e.TotalSize) * 100
	}
	
	return map[string]interface{}{
		"infoHash":  e.InfoHash,
		"name":      e.Torrent.Info().Name,
		"status":    e.Status,
		"peers":     e.Peers,
		"files":     len(e.Files),
		"downloaded": e.Downloaded,
		"totalSize": e.TotalSize,
		"progress":  progress,
		"createdAt": e.CreatedAt,
	}
}

// getContentType determines the content type based on file extension
func getContentType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	
	switch ext {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".avi":
		return "video/x-msvideo"
	case ".mkv":
		return "video/x-matroska"
	case ".mov":
		return "video/quicktime"
	case ".wmv":
		return "video/x-ms-wmv"
	case ".flv":
		return "video/x-flv"
	case ".webm":
		return "video/webm"
	case ".ts":
		return "video/MP2T"
	case ".m3u8":
		return "application/vnd.apple.mpegurl"
	case ".srt":
		return "text/plain"
	case ".vtt":
		return "text/vtt"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	default:
		return "application/octet-stream"
	}
}

// calculateInfoHash calculates the SHA1 hash of torrent info
func calculateInfoHash(info *metainfo.Info) string {
	var buf bytes.Buffer
	_ = bencode.NewEncoder(&buf).Encode(info)
	hash := sha1.Sum(buf.Bytes())
	return hex.EncodeToString(hash[:])
}

// ensureCacheDir ensures the cache directory exists
func ensureCacheDir(cachePath string) error {
	return os.MkdirAll(cachePath, 0755)
} 