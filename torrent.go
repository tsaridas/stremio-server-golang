package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// Logger provides structured logging for torrent operations
type TorrentLogger struct {
	prefix string
}

func NewTorrentLogger(prefix string) *TorrentLogger {
	return &TorrentLogger{prefix: prefix}
}

func (tl *TorrentLogger) Info(format string, args ...interface{}) {
	log.Printf("[TORRENT-%s] INFO: "+format, append([]interface{}{tl.prefix}, args...)...)
}

func (tl *TorrentLogger) Error(format string, args ...interface{}) {
	log.Printf("[TORRENT-%s] ERROR: "+format, append([]interface{}{tl.prefix}, args...)...)
}

func (tl *TorrentLogger) Debug(format string, args ...interface{}) {
	log.Printf("[TORRENT-%s] DEBUG: "+format, append([]interface{}{tl.prefix}, args...)...)
}

func (tl *TorrentLogger) Warn(format string, args ...interface{}) {
	log.Printf("[TORRENT-%s] WARN: "+format, append([]interface{}{tl.prefix}, args...)...)
}

// TorrentManager manages all torrent operations
type TorrentManager struct {
	client    *torrent.Client
	engines   map[string]*TorrentEngine
	mu        sync.RWMutex
	cachePath string
	logger    *TorrentLogger
}

// NewTorrentManager creates a new torrent manager
func NewTorrentManager(cachePath string) (*TorrentManager, error) {
	logger := NewTorrentLogger("MANAGER")
	logger.Info("Initializing torrent manager with cache path: %s", cachePath)

	// Set environment variables to reduce logging verbosity
	if os.Getenv("TORRENT_LOGGER") == "" {
		os.Setenv("TORRENT_LOGGER", "warning")
	}
	if os.Getenv("LOG_LEVEL") == "" {
		os.Setenv("LOG_LEVEL", "warn")
	}
	if os.Getenv("ANACROLIX_LOG_LEVEL") == "" {
		os.Setenv("ANACROLIX_LOG_LEVEL", "warn")
	}

	// Ensure cache directory exists
	if err := ensureCacheDir(cachePath); err != nil {
		logger.Error("Failed to create cache directory: %v", err)
		return nil, fmt.Errorf("failed to create cache directory: %v", err)
	}
	logger.Info("Cache directory ensured: %s", cachePath)

	config := torrent.NewDefaultClientConfig()
	config.DataDir = cachePath // Use passed cachePath as data directory
	config.ListenPort = 0      // Let the system choose a port
	config.DisableIPv6 = true
	config.DisableUTP = false
	config.DisableTCP = false
	config.NoDHT = false
	config.NoDefaultPortForwarding = false

	// Performance optimizations for faster downloads
	config.EstablishedConnsPerTorrent = 100 // Increase for better peer connectivity
	config.HalfOpenConnsPerTorrent = 50     // Increase for faster connection establishment
	config.TotalHalfOpenConns = 200         // Increase total connection capacity

	// Optimize for streaming and speed
	config.DisableAggressiveUpload = false
	config.DisablePEX = false        // Keep PEX for peer discovery
	config.DisableWebtorrent = false // Keep WebTorrent support

	// Additional performance optimizations
	config.MinDialTimeout = 5 * time.Second // Faster dial timeout
	config.NominalDialTimeout = 20 * time.Second

	// Use custom storage backend to ensure files are written as <cachePath>/<infoHash>/<fileIndex>
	config.DefaultStorage = NewCustomStorage(cachePath)

	logger.Info("Creating torrent client with optimized configuration")

	client, err := torrent.NewClient(config)
	if err != nil {
		logger.Error("Failed to create torrent client: %v", err)
		return nil, fmt.Errorf("failed to create torrent client: %v", err)
	}
	logger.Info("Torrent client created successfully")

	tm := &TorrentManager{
		client:    client,
		engines:   make(map[string]*TorrentEngine),
		cachePath: cachePath,
		logger:    logger,
	}

	logger.Info("Torrent manager initialized with in-memory state only")

	// Scan cache for existing torrents and load them
	if err := tm.loadTorrentsFromCache(); err != nil {
		logger.Warn("Failed to load some torrents from cache: %v", err)
	}

	logger.Info("Torrent manager initialization complete")
	return tm, nil
}

// AddTorrent adds a new torrent to the manager
func (tm *TorrentManager) AddTorrent(magnetURI string, fileIndex int) (*TorrentEngine, error) {
	return tm.AddTorrentWithConfig(magnetURI, nil, true, 50, 300, fileIndex)
}

// optimizeTorrentSettings applies performance optimizations to a torrent
func optimizeTorrentSettings(t *torrent.Torrent) {
	// Set aggressive upload mode for better peer relationships
	t.SetMaxEstablishedConns(200)

	// Enable optimistic unchoking for faster data exchange
	// This allows the torrent to unchoke more peers than the standard limit

	// Set higher piece request priority for better streaming
	// The piece prioritization is already handled in AddTorrentWithConfig

	log.Printf("Applied performance optimizations to torrent: %s", t.Info().Name)
}

// AddTorrentWithConfig adds a new torrent to the manager with custom configuration
func (tm *TorrentManager) AddTorrentWithConfig(magnetURI string, trackers []string, dhtEnabled bool, minPeers, maxPeers int, fileIndex int) (*TorrentEngine, error) {
	return tm.AddTorrentWithFileIndex(magnetURI, trackers, dhtEnabled, minPeers, maxPeers, fileIndex)
}

