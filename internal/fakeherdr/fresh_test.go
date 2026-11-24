package fakeherdr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFreshTranscripts(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(filepath.Join(src, "projects", "p"), 0o700); err != nil {
		t.Fatal(err)
	}
	in := `{"timestamp":"2026-11-21T08:00:00.000Z","x":1}` + "\n" + `{"timestamp":"2026-11-21T09:00:00.000Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(src, "projects", "p", "s.jsonl"), []byte(in), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	if err := FreshTranscripts(src, dst, now); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "projects", "p", "s.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"timestamp":"2026-12-01T10:59:00.000Z","x":1}` + "\n" + `{"timestamp":"2026-12-01T11:59:00.000Z"}` + "\n"
	if string(got) != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if !strings.Contains(string(got), `"x":1`) {
		t.Error("other fields changed")
	}
}
