// RoxBox Torrent Streaming Server
// Compiles to a single binary that runs on Android ARM64.
// Starts a sequential-download torrent session and serves the video
// file over HTTP on 127.0.0.1:8888 so media_kit can play it.
//
// Build for Android ARM64:
//   GOOS=android GOARCH=arm64 CGO_ENABLED=0 \
//     go build -ldflags="-s -w" -o torrent_server_arm64 .
//
// Build for Android ARMv7:
//   GOOS=android GOARCH=arm GOARM=7 CGO_ENABLED=0 \
//     go build -ldflags="-s -w" -o torrent_server_arm .

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/storage"
)

// ── Status struct sent back to Flutter ────────────────────────────────────────
type StatusResponse struct {
	State       string  `json:"state"`        // "idle" | "loading" | "ready" | "error"
	Progress    float64 `json:"progress"`     // 0–100
	DownloadMB  float64 `json:"download_mb"`
	SpeedKBs    float64 `json:"speed_kbs"`
	Peers       int     `json:"peers"`
	StreamURL   string  `json:"stream_url"`   // http://127.0.0.1:8888/stream
	Error       string  `json:"error,omitempty"`
}

// ── Global state ───────────────────────────────────────────────────────────────
var (
	mu          sync.RWMutex
	currentFile *torrent.File
	currentTorr *torrent.Torrent
	client      *torrent.Client
	cacheDir    string
	status      = StatusResponse{State: "idle"}
	port        = "8888"
)

func main() {
	// Allow overriding port and cache dir via env
	if p := os.Getenv("ROXBOX_PORT"); p != "" {
		port = p
	}
	cacheDir = os.Getenv("ROXBOX_CACHE")
	if cacheDir == "" {
		cacheDir = filepath.Join(os.TempDir(), "roxbox_torrent")
	}
	_ = os.MkdirAll(cacheDir, 0755)

	// Init torrent client
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = cacheDir
	cfg.DefaultStorage = storage.NewFileByInfoHash(cacheDir)
	cfg.Seed = false // We're a pure leecher for streaming
	cfg.EstablishedConnsPerTorrent = 80
	cfg.HalfOpenConnsPerTorrent = 50
	cfg.NoDHT = false
	cfg.NoDefaultPortForwarding = true
	// Sequential read optimisation: high connection count, fast unchoke
	cfg.DisableIPv6 = false

	var err error
	client, err = torrent.NewClient(cfg)
	if err != nil {
		log.Fatalf("torrent client: %v", err)
	}
	defer client.Close()

	// HTTP routes
	mux := http.NewServeMux()
	mux.HandleFunc("/add",    handleAdd)    // POST  ?magnet=...
	mux.HandleFunc("/status", handleStatus) // GET
	mux.HandleFunc("/stream", handleStream) // GET  (video bytes)
	mux.HandleFunc("/stop",   handleStop)   // POST
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, "OK")
	})

	addr := "127.0.0.1:" + port
	log.Printf("RoxBox server listening on %s", addr)

	srv := &http.Server{Addr: addr, Handler: mux}

	// Graceful shutdown
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Println("Shutting down…")
		srv.Close()
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// ── POST /add?magnet=<uri> ────────────────────────────────────────────────────
func handleAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	magnetURI := r.URL.Query().Get("magnet")
	if magnetURI == "" {
		_ = r.ParseForm()
		magnetURI = r.FormValue("magnet")
	}
	if magnetURI == "" {
		http.Error(w, "magnet param required", 400)
		return
	}

	// Stop any active torrent
	handleStopInternal()

	mu.Lock()
	status = StatusResponse{State: "loading", Progress: 0}
	mu.Unlock()

	go func() {
		t, err := client.AddMagnet(magnetURI)
		if err != nil {
			setError(fmt.Sprintf("AddMagnet: %v", err))
			return
		}

		mu.Lock()
		currentTorr = t
		mu.Unlock()

		log.Println("Waiting for torrent info…")
		<-t.GotInfo()
		log.Printf("Got info: %s", t.Name())

		// Pick the largest file (the video)
		f := largestFile(t)
		if f == nil {
			setError("no video file found in torrent")
			return
		}

		mu.Lock()
		currentFile = f
		mu.Unlock()

		// Prioritise the first 5 % and last 1 % of the file for fast seeking
		prioStart := f.Length() / 20 // 5%
		prioEnd   := f.Length() / 100 // 1%

		f.Download()

		// Set sequential priority on the entire file
		f.SetPriority(torrent.PiecePriorityNormal)
		setPieceSequential(t, f, prioStart, prioEnd)

		// Start stats loop
		go statsLoop(t, f)

		mu.Lock()
		status.State = "ready"
		status.StreamURL = "http://127.0.0.1:" + port + "/stream"
		mu.Unlock()

		log.Println("Stream ready at", "http://127.0.0.1:"+port+"/stream")
	}()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "loading"})
}