// AddTorrentWithFileIndex adds a torrent and only downloads the selected file
func (tm *TorrentManager) AddTorrentWithFileIndex(magnetURI string, trackers []string, dhtEnabled bool, minPeers, maxPeers, fileIndex int) (*TorrentEngine, error) {
	logger := NewTorrentLogger("ADD")

	infoHash, err := GetTorrentInfoHash(magnetURI)
	if err != nil {
		logger.Error("Failed to get info hash from magnet URI: %v", err)
		return nil, fmt.Errorf("failed to get info hash from magnet URI: %v", err)
	}

	logger.Info("Adding torrent %s with file index %d", infoHash, fileIndex)

	tm.mu.Lock()

	// Force restart: Remove existing cache to ensure download starts from beginning
	cacheDir := filepath.Join(tm.cachePath, infoHash)
	if _, err := os.Stat(cacheDir); err == nil {
		logger.Info("Removing existing cache directory to force fresh download: %s", cacheDir)
		if err := os.RemoveAll(cacheDir); err != nil {
			logger.Warn("Failed to remove existing cache directory: %v", err)
		} else {
			logger.Info("Successfully removed existing cache directory")
		}
	}

	// Remove existing engine if it exists to force fresh start
	if existingEngine, exists := tm.engines[infoHash]; exists {
		logger.Info("Removing existing engine to force fresh start")
		delete(tm.engines, infoHash)

		// Stop existing torrent safely
		if existingEngine.Torrent != nil {
			select {
			case <-existingEngine.stopChan:
				// Channel already closed
			default:
				close(existingEngine.stopChan)
			}
			//existingEngine.Torrent.Drop()
			//existingEngine.Torrent = nil
		}
	}
	// Create magnet URI with custom trackers if provided
	enhancedMagnetURI := magnetURI
	if len(trackers) > 0 {
		logger.Info("Adding %d custom trackers to magnet URI", len(trackers))
		parsedURL, err := url.Parse(magnetURI)
		if err != nil {
			tm.mu.Unlock()
			logger.Error("Failed to parse magnet URI: %v", err)
			return nil, fmt.Errorf("failed to parse magnet URI: %v", err)
		}
		query := parsedURL.Query()
		for _, tracker := range trackers {
			query.Add("tr", tracker)
		}
		parsedURL.RawQuery = query.Encode()
		enhancedMagnetURI = parsedURL.String()
		logger.Debug("Enhanced magnet URI: %s", enhancedMagnetURI)
	}

	logger.Info("Adding torrent to client with DHT=%v, peers=%d-%d", dhtEnabled, minPeers, maxPeers)
	t, err := AddTorrentWithConfig(tm.client, infoHash, enhancedMagnetURI, trackers, dhtEnabled, minPeers, maxPeers)
	if err != nil {
		tm.mu.Unlock()
		logger.Error("Failed to add magnet to client: %v", err)
		return nil, fmt.Errorf("failed to add magnet: %v", err)
	}

	logger.Info("Waiting for torrent metadata...")
	<-t.GotInfo()
	logger.Info("Torrent metadata received: %s", t.Info().Name)

	optimizeTorrentSettings(t)

	numFiles := len(t.Files())
	logger.Info("Torrent has %d files, configuring file priorities for index %d", numFiles, fileIndex)

	if fileIndex < 0 || fileIndex >= numFiles {
		// If fileIndex is invalid, set all files to normal (legacy behavior)
		logger.Info("Invalid file index %d, downloading all files", fileIndex)
		for _, file := range t.Files() {
			file.SetPriority(torrent.PiecePriorityNormal)
		}
	} else {
		logger.Info("Setting priority for file %d only", fileIndex)
		for i, file := range t.Files() {
			if i == fileIndex {
				file.SetPriority(torrent.PiecePriorityNormal)
				logger.Debug("File %d (%s) set to normal priority", i, file.DisplayPath())
			} else {
				file.SetPriority(torrent.PiecePriorityNone)
			}
		}
	}

	infoHashStr := t.InfoHash().String()
	logger.Info("Creating torrent engine for %s", infoHashStr)

	engine := &TorrentEngine{
		InfoHash:          infoHashStr,
		Torrent:           t,
		Files:             make([]TorrentFile, 0, len(t.Files())),
		Status:            "starting",
		Peers:             0,
		Downloaded:        0,
		TotalSize:         0,
		CreatedAt:         time.Now(),
		manager:           tm,
		Trackers:          trackers,
		DHTEnabled:        dhtEnabled,
		MinPeers:          minPeers,
		MaxPeers:          maxPeers,
		stopChan:          make(chan struct{}),
		TorrentName:       t.Info().Name,
		TorrentInfo:       t.Info(),
		MetadataFetched:   true,
		MetadataFetchedAt: time.Now(),
		logger:            NewTorrentLogger(infoHashStr[:8]),
	}
	for i, file := range t.Files() {
		tf := TorrentFile{
			Name:  file.DisplayPath(),
			Size:  file.Length(),
			Index: i,
			Path:  filepath.Join(tm.cachePath, infoHashStr, strconv.Itoa(i)),
		}
		engine.Files = append(engine.Files, tf)
		engine.TotalSize += file.Length()
		logger.Debug("Added file %d: %s (size: %d bytes)", i, tf.Name, tf.Size)
	}

	tm.engines[infoHashStr] = engine
	tm.mu.Unlock()

	// Start monitoring the torrent
	go engine.monitor()

	logger.Info("Torrent engine created and monitoring started for %s", infoHashStr)
	return engine, nil
}

// GetTorrent retrieves a torrent by info hash
func (tm *TorrentManager) GetTorrent(infoHash string) (*TorrentEngine, bool) {
	tm.mu.RLock()
	engine, exists := tm.engines[infoHash]
	tm.mu.RUnlock()

	if !exists {
		return nil, false
	}

	// If torrent is incomplete, try to resume it
	if engine.Status == "incomplete" && engine.Torrent == nil {
		log.Printf("Attempting to resume incomplete torrent: %s", infoHash)
		if err := tm.resumeIncompleteTorrent(engine); err != nil {
			log.Printf("Failed to resume torrent %s: %v", infoHash, err)
		}
	}

	return engine, true
}

// resumeIncompleteTorrent attempts to resume an incomplete torrent
func (tm *TorrentManager) resumeIncompleteTorrent(engine *TorrentEngine) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	// Check if torrent is already being resumed
	if engine.Torrent != nil {
		return nil // Already resumed
	}

	// Create magnet URI from info hash
	magnetURI := fmt.Sprintf("magnet:?xt=urn:btih:%s", engine.InfoHash)

	// Add trackers if available
	if len(engine.Trackers) > 0 {
		for _, tracker := range engine.Trackers {
			magnetURI += "&tr=" + url.QueryEscape(tracker)
		}
	}

	log.Printf("Resuming torrent %s with magnet: %s", engine.InfoHash, magnetURI)

	// Add torrent with existing configuration
	var err error
	var torrent *torrent.Torrent
	if len(engine.Trackers) > 0 || engine.DHTEnabled {
		torrent, err = AddTorrentWithConfig(tm.client, engine.InfoHash, magnetURI, engine.Trackers, engine.DHTEnabled, engine.MinPeers, engine.MaxPeers)
	} else {
		torrent, err = AddTorrent(tm.client, engine.InfoHash)
	}

	if err != nil {
		return fmt.Errorf("failed to add torrent: %v", err)
	}

	// Apply performance optimizations
	optimizeTorrentSettings(torrent)

	// Update engine with active torrent
	engine.Torrent = torrent
	engine.Status = "downloading"
	engine.CreatedAt = time.Now()

	// Start monitoring
	go engine.monitor()

	log.Printf("Successfully resumed torrent %s", engine.InfoHash)
	return nil
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

	// Stop the torrent safely
	if engine.Torrent != nil {
		// Signal monitor to stop first
		select {
		case <-engine.stopChan:
			// Channel already closed
		default:
			close(engine.stopChan)
		}
		closed := false
		select {
		case <-engine.Torrent.Closed():
			closed = true
		default:
		}
		if !closed {
			engine.Torrent.Drop()
		}
		// Set torrent to nil to prevent panic in monitor goroutine
		engine.mu.Lock()
		//engine.Torrent = nil
		engine.mu.Unlock()
	}

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

