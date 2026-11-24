package audit_test

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tsoporan/herdr-pocket/internal/audit"
)

func TestRecord(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	l, err := audit.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("deploy the staging branch please")
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if err := l.Record(audit.Entry{Login: "owner@example.com", Node: "phone", Method: "prompt", PaneID: "w1:p1", Result: "ok"}, payload); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, audit.FileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode %o, want 600", mode)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "staging") {
		t.Error("the log holds payload text")
	}
	sum := sha256.Sum256(payload)
	n := 0
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		n++
		var e audit.Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("line %d: %v", n, err)
		}
		if e.Size != len(payload) || e.SHA256 != hex.EncodeToString(sum[:]) || e.Time.IsZero() || e.Method != "prompt" || e.Result != "ok" {
			t.Errorf("line %d: %+v", n, e)
		}
	}
	if n != 20 {
		t.Errorf("%d lines, want 20", n)
	}

	// Reopening appends and tightens a loosened mode.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	l, err = audit.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.Record(audit.Entry{Method: "keys", Result: "forbidden"}, nil); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(path)
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("reopened mode %o, want 600", mode)
	}
	data, _ = os.ReadFile(path)
	if lines := strings.Count(string(data), "\n"); lines != 21 {
		t.Errorf("%d lines after reopen, want 21", lines)
	}
}
