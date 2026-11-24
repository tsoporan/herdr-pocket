// Package tailnet runs herdr-pocket's own tailnet node and owns its state
// directory: the single-instance lock, status.json (the only channel the CLI
// uses to reach a running server) and the node's tsnet state.
//
// Everything here except Node works without a live node.
package tailnet

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// States in status.json.
const (
	StateNeedsLogin = "needs_login"
	StateStarting   = "starting"
	StateRunning    = "running"
	StateStopped    = "stopped"
)

const (
	lockFile   = "tsnet.lock"
	statusFile = "status.json"
	tsnetDir   = "tsnet"
)

// DefaultStateDir is $XDG_STATE_HOME/herdr-pocket, else
// ~/.local/state/herdr-pocket. It only names the dir; nothing is created.
func DefaultStateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "herdr-pocket")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".local", "state", "herdr-pocket")
	}
	return filepath.Join(home, ".local", "state", "herdr-pocket")
}

// ErrLocked means another instance holds the state dir's lock.
var ErrLocked = errors.New("herdr-pocket is already running")

// Lock takes the exclusive lock on <stateDir>/tsnet.lock, creating the state
// dir (0700) if needed. One process owns the node; a second gets ErrLocked.
// The lock is a flock on a fresh open file, so it is released when unlock
// runs or the process exits, and two Locks in one process also conflict.
func Lock(stateDir string) (unlock func() error, err error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(stateDir, lockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
	}
	return f.Close, nil // closing the file releases the flock
}

// StatusFile is <state>/status.json. State is one of the State constants.
type StatusFile struct {
	State          string     `json:"state"`
	AuthURL        string     `json:"auth_url,omitempty"`
	FQDN           string     `json:"fqdn,omitempty"`
	KeyExpiry      *time.Time `json:"key_expiry,omitempty"`
	HTTPSOff       bool       `json:"https_off,omitempty"` // signed in, but the tailnet issues no HTTPS certificates
	HerdrProtocol  int        `json:"herdr_protocol,omitempty"`
	TranscriptDirs []string   `json:"transcript_dirs,omitempty"`
	PID            int        `json:"pid"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// ReadStatus reads <stateDir>/status.json. A missing file is an error
// satisfying errors.Is(err, fs.ErrNotExist).
func ReadStatus(stateDir string) (StatusFile, error) {
	var s StatusFile
	data, err := os.ReadFile(filepath.Join(stateDir, statusFile))
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("%s: %w", statusFile, err)
	}
	return s, nil
}

// WriteStatus replaces <stateDir>/status.json atomically (a temp file in the
// same dir, then a rename), with mode 0600. A zero UpdatedAt is set to now.
func WriteStatus(stateDir string, s StatusFile) error {
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = time.Now().UTC()
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(stateDir, statusFile+".*") // CreateTemp uses 0600
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op after the rename
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(stateDir, statusFile))
}

// TsnetDir is where the node keeps its keys and tailnet state.
func TsnetDir(stateDir string) string { return filepath.Join(stateDir, tsnetDir) }