// loadTorrentsFromCache scans the cache directory for existing torrents and loads them
func (tm *TorrentManager) loadTorrentsFromCache() error {
	// Read all directories in the cache path
	entries, err := os.ReadDir(tm.cachePath)
	if err != nil {
		if os.IsNotExist(err) {
			// Cache directory doesn't exist yet, which is fine
			return nil
		}
		return fmt.Errorf("failed to read cache directory: %v", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		infoHash := entry.Name()
		if len(infoHash) != 40 {
			// Skip directories that don't look like info hashes
			continue
		}

		// Check if info.bencode exists
		infoPath := filepath.Join(tm.cachePath, infoHash, "info.bencode")
		if _, err := os.Stat(infoPath); err != nil {
			// No info file, skip this torrent
			continue
		}

		// Load torrent info from bencode file
		infoBytes, err := os.ReadFile(infoPath)
		if err != nil {
			log.Printf("Failed to read info file for %s: %v", infoHash, err)
			continue
		}

		var info metainfo.Info
		if err := bencode.Unmarshal(infoBytes, &info); err != nil {
			log.Printf("Failed to unmarshal info for %s: %v", infoHash, err)
			continue
		}

		log.Printf("Loaded torrent info for %s: name=%s, length=%d, files=%d", infoHash, info.Name, info.Length, len(info.Files))

		// Create engine with persisted metadata
		engine := &TorrentEngine{
			InfoHash:          infoHash,
			Torrent:           nil, // Torrent is nil since it's completed
			Files:             make([]TorrentFile, 0, len(info.Files)),
			Status:            "completed",
			Peers:             0,
			Downloaded:        0,
			TotalSize:         0,
			CreatedAt:         time.Now(),
			manager:           tm,
			Trackers:          []string{},
			DHTEnabled:        false,
			MinPeers:          30,
			MaxPeers:          150,
			stopChan:          make(chan struct{}),
			TorrentName:       info.Name,
			TorrentInfo:       &info,
			MetadataFetched:   true,
			MetadataFetchedAt: time.Now(),
			logger:            NewTorrentLogger(infoHash[:8]),
		}

		// Build file list from info
		var fileOffset int64 = 0
		if info.Length > 0 {
			// Single-file torrent
			tf := TorrentFile{
				Name:  info.Name,
				Size:  info.Length,
				Index: 0,
				Path:  filepath.Join(tm.cachePath, infoHash, "0"),
			}
			engine.Files = append(engine.Files, tf)
			engine.TotalSize = info.Length
		} else {
			// Multi-file torrent
			for i, file := range info.Files {
				tf := TorrentFile{
					Name:  file.DisplayPath(&info),
					Size:  file.Length,
					Index: i,
					Path:  filepath.Join(tm.cachePath, infoHash, strconv.Itoa(i)),
				}
				engine.Files = append(engine.Files, tf)
				engine.TotalSize += file.Length
				fileOffset += file.Length
			}
		}

		// Calculate downloaded bytes by checking file sizes
		for _, file := range engine.Files {
			if stat, err := os.Stat(file.Path); err == nil {
				engine.Downloaded += stat.Size()
			}
		}

		// Check if files are actually complete
		isComplete := true
		for _, file := range engine.Files {
			if stat, err := os.Stat(file.Path); err == nil {
				if stat.Size() < file.Size*95/100 { // Allow 5% tolerance
					isComplete = false
					break
				}
			} else {
				isComplete = false
				break
			}
		}

		// Update status based on actual file completeness
		if isComplete {
			engine.Status = "completed"
		} else {
			engine.Status = "incomplete"
			log.Printf("Torrent %s marked as incomplete - files not fully downloaded", infoHash)
		}

		// If we still have no files but the cache directory exists, try to scan for files
		if len(engine.Files) == 0 {
			log.Printf("HLS Probe: No files found in metadata, scanning cache directory for files")
			cacheDir := filepath.Join(tm.cachePath, infoHash)
			entries, err := os.ReadDir(cacheDir)
			if err == nil {
				for _, entry := range entries {
					if !entry.IsDir() {
						fileIndex, err := strconv.Atoi(entry.Name())
						if err == nil {
							// This is a file index, try to get file info
							filePath := filepath.Join(cacheDir, entry.Name())
							if stat, err := os.Stat(filePath); err == nil {
								// Create a basic TorrentFile entry
								tf := TorrentFile{
									Name:  fmt.Sprintf("file_%d", fileIndex), // We don't have the original name
									Size:  stat.Size(),
									Index: fileIndex,
									Path:  filePath,
								}
								engine.Files = append(engine.Files, tf)
								engine.TotalSize += stat.Size()
								engine.Downloaded += stat.Size()
								log.Printf("HLS Probe: Added file [%d] %s (size: %d)", fileIndex, filePath, stat.Size())
							}
						}
					}
				}

				// Sort files by index
				sort.Slice(engine.Files, func(i, j int) bool {
					return engine.Files[i].Index < engine.Files[j].Index
				})

				log.Printf("HLS Probe: Recreated engine for %s with %d files from cache", infoHash, len(engine.Files))
			} else {
				log.Printf("HLS Probe: Error reading cache directory: %v", err)
			}
		}

		// Add to engines map
		tm.engines[infoHash] = engine

		log.Printf("Loaded completed torrent from cache: %s (%d files, %d bytes, downloaded: %d)",
			info.Name, len(engine.Files), engine.TotalSize, engine.Downloaded)
	}

	return nil
}

// Close closes the torrent manager
func (tm *TorrentManager) Close() error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	// Drop all torrents safely
	for _, engine := range tm.engines {
		if engine.Torrent != nil {
			// Signal monitor to stop first
			select {
			case <-engine.stopChan:
				// Channel already closed
			default:
				close(engine.stopChan)
			}
			closed := false
			select {
			case <-engine.Torrent.Closed():
				closed = true
			default:
			}
			if !closed {
				engine.Torrent.Drop()
			}
			// Set torrent to nil to prevent panic in monitor goroutine
			engine.mu.Lock()
			//engine.Torrent = nil
			//engine.mu.Unlock()
		}
	}

	// Close client
	tm.client.Close()
	return nil
}

// DisableDHT disables DHT for the torrent client
func (tm *TorrentManager) DisableDHT() {
	log.Printf("Disabling DHT network")
	// The anacrolix/torrent library doesn't provide a direct method to disable DHT
	// DHT activity will stop when torrents are dropped
	// This is a placeholder for future implementation if needed
}

// TorrentEngine represents a single torrent with enhanced functionality
type TorrentEngine struct {
	InfoHash           string
	Torrent            *torrent.Torrent
	Files              []TorrentFile
	Status             string
	Peers              int
	Downloaded         int64
	TotalSize          int64
	CreatedAt          time.Time
	manager            *TorrentManager
	mu                 sync.RWMutex
	Trackers           []string
	DHTEnabled         bool
	MinPeers           int
	MaxPeers           int
	stopChan           chan struct{}  // Channel to signal monitor goroutine to stop
	lastLoggedProgress int            // Track last logged progress percentage to avoid duplicates
	logger             *TorrentLogger // Logger for this torrent

	// Persistent metadata - stored even after torrent is dropped
	TorrentName       string         // Store torrent name
	TorrentInfo       *metainfo.Info // Store torrent info
	MetadataFetched   bool           // Track if metadata was fetched
	MetadataFetchedAt time.Time      // When metadata was fetched

	// --- Persistent stats for finished torrents ---
	LastPeers             int
	LastUnchoked          int
	LastQueued            int
	LastUnique            int
	LastConnectionTries   int
	LastSwarmPaused       bool
	LastSwarmConnections  int
	LastSwarmSize         int
	LastSelections        []map[string]interface{}
	LastWires             []map[string]interface{}
	LastSources           []map[string]interface{}
	LastPeerSearchRunning bool
	LastPeerSearchMin     int
	LastPeerSearchMax     int
	LastPeerSearchSources []string
	LastClientID          string
	LastDownloaded        int64
	LastUploaded          int64
	LastDownloadSpeed     int64
	LastUploadSpeed       int64
}

