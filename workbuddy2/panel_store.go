// panel_store.go is the plugin's lightweight persistence layer for panel
// observability data: request log, credit ledger and task records.
//
// Design: one mutex-guarded in-memory slice per stream, flushed to disk with
// an atomic tmp+rename write on a debounced ticker (the chat hot path only
// appends to memory). Each stream is capped so files cannot grow unbounded on
// a long-running deployment. No database, no external dependency — the plugin
// runs inside the CPA process and must stay self-contained.
package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	panelStoreMaxRows    = 2000 // per stream, oldest trimmed first
	panelStoreFlushDelay = 5 * time.Second
)

// panelStream names one persisted collection.
type panelStream string

const (
	streamRequests panelStream = "requests"
	streamLedger   panelStream = "ledger"
	streamTasks    panelStream = "tasks"
)

type panelStore struct {
	mu      sync.Mutex
	dir     string
	rows    map[panelStream][]json.RawMessage
	loaded  map[panelStream]bool
	dirty   bool
	stopCh  chan struct{}
	started bool
}

var panelData = newPanelStore()

func newPanelStore() *panelStore {
	return &panelStore{
		rows:   map[panelStream][]json.RawMessage{},
		loaded: map[panelStream]bool{},
		stopCh: make(chan struct{}),
	}
}

// panelDataDir resolves the directory for panel observability data:
// $WB2_DATA_DIR > <user config>/CLIProxyAPI/workbuddy/data. The directory is
// created lazily on first write.
func panelDataDir() string {
	if d := os.Getenv("WB2_DATA_DIR"); d != "" {
		return d
	}
	cfgDir, err := os.UserConfigDir()
	if err != nil || cfgDir == "" {
		cfgDir = os.TempDir()
	}
	return filepath.Join(cfgDir, "CLIProxyAPI", "workbuddy", "data")
}

func (s *panelStore) path(stream panelStream) string {
	return filepath.Join(s.dir, string(stream)+".json")
}

// loadLocked reads one stream from disk (once per process, or after a reload).
func (s *panelStore) loadLocked(stream panelStream) {
	if s.loaded[stream] || s.dir == "" {
		return
	}
	raw, err := os.ReadFile(s.path(stream))
	if err != nil {
		if os.IsNotExist(err) {
			// Nothing on disk yet — safe to start from empty and never re-read.
			s.loaded[stream] = true
			return
		}
		// A transient read error must not mark the stream loaded: the next flush
		// would then write only the in-memory rows over a readable file, silently
		// destroying the history it failed to read. Leave it unloaded so the read
		// is retried on the next append.
		log.Printf("WARN: panel store: read %s: %v", stream, err)
		return
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		// Corrupt file: keep it for inspection rather than overwriting it with
		// the next flush, and skip loading so the failure stays visible.
		log.Printf("WARN: panel store: parse %s: %v (leaving file untouched)", stream, err)
		return
	}
	if len(rows) > panelStoreMaxRows {
		rows = rows[len(rows)-panelStoreMaxRows:]
	}
	s.rows[stream] = rows
	s.loaded[stream] = true
}

// append adds one row to a stream and marks it dirty. Never blocks on I/O.
func (s *panelStore) append(stream panelStream, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLoadedLocked()
	s.loadLocked(stream)
	rows := append(s.rows[stream], json.RawMessage(raw))
	if len(rows) > panelStoreMaxRows {
		rows = rows[len(rows)-panelStoreMaxRows:]
	}
	s.rows[stream] = rows
	s.dirty = true
	s.ensureFlusherLocked()
}

// ensureLoadedLocked resolves the data dir once.
func (s *panelStore) ensureLoadedLocked() {
	if s.dir != "" {
		return
	}
	s.dir = panelDataDir()
	_ = os.MkdirAll(s.dir, 0o700)
}

// ensureFlusherLocked starts the debounced background flusher once.
func (s *panelStore) ensureFlusherLocked() {
	if s.started {
		return
	}
	s.started = true
	go func() {
		ticker := time.NewTicker(panelStoreFlushDelay)
		defer ticker.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case <-ticker.C:
				s.Flush()
			}
		}
	}()
}

// Flush writes all dirty streams to disk. Safe to call at any time. A write
// failure keeps the stream dirty so the next tick retries it instead of
// silently dropping the rows.
func (s *panelStore) Flush() {
	s.mu.Lock()
	if !s.dirty || s.dir == "" {
		s.mu.Unlock()
		return
	}
	snapshot := make(map[panelStream][]json.RawMessage, len(s.rows))
	for k, v := range s.rows {
		cp := make([]json.RawMessage, len(v))
		copy(cp, v)
		snapshot[k] = cp
	}
	dir := s.dir
	s.mu.Unlock()

	failed := false
	for stream, rows := range snapshot {
		if err := writePanelJSON(filepath.Join(dir, string(stream)+".json"), rows); err != nil {
			failed = true
			log.Printf("WARN: panel store: flush %s: %v", stream, err)
		}
	}
	if failed {
		// Re-arm so the next tick retries; rows appended meanwhile are still in
		// s.rows and will be included.
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.dirty = false
	s.mu.Unlock()
}

// writePanelJSON atomically replaces a JSON file (tmp + rename).
func writePanelJSON(path string, rows []json.RawMessage) error {
	if rows == nil {
		rows = []json.RawMessage{}
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// list returns up to limit rows of a stream, newest first.
func (s *panelStore) list(stream panelStream, limit int) []json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLoadedLocked()
	s.loadLocked(stream)
	rows := s.rows[stream]
	out := make([]json.RawMessage, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		if limit > 0 && len(out) >= limit {
			break
		}
		out = append(out, rows[i])
	}
	return out
}

// stats reports per-stream row counts (for the panel header).
func (s *panelStore) stats() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLoadedLocked()
	out := map[string]int{}
	for _, stream := range []panelStream{streamRequests, streamLedger, streamTasks} {
		s.loadLocked(stream)
		out[string(stream)] = len(s.rows[stream])
	}
	return out
}
