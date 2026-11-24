package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// dropReport is the app's account of how its last open socket ended, sent as
// the drop query parameter of the next try (web/src/lib/socket.ts
// DropReport). It carries no prompt or output text, but it comes from the
// phone, so only these fields are read and the strings are bounded.
type dropReport struct {
	Code              int    `json:"code"`
	Reason            string `json:"reason"`
	Clean             bool   `json:"clean"`
	Visible           bool   `json:"visible"`
	SinceVisibilityMS int64  `json:"since_visibility_ms"`
	Online            bool   `json:"online"`
	Net               string `json:"net"`
	Effective         string `json:"effective"`
	LivedMS           int64  `json:"lived_ms"`
	FailedTries       int    `json:"failed_tries"`
	AgoMS             int64  `json:"ago_ms"`
}

// maxDropParam bounds the parameter; a real report is about 250 bytes.
const maxDropParam = 1024

// describeDrop turns the drop parameter into one log phrase, or "" when
// there is none. A malformed one is said to be so rather than dropped.
func describeDrop(param string) string {
	if param == "" {
		return ""
	}
	if len(param) > maxDropParam {
		return "unreadable (too long)"
	}
	var d dropReport
	if err := json.Unmarshal([]byte(param), &d); err != nil {
		return "unreadable"
	}
	page := "hidden"
	if d.Visible {
		page = "visible"
	}
	net := "offline"
	if d.Online {
		net = "online"
	}
	if d.Net != "" || d.Effective != "" {
		net += " " + strings.Trim(bounded(d.Net)+"/"+bounded(d.Effective), "/")
	}
	clean := ""
	if !d.Clean {
		clean = ", not clean"
	}
	return fmt.Sprintf("code %d %q%s after %s; page %s for %s; %s; %d failed tries; %s ago",
		d.Code, bounded(d.Reason), clean, ms(d.LivedMS), page, ms(d.SinceVisibilityMS), net, d.FailedTries, ms(d.AgoMS))
}

func ms(n int64) time.Duration {
	return (time.Duration(n) * time.Millisecond).Round(100 * time.Millisecond)
}

// bounded keeps a phone-sent string to one short printable line.
func bounded(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
	if r := []rune(s); len(r) > 64 {
		s = string(r[:64]) + "…"
	}
	return s
}
