// Package transcript reads Claude Code session transcripts into the plain
// language the agent view shows: the Now line, the Steps and the pending
// tool call. The record structure it relies on is described in record.go.
package transcript

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

var (
	// ErrNotFound means no config dir has a transcript for the session.
	ErrNotFound = errors.New("transcript: not found")
	// ErrUnusable means the file has no user or assistant records this
	// parser understands. The caller falls back to the terminal.
	ErrUnusable = errors.New("transcript: no usable records")
)

// sessionRe bounds session ids, which come from herdr and end up in a path.
var sessionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// Reader finds and incrementally reads session transcripts. It is safe for
// concurrent use.
type Reader struct {
	dirs  []string
	mu    sync.Mutex
	files map[string]*file // by session id
}

type file struct {
	path string
	size int64
	mod  time.Time
	off  int64 // bytes consumed: always just past a newline
	st   *state
	snap Snapshot
}

// NewReader reads transcripts under the given Claude config dirs (each has a
// projects/ subdirectory).
func NewReader(configDirs ...string) *Reader {
	return &Reader{dirs: configDirs, files: map[string]*file{}}
}

// DefaultConfigDirs is ~/.claude, then $CLAUDE_CONFIG_DIR when set, then
// extra. Dirs whose projects/ resolve to the same place (for example a
// symlinked second config dir) are kept once. Lookups are by unique session
// id, so searching more dirs never picks up the wrong transcript.
func DefaultConfigDirs(extra ...string) []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".claude"))
	}
	if env := os.Getenv("CLAUDE_CONFIG_DIR"); env != "" {
		dirs = append(dirs, env)
	}
	return dedupeDirs(append(dirs, extra...))
}

// dedupeDirs drops dirs whose projects/ subdirectory resolves to one already
// listed. A dir whose projects/ doesn't resolve is compared by its clean path.
func dedupeDirs(dirs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range dirs {
		key := filepath.Clean(filepath.Join(d, "projects"))
		if real, err := filepath.EvalSymlinks(key); err == nil {
			key = real
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, d)
	}
	return out
}

// Read returns the session's current snapshot. It reads only what was
// appended since the last call, and nothing when the file's size and mtime
// are unchanged.
func (r *Reader) Read(sessionID string) (Snapshot, error) {
	if !sessionRe.MatchString(sessionID) {
		return Snapshot{}, fmt.Errorf("transcript: bad session id %q", sessionID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	f := r.files[sessionID]
	var fi os.FileInfo
	var err error
	if f != nil {
		fi, err = os.Stat(f.path)
	}
	if f == nil || err != nil {
		p, info, ferr := r.find(sessionID)
		if ferr != nil {
			delete(r.files, sessionID)
			return Snapshot{}, ferr
		}
		f = &file{path: p, st: newState()}
		r.files[sessionID] = f
		fi = info
	}
	if f.st.records > 0 && fi.Size() == f.size && fi.ModTime().Equal(f.mod) {
		return f.snap.clone(), nil
	}
	if fi.Size() < f.off { // truncated or replaced: start over
		f.off, f.st = 0, newState()
	}
	if err := f.tail(); err != nil {
		return Snapshot{}, err
	}
	f.size, f.mod = fi.Size(), fi.ModTime()
	if f.st.records == 0 {
		return Snapshot{}, ErrUnusable
	}
	f.snap = f.st.snapshot()
	return f.snap.clone(), nil
}

// find looks for <dir>/projects/*/<session>.jsonl, newest first if more
// than one dir has it.
func (r *Reader) find(sessionID string) (string, os.FileInfo, error) {
	var best string
	var bestInfo os.FileInfo
	for _, dir := range r.dirs {
		projects := filepath.Join(dir, "projects")
		entries, err := os.ReadDir(projects)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			p := filepath.Join(projects, e.Name(), sessionID+".jsonl")
			info, err := os.Stat(p)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			if bestInfo == nil || info.ModTime().After(bestInfo.ModTime()) {
				best, bestInfo = p, info
			}
		}
	}
	if bestInfo == nil {
		return "", nil, ErrNotFound
	}
	return best, bestInfo, nil
}

// maxLine caps one transcript record. A longer one (a huge tool result, say)
// is skipped, so memory stays bounded; if it was a tool result, its call
// keeps looking pending.
const maxLine = 1 << 20

// tail applies every complete line after f.off. A last line with no newline
// is still being written, so it waits for the next read.
func (f *file) tail() error {
	fh, err := os.Open(f.path)
	if err != nil {
		return fmt.Errorf("transcript: %w", err)
	}
	defer fh.Close()
	if _, err := fh.Seek(f.off, io.SeekStart); err != nil {
		return fmt.Errorf("transcript: %w", err)
	}
	br := bufio.NewReaderSize(fh, 64<<10)
	for {
		line, n, err := readLine(br)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("transcript: %w", err)
		}
		f.off += n
		if line == nil {
			continue // longer than maxLine
		}
		if rec, ok := decodeLine(line); ok {
			f.st.apply(rec)
		}
	}
}

// readLine reads up to and including the next newline. It returns the line,
// or nil if it is longer than maxLine, and its length n either way. At
// io.EOF the line is incomplete and n must not be consumed.
func readLine(br *bufio.Reader) (line []byte, n int64, err error) {
	for {
		chunk, err := br.ReadSlice('\n')
		n += int64(len(chunk))
		if n <= maxLine {
			line = append(line, chunk...)
		} else {
			line = nil
		}
		if err != bufio.ErrBufferFull {
			return line, n, err
		}
	}
}
