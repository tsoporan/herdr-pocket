package main

import (
	"testing"
	"time"
)

func TestCheckDevAddr(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:8390": true,
		"localhost:8390": true,
		"[::1]:8390":     true,
		":8390":          false,
		"0.0.0.0:8390":   false,
		"192.0.2.7:8390": false,
		"127.0.0.1":      false,
	} {
		if err := checkDevAddr(addr); (err == nil) != ok {
			t.Errorf("checkDevAddr(%q) = %v, want ok=%v", addr, err, ok)
		}
	}
}

func TestCheckDevOrigin(t *testing.T) {
	for origin, ok := range map[string]bool{
		"http://localhost:5173":    true,
		"http://127.0.0.1:5173":    true,
		"http://[::1]:5173":        true,
		"http://localhost":         false,
		"https://localhost:5173":   false,
		"http://evil.example:5173": false,
		"http://192.0.2.7:5173":    false,
		"http://localhost:5173/":   false,
		"http://u@localhost:5173":  false,
		"localhost:5173":           false,
		"*":                        false,
	} {
		if err := checkDevOrigin(origin); (err == nil) != ok {
			t.Errorf("checkDevOrigin(%q) = %v, want ok=%v", origin, err, ok)
		}
	}
}

func TestParseDays(t *testing.T) {
	for in, want := range map[string]time.Duration{"9d": 9 * 24 * time.Hour, "1d": 24 * time.Hour, "36h": 36 * time.Hour} {
		if got, err := parseDays(in); err != nil || got != want {
			t.Errorf("parseDays(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "d", "nined", "9x"} {
		if _, err := parseDays(in); err == nil {
			t.Errorf("parseDays(%q): no error", in)
		}
	}
}