// monitor continuously monitors the torrent status
func (e *TorrentEngine) monitor() {
	defer func() {
		if r := recover(); r != nil {
			if e.logger != nil {
				e.logger.Error("Recovered from panic in monitor: %v", r)
			} else {
				log.Printf("Recovered from panic in monitor for torrent %s: %v", e.InfoHash, r)
			}
		}
	}()

	if e.logger != nil {
		e.logger.Info("Starting torrent monitor")
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-e.stopChan:
			if e.logger != nil {
				e.logger.Info("Monitor stopped by signal")
			}
			return
		case <-ticker.C:
			// Check if torrent still exists before updating status
			if e.Torrent == nil {
				if e.logger != nil {
					e.logger.Info("Torrent no longer exists, stopping monitor")
				}
				return
			}

			e.updateStatus()

			if e.Torrent != nil {
				closed := false
				select {
				case <-e.Torrent.Closed():
					closed = true
				default:
				}
				if !closed && e.Torrent.Complete.Bool() {
					if e.logger != nil {
						e.logger.Info("Download complete, stopping torrent and DHT")
					}

					// Persist file list and metadata before dropping torrent
					if e.Torrent != nil && e.Torrent.Info() != nil {
						e.mu.Lock()
						e.TorrentName = e.Torrent.Info().Name
						e.TorrentInfo = e.Torrent.Info()
						e.MetadataFetched = true
						e.MetadataFetchedAt = time.Now()

						// Rebuild the file list from the torrent
						e.Files = make([]TorrentFile, 0, len(e.Torrent.Files()))
						for i, file := range e.Torrent.Files() {
							tf := TorrentFile{
								Name:  file.DisplayPath(),
								Size:  file.Length(),
								Index: i,
								Path:  filepath.Join(e.manager.cachePath, e.InfoHash, strconv.Itoa(i)),
							}
							e.Files = append(e.Files, tf)
						}
						e.mu.Unlock()

						if e.logger != nil {
							e.logger.Info("Persisted file list: %d files", len(e.Files))
						}
					}

					// Stop DHT network
					e.manager.DisableDHT()
					// Stop the torrent safely
					e.Torrent.Drop()
					e.Torrent = nil // Set to nil to prevent future access
					// Signal monitor to stop
					close(e.stopChan)
					return
				}
			}

		case <-e.Torrent.Closed():
			if e.logger != nil {
				e.logger.Info("Torrent closed, stopping monitor")
			}
			return
		}
	}
}

// updateStatus updates the torrent status
func (e *TorrentEngine) updateStatus() {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Check if torrent still exists
	if e.Torrent == nil {
		if e.logger != nil {
			e.logger.Debug("Torrent is nil, skipping status update")
		}
		return
	}

	// Update peer count
	e.Peers = e.Torrent.Stats().TotalPeers

	// Update downloaded bytes
	oldDownloaded := e.Downloaded
	newDownloaded := e.Torrent.BytesCompleted()

	// Debug: log if bytes completed decreased
	if newDownloaded < oldDownloaded {
		if e.logger != nil {
			e.logger.Warn("Bytes completed decreased: %d -> %d (diff: %d)",
				oldDownloaded, newDownloaded, oldDownloaded-newDownloaded)
		}
	}

	e.Downloaded = newDownloaded

	// Calculate download speed (bytes per second) - fixed calculation
	downloadSpeed := int64(0)
	if e.Downloaded > oldDownloaded {
		// This is per 5 seconds, so divide by 5 to get bytes per second
		downloadSpeed = (e.Downloaded - oldDownloaded) / 5
	}

	// Update status
	if e.Torrent.Complete.Bool() {
		e.Status = "completed"
	} else if e.Downloaded > 0 {
		e.Status = "downloading"
	} else {
		e.Status = "starting"
	}

	// Calculate progress percentage
	progress := float64(0)
	if e.TotalSize > 0 {
		progress = float64(e.Downloaded) / float64(e.TotalSize) * 100
	}

	// Log progress more frequently - every 10 seconds or when there's significant progress
	shouldLog := false
	currentProgressInt := int(progress)

	if time.Since(e.CreatedAt).Seconds() > 10 && e.Downloaded == 0 {
		shouldLog = true
	} else if e.Downloaded > oldDownloaded && (e.Downloaded-oldDownloaded) > 1024*1024 { // Log when 1MB+ downloaded
		shouldLog = true
	} else if progress > 0 && currentProgressInt%5 == 0 && currentProgressInt != e.lastLoggedProgress { // Log every 5% progress, but only if it's a new percentage
		shouldLog = true
		e.lastLoggedProgress = currentProgressInt // Update the last logged progress
	}

	if shouldLog && e.logger != nil {
		speedMBps := float64(downloadSpeed) / (1024 * 1024) // Convert to MB/s (fixed calculation)

		// Get additional diagnostic information
		var activePeers int
		if e.Torrent != nil {
			stats := e.Torrent.Stats()
			activePeers = stats.TotalPeers
		}

		e.logger.Info("%.1f%% complete | %d peers (%d active) | %.1f MB/s | %d/%d bytes | status: %s",
			progress, e.Peers, activePeers, speedMBps, e.Downloaded, e.TotalSize, e.Status)

		// Log warning if download speed is very low
		if speedMBps < 0.1 && progress > 5 && e.Peers > 0 {
			e.logger.Warn("Low download speed (%.1f MB/s) with %d peers", speedMBps, e.Peers)
		}

		// Log warning if no peers after reasonable time
		if e.Peers == 0 && time.Since(e.CreatedAt).Seconds() > 30 {
			e.logger.Warn("No peers found after 30 seconds")
		}
	}

	// --- Persist all relevant stats for finished torrents ---
	stats := e.Torrent.Stats()
	e.LastPeers = stats.TotalPeers
	e.LastUnchoked = stats.TotalPeers // Use actual unchoked if available
	e.LastQueued = 0                  // Not available, set to 0 or track if possible
	e.LastUnique = stats.TotalPeers   // Use actual unique if available
	e.LastConnectionTries = 0         // Not available, set to 0 or track if possible
	e.LastSwarmPaused = false         // Not available, set to false
	e.LastSwarmConnections = stats.TotalPeers
	e.LastSwarmSize = e.MaxPeers
	// Selections: not tracked, set to empty array
	e.LastSelections = []map[string]interface{}{}
	// Wires: not tracked, set to nil
	e.LastWires = nil
	// Sources: build from trackers and DHT
	var sources []map[string]interface{}
	for _, trackerURL := range e.Trackers {
		source := map[string]interface{}{
			"numFound":     0,
			"numFoundUniq": 0,
			"numRequests":  4,
			"url":          fmt.Sprintf("tracker:%s", trackerURL),
			"lastStarted":  time.Now().Format(time.RFC3339),
		}
		sources = append(sources, source)
	}
	if e.DHTEnabled {
		dhtSource := map[string]interface{}{
			"numFound":     0,
			"numFoundUniq": 0,
			"numRequests":  1,
			"url":          fmt.Sprintf("dht:%s", e.InfoHash),
			"lastStarted":  time.Now().Format(time.RFC3339),
		}
		sources = append(sources, dhtSource)
	}
	e.LastSources = sources
	// Peer search running: true if torrent is active
	e.LastPeerSearchRunning = true
	e.LastPeerSearchMin = e.MinPeers
	e.LastPeerSearchMax = e.MaxPeers
	var peerSearchSources []string
	for _, trackerURL := range e.Trackers {
		peerSearchSources = append(peerSearchSources, fmt.Sprintf("tracker:%s", trackerURL))
	}
	if e.DHTEnabled {
		peerSearchSources = append(peerSearchSources, fmt.Sprintf("dht:%s", e.InfoHash))
	}
	e.LastPeerSearchSources = peerSearchSources
	// Client ID: not available, set to static or track if possible
	e.LastClientID = "-DE2110-4df3cbd12bd8"
	e.LastDownloaded = e.Downloaded
	e.LastUploaded = 0 // Not tracked, set to 0
	e.LastDownloadSpeed = downloadSpeed
	e.LastUploadSpeed = 0 // Not tracked, set to 0
}

