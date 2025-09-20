// Package main implements a web server that acts as a torrent client,
// allowing streaming, file listing, metadata retrieval, and status checking
// of torrents via an HTTP API. It features in-memory caching, persistent
// metadata storage, and automatic cleanup of inactive torrents.
package main

import (
	"bytes"
	"context"
	"crypto/sha256" // Add this import
	"embed"       // Add this import
	"io/fs"       // Add this import
	"encoding/hex"  // Add this import
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"os/user" // Add this import
	"path/filepath"
	"runtime" // Add this import
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	lru "github.com/hashicorp/golang-lru"
	"github.com/lotusdblabs/lotusdb/v2"
)

//go:embed index.html style.css script.js favicon.ico jassub_dist
var staticFiles embed.FS // Add this global variable

// --- Structs for Caching ---
// cacheEntry holds the torrent and data for calculating download speed.
type cacheEntry struct {
	mu            sync.Mutex
	torrent       *torrent.Torrent
	prevBytesRead int64
	prevReadTime  time.Time
	lastAccessed  time.Time
}

// --- Structs for API JSON Responses ---
type FileInfo struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	SizeHuman  string `json:"size_human"`
	IsSubtitle bool   `json:"isSubtitle,omitempty"` // New field
}
type Metadata struct {
	Name           string     `json:"name"`
	InfoHash       string     `json:"infoHash"`
	TotalSize      int64      `json:"totalSize"`
	TotalSizeHuman string     `json:"totalSize_human"`
	FileCount      int        `json:"fileCount"`
	Files          []FileInfo `json:"files,omitempty"`
}
type FileStatus struct {
	Path                string  `json:"path"`
	Size                int64   `json:"size"`
	BytesCompleted      int64   `json:"bytesCompleted"`
	PercentageCompleted float64 `json:"percentageCompleted"`
}
type StatusInfo struct {
	InfoHash            string       `json:"infoHash"`
	Name                string       `json:"name"`
	TotalBytes          int64        `json:"totalBytes"`
	BytesCompleted      int64        `json:"bytesCompleted"`
	PercentageCompleted float64      `json:"percentageCompleted"`
	DownloadSpeedBps    float64      `json:"downloadSpeedBps"`
	DownloadSpeedHuman  string       `json:"downloadSpeedHuman"`
	ConnectedPeers      int          `json:"connectedPeers"`
	Files               []FileStatus `json:"files"`
	StreamingFileSize   int64        `json:"streamingFileSize,omitempty"`
	StreamingFileSizeHuman string    `json:"streamingFileSizeHuman,omitempty"`
}

// TorrentClient holds the main torrent client and cache.
type TorrentClient struct {
	client       *torrent.Client
	ctx          context.Context
	cache        *lru.Cache
	db           *lotusdb.DB
	restartChan  chan<- bool
	downloadDir  string            // Add downloadDir to TorrentClient
	vttFileMap   map[string]string // New: Map vttKey (filename) to full path for cleanup
	vttFileMapMu sync.Mutex        // New: Mutex to protect vttFileMap
	port         int
}

