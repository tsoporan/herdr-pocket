package hub

import (
	"encoding/json"
	"testing"
	"time"
)

func TestExpiryAt(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	day := 24 * time.Hour
	for name, tc := range map[string]struct {
		key  *time.Time
		want int // -100: nil
	}{
		"off":              {nil, -100},
		"15 days":          {at(15 * day), -100},
		"just over 14":     {at(14*day + time.Minute), -100},
		"14 days":          {at(14 * day), 14},
		"9 days":           {at(9 * day), 9},
		"9 days less 1ms":  {at(9*day - time.Millisecond), 9},
		"23 hours":         {at(23 * time.Hour), 1},
		"just ended":       {at(-time.Minute), 0},
		"ended 2 days ago": {at(-2 * day), -2},
	} {
		got := ExpiryAt(tc.key, now)
		switch {
		case tc.want == -100 && got != nil:
			t.Errorf("%s: %+v, want nil", name, got)
		case tc.want != -100 && (got == nil || got.DaysLeft != tc.want):
			t.Errorf("%s: %+v, want %d days", name, got, tc.want)
		}
	}
}

func TestExpiryPatchJSON(t *testing.T) {
	b, err := json.Marshal(Patch{Type: "patch"})
	if err != nil || string(b) != `{"type":"patch"}` {
		t.Errorf("no expiry: %s %v", b, err)
	}
	b, _ = json.Marshal(Patch{Type: "patch", Expiry: &ExpiryPatch{}})
	if string(b) != `{"type":"patch","expiry":null}` {
		t.Errorf("cleared: %s", b)
	}
	at := time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)
	b, _ = json.Marshal(Patch{Type: "patch", Expiry: &ExpiryPatch{Value: &Expiry{At: at, DaysLeft: 9}}})
	if string(b) != `{"type":"patch","expiry":{"at":"2026-11-30T00:00:00Z","days_left":9}}` {
		t.Errorf("set: %s", b)
	}
	var p Patch
	if err := json.Unmarshal(b, &p); err != nil || p.Expiry == nil || p.Expiry.Value.DaysLeft != 9 {
		t.Errorf("decode: %+v %v", p, err)
	}
}
