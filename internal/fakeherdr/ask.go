package fakeherdr

import (
	"fmt"
	"strings"

	"github.com/tsoporan/herdr-pocket/internal/herdr"
)

// askFlow walks a pane through Claude Code's AskUserQuestion dialog with
// several single-choice questions, as read in an isolated herdr session on
// 2026-11-22 and 2026-11-23:
//
//   - a digit answers the current tab and moves to the next;
//   - the "Type something." row's digit focuses its box (herdr then reports
//     the agent done or idle; this fake says idle), typed text replaces the
//     label, and Enter answers the tab with it;
//   - after the last tab, the review: 1 sends the answers (the agent works),
//     2 cancels the whole ask (the agent is idle);
//   - esc cancels at any point.
//
// The layout copies the real one; every word is invented.
type askFlow struct {
	lead      string // the agent's words above the dialog
	questions []askQuestion
	tab       int      // the current question; len(questions) is the review
	answers   []string // by tab
	own       bool     // the own-answer box is focused
	typed     string   // what's in it
}

type askQuestion struct {
	header, text string
	options      [][2]string // label, description
}

// askScripts are the panes that ask with a scripted flow while blocked.
// w2:p1's flow replaces w2-p1.blocked.txt, which the parser tests still use.
var askScripts = map[string]func() *askFlow{
	"w2:p1": func() *askFlow {
		return &askFlow{
			lead: "⏺ The schema diff is ready: 14 tables, 2 need a rewrite for the new\n" +
				"  identity columns. Before I run anything I need to know where to start.",
			questions: []askQuestion{
				{"Environment", "Which database should I migrate to Postgres 18 first?", [][2]string{
					{"staging", "Shared staging cluster; a full rehearsal with real-sized data"},
					{"dev", "Local dev database only; fast, but small data"},
				}},
				{"Window", "When should the migration run?", [][2]string{
					{"Tonight at 2 am", "Lowest traffic; I'll watch the replica lag"},
					{"Right now", "Staging is quiet; about 20 minutes"},
				}},
			},
		}
	},
}

const askRule = "────────────────────────────────────────────────────────────────────────────────"

// render draws the dialog as Claude Code does.
func (f *askFlow) render() string {
	var b strings.Builder
	b.WriteString(f.lead + "\n\n" + askRule + "\n")
	tabs := []string{}
	for i, q := range f.questions {
		box := "☐"
		if i < len(f.answers) && f.answers[i] != "" {
			box = "☒"
		}
		tabs = append(tabs, box+" "+q.header)
	}
	if len(f.questions) == 1 {
		b.WriteString(" " + tabs[0] + "\n\n")
	} else {
		b.WriteString("←  " + strings.Join(tabs, "  ") + "  ✔ Submit  →\n\n")
	}
	if f.tab == len(f.questions) {
		b.WriteString("Review your answers\n\n")
		for i, q := range f.questions {
			fmt.Fprintf(&b, " ● %s\n   → %s\n", q.text, f.answers[i])
		}
		b.WriteString("\n❯ 1. Submit answers\n  2. Cancel\n")
		return b.String()
	}
	q := f.questions[f.tab]
	b.WriteString(q.text + "\n\n")
	for i, o := range q.options {
		cursor := "  "
		if i == 0 && !f.own {
			cursor = "❯ "
		}
		fmt.Fprintf(&b, "%s%d. %s\n     %s\n", cursor, i+1, o[0], o[1])
	}
	own, cursor := "Type something.", "  "
	if f.own {
		cursor = "❯ "
		if f.typed != "" {
			own = f.typed
		}
	}
	n := len(q.options)
	fmt.Fprintf(&b, "%s%d. %s\n%s\n  %d. Chat about this\n\n", cursor, n+1, own, askRule, n+2)
	if f.own {
		b.WriteString("-- INSERT -- · Enter to select · Tab/Arrow keys to navigate · ctrl+g to edit in Nvim\n")
	} else {
		b.WriteString("Enter to select · Tab/Arrow keys to navigate · Esc to cancel\n")
	}
	return b.String()
}

// askFor returns the pane's flow, starting its script when a scripted pane
// is blocked. A screen set with SetScreen wins over it. Callers hold s.mu.
func (s *Server) askFor(a *herdr.Agent) *askFlow {
	if _, set := s.screens[a.PaneID]; set {
		return nil
	}
	if f := s.asks[a.PaneID]; f != nil {
		return f
	}
	script := askScripts[a.PaneID]
	if script == nil || a.AgentStatus != herdr.StatusBlocked {
		return nil
	}
	f := script()
	s.asks[a.PaneID] = f
	return f
}

// askKeys handles keys on an open ask. Callers hold s.mu.
func (s *Server) askKeys(a *herdr.Agent, f *askFlow, keys []string) {
	for _, k := range keys {
		if s.asks[a.PaneID] != f {
			return // finished or cancelled by an earlier key
		}
		switch {
		case k == "esc" || k == "escape" || k == "ctrl+c":
			s.endAsk(a, herdr.StatusIdle)
		case f.own && (k == "enter" || k == "Enter"):
			if f.typed != "" {
				s.askAnswer(a, f, f.typed)
			}
		case f.own && len(k) == 1:
			f.typed += k // a digit typed into the box is text
		case f.tab == len(f.questions) && k == "1":
			s.endAsk(a, herdr.StatusWorking)
		case f.tab == len(f.questions) && k == "2":
			s.endAsk(a, herdr.StatusIdle)
		case f.tab < len(f.questions) && len(k) == 1 && k[0] >= '1' && k[0] <= '9':
			q, n := f.questions[f.tab], int(k[0]-'0')
			switch {
			case n <= len(q.options):
				s.askAnswer(a, f, q.options[n-1][0])
			case n == len(q.options)+1:
				f.own, f.typed = true, ""
				s.askStatus(a, f, herdr.StatusIdle)
			}
		}
	}
}

// askText types into a focused own-answer box; elsewhere it's ignored.
func (s *Server) askText(f *askFlow, text string) {
	if f.own {
		f.typed += text
	}
}

// askAnswer records the current tab's answer and moves to the next tab, or
// the review.
func (s *Server) askAnswer(a *herdr.Agent, f *askFlow, answer string) {
	for len(f.answers) <= f.tab {
		f.answers = append(f.answers, "")
	}
	f.answers[f.tab] = answer
	f.tab++
	f.own, f.typed = false, ""
	if len(f.questions) == 1 {
		s.endAsk(a, herdr.StatusWorking) // one question: no review
		return
	}
	s.askStatus(a, f, herdr.StatusBlocked)
}

// askStatus sets the status when it changes, keeping the flow (setStatus
// drops it with any screen override).
func (s *Server) askStatus(a *herdr.Agent, f *askFlow, status herdr.Status) {
	if a.AgentStatus != status {
		s.setStatus(a, status)
		s.asks[a.PaneID] = f
	}
}

func (s *Server) endAsk(a *herdr.Agent, status herdr.Status) {
	delete(s.asks, a.PaneID)
	s.setStatus(a, status)
}