// NewTorrentClient initializes the application.
func NewTorrentClient(ctx context.Context, downloadDir string, restartChan chan<- bool, port int) (*TorrentClient, error) {
	http.DefaultClient.Transport = &http.Transport{
		Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
	}
	cfg := torrent.NewDefaultClientConfig()
	cfg.ListenPort = 0 // Use a random open port
	cfg.Seed = false
	cfg.DataDir = downloadDir
	// --- Performance Tuning ---
	cfg.EstablishedConnsPerTorrent = 100 // Increase connection limit

	client, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, err
	}

	// Resolve absolute path for downloadDir
	absDownloadDir, err := filepath.Abs(downloadDir)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path for download directory: %w", err)
	}

	// --- LotusDB Initialization ---
	dbPath := filepath.Join(absDownloadDir, "lotusdb_meta")
	if err := os.MkdirAll(dbPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create lotusdb directory: %w", err)
	}
	opts := lotusdb.DefaultOptions
	opts.DirPath = dbPath
	var db *lotusdb.DB
	for i := 0; i < 5; i++ {
		db, err = lotusdb.Open(opts)
		if err == nil {
			break
		}
		log.Printf("Failed to open lotusdb, retrying... (%d/5): %v", i+1, err)
		if strings.Contains(err.Error(), "the database directory is used by another process") {
			lockFilePath := filepath.Join(opts.DirPath, "FLOCK")
			log.Printf("Database is locked. Attempting to remove lock file: %s", lockFilePath)
			if removeErr := os.Remove(lockFilePath); removeErr != nil {
				log.Printf("Failed to remove lock file: %v", removeErr)
			}
		}
		time.Sleep(1 * time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to open lotusdb after 5 retries: %w", err)
	}
	// --- End LotusDB Initialization ---

	tc := &TorrentClient{client: client, ctx: ctx, db: db, restartChan: restartChan, downloadDir: absDownloadDir, vttFileMap: make(map[string]string), port: port}

	// --- LRU Cache Initialization ---
	lruCache, err := lru.NewWithEvict(2, func(key interface{}, value interface{}) {
		if entry, ok := value.(*cacheEntry); ok {
			log.Printf("Evicting torrent from LRU cache: %s", entry.torrent.Name())
			entry.torrent.Drop()
			tc.cleanupTorrentAssociatedFiles(entry.torrent.InfoHash().HexString()) // Clean up associated files
		}
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create LRU cache: %w", err)
	}
	tc.cache = lruCache
	// --- End LRU Cache Initialization ---

	return tc, nil
}



func sanitize(s string) string {
	// Replace a set of special characters with underscores.
	return strings.NewReplacer(
		"<", "_", ">", "_", ":", "_", "\"", "_", "/", "_", "\\", "_", "|", "_", "?", "_", "*", "_",
		"[", "_", "]", "_", "(", "_", ")", "_",
	).Replace(s)
}

// --- Middleware ---
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Get the origin from the request header
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		} else {
			// Fallback to * if no origin is provided (e.g., for same-origin requests or direct access)
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}

		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Filename, X-Filesize, X-Content-Type") // Added X- headers to allowed headers
		w.Header().Set("Access-Control-Expose-Headers", "X-Filename, X-Filesize, X-Content-Type")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin") // Add Referrer-Policy header

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- Helper Functions ---
func (tc *TorrentClient) getTorrentFromMagnet(magnetLink string) (*torrent.Torrent, error) {
	spec, err := metainfo.ParseMagnetURI(magnetLink)
	if err != nil {
		return nil, fmt.Errorf("invalid magnet link: %w", err)
	}
	spec.DisplayName = sanitize(spec.DisplayName)
	infoHash := spec.InfoHash.HexString()

	// 1. Check in-memory LRU cache
	if val, found := tc.cache.Get(infoHash); found {
		log.Printf("Using in-memory cached torrent for infohash: %s", infoHash)
		entry := val.(*cacheEntry)
		entry.mu.Lock()
		entry.lastAccessed = time.Now()
		entry.mu.Unlock()
		return entry.torrent, nil
	}

	// 2. Check LotusDB for persisted metadata
	if metaBytes, err := tc.db.Get([]byte(infoHash)); err == nil {
		log.Printf("Found metadata in LotusDB for infohash: %s", infoHash)
		mi, err := metainfo.Load(bytes.NewReader(metaBytes))
		if err != nil {
			log.Printf("Error loading metadata from LotusDB: %v. Falling back to magnet.", err)
		} else {
			t, err := tc.client.AddTorrent(mi)
			if err != nil {
				return nil, fmt.Errorf("failed to add torrent from cached metadata: %w", err)
			}
			<-t.GotInfo() // Should be immediate
			log.Printf("Torrent info loaded from DB for: %s", t.Name())
			entry := &cacheEntry{torrent: t, prevReadTime: time.Now(), lastAccessed: time.Now()}
			tc.cache.Add(infoHash, entry)
			return t, nil
		}
	}

	// 3. Fetch from magnet link as a last resort
	log.Printf("Adding magnet link to client: %s", magnetLink)
	t, err := tc.client.AddMagnet(spec.String())
	if err != nil {
		return nil, fmt.Errorf("failed to add magnet link: %w", err)
	}

	log.Println("Waiting for torrent info...")
	select {
	case <-t.GotInfo():
		log.Printf("Torrent info received for: %s", t.Name())

		// Persist metadata to LotusDB
		var buf bytes.Buffer
		mi := t.Metainfo()
		if err := mi.Write(&buf); err != nil {
			log.Printf("Error writing metainfo to buffer for infohash %s: %v", infoHash, err)
		} else {
			if err := tc.db.Put([]byte(infoHash), buf.Bytes()); err != nil {
				log.Printf("Error saving metainfo to LotusDB for infohash %s: %v", infoHash, err)
			} else {
				log.Printf("Successfully saved metadata to LotusDB for infohash: %s", infoHash)
			}
		}
		entry := &cacheEntry{torrent: t, prevReadTime: time.Now(), lastAccessed: time.Now()}
		tc.cache.Add(infoHash, entry)
		return t, nil
	case <-tc.ctx.Done():
		return nil, tc.ctx.Err()
	case <-time.After(30 * time.Second):
		log.Printf("Timeout waiting for torrent info for infohash: %s", infoHash)
		t.Drop()
		return nil, errors.New("timeout getting torrent info")
	}
}

func humanReadableSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func humanReadableSpeed(bytesPerSecond float64) string {
	return humanReadableSize(int64(bytesPerSecond)) + "/s"
}