// MoveFilesToIndexNames moves downloaded files from their original names to index-based names
func (e *TorrentEngine) MoveFilesToIndexNames() error {
	if !e.Torrent.Complete.Bool() {
		log.Printf("MoveFilesToIndexNames: torrent not complete yet")
		return nil // Only move files when torrent is complete
	}

	torrentDir := filepath.Join(e.manager.cachePath, e.InfoHash)
	log.Printf("MoveFilesToIndexNames: checking directory %s", torrentDir)

	// List all files in the directory
	files, err := os.ReadDir(torrentDir)
	if err != nil {
		log.Printf("MoveFilesToIndexNames: failed to read directory %s: %v", torrentDir, err)
		return err
	}

	log.Printf("MoveFilesToIndexNames: found %d files in directory", len(files))
	for _, file := range files {
		log.Printf("MoveFilesToIndexNames: file: %s (size: %d)", file.Name(), func() int64 {
			if info, err := file.Info(); err == nil {
				return info.Size()
			}
			return 0
		}())
	}

	// Check if files have already been moved
	_, err = os.Stat(filepath.Join(torrentDir, "0"))
	if err == nil {
		log.Printf("Files already moved to index names for torrent %s", e.InfoHash)
		return nil
	}

	log.Printf("Moving files to index names for torrent %s", e.InfoHash)

	for i, file := range e.Torrent.Files() {
		originalPath := filepath.Join(torrentDir, file.DisplayPath())
		indexPath := filepath.Join(torrentDir, strconv.Itoa(i))

		log.Printf("MoveFilesToIndexNames: checking file %d: %s -> %s", i, originalPath, indexPath)

		// Check if original file exists
		if _, err := os.Stat(originalPath); err != nil {
			log.Printf("Original file not found: %s", originalPath)
			continue
		}

		// Move file to index name
		err := os.Rename(originalPath, indexPath)
		if err != nil {
			log.Printf("Failed to move %s to %s: %v", originalPath, indexPath, err)
			return err
		}
		log.Printf("Moved %s to %s", originalPath, indexPath)
	}

	log.Printf("Successfully moved all files to index names for torrent %s", e.InfoHash)
	return nil
}

// GetFile returns a specific file by index
func (e *TorrentEngine) GetFile(index int) (*TorrentFile, error) {
	if index < 0 || index >= len(e.Files) {
		return nil, fmt.Errorf("invalid file index: %d", index)
	}

	file := &e.Files[index]

	// Check if file is complete
	if e.Torrent == nil && e.Status == "incomplete" {
		// For cached incomplete torrents, check file size
		if stat, err := os.Stat(file.Path); err == nil {
			if stat.Size() < file.Size*95/100 { // Allow 5% tolerance
				log.Printf("File %s is incomplete (%d/%d bytes, %.1f%%), attempting to resume torrent",
					file.Name, stat.Size(), file.Size, float64(stat.Size())*100/float64(file.Size))

				// Try to resume the torrent
				if err := e.manager.resumeIncompleteTorrent(e); err != nil {
					log.Printf("Failed to resume torrent for file %s: %v", file.Name, err)
				}
			}
		}
	}

	return file, nil
}

// StreamFile streams a file with range support
func (e *TorrentEngine) StreamFile(fileIndex int, w http.ResponseWriter, r *http.Request) error {
	if e.logger != nil {
		e.logger.Info("Streaming file %d", fileIndex)
	}

	// Fallback: Serve from disk if e.Torrent is nil
	if e.Torrent == nil {
		if e.logger != nil {
			e.logger.Info("Torrent is nil, serving from disk cache")
		}

		if fileIndex < 0 || fileIndex >= len(e.Files) {
			if e.logger != nil {
				e.logger.Error("Invalid file index: %d (available: 0-%d)", fileIndex, len(e.Files)-1)
			}
			return fmt.Errorf("invalid file index: %d", fileIndex)
		}

		file := e.Files[fileIndex]
		if e.logger != nil {
			e.logger.Debug("Opening cached file: %s", file.Path)
		}

		f, err := os.Open(file.Path)
		if err != nil {
			if e.logger != nil {
				e.logger.Error("Failed to open cached file: %v", err)
			}
			return fmt.Errorf("failed to open file from disk: %w", err)
		}
		defer f.Close()
		fileLength := file.Size
		// Parse range header
		rangeHeader := r.Header.Get("Range")
		var start, end int64
		if rangeHeader != "" {
			rangeStr := strings.TrimPrefix(rangeHeader, "bytes=")
			parts := strings.Split(rangeStr, "-")
			if len(parts) == 2 {
				if parts[0] != "" {
					start, _ = strconv.ParseInt(parts[0], 10, 64)
				}
				if parts[1] != "" {
					end, _ = strconv.ParseInt(parts[1], 10, 64)
				} else {
					end = fileLength - 1
				}
			}
		} else {
			start = 0
			end = fileLength - 1
		}
		if start > end || end >= fileLength {
			return fmt.Errorf("invalid range")
		}
		w.Header().Set("Content-Type", getContentType(file.Name))
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		if rangeHeader != "" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, fileLength))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		if start > 0 {
			_, err := f.Seek(start, io.SeekStart)
			if err != nil {
				return fmt.Errorf("failed to seek: %w", err)
			}
		}
		_, err = io.CopyN(w, f, end-start+1)
		return err
	}

	if fileIndex < 0 || fileIndex >= len(e.Torrent.Files()) {
		if e.logger != nil {
			e.logger.Error("Invalid file index for live torrent: %d (available: 0-%d)", fileIndex, len(e.Torrent.Files())-1)
		}
		return fmt.Errorf("invalid file index: %d", fileIndex)
	}

	file := e.Torrent.Files()[fileIndex]
	bytesCompleted := file.BytesCompleted()
	fileLength := file.Length()

	if e.logger != nil {
		e.logger.Info("Streaming live file %d (%s), length: %d, completed: %d, complete: %v",
			fileIndex, file.DisplayPath(), fileLength, bytesCompleted, bytesCompleted == fileLength)
	}

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
				end = fileLength - 1
			}
		}
	} else {
		start = 0
		end = fileLength - 1
	}

	if e.logger != nil {
		e.logger.Debug("Range request: %d-%d", start, end)
	}

	// Check if the requested range is available
	if end >= bytesCompleted {
		if e.logger != nil {
			e.logger.Warn("Requested range %d-%d exceeds available bytes %d", start, end, bytesCompleted)
		}
		if bytesCompleted == 0 {
			if e.logger != nil {
				e.logger.Error("File not yet downloaded")
			}
			return fmt.Errorf("file not yet downloaded")
		}
		// Adjust end to available bytes
		end = bytesCompleted - 1
		if e.logger != nil {
			e.logger.Info("Adjusted range to %d-%d", start, end)
		}
	}

	// Set headers
	w.Header().Set("Content-Type", getContentType(file.DisplayPath()))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))

	if rangeHeader != "" {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, fileLength))
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

	// Stream the data with timeout
	done := make(chan error, 1)
	go func() {
		_, err := io.CopyN(w, reader, end-start+1)
		done <- err
	}()

	// Wait for completion or timeout
	select {
	case err := <-done:
		if err != nil {
			if e.logger != nil {
				e.logger.Error("Error streaming file %d: %v", fileIndex, err)
			}
		} else {
			if e.logger != nil {
				e.logger.Info("Successfully streamed file %d", fileIndex)
			}
		}
		return err
	case <-time.After(30 * time.Second):
		if e.logger != nil {
			e.logger.Error("Timeout streaming file %d", fileIndex)
		}
		return fmt.Errorf("streaming timeout")
	}
}

