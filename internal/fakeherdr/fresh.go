package fakeherdr

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// timestampField is a transcript record's "timestamp" value.
var timestampField = regexp.MustCompile(`"timestamp":"([0-9T:.\-]+Z)"`)

// FreshTranscripts copies the fixture transcripts under src to dst, moving
// every record's timestamp by the same amount so the newest is a minute
// before now. The fixtures keep fixed dates, so without this every age in
// `just dev` and its screenshots reads days. dst is replaced.
func FreshTranscripts(src, dst string, now time.Time) error {
	var newest time.Time
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range timestampField.FindAllSubmatch(data, -1) {
			if t, err := time.Parse(time.RFC3339Nano, string(m[1])); err == nil && t.After(newest) {
				newest = t
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	shift := now.Add(-time.Minute).Sub(newest)
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		data = timestampField.ReplaceAllFunc(data, func(m []byte) []byte {
			sub := timestampField.FindSubmatch(m)
			t, err := time.Parse(time.RFC3339Nano, string(sub[1]))
			if err != nil {
				return m
			}
			return []byte(`"timestamp":"` + t.Add(shift).UTC().Format("2006-01-02T15:04:05.000Z") + `"`)
		})
		return os.WriteFile(out, data, 0o600)
	})
}
