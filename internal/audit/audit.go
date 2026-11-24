// Package audit appends one JSON line per app mutation to <state>/audit.jsonl
// (mode 0600). A line records who asked, what and where, and the payload's
// size and SHA-256, never the payload itself.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileName is the log's name inside the state dir.
const FileName = "audit.jsonl"

// Entry is one line. Result is "ok" or the ack's error code.
type Entry struct {
	Time   time.Time `json:"time"`
	Login  string    `json:"login"`
	Node   string    `json:"node"`
	Method string    `json:"method"`
	PaneID string    `json:"pane_id"`
	// WorkspaceID is a queue item's workspace target, "the next free agent
	// there"; ItemID names the queue item a line is about.
	WorkspaceID string `json:"workspace_id,omitempty"`
	ItemID      string `json:"item_id,omitempty"`
	// Kind and Name are a start job's agent kind and name; ItemID is then
	// the job's id.
	Kind   string `json:"kind,omitempty"`
	Name   string `json:"name,omitempty"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
	Result string `json:"result"`
	// Via is where a request came from when it isn't the app's WebSocket:
	// "notification" for POST /api/answer, "queue" for a lined-up message
	// the server sent (Login is then who lined it up, Node empty), "start"
	// for a start job's herdr calls (likewise).
	Via string `json:"via,omitempty"`
}

// Log is an open audit log; it is safe for concurrent use.
type Log struct {
	mu sync.Mutex
	f  *os.File
}

// Open opens <stateDir>/audit.jsonl for appending, creating the state dir
// (0700) and the file (0600) if needed. An existing file is set to 0600.
func Open(stateDir string) (*Log, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	// MkdirAll leaves an existing dir's mode alone; the state dir holds the
	// audit log and node keys, so tighten it.
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(stateDir, FileName), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, err
	}
	return &Log{f: f}, nil
}

// Record fills in e's time (when zero), and payload's size and hash, then
// appends the line. The payload itself is not written.
func (l *Log) Record(e Entry, payload []byte) error {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	sum := sha256.Sum256(payload)
	e.Size, e.SHA256 = len(payload), hex.EncodeToString(sum[:])
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err = l.f.Write(append(line, '\n'))
	return err
}

// Close closes the file.
func (l *Log) Close() error { return l.f.Close() }
