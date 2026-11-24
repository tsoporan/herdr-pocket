package hub

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tsoporan/herdr-pocket/internal/answer"
	"github.com/tsoporan/herdr-pocket/internal/herdr"
	"github.com/tsoporan/herdr-pocket/internal/transcript"
)

// Source says where a detail's text comes from.
type Source string

const (
	SourceTranscript Source = "transcript" // the Claude Code session file
	SourceTerminal   Source = "terminal"   // the visible screen only
)

// Step is one line of the agent view's Steps.
type Step struct {
	Kind transcript.StepKind `json:"kind"`
	Text string              `json:"text"`
	Meta string              `json:"meta"`
}

// Detail is the agent view of one watched pane.
type Detail struct {
	Type        string           `json:"type"` // "detail"
	PaneID      string           `json:"pane_id"`
	Source      Source           `json:"source"`
	AgentLabel  string           `json:"agent_label"`
	StatusSince time.Time        `json:"status_since"`
	Now         string           `json:"now"`
	Steps       []Step           `json:"steps"`
	ContextPct  *int             `json:"context_pct"`
	PR          *string          `json:"pr"`
	Question    *answer.Question `json:"question"`
	// LastMessage is the agent's whole last message: the transcript's latest
	// assistant text, bounded by transcript.BoundMessage, or the screen's last
	// lines, bounded by boundLines. Nil when there is none.
	LastMessage *string `json:"last_message"`
	Asks        bool    `json:"asks"` // the whole message ends in a question; never for the screen
}

// Output is a watched pane's visible screen, sent when it changes.
type Output struct {
	Type   string `json:"type"` // "output"
	PaneID string `json:"pane_id"`
	Text   string `json:"text"`
}

// buildDetail assembles a detail. snap is nil for a terminal-source agent;
// screen is the latest visible screen, if read, and recent the latest
// recent-unwrapped read, if any. now dates the prompts ("You · 14 min ago").
func buildDetail(a herdr.Agent, since time.Time, snap *transcript.Snapshot, q *answer.Question, screen, recent string, now time.Time) Detail {
	d := Detail{
		Type:        "detail",
		PaneID:      a.PaneID,
		Source:      SourceTerminal,
		AgentLabel:  agentLabel(a),
		StatusSince: since.UTC().Truncate(time.Millisecond),
		Steps:       []Step{},
		ContextPct:  contextPct(a.Tokens["ctx"]),
		Question:    q,
	}
	if pr := a.Tokens["pr"]; pr != "" {
		d.PR = &pr
	}
	if snap == nil {
		d.Now = terminalNow(screen)
		if recent == "" {
			recent = screen // not read yet, or the read failed
		}
		if m := terminalLastLines(recent); m != "" {
			d.LastMessage = &m // asks stays false: a screen's "?" is often chrome
		}
		return d
	}
	d.Source = SourceTranscript
	d.Now = snap.Now
	if m := snap.LastMessage; m != "" {
		d.LastMessage = &m
		d.Asks = snap.LastAsks
	}
	for _, s := range snap.Steps {
		meta := s.Meta
		if s.Kind == transcript.KindYou && !s.At.IsZero() {
			meta += " · " + ago(now.Sub(s.At))
		}
		d.Steps = append(d.Steps, Step{Kind: s.Kind, Text: s.Text, Meta: meta})
	}
	if d.PR == nil && snap.PRNumber > 0 {
		pr := "#" + strconv.Itoa(snap.PRNumber)
		d.PR = &pr
	}
	return d
}

// ago matches the app's durationLabel (web/src/lib/words.ts): "just now",
// "14 min ago", "2 h 5 min ago", "3 d ago".
func ago(d time.Duration) string {
	m := int(max(d, 0) / time.Minute)
	switch {
	case m < 1:
		return "just now"
	case m < 60:
		return fmt.Sprintf("%d min ago", m)
	case m < 24*60 && m%60 != 0:
		return fmt.Sprintf("%d h %d min ago", m/60, m%60)
	case m < 24*60:
		return fmt.Sprintf("%d h ago", m/60)
	default:
		return fmt.Sprintf("%d d ago", m/(24*60))
	}
}