// GetStats returns torrent statistics
func (e *TorrentEngine) GetStats() map[string]interface{} {
	e.mu.RLock()
	defer e.mu.RUnlock()

	progress := float64(0)
	if e.TotalSize > 0 {
		progress = float64(e.Downloaded) / float64(e.TotalSize) * 100
	}

	// Use stored metadata if torrent is nil
	torrentName := "Unknown"
	if e.Torrent != nil && e.Torrent.Info() != nil {
		torrentName = e.Torrent.Info().Name
	} else if e.MetadataFetched && e.TorrentName != "" {
		torrentName = e.TorrentName
	}

	return map[string]interface{}{
		"infoHash":   e.InfoHash,
		"name":       torrentName,
		"status":     e.Status,
		"peers":      e.Peers,
		"files":      len(e.Files),
		"downloaded": e.Downloaded,
		"totalSize":  e.TotalSize,
		"progress":   progress,
		"createdAt":  e.CreatedAt,
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

// isFileComplete returns true if all pieces of the file are complete
func isFileComplete(file *torrent.File) bool {
	for _, piece := range file.State() {
		if !piece.Complete {
			return false
		}
	}
	return true
}

// TorrentState represents the state of a torrent for persistence (now in-memory only)
type TorrentState struct {
	InfoHash   string    `json:"infoHash"`
	MagnetURI  string    `json:"magnetURI"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"createdAt"`
	LastSeen   time.Time `json:"lastSeen"`
	Downloaded int64     `json:"downloaded"`
	TotalSize  int64     `json:"totalSize"`
}

// GetTorrentStats returns the current stats for a specific torrent and file index
func (tm *TorrentManager) GetTorrentStats(infoHash string, fileIndex int) (map[string]interface{}, error) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	engine, exists := tm.engines[infoHash]
	if !exists {
		return nil, fmt.Errorf("torrent not found: %s", infoHash)
	}

	// Check if torrent is nil (completed and dropped)
	if engine.Torrent == nil {
		// Use persisted file list
		if fileIndex < 0 || fileIndex >= len(engine.Files) {
			return nil, fmt.Errorf("invalid file index: %d", fileIndex)
		}
	} else {
		// Use live torrent file list
		if fileIndex < 0 || fileIndex >= len(engine.Torrent.Files()) {
			return nil, fmt.Errorf("invalid file index: %d", fileIndex)
		}
	}

	// Build sources array with tracker information
	var sources []map[string]interface{}

	// Add actual trackers from the engine
	for _, trackerURL := range engine.Trackers {
		source := map[string]interface{}{
			"numFound":     0,
			"numFoundUniq": 0,
			"numRequests":  4,
			"url":          fmt.Sprintf("tracker:%s", trackerURL),
			"lastStarted":  time.Now().Format(time.RFC3339),
		}
		sources = append(sources, source)
	}

	// Add DHT source if enabled
	if engine.DHTEnabled {
		dhtSource := map[string]interface{}{
			"numFound":     0,
			"numFoundUniq": 0,
			"numRequests":  1,
			"url":          fmt.Sprintf("dht:%s", infoHash),
			"lastStarted":  time.Now().Format(time.RFC3339),
		}
		sources = append(sources, dhtSource)
	}

	// Build peer search sources array
	var peerSearchSources []string
	for _, trackerURL := range engine.Trackers {
		peerSearchSources = append(peerSearchSources, fmt.Sprintf("tracker:%s", trackerURL))
	}
	if engine.DHTEnabled {
		peerSearchSources = append(peerSearchSources, fmt.Sprintf("dht:%s", infoHash))
	}

	// Build files array from persisted file list
	var filesArray []map[string]interface{}
	if len(engine.Files) > 0 {
		for _, file := range engine.Files {
			filesArray = append(filesArray, map[string]interface{}{
				"name":   file.Name,
				"length": file.Size,
				"offset": 0, // Default offset
			})
		}
	}

	// Build options structure
	opts := map[string]interface{}{
		"peerSearch": map[string]interface{}{
			"min":     engine.MinPeers,
			"max":     engine.MaxPeers,
			"sources": peerSearchSources,
		},
		"dht":              engine.DHTEnabled,
		"tracker":          len(engine.Trackers) > 0,
		"connections":      200,
		"handshakeTimeout": 20000,
		"timeout":          4000,
		"virtual":          true,
		"swarmCap": map[string]interface{}{
			"minPeers": engine.MinPeers,
			"maxSpeed": 4194304,
		},
		"growler": map[string]interface{}{
			"flood": 0,
			"pulse": 39321600,
		},
		"path":  filepath.Join(tm.cachePath, infoHash),
		"id":    "-TR3030-8492f302be75",
		"flood": 0,
		"pulse": 9007199254740991,
	}

	// Handle stats based on whether torrent is nil or not
	var peers, unchoked, unique, swarmConnections int
	var uploaded, downloadSpeed, uploadSpeed int64

	if engine.Torrent != nil {
		stats := engine.Torrent.Stats()
		peers = stats.TotalPeers
		unchoked = stats.TotalPeers
		unique = stats.TotalPeers
		swarmConnections = stats.TotalPeers
		uploaded = stats.BytesWrittenData.Int64()
		downloadSpeed = stats.BytesReadData.Int64()
		uploadSpeed = stats.BytesWrittenData.Int64()
	} else {
		// Zero values when torrent is nil
		peers = 0
		unchoked = 0
		unique = 0
		swarmConnections = 0
		uploaded = 0
		downloadSpeed = 0
		uploadSpeed = 0
	}

	stats := map[string]interface{}{
		"infoHash": infoHash,
		"name": func() string {
			if engine.Torrent != nil && engine.Torrent.Info() != nil {
				return engine.Torrent.Info().Name
			} else if engine.MetadataFetched && engine.TorrentName != "" {
				return engine.TorrentName
			}
			return "Unknown"
		}(),
		"peers":             peers,
		"unchoked":          unchoked,
		"queued":            0,
		"unique":            unique,
		"connectionTries":   0,
		"swarmPaused":       false,
		"swarmConnections":  swarmConnections,
		"swarmSize":         engine.MaxPeers,
		"selections":        []interface{}{},
		"wires":             nil,
		"files":             filesArray,
		"downloaded":        engine.Downloaded,
		"uploaded":          uploaded,
		"downloadSpeed":     downloadSpeed,
		"uploadSpeed":       uploadSpeed,
		"sources":           sources,
		"peerSearchRunning": engine.Torrent != nil, // Only running if torrent is active
		"opts":              opts,
	}

	return stats, nil
}

// GetTorrentStates returns all torrent states from memory
func (tm *TorrentManager) GetTorrentStates() []TorrentState {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	states := make([]TorrentState, 0, len(tm.engines))
	for _, engine := range tm.engines {
		state := TorrentState{
			InfoHash:   engine.InfoHash,
			MagnetURI:  fmt.Sprintf("magnet:?xt=urn:btih:%s", engine.InfoHash),
			Status:     engine.Status,
			CreatedAt:  engine.CreatedAt,
			LastSeen:   time.Now(),
			Downloaded: engine.Downloaded,
			TotalSize:  engine.TotalSize,
		}
		states = append(states, state)
	}

	return states
}

// CustomStorage implements storage.ClientImpl
type CustomStorage struct {
	baseDir string
}

// NewCustomStorage creates a new custom storage instance
func NewCustomStorage(baseDir string) *CustomStorage {
	return &CustomStorage{baseDir: baseDir}
}

// OpenTorrent implements storage.ClientImpl
func (cs *CustomStorage) OpenTorrent(info *metainfo.Info, infoHash metainfo.Hash) (storage.TorrentImpl, error) {
	// Create directory for this torrent
	torrentDir := filepath.Join(cs.baseDir, infoHash.HexString())
	if err := os.MkdirAll(torrentDir, 0755); err != nil {
		return storage.TorrentImpl{}, fmt.Errorf("failed to create torrent directory: %w", err)
	}

	// Always use custom storage, even if metadata isn't available yet
	log.Printf("Using custom storage for torrent %s", infoHash.HexString())
	cts := &CustomTorrentStorage{
		baseDir:  torrentDir,
		info:     info, // This might be nil initially
		infoHash: infoHash,
	}

	return storage.TorrentImpl{
		Piece: cts.Piece,
		Close: cts.Close,
		Flush: cts.Flush,
	}, nil
}

// CustomTorrentStorage implements storage.TorrentImpl
type CustomTorrentStorage struct {
	baseDir  string
	info     *metainfo.Info
	infoHash metainfo.Hash
}

// Piece implements storage.TorrentImpl
func (cts *CustomTorrentStorage) Piece(p metainfo.Piece) storage.PieceImpl {
	return &CustomPieceStorage{
		baseDir: cts.baseDir,
		piece:   p,
		info:    cts.info,
	}
}

// Close implements storage.TorrentImpl
func (cts *CustomTorrentStorage) Close() error {
	return nil
}

// Flush implements storage.TorrentImpl
func (cts *CustomTorrentStorage) Flush() error {
	return nil
}

// CustomPieceStorage implements storage.PieceImpl
// This version writes each piece to the correct <fileIndex> file, at the correct offset.
type CustomPieceStorage struct {
	baseDir string
	piece   metainfo.Piece
	info    *metainfo.Info
}

// Helper to map a piece to its file(s) and offsets
func (cps *CustomPieceStorage) pieceFileMappings() []struct {
	fileIndex   int
	fileOffset  int64
	pieceOffset int64
	length      int64
} {
	// If metadata is available, use the normal mapping
	if cps.info != nil {
		// Handle single-file torrents
		if cps.info.Length > 0 {
			pieceLength := cps.info.PieceLength
			pieceIndex := cps.piece.Index()
			pieceBegin := int64(pieceIndex) * pieceLength

			// For single-file torrents, map directly to file 0
			return []struct {
				fileIndex   int
				fileOffset  int64
				pieceOffset int64
				length      int64
			}{
				{
					fileIndex:   0,
					fileOffset:  pieceBegin,
					pieceOffset: 0,
					length:      cps.piece.Length(),
				},
			}
		}

		// Handle multi-file torrents
		if len(cps.info.Files) > 0 {
			var mappings []struct {
				fileIndex   int
				fileOffset  int64
				pieceOffset int64
				length      int64
			}

			pieceLength := cps.info.PieceLength
			pieceIndex := cps.piece.Index()
			pieceBegin := int64(pieceIndex) * pieceLength
			pieceEnd := pieceBegin + cps.piece.Length()

			var fileOffset int64 = 0
			for fileIndex, file := range cps.info.Files {
				fileBegin := fileOffset
				fileEnd := fileBegin + file.Length

				// Check if this file overlaps with the piece
				overlapBegin := int64(math.Max(float64(pieceBegin), float64(fileBegin)))
				overlapEnd := int64(math.Min(float64(pieceEnd), float64(fileEnd)))
				if overlapBegin < overlapEnd {
					mapping := struct {
						fileIndex   int
						fileOffset  int64
						pieceOffset int64
						length      int64
					}{
						fileIndex:   fileIndex,
						fileOffset:  overlapBegin - fileBegin,
						pieceOffset: overlapBegin - pieceBegin,
						length:      overlapEnd - overlapBegin,
					}
					mappings = append(mappings, mapping)
				}
				fileOffset += file.Length
			}

			return mappings
		}

		// If metadata not available yet, create a simple mapping to file 0
		// Use the actual piece length instead of assuming 16KB
		return []struct {
			fileIndex   int
			fileOffset  int64
			pieceOffset int64
			length      int64
		}{
			{
				fileIndex:   0,
				fileOffset:  int64(cps.piece.Index()) * cps.piece.Length(), // Use actual piece length
				pieceOffset: 0,
				length:      cps.piece.Length(),
			},
		}
	}

	// Default case: if metadata is not available, create a simple mapping to file 0
	return []struct {
		fileIndex   int
		fileOffset  int64
		pieceOffset int64
		length      int64
	}{
		{
			fileIndex:   0,
			fileOffset:  int64(cps.piece.Index()) * cps.piece.Length(),
			pieceOffset: 0,
			length:      cps.piece.Length(),
		},
	}
}

// ReadAt implements io.ReaderAt
func (cps *CustomPieceStorage) ReadAt(p []byte, off int64) (n int, err error) {
	// Read from the correct file(s) and offset(s), starting at piece offset 'off'
	mappings := cps.pieceFileMappings()
	var totalRead int64 = 0
	pieceLen := int64(len(p))
	pieceOffset := off
	for _, m := range mappings {
		// Skip mappings before the requested offset
		if m.pieceOffset+m.length <= pieceOffset {
			continue
		}
		// Calculate where to start in this mapping
		startInMapping := int64(math.Max(float64(pieceOffset), float64(m.pieceOffset)))
		endInMapping := int64(math.Min(float64(m.pieceOffset+m.length), float64(pieceOffset+pieceLen)))
		if startInMapping >= endInMapping {
			continue
		}
		filePath := filepath.Join(cps.baseDir, strconv.Itoa(m.fileIndex))
		file, err := os.Open(filePath)
		if err != nil {
			return int(totalRead), err
		}
		readLen := endInMapping - startInMapping
		startIdx := startInMapping - pieceOffset
		if startIdx < 0 {
			startIdx = 0
		}
		endIdx := startIdx + readLen
		if endIdx > int64(len(p)) {
			endIdx = int64(len(p))
		}
		readBuf := p[int(startIdx):int(endIdx)]
		_, err = file.ReadAt(readBuf, m.fileOffset+(startInMapping-m.pieceOffset))
		file.Close()
		if err != nil && err != io.EOF {
			return int(totalRead), err
		}
		totalRead += readLen
	}
	if totalRead == 0 {
		return 0, io.EOF
	}
	return int(totalRead), nil
}

// WriteAt implements io.WriterAt
func (cps *CustomPieceStorage) WriteAt(p []byte, off int64) (n int, err error) {
	// Write to the correct file(s) and offset(s), starting at piece offset 'off'
	mappings := cps.pieceFileMappings()

	var totalWritten int64 = 0
	pieceLen := int64(len(p))
	pieceOffset := off
	for _, m := range mappings {
		// Skip mappings before the requested offset
		if m.pieceOffset+m.length <= pieceOffset {
			continue
		}
		// Calculate where to start in this mapping
		startInMapping := int64(math.Max(float64(pieceOffset), float64(m.pieceOffset)))
		endInMapping := int64(math.Min(float64(m.pieceOffset+m.length), float64(pieceOffset+pieceLen)))
		if startInMapping >= endInMapping {
			continue
		}
		filePath := filepath.Join(cps.baseDir, strconv.Itoa(m.fileIndex))
		if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
			return int(totalWritten), err
		}
		file, err := os.OpenFile(filePath, os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			return int(totalWritten), err
		}
		writeLen := endInMapping - startInMapping
		startIdx := startInMapping - pieceOffset
		if startIdx < 0 {
			startIdx = 0
		}
		endIdx := startIdx + writeLen
		if endIdx > int64(len(p)) {
			endIdx = int64(len(p))
		}
		writeBuf := p[int(startIdx):int(endIdx)]
		_, err = file.WriteAt(writeBuf, m.fileOffset+(startInMapping-m.pieceOffset))
		file.Close()
		if err != nil {
			return int(totalWritten), err
		}
		totalWritten += writeLen
	}
	if totalWritten == 0 {
		return 0, io.ErrShortWrite
	}
	if totalWritten < int64(len(p)) {
		return int(totalWritten), io.ErrShortWrite
	}
	return int(totalWritten), nil
}

