package server

import (
	"strings"
	"testing"
)

func TestDescribeDrop(t *testing.T) {
	for _, tc := range []struct{ name, param, want string }{
		{"none", "", ""},
		{"reset while hidden",
			`{"code":1006,"reason":"","clean":false,"visible":false,"since_visibility_ms":42000,"online":true,"net":"wifi","effective":"4g","lived_ms":95250,"failed_tries":2,"ago_ms":61000}`,
			`code 1006 "", not clean after 1m35.3s; page hidden for 42s; online wifi/4g; 2 failed tries; 1m1s ago`},
		{"app gave up, no connection info",
			`{"code":4000,"reason":"no reply to the probe","clean":false,"visible":true,"since_visibility_ms":3000,"online":true,"lived_ms":52000,"failed_tries":0,"ago_ms":150}`,
			`code 4000 "no reply to the probe", not clean after 52s; page visible for 3s; online; 0 failed tries; 200ms ago`},
		{"malformed", `{"code":"x"}`, "unreadable"},
		{"too long", strings.Repeat("x", maxDropParam+1), "unreadable (too long)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeDrop(tc.param); got != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestDescribeDropBoundsStrings(t *testing.T) {
	got := describeDrop(`{"code":1000,"reason":"a\nb` + strings.Repeat("y", 100) + `","clean":true,"online":false}`)
	if strings.Contains(got, "\n") || strings.Contains(got, "\\n") || !strings.Contains(got, "…") {
		t.Errorf("not bounded: %s", got)
	}
}