// question builds the blocked agent's question from its screen, with the
// transcript's pending call for the exact command and its context. epoch
// changes on every status change the hub sees, so a second, identical
// prompt gets a new id.
func question(a herdr.Agent, epoch uint64, screen string, snap *transcript.Snapshot) *answer.Question {
	var p answer.Pending
	cwd := a.Cwd
	if snap != nil {
		if snap.Cwd != "" {
			cwd = snap.Cwd
		}
		if pd := snap.Pending; pd != nil {
			if pd.Cwd != "" {
				cwd = pd.Cwd
			}
			p.LedHere = pd.LedHere
			// The transcript knows only the last call. Trust its command
			// only when the screen asks about that same command.
			if s := answer.Parse(screen); s.Tool == "Bash command" && sameCommand(s.Command, pd.Command) {
				p.Command, p.Description = pd.Command, pd.Description
			}
		}
	}
	if cwd != "" {
		p.Cwd = transcript.ShortDir(cwd)
	}
	q := answer.Build(a.PaneID, epoch, screen, p)
	return &q
}

// sameCommand compares a command as the screen wraps it with the
// transcript's, ignoring whitespace.
func sameCommand(screen, exact string) bool {
	squash := func(s string) string { return strings.Join(strings.Fields(s), "") }
	return exact != "" && squash(screen) == squash(exact)
}

func agentLabel(a herdr.Agent) string {
	if a.DisplayAgent != "" {
		return a.DisplayAgent
	}
	switch a.Agent {
	case "claude":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "":
		return "Terminal"
	}
	return a.Agent
}

// contextPct reads a ctx token given as a percentage ("27%"). A token count
// such as "112k" says nothing without the model's window, so it gives nil.
func contextPct(ctx string) *int {
	s, ok := strings.CutSuffix(strings.TrimSpace(ctx), "%")
	if !ok {
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 || n > 100 {
		return nil
	}
	return &n
}

const terminalNowRunes = 160

// meaningful reports whether a screen line has a letter or digit in it, so
// rules, box edges and bare prompts are not.
func meaningful(l string) bool {
	return strings.IndexFunc(l, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0
}

// terminalNow is the last meaningful line of a screen.
func terminalNow(screen string) string {
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if !meaningful(l) {
			continue
		}
		if r := []rune(l); len(r) > terminalNowRunes {
			l = string(r[:terminalNowRunes-1]) + "…"
		}
		return l
	}
	return ""
}

const (
	recentReadLines = 120 // lines asked of a recent-unwrapped read
	lastLinesMax    = 40  // lines kept for a terminal agent's last message
	inputBoxLines   = 6   // how near the bottom Claude Code's input box sits
)

// terminalLastLines is a terminal agent's last message: the last
// lastLinesMax lines of the screen, each trimmed, without lines that aren't
// meaningful and with runs of blank lines kept as one. Claude Code's input
// box (a ── rule over a ">" prompt, in the last inputBoxLines lines) and
// everything under it is dropped.
func terminalLastLines(screen string) string {
	lines := strings.Split(strings.TrimRight(screen, " \t\n"), "\n")
	for i := len(lines) - 1; i > 0 && i >= len(lines)-inputBoxLines; i-- {
		if p := strings.TrimSpace(lines[i]); (strings.HasPrefix(p, ">") || strings.HasPrefix(p, "❯")) && isRule(lines[i-1]) {
			lines = lines[:i-1]
			break
		}
	}
	var out []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		switch {
		case l == "":
			if len(out) > 0 && out[len(out)-1] != "" {
				out = append(out, "")
			}
		case meaningful(l):
			out = append(out, l)
		}
	}
	if len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	if len(out) > lastLinesMax {
		out = out[len(out)-lastLinesMax:]
		if out[0] == "" {
			out = out[1:]
		}
	}
	return boundLines(out)
}

// boundLines joins screen lines within transcript.MaxMessage characters.
// The newest lines matter, so whole lines go from the top, after a "…"
// line; a last line too long on its own keeps its end.
func boundLines(lines []string) string {
	n := len(lines) - 1 // the line breaks
	for _, l := range lines {
		n += utf8.RuneCountInString(l)
	}
	if n <= transcript.MaxMessage {
		return strings.Join(lines, "\n")
	}
	budget := transcript.MaxMessage - 2 // room for "…\n"
	i := len(lines)
	for i > 0 {
		l := utf8.RuneCountInString(lines[i-1]) + 1
		if l > budget {
			break
		}
		budget -= l
		i--
	}
	if i == len(lines) {
		r := []rune(lines[i-1])
		return "…" + string(r[max(len(r)-(transcript.MaxMessage-1), 0):])
	}
	kept := lines[i:]
	if kept[0] == "" {
		kept = kept[1:]
	}
	return "…\n" + strings.Join(kept, "\n")
}

// isRule reports whether a line is a horizontal rule of ─ characters.
func isRule(l string) bool {
	l = strings.TrimSpace(l)
	return l != "" && strings.Trim(l, "─") == ""
}