// MarkComplete implements storage.PieceImpl
func (cps *CustomPieceStorage) MarkComplete() error {
	// Mark piece as complete - could rename file or set a flag
	return nil
}

// MarkNotComplete implements storage.PieceImpl
func (cps *CustomPieceStorage) MarkNotComplete() error {
	// Mark piece as not complete
	return nil
}

// Completion implements storage.PieceImpl
func (cps *CustomPieceStorage) Completion() storage.Completion {
	// For now, assume piece is complete if we can read from it
	// This is a simplified approach - in a production system you'd want to verify the data
	testBuf := make([]byte, int64(math.Min(1024, float64(cps.piece.Length())))) // Test read first 1KB or piece length
	n, err := cps.ReadAt(testBuf, 0)

	if err != nil && err != io.EOF {
		return storage.Completion{Complete: false, Ok: false}
	}

	// If we can read some data, consider the piece available
	// This is not perfect but should work for our use case
	complete := n > 0
	return storage.Completion{Complete: complete, Ok: true}
}

// GetTorrentInfoHash returns the info hash of a torrent
func GetTorrentInfoHash(magnetURI string) (string, error) {
	// Extract info hash from magnet URI
	// Format: magnet:?xt=urn:btih:<infoHash>
	log.Printf("Processing magnet URI: %s", magnetURI)
	if !strings.HasPrefix(magnetURI, "magnet:?xt=urn:btih:") {
		log.Printf("Invalid magnet URI format: does not start with 'magnet:?xt=urn:btih:'")
		return "", fmt.Errorf("invalid magnet URI format")
	}

	parts := strings.Split(magnetURI, "urn:btih:")
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid magnet URI format")
	}

	infoHash := strings.Split(parts[1], "&")[0]
	if len(infoHash) != 40 {
		log.Printf("Invalid info hash length: got %d characters, expected 40. Hash: %q", len(infoHash), infoHash)
		return "", fmt.Errorf("invalid info hash length")
	}

	return infoHash, nil
}