func getFileToStream(t *torrent.Torrent, index int) *torrent.File {
	files := t.Files()
	if index >= 0 && index < len(files) {
		return files[index]
	}
	var largestFile *torrent.File
	var largestSize int64
	for _, file := range files {
		if file.Length() > largestSize {
			largestFile = file
			largestSize = file.Length()
		}
	}
	return largestFile
}

func getContentType(filename string) string {
	switch {
	case strings.HasSuffix(filename, ".mp4"):
		return "video/mp4"
	case strings.HasSuffix(filename, ".mkv"):
		return "video/x-matroska"
	default:
		return "application/octet-stream"
	}
}

// --- HTTP Handlers (DEFINED ONLY ONCE) ---

// ***************************************************************
// ***               START OF UPDATED FUNCTION                   ***
// ***************************************************************

func (tc *TorrentClient) streamHandler(w http.ResponseWriter, r *http.Request) {
	magnetLink := r.URL.Query().Get("url")
	if magnetLink == "" {
		http.Error(w, "Missing 'url' query parameter", http.StatusBadRequest)
		return
	}

	t, err := tc.getTorrentFromMagnet(magnetLink)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(t.Files()) == 0 {
		http.Error(w, "No files in torrent", http.StatusNotFound)
		return
	}

	indexStr := r.URL.Query().Get("index")
	index, err := strconv.Atoi(indexStr)
	if err != nil {
		index = -1 // Will select the largest file by default
	}

	file := getFileToStream(t, index)
	if file == nil {
		http.Error(w, "Could not find a file in the torrent to stream", http.StatusInternalServerError)
		return
	}

	filename := filepath.Base(file.DisplayPath())
	fileSize := file.Length()
	contentType := getContentType(filename)

	log.Printf("Streaming file: %s (size: %d bytes)", filename, fileSize)

	// --- START of Manual Range Request Handling (from old code) ---
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=\"%s\"; filename*=UTF-8''%s", filename, url.QueryEscape(filename)))
	w.Header().Set("X-Filename", filename)
	w.Header().Set("X-Filesize", strconv.FormatInt(fileSize, 10))
	w.Header().Set("X-Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")

	rangeHeader := r.Header.Get("Range")
	var start, end int64
	var contentLength int64

	if rangeHeader != "" {
		fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end)
		if end == 0 || end >= fileSize {
			end = fileSize - 1
		}
		contentLength = end - start + 1

		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, fileSize))
		w.WriteHeader(http.StatusPartialContent) // Send 206 Partial Content status
	} else {
		// No range request, so stream the whole file
		start = 0
		end = fileSize - 1
		contentLength = fileSize
		w.WriteHeader(http.StatusOK) // Send 200 OK status
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))

	reader := file.NewReader()
	defer reader.Close()

	_, err = reader.Seek(start, io.SeekStart)
	if err != nil {
		log.Printf("Error seeking in file: %v", err)
		http.Error(w, "Error seeking in file", http.StatusInternalServerError)
		return
	}

	// Manual streaming loop with a buffer and flushing
	buf := make([]byte, 1024*512) // 512KB buffer
	bytesWritten := int64(0)
	for bytesWritten < contentLength {
		bytesToRead := contentLength - bytesWritten
		if int64(len(buf)) < bytesToRead {
			bytesToRead = int64(len(buf))
		}

		n, err := reader.Read(buf[:bytesToRead])
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				log.Printf("Client disconnected during stream: %v", writeErr)
				return // Client probably closed the connection
			}
			w.(http.Flusher).Flush() // Force data to be sent
			bytesWritten += int64(n)
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("Error reading from torrent stream: %v", err)
			}
			break
		}
	}
	// --- END of Manual Range Request Handling ---
}

// ***************************************************************
// ***                 END OF UPDATED FUNCTION                   ***
// ***************************************************************

// srtToVtt converts SRT format subtitles to VTT format.
func srtToVtt(srt string) string {
	var vtt strings.Builder
	vtt.WriteString("WEBVTT\n\n")

	// Normalize newlines and split into blocks
	blocks := strings.Split(strings.ReplaceAll(srt, "\r\n", "\n"), "\n\n")

	for _, block := range blocks {
		trimmedBlock := strings.TrimSpace(block)
		if trimmedBlock == "" {
			continue
		}

		lines := strings.Split(trimmedBlock, "\n")

		// Find the timestamp line
		timeLineIndex := -1
		for i, line := range lines {
			if strings.Contains(line, "-->") {
				timeLineIndex = i
				break
			}
		}

		if timeLineIndex != -1 {
			// Write timestamp line (converted)
			vtt.WriteString(strings.ReplaceAll(lines[timeLineIndex], ",", ".") + "\n")
			// Write text lines
			for i := timeLineIndex + 1; i < len(lines); i++ {
				vtt.WriteString(lines[i] + "\n")
			}
			vtt.WriteString("\n")
		}
	}
	return vtt.String()
}