// ── GET /status ───────────────────────────────────────────────────────────────
func handleStatus(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	s := status
	mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s)
}

// ── GET /stream ───────────────────────────────────────────────────────────────
// Serves the torrent file as a seekable HTTP stream (supports Range requests).
func handleStream(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	f := currentFile
	mu.RUnlock()

	if f == nil {
		http.Error(w, "no active torrent", 503)
		return
	}

	reader := f.NewReader()
	reader.SetReadahead(8 * 1024 * 1024) // 8 MB readahead
	reader.SetResponsive()               // Sequential mode

	name := f.DisplayPath()
	// Guess MIME from extension
	ext := strings.ToLower(filepath.Ext(name))
	mime := "video/mp4"
	switch ext {
	case ".mkv":
		mime = "video/x-matroska"
	case ".avi":
		mime = "video/x-msvideo"
	case ".webm":
		mime = "video/webm"
	}

	w.Header().Set("Content-Type", mime)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "no-cache")

	// Use http.ServeContent for proper Range support + ETag
	http.ServeContent(w, r, name, time.Time{}, reader)
}

// ── POST /stop ────────────────────────────────────────────────────────────────
func handleStop(w http.ResponseWriter, r *http.Request) {
	handleStopInternal()
	w.WriteHeader(200)
	fmt.Fprint(w, "stopped")
}

func handleStopInternal() {
	mu.Lock()
	defer mu.Unlock()
	if currentTorr != nil {
		currentTorr.Drop()
		currentTorr = nil
		currentFile = nil
	}
	status = StatusResponse{State: "idle"}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func largestFile(t *torrent.Torrent) *torrent.File {
	files := t.Files()
	if len(files) == 0 {
		return nil
	}
	videoExts := map[string]bool{
		".mp4": true, ".mkv": true, ".avi": true,
		".mov": true, ".wmv": true, ".webm": true,
		".m4v": true, ".ts": true,
	}
	// Filter to video files only
	var videos []*torrent.File
	for _, f := range files {
		ext := strings.ToLower(filepath.Ext(f.DisplayPath()))
		if videoExts[ext] {
			videos = append(videos, f)
		}
	}
	if len(videos) == 0 {
		// Fall back to all files
		videos = files
	}
	sort.Slice(videos, func(i, j int) bool {
		return videos[i].Length() > videos[j].Length()
	})
	return videos[0]
}

// setPieceSequential boosts sequential priority on the file and
// ultra-boosts the first and last chunks so seek + playback starts fast.
func setPieceSequential(t *torrent.Torrent, f *torrent.File, prioStart, prioEnd int64) {
	info   := t.Info()
	pieceLen := int64(info.PieceLength)
	if pieceLen == 0 {
		return
	}
	fileOff  := f.Offset()
	fileEnd  := fileOff + f.Length()
	startEnd := fileOff + prioStart
	tailStart := fileEnd - prioEnd

	for i := 0; i < t.NumPieces(); i++ {
		pieceStart := int64(i) * pieceLen
		pieceEnd   := pieceStart + pieceLen
		if pieceEnd <= fileOff || pieceStart >= fileEnd {
			continue // outside our file
		}
		p := t.Piece(i)
		if pieceStart < startEnd || pieceStart >= tailStart {
			p.SetPriority(torrent.PiecePriorityNow) // start/end: highest
		} else {
			p.SetPriority(torrent.PiecePriorityNormal)
		}
	}
}

func setError(msg string) {
	mu.Lock()
	status = StatusResponse{State: "error", Error: msg}
	mu.Unlock()
	log.Println("ERROR:", msg)
}

// statsLoop updates the global status struct every second.
func statsLoop(t *torrent.Torrent, f *torrent.File) {
	var lastBytes int64
	for {
		time.Sleep(time.Second)
		mu.RLock()
		if currentTorr != t {
			mu.RUnlock()
			return
		}
		mu.RUnlock()

		stats     := t.Stats()
		downloaded := stats.BytesReadUsefulData.Int64()
		speed      := float64(downloaded-lastBytes) / 1024 // KB/s
		lastBytes   = downloaded
		pct         := float64(downloaded) / float64(f.Length()) * 100
		if pct > 100 {
			pct = 100
		}

		mu.Lock()
		status.Progress    = pct
		status.DownloadMB  = float64(downloaded) / (1024 * 1024)
		status.SpeedKBs    = speed
		status.Peers       = stats.ActivePeers
		if status.State != "error" {
			if pct >= 3 {
				status.State = "ready"
			} else {
				status.State = "loading"
			}
		}
		mu.Unlock()

		log.Printf("[%s] %.1f%% | %.1f MB | %.0f KB/s | %d peers",
			t.Name(), pct, float64(downloaded)/(1024*1024), speed, stats.ActivePeers)
	}
}

// parseInt64 is a small helper used nowhere currently but kept for future seek support
func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
}

// ensure parseInt64 is referenced
var _ = parseInt64
var _ = io.EOF