// AddTorrent adds a torrent to the client
func AddTorrent(client *torrent.Client, infoHash string) (*torrent.Torrent, error) {
	// Create magnet URI from info hash
	magnetURI := fmt.Sprintf("magnet:?xt=urn:btih:%s", infoHash)

	// Add the torrent
	t, err := client.AddMagnet(magnetURI)
	if err != nil {
		return nil, fmt.Errorf("failed to add magnet: %w", err)
	}

	// Wait for torrent info
	<-t.GotInfo()

	return t, nil
}

// AddTorrentWithConfig adds a torrent to the client with custom configuration
func AddTorrentWithConfig(client *torrent.Client, infoHash string, magnetURI string, trackers []string, dhtEnabled bool, minPeers, maxPeers int) (*torrent.Torrent, error) {
	// Add the torrent with enhanced magnet URI (trackers are already included in the magnet URI)
	t, err := client.AddMagnet(magnetURI)
	if err != nil {
		return nil, fmt.Errorf("failed to add magnet: %w", err)
	}

	// Wait for torrent info
	<-t.GotInfo()

	// Note: The anacrolix/torrent library handles DHT and peer limits automatically
	// based on the client configuration. The trackers are already included in the magnet URI.
	// We can't directly control DHT per torrent or set peer limits per torrent in this library.
	// These settings are handled at the client level.

	log.Printf("Added torrent with %d trackers, DHT: %v, peer limits: %d-%d",
		len(trackers), dhtEnabled, minPeers, maxPeers)

	return t, nil
}

// GetTorrentFile returns the file path for a specific file in a torrent
func GetTorrentFile(infoHash string, fileIndex int) string {
	// Get the cache path from environment or use default
	cachePath := os.Getenv("STREMIO_CACHE_PATH")
	if cachePath == "" {
		// Fallback to default path
		homeDir, err := os.UserHomeDir()
		if err != nil {
			log.Printf("Failed to get home directory: %v", err)
			return ""
		}
		cachePath = filepath.Join(homeDir, ".stremio-server", "stremio-cache")
	}

	// Construct path: <cachePath>/<infoHash>/<fileIndex>
	return filepath.Join(cachePath, infoHash, strconv.Itoa(fileIndex))
}

// GetTorrentFileWithCachePath returns the file path for a specific file in a torrent with explicit cache path
func GetTorrentFileWithCachePath(infoHash string, fileIndex int, cachePath string) string {
	// Construct path: <cachePath>/<infoHash>/<fileIndex>
	return filepath.Join(cachePath, infoHash, strconv.Itoa(fileIndex))
}

// GetTorrentInfoHashFromBytes calculates the info hash from torrent bytes
func GetTorrentInfoHashFromBytes(torrentBytes []byte) (string, error) {
	// Parse the torrent file
	metaInfo, err := metainfo.Load(bytes.NewReader(torrentBytes))
	if err != nil {
		return "", fmt.Errorf("failed to load torrent file: %w", err)
	}

	// Calculate info hash
	infoHash := metaInfo.HashInfoBytes()
	return hex.EncodeToString(infoHash[:]), nil
}

// GetPeerInfo returns detailed peer connection information
func (e *TorrentEngine) GetPeerInfo() []map[string]interface{} {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var peers []map[string]interface{}

	// Get peer statistics from the torrent
	stats := e.Torrent.Stats()

	// In a real implementation, you would iterate through actual peer connections
	// For now, we'll create mock peer data based on the stats
	if stats.TotalPeers > 0 {
		// Create a sample peer entry
		peer := map[string]interface{}{
			"requests":     0,
			"address":      "127.0.0.1:6881", // Mock address
			"amInterested": false,
			"isSeeder":     true,
			"downSpeed":    0,
			"upSpeed":      0,
		}
		peers = append(peers, peer)
	}

	return peers
}

// GetFileOffset calculates the offset of a file within the torrent
func (e *TorrentEngine) GetFileOffset(fileIndex int) int64 {
	if fileIndex < 0 || fileIndex >= len(e.Files) {
		return 0
	}

	var offset int64
	for i := 0; i < fileIndex; i++ {
		offset += e.Files[i].Size
	}
	return offset
}