func (tc *TorrentClient) cleanupTorrentAssociatedFiles(infoHash string) {
	tc.vttFileMapMu.Lock()
	defer tc.vttFileMapMu.Unlock()

	keysToDelete := []string{}
	for key, filePath := range tc.vttFileMap {
		if strings.HasPrefix(key, infoHash+"_") { // Assuming vttKey starts with infoHash
			log.Printf("Deleting VTT file: %s", filePath)
			if err := os.Remove(filePath); err != nil {
				log.Printf("Error deleting VTT file %s: %v", filePath, err)
			}
			keysToDelete = append(keysToDelete, key)
		}
	}

	for _, key := range keysToDelete {
		delete(tc.vttFileMap, key)
	}

	// --- New ASS and Log file cleanup ---
	patterns := []string{
		filepath.Join(tc.downloadDir, fmt.Sprintf("%s_*.ass", infoHash)),
		filepath.Join(tc.downloadDir, fmt.Sprintf("%s_*.log", infoHash)),
	}

	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			log.Printf("Error globbing files for pattern %s: %v", pattern, err)
			continue
		}
		for _, file := range matches {
			log.Printf("Deleting associated file: %s", file)
			if err := os.Remove(file); err != nil {
				log.Printf("Error deleting associated file %s: %v", file, err)
			}
		}
	}
	// --- End New ASS and Log file cleanup ---
}

func (tc *TorrentClient) downloadSubtitleHandler(w http.ResponseWriter, r *http.Request) {
	magnetLink := r.URL.Query().Get("url")
	if magnetLink == "" {
		http.Error(w, "Missing 'url' query parameter", http.StatusBadRequest)
		return
	}

	filePath := r.URL.Query().Get("filePath")
	if filePath == "" {
		http.Error(w, "Missing 'filePath' query parameter", http.StatusBadRequest)
		return
	}

	spec, err := metainfo.ParseMagnetURI(magnetLink)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid magnet link: %v", err), http.StatusBadRequest)
		return
	}
	infoHash := spec.InfoHash.HexString()

	t, err := tc.getTorrentFromMagnet(magnetLink)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var targetFile *torrent.File
	for _, file := range t.Files() {
		if file.DisplayPath() == filePath {
			targetFile = file
			break
		}
	}

	if targetFile == nil {
		http.Error(w, "Subtitle file not found in torrent", http.StatusNotFound)
		return
	}

	reader := targetFile.NewReader()
	defer reader.Close()

	srtBytes, err := io.ReadAll(reader)
	if err != nil {
		http.Error(w, "Failed to read subtitle file", http.StatusInternalServerError)
		return
	}

	vttContent := srtToVtt(string(srtBytes))

	// Construct a deterministic VTT filename: infoHash_filePathHash.vtt
	// Use a hash of infoHash and filePath to ensure uniqueness and consistency
	uniqueKey := infoHash + filePath
	hash := sha256.Sum256([]byte(uniqueKey))
	vttFilename := fmt.Sprintf("%s_%s.vtt", infoHash, hex.EncodeToString(hash[:]))
	vttFilePath := filepath.Join(tc.downloadDir, vttFilename)

	// Check if this VTT file already exists and is valid
	if _, err := os.Stat(vttFilePath); err == nil {
		log.Printf("Deterministic VTT file already exists for %s, returning existing key.", filePath)
		// File exists, assume it's valid and return its key
		tc.vttFileMapMu.Lock()
		tc.vttFileMap[vttFilename] = vttFilePath
		tc.vttFileMapMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"vttKey": vttFilename})
		return
	}

	// Write VTT content to file
	if err := os.WriteFile(vttFilePath, []byte(vttContent), 0644); err != nil {
		log.Printf("Error writing VTT file %s: %v", vttFilePath, err)
		http.Error(w, "Failed to save VTT file", http.StatusInternalServerError)
		return
	}

	// Store VTT filename (key) to full path mapping
	tc.vttFileMapMu.Lock()
	tc.vttFileMap[vttFilename] = vttFilePath
	tc.vttFileMapMu.Unlock()

	// Respond with the VTT filename (which acts as the key for streamVttHandler)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"vttKey": vttFilename})
}

