package hub

import (
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tsoporan/herdr-pocket/internal/herdr"
	"github.com/tsoporan/herdr-pocket/internal/transcript"
)

// Line is the plain-language row line. snap is the agent's transcript, or
// nil when it has none; now dates a working agent's current step.
//
//   - blocked: "Wants to run a command", "Asks: <question>" or "Wants to
//     change <file>", from the pending call;
//   - done: "Finished · <the Now line, shortened>";
//   - working: "<the now step> · <how long it has run>", else the terminal
//     title.
func Line(a herdr.Agent, snap *transcript.Snapshot, now time.Time) string {
	if snap != nil {
		switch a.AgentStatus {
		case herdr.StatusBlocked:
			if p := snap.Pending; p != nil {
				switch p.Tool {
				case "AskUserQuestion":
					if p.Question != "" {
						return "Asks: " + p.Question
					}
				case "Edit", "Write", "MultiEdit", "NotebookEdit":
					if p.Path != "" {
						return "Wants to change " + path.Base(p.Path)
					}
				}
			}
		case herdr.StatusDone:
			if s := shortNow(snap.Now); s != "" {
				return "Finished · " + s
			}
		case herdr.StatusWorking:
			if st, ok := nowStep(snap); ok {
				return st.Text + " · " + Age(now.Sub(st.At))
			}
		}
	}
	switch a.AgentStatus {
	case herdr.StatusBlocked:
		return "Wants to run a command"
	case herdr.StatusDone:
		return "Finished"
	case herdr.StatusWorking:
		if a.TerminalTitleStripped != "" {
			return a.TerminalTitleStripped
		}
		return "Working"
	case herdr.StatusIdle:
		return "Waiting for your next task"
	default:
		return "Can't tell what it's doing"
	}
}

// nowStep is the latest step still running.
func nowStep(snap *transcript.Snapshot) (transcript.Step, bool) {
	for i := len(snap.Steps) - 1; i >= 0; i-- {
		if snap.Steps[i].Kind == transcript.KindNow {
			return snap.Steps[i], true
		}
	}
	return transcript.Step{}, false
}

// Age is a short duration as the list shows it: "40s", "14m", "2h", "3d".
func Age(d time.Duration) string {
	d = max(d, 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}

const shortNowRunes = 48

// shortNow cuts the Now line to its first clause: "PR #48 is ready: the
// test waited…" gives "PR #48 is ready".
func shortNow(s string) string {
	for _, sep := range []string{". ", ": ", "; ", " — "} {
		if i := strings.Index(s, sep); i > 0 {
			s = s[:i]
		}
	}
	s = strings.TrimRight(strings.TrimSpace(s), ".:;")
	if utf8.RuneCountInString(s) <= shortNowRunes {
		return s
	}
	r := []rune(s)[:shortNowRunes-1]
	cut := string(r)
	if i := strings.LastIndexByte(cut, ' '); i > 0 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,") + "…"
}