func (tc *TorrentClient) streamVttHandler(w http.ResponseWriter, r *http.Request) {
	vttFilename := r.URL.Query().Get("key")
	if vttFilename == "" {
		http.Error(w, "Missing 'key' query parameter (VTT filename)", http.StatusBadRequest)
		return
	}

	tc.vttFileMapMu.Lock()
	vttFilePath, found := tc.vttFileMap[vttFilename]
	tc.vttFileMapMu.Unlock()

	if !found {
		http.Error(w, "VTT file not found or no longer active", http.StatusNotFound)
		return
	}

	vttContent, err := os.ReadFile(vttFilePath)
	if err != nil {
		log.Printf("Error reading VTT file %s: %v", vttFilePath, err)
		http.Error(w, "Failed to read VTT file", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(vttContent)))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(vttContent); err != nil {
		log.Printf("Error writing VTT content: %v", err)
	}
}

func (tc *TorrentClient) extractSubtitlesHandler(w http.ResponseWriter, r *http.Request) {
	magnetLink := r.URL.Query().Get("url")
	if magnetLink == "" {
		http.Error(w, "Missing 'url' query parameter", http.StatusBadRequest)
		return
	}
	indexStr := r.URL.Query().Get("index")
	index, err := strconv.Atoi(indexStr)
	if err != nil {
		http.Error(w, "Missing or invalid 'index' query parameter", http.StatusBadRequest)
		return
	}

	spec, err := metainfo.ParseMagnetURI(magnetLink)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid magnet link: %v", err), http.StatusBadRequest)
		return
	}
	infoHash := spec.InfoHash.HexString()

	t, err := tc.getTorrentFromMagnet(magnetLink)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	file := getFileToStream(t, index)
	if file == nil {
		http.Error(w, "Could not find the specified file in the torrent", http.StatusInternalServerError)
		return
	}

	inputStreamURL := fmt.Sprintf("http://localhost:%d/stream?url=%s&index=%d", tc.port, url.QueryEscape(magnetLink), index)

	subtitleFileName := fmt.Sprintf("%s_%d.ass", infoHash, index)
	subtitleFilePath := filepath.Join(tc.downloadDir, subtitleFileName)
	logFileName := fmt.Sprintf("%s_%d.log", infoHash, index)
	logFilePath := filepath.Join(tc.downloadDir, logFileName)

	// Clean up old log file if it exists
	os.Remove(logFilePath)

	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		log.Printf("ffmpeg executable not found in PATH: %v", err)
		http.Error(w, "ffmpeg executable not found. Please ensure ffmpeg is installed and in your system's PATH.", http.StatusInternalServerError)
		return
	}

	cmd := exec.Command(ffmpegPath, "-y", "-i", inputStreamURL, "-map", "0:s:0", "-c", "copy", subtitleFilePath)

	go func() {
		log.Printf("Starting subtitle extraction for %s, index %d", t.Name(), index)
		log.Printf("Executing command: %s", cmd.String())

		logFile, err := os.Create(logFilePath)
		if err != nil {
			log.Printf("Error creating log file for extraction: %v", err)
			return
		}
		defer logFile.Close()

		cmd.Stderr = logFile
		cmd.Stdout = logFile

		        cmdErr := cmd.Run()
				if cmdErr != nil {
					log.Printf("Error during subtitle extraction: %v", cmdErr)
					logFile.WriteString(fmt.Sprintf("\n\nExtraction failed: %v", cmdErr))
				} else {
					// Check if the file was created and has content
					info, statErr := os.Stat(subtitleFilePath)
					if statErr != nil || info.Size() == 0 {
						log.Printf("Subtitle extraction seemed to succeed, but output file is missing or empty: %s", subtitleFilePath)
						logFile.WriteString("\n\nExtraction failed: Output file is missing or empty.")
					} else {
						log.Printf("Subtitle extraction finished successfully for %s, index %d. Output: %s", t.Name(), index, subtitleFilePath)
						logFile.WriteString("\n\nExtraction finished successfully.")
					}
				}	}()

	response := map[string]string{
		"logFile":      logFileName,
		"subtitleFile": subtitleFileName,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (tc *TorrentClient) serveSubtitleFileHandler(w http.ResponseWriter, r *http.Request) {
	fileName := r.URL.Query().Get("file")
	if fileName == "" {
		http.Error(w, "Missing 'file' query parameter", http.StatusBadRequest)
		return
	}

	filePath := filepath.Join(tc.downloadDir, fileName)

	if !strings.HasPrefix(filepath.Clean(filePath), tc.downloadDir) {
		http.Error(w, "Invalid file path", http.StatusBadRequest)
		return
	}

	http.ServeFile(w, r, filePath)
}

func (tc *TorrentClient) uploadTorrentHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Only POST method is allowed", http.StatusMethodNotAllowed)
		return
	}

	torrentBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to read torrent file: %v", err), http.StatusInternalServerError)
		return
	}

	mi, err := metainfo.Load(bytes.NewReader(torrentBytes))
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to parse torrent file: %v", err), http.StatusBadRequest)
		return
	}

	magnetLink := mi.Magnet(nil, nil).String()

	response := map[string]string{"magnetLink": magnetLink}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

type FetchTorrentURLRequest struct {
	URL string `json:"url"`
}

func (tc *TorrentClient) fetchTorrentURLHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Only POST method is allowed", http.StatusMethodNotAllowed)
		return
	}

	var req FetchTorrentURLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	log.Printf("Attempting to fetch URL: %s", req.URL)
	resp, err := http.Get(req.URL)
	if err != nil {
		log.Printf("Error fetching URL %s: %v", req.URL, err)
		http.Error(w, fmt.Sprintf("Failed to fetch URL: %v", err), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	log.Printf("Fetched URL %s, Status: %s, Content-Type: %s", req.URL, resp.Status, resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusOK {
		log.Printf("Non-OK status code for URL %s: %s", req.URL, resp.Status)
		http.Error(w, fmt.Sprintf("Failed to fetch .torrent file from URL: %s", resp.Status), resp.StatusCode)
		return
	}

	torrentBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Error reading .torrent content from URL %s: %v", req.URL, err)
		http.Error(w, fmt.Sprintf("Failed to read .torrent content: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("Successfully read %d bytes from URL: %s", len(torrentBytes), req.URL)
	mi, err := metainfo.Load(bytes.NewReader(torrentBytes))
	if err != nil {
		log.Printf("Error parsing .torrent file from URL %s: %v", req.URL, err)
		http.Error(w, fmt.Sprintf("Failed to parse .torrent file from URL: %v", err), http.StatusBadRequest)
		return
	}

	magnetLink := mi.Magnet(nil, nil).String()
	log.Printf("Successfully generated magnet link for URL %s: %s", req.URL, magnetLink);

	response := map[string]string{"magnetLink": magnetLink}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (tc *TorrentClient) filesHandler(w http.ResponseWriter, r *http.Request) {
	magnetLink := r.URL.Query().Get("url")
	if magnetLink == "" {
		http.Error(w, "Missing 'url' query parameter", http.StatusBadRequest)
		return
	}
	t, err := tc.getTorrentFromMagnet(magnetLink)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var fileList []FileInfo
	for _, file := range t.Files() {
		isSubtitle := strings.HasSuffix(strings.ToLower(file.DisplayPath()), ".srt")
		fileList = append(fileList, FileInfo{Path: file.DisplayPath(), Size: file.Length(), SizeHuman: humanReadableSize(file.Length()), IsSubtitle: isSubtitle})
	}
	response := struct {
		InfoHash string
		Files    []FileInfo
	}{InfoHash: t.InfoHash().HexString(), Files: fileList}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (tc *TorrentClient) metadataHandler(w http.ResponseWriter, r *http.Request) {
	magnetLink := r.URL.Query().Get("url")
	if magnetLink == "" {
		http.Error(w, "Missing 'url' query parameter", http.StatusBadRequest)
		return
	}
	t, err := tc.getTorrentFromMagnet(magnetLink)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var totalSize int64
	for _, file := range t.Files() {
		totalSize += file.Length()
	}
	metadata := Metadata{Name: t.Name(), InfoHash: t.InfoHash().HexString(), TotalSize: totalSize, TotalSizeHuman: humanReadableSize(totalSize), FileCount: len(t.Files())}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metadata)
}

func (tc *TorrentClient) statusHandler(w http.ResponseWriter, r *http.Request) {
	magnetLink := r.URL.Query().Get("url")
	if magnetLink == "" {
		http.Error(w, "Missing 'url' query parameter", http.StatusBadRequest)
		return
	}
	spec, err := metainfo.ParseMagnetURI(magnetLink)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid magnet link: %v", err), http.StatusBadRequest)
		return
	}
	infoHashStr := spec.InfoHash.HexString()
	val, found := tc.cache.Get(infoHashStr)
	if !found {
		http.Error(w, "Torrent not found or not active", http.StatusNotFound)
		return
	}

	cachedEntry := val.(*cacheEntry)
	t := cachedEntry.torrent
	<-t.GotInfo()

	var streamingFileSize int64
	var streamingFileSizeHuman string

	indexStr := r.URL.Query().Get("index")
	if indexStr != "" {
		index, parseErr := strconv.Atoi(indexStr)
		if parseErr == nil {
			streamingFile := getFileToStream(t, index)
			if streamingFile != nil {
				streamingFileSize = streamingFile.Length()
				streamingFileSizeHuman = humanReadableSize(streamingFileSize)
			}
		}
	}

	var fileStatuses []FileStatus
	for _, file := range t.Files() {
		fileSize := file.Length()
		bytesCompleted := file.BytesCompleted()
		percentage := 0.0
		if fileSize > 0 {
			percentage = float64(bytesCompleted) / float64(fileSize) * 100
		}
		fileStatuses = append(fileStatuses, FileStatus{Path: file.DisplayPath(), Size: fileSize, BytesCompleted: bytesCompleted, PercentageCompleted: percentage})
	}
	totalBytes := t.Info().TotalLength()
	bytesCompleted := t.BytesCompleted()

	var downloadSpeed float64
	now := time.Now()

	cachedEntry.mu.Lock()
	timeDelta := now.Sub(cachedEntry.prevReadTime).Seconds()
	if timeDelta > 0.5 { // Only update speed every half second to avoid noisy data
		byteDelta := bytesCompleted - cachedEntry.prevBytesRead
		downloadSpeed = float64(byteDelta) / timeDelta

		cachedEntry.prevBytesRead = bytesCompleted
		cachedEntry.prevReadTime = now
	}
	cachedEntry.mu.Unlock()

	percentageCompleted := 0.0
	if totalBytes > 0 {
		percentageCompleted = float64(bytesCompleted) / float64(totalBytes) * 100
	}

	response := StatusInfo{
		InfoHash:            t.InfoHash().HexString(), Name: t.Name(), TotalBytes: totalBytes, BytesCompleted: bytesCompleted,
		PercentageCompleted: percentageCompleted, DownloadSpeedBps:    downloadSpeed,
		DownloadSpeedHuman:  humanReadableSpeed(downloadSpeed),
		ConnectedPeers:      t.Stats().ActivePeers, Files:               fileStatuses,
		StreamingFileSize:   streamingFileSize,
		StreamingFileSizeHuman: streamingFileSizeHuman,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (tc *TorrentClient) Close() {
	tc.client.Close()
	if err := tc.db.Close(); err != nil {
		log.Printf("Error closing LotusDB: %v", err)
	}
}

func (tc *TorrentClient) restartHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("Restart triggered via API.")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "The server has been restarted.")
	// Non-blocking send in case no one is listening.
	select {
	case tc.restartChan <- true:
	default:
	}
}

// --- Automatic Cleanup of Inactive Torrents ---

func (tc *TorrentClient) cleanupInactiveTorrents(maxInactiveTime time.Duration) {
	log.Println("Running cleanup for inactive torrents...")
	keysToDrop := []string{}

	for _, key := range tc.cache.Keys() {
		if val, ok := tc.cache.Get(key); ok {
			entry := val.(*cacheEntry)
			entry.mu.Lock()
			inactiveDuration := time.Since(entry.lastAccessed)
			entry.mu.Unlock()

			if inactiveDuration > maxInactiveTime {
				infoHashStr, isString := key.(string)
				if !isString {
					continue
				}
				log.Printf("Torrent '%s' (hash: %s) inactive for %v, queueing for removal.", entry.torrent.Name(), infoHashStr, inactiveDuration)
				keysToDrop = append(keysToDrop, infoHashStr)
			}
		}
	}

	if len(keysToDrop) > 0 {
		log.Printf("Removing %d inactive torrent(s).", len(keysToDrop))
		for _, infoHash := range keysToDrop {
			if val, ok := tc.cache.Get(infoHash); ok {
				entry := val.(*cacheEntry)
				log.Printf("Dropping torrent '%s' (hash: %s).", entry.torrent.Name(), infoHash)
				entry.torrent.Drop()
				tc.cache.Remove(infoHash)
				if err := tc.db.Delete([]byte(infoHash)); err != nil {
					log.Printf("Failed to delete torrent metadata from LotusDB for hash %s: %v", infoHash, err)
				}
			}
		}
	} else {
		log.Println("No inactive torrents to clean up.")
	}
}

func (tc *TorrentClient) periodicCleanup(interval time.Duration, maxInactiveTime time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			tc.cleanupInactiveTorrents(maxInactiveTime)
		case <-tc.ctx.Done():
			log.Println("Stopping periodic cleanup.")
			return
		}
	}
}

// --- Main Function ---
func main() {
	defaultDownloadDir := "." // Default for non-Windows
	if runtime.GOOS == "windows" {
		usr, err := user.Current()
		if err != nil {
			log.Printf("Warning: Could not get current user home directory: %v. Falling back to current directory for downloads.", err)
			defaultDownloadDir = "."
		} else {
			defaultDownloadDir = filepath.Join(usr.HomeDir, "Downloads")
		}
	}

	port := flag.Int("port", 3000, "Port to listen on")
	downloadDir := flag.String("download-dir", defaultDownloadDir, "Directory to save downloaded files")
	cleanupInactiveAfter := flag.Duration("cleanup-inactive-after", 30*time.Minute, "Duration after which to clean up inactive torrents (e.g., '30m', '2h'). Set to '0' to disable.")
	flag.Parse()

	var err error // Declare err here

	// --- PID File Management ---
	pidFile := filepath.Join(os.TempDir(), "rss.pid")
	if pidStr, readErr := os.ReadFile(pidFile); readErr == nil { // Use readErr for local scope
		if pid, parseErr := strconv.Atoi(string(pidStr)); parseErr == nil { // Use parseErr for local scope
			if process, findErr := os.FindProcess(pid); findErr == nil { // Use findErr for local scope
				if signalErr := process.Signal(syscall.Signal(0)); signalErr == nil { // Use signalErr for local scope
					log.Printf("Found existing process with PID %d. Terminating it.", pid)
					if killErr := process.Kill(); killErr != nil { // Use killErr for local scope
						log.Printf("Failed to kill existing process: %v", killErr)
					}
					time.Sleep(1 * time.Second)
				}
			}
		}
	}
	if err = os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0644); err != nil { // Assign to declared err
		log.Fatalf("Failed to write PID file: %v", err)
	}
	defer os.Remove(pidFile)

	// Check for ffmpeg at startup
	log.Println("Checking for ffmpeg executable...")
	_, err = exec.LookPath("ffmpeg")
	if err != nil {
		log.Fatalf("ffmpeg executable not found in system PATH. Subtitle extraction will not work.\nPlease install ffmpeg from: https://github.com/BtbN/FFmpeg-Builds/releases/tag/latest")
	}
	log.Println("ffmpeg executable found.")
	// --- End PID File Management ---

	// Ensure the selected download directory exists.
	log.Printf("Using download directory: %s", *downloadDir)
	if err := os.MkdirAll(*downloadDir, 0755); err != nil {
		log.Fatalf("Failed to create download directory: %v", err)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	for {
		log.Println("Starting server...")
		ctx, cancel := context.WithCancel(context.Background())
		restartChan := make(chan bool, 1)

		client, err := NewTorrentClient(ctx, *downloadDir, restartChan, *port)
		if err != nil {
			log.Fatalf("Failed to create torrent client: %v", err)
		}

		if *cleanupInactiveAfter > 0 {
			log.Printf("Automatic cleanup of torrents inactive for over %v is enabled.", *cleanupInactiveAfter)
			// Check for inactive torrents every 5 minutes.
			go client.periodicCleanup(5*time.Minute, *cleanupInactiveAfter)
		}

		mux := http.NewServeMux()
		mux.Handle("/stream", corsMiddleware(http.HandlerFunc(client.streamHandler)))
		mux.Handle("/files", corsMiddleware(http.HandlerFunc(client.filesHandler)))
		mux.Handle("/metadata", corsMiddleware(http.HandlerFunc(client.metadataHandler)))
		mux.Handle("/status", corsMiddleware(http.HandlerFunc(client.statusHandler)))
		mux.Handle("/restart", corsMiddleware(http.HandlerFunc(client.restartHandler)))
		mux.Handle("/download-subtitle", corsMiddleware(http.HandlerFunc(client.downloadSubtitleHandler)))
		mux.Handle("/fetch-torrent-url", corsMiddleware(http.HandlerFunc(client.fetchTorrentURLHandler)))
		mux.Handle("/upload-torrent", corsMiddleware(http.HandlerFunc(client.uploadTorrentHandler)))
		mux.Handle("/stream-vtt", corsMiddleware(http.HandlerFunc(client.streamVttHandler)))
		mux.Handle("/extract-subtitles", corsMiddleware(http.HandlerFunc(client.extractSubtitlesHandler)))
		mux.Handle("/subtitles", corsMiddleware(http.HandlerFunc(client.serveSubtitleFileHandler)))

		// Create a sub-filesystem for jassub_dist
		jassubFS, err := fs.Sub(staticFiles, "jassub_dist")
		if err != nil {
			log.Fatalf("Failed to create sub-filesystem for jassub_dist: %v", err)
		}
		mux.Handle("/jassub_dist/", http.StripPrefix("/jassub_dist/", http.FileServer(http.FS(jassubFS))))
		// Serve static files
		mux.Handle("/", http.FileServer(http.FS(staticFiles)))

		server := &http.Server{Addr: ":" + strconv.Itoa(*port), Handler: mux}

		go func() {
			log.Printf("Server listening on port %d", *port)
			log.Println("Available endpoints: /stream, /files, /metadata, /status, /restart")
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("HTTP server error: %v", err)
			}
		}()

		select {
		case <-sigChan:
			log.Println("Hard termination triggered by signal. Killing process.")
			os.Remove(pidFile)
			os.Exit(0)
		case <-restartChan:
			log.Println("Restarting server...")
			client.Close()
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer shutdownCancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				log.Printf("Server shutdown error: %v", err)
			} else {
				log.Println("Server shut down gracefully.")
			}
			cancel()
			log.Println("Waiting a moment before restarting...")
			time.Sleep(1 * time.Second)
			// Continue to the next iteration of the loop
		}
	}
}