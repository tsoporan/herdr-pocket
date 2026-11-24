package fakeherdr

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/herdr"
)

// Screens: agent.read returns, in order, the text set with SetScreen, then
// testdata/screens/<pane>.<status>.txt (the pane id with ":" as "-"), then
// testdata/screens/<status>.txt. The blocked fixtures are
// w1-p1.blocked.txt (a Claude Code permission prompt for a Bash command),
// w2-p1.blocked.txt (an AskUserQuestion list; while blocked, w2:p1 shows
// the scripted two-question flow in ask.go instead) and w1-p2.blocked.txt (for the
// demo script); blocked.txt is an edit-permission prompt. The two Bash
// prompts copy the layout of a real one read on 2026-11-21 (structure only;
// all text is invented, including the footer and rule label). The ask and
// edit layouts are unverified guesses from Claude Code's docs. The
// w4-p2 screens are an invented Codex session (layout unverified).
// trust.blocked.txt is Claude Code's folder trust dialog, shown on a pane
// whose agent.start was StartNotReady. Its wording and its two unnumbered
// options follow the dialog seen on 2026-11-22; the spacing and the folder
// are invented.
//
//go:embed testdata/screens/*.txt
var screenFS embed.FS

// Input is one input that reached an agent's terminal, a rename or focus, or
// a tab.create or agent.start. Refused requests (a bad key, a prompt to a
// blocked agent, an agent.start with a taken name) are not logged.
type Input struct {
	Method string          // agent.prompt, agent.send_keys, pane.send_text, agent.rename, agent.focus, tab.create or agent.start
	PaneID string          // the resolved pane; tab.create's new root pane
	Text   string          // prompt and send_text
	Keys   []string        // send_keys
	Name   string          // rename (empty clears) and agent.start
	Kind   string          // agent.start
	Label  string          // tab.create
	Cwd    string          // tab.create: where the tab opened
	Params json.RawMessage // tab.create and agent.start: the params as sent
}

// OnInput sets a function called with each input as it is logged, for
// cmd/fake-herdr to print. It runs with the server's lock held.
func (s *Server) OnInput(f func(Input)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onInput = f
}

// logInput records in. Callers hold s.mu.
func (s *Server) logInput(in Input) {
	s.inputs = append(s.inputs, in)
	if s.onInput != nil {
		s.onInput(in)
	}
}

// InputLog returns a copy of every input so far, oldest first.
func (s *Server) InputLog() []Input {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.inputs)
}

// SetScreen replaces what agent.read returns for a pane until its status next
// changes, for example to change the question under a pending answer.
func (s *Server) SetScreen(paneID, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.agent(paneID)
	if err != nil {
		return err
	}
	s.screens[paneID] = text
	a.Revision++
	return nil
}

// SetTurn makes every turn a prompt starts end by itself after d: the agent
// goes from working back to idle, unless its status changed meanwhile. Zero
// (the default) leaves it working until SetStatus, as tests usually want.
func (s *Server) SetTurn(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turn = d
}

// HoldTurns makes a prompt leave the agent's status alone, so a test sees
// what happens between herdr accepting a prompt and the agent starting, and
// starts and ends the turn itself with SetStatus.
func (s *Server) HoldTurns() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holdTurns = true
}

// endTurnAfter sets a back to idle after s.turn if it is still in the
// working status the prompt put it in. Callers hold s.mu.
func (s *Server) endTurnAfter(a *herdr.Agent) {
	if s.turn <= 0 {
		return
	}
	pane, seq := a.PaneID, a.StateChangeSeq
	time.AfterFunc(s.turn, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if a, err := s.agent(pane); err == nil && a.AgentStatus == herdr.StatusWorking && a.StateChangeSeq == seq {
			s.setStatus(a, herdr.StatusIdle)
		}
	})
}

// KeepBlocked makes a digit leave the pane blocked, as when the dialog
// ignores it or another prompt comes up at once.
func (s *Server) KeepBlocked(paneID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.agent(paneID); err != nil {
		return err
	}
	s.sticky[paneID] = true
	return nil
}

// screen returns the text agent.read shows for a. Callers hold s.mu.
func (s *Server) screen(a *herdr.Agent) string {
	if text, ok := s.screens[a.PaneID]; ok {
		return text
	}
	if f := s.askFor(a); f != nil {
		return f.render()
	}
	pane := strings.ReplaceAll(a.PaneID, ":", "-")
	for _, name := range []string{pane + "." + string(a.AgentStatus) + ".txt", string(a.AgentStatus) + ".txt"} {
		b, err := screenFS.ReadFile("testdata/screens/" + name)
		if err == nil {
			return string(b)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			panic("fakeherdr: embedded screen: " + err.Error())
		}
	}
	return ""
}

// inputMethods serve agent.read, tab.create, agent.start and the input
// methods. Each runs
// with s.mu held, so it must use setStatus, never SetStatus.
var inputMethods = map[string]func(*Server, json.RawMessage) (any, *errorBody){
	"agent.read":      (*Server).read,
	"tab.create":      (*Server).createTab,
	"agent.start":     (*Server).startAgent,
	"agent.send_keys": (*Server).sendKeys,
	"pane.send_text":  (*Server).sendText,
	"agent.rename":    (*Server).rename,
	"agent.focus":     (*Server).agentFocus,
}

// waitingMethods may wait for a while, so they take s.mu themselves and
// marshal their result under it (see start.go).
var waitingMethods = map[string]func(*Server, json.RawMessage) (json.RawMessage, *errorBody){
	"agent.wait":   (*Server).waitAgent,
	"agent.prompt": (*Server).promptMethod,
}

// decode unmarshals params, requiring each named field to be present.
func decode(raw json.RawMessage, v any, required ...string) *errorBody {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return invalid("invalid request: %v", err)
	}
	for _, f := range required {
		if _, ok := fields[f]; !ok {
			return invalid("invalid request: missing field `%s`", f)
		}
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return invalid("invalid request: %v", err)
	}
	return nil
}

// target resolves an agent target, a pane id or an agent name, as herdr does.
// Callers hold s.mu.
func (s *Server) target(t string) (*herdr.Agent, *errorBody) {
	if a, err := s.agent(t); err == nil {
		return a, nil
	}
	for i := range s.fx.Agents {
		if s.fx.Agents[i].Name == t {
			return &s.fx.Agents[i], nil
		}
	}
	return nil, &errorBody{Code: herdr.CodeAgentNotFound, Message: "agent target " + t + " not found"}
}

func (s *Server) read(raw json.RawMessage) (any, *errorBody) {
	var p struct {
		Target string           `json:"target"`
		Source herdr.ReadSource `json:"source"`
		Format string           `json:"format"`
		Lines  *int             `json:"lines"`
	}
	if eb := decode(raw, &p, "target", "source"); eb != nil {
		return nil, eb
	}
	switch p.Source {
	case herdr.SourceVisible, herdr.SourceRecent, herdr.SourceRecentUnwrapped, herdr.SourceDetection:
	default:
		return nil, invalid("invalid request: unknown variant `%s`", p.Source)
	}
	if p.Format == "" {
		p.Format = "text"
	}
	a, eb := s.target(p.Target)
	if eb != nil {
		return nil, eb
	}
	text, truncated := s.screen(a), false
	if p.Lines != nil && p.Source != herdr.SourceVisible {
		lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
		if n := *p.Lines; n < len(lines) {
			text, truncated = strings.Join(lines[len(lines)-n:], "\n")+"\n", true
		}
	}
	return map[string]any{"type": "pane_read", "read": herdr.ReadResult{
		PaneID: a.PaneID, WorkspaceID: a.WorkspaceID, TabID: a.TabID, Source: p.Source,
		Format: p.Format, Text: text, Revision: a.Revision, Truncated: truncated,
	}}, nil
}

// prompt refuses a blocked agent like herdr, and otherwise starts a turn
// (see SetTurn for how it ends, and HoldTurns). With wait, promptMethod
// (start.go) then waits for the agent.
func (s *Server) prompt(raw json.RawMessage) (any, *errorBody) {
	var p struct {
		Target string `json:"target"`
		Text   string `json:"text"`
	}
	if eb := decode(raw, &p, "target", "text"); eb != nil {
		return nil, eb
	}
	a, eb := s.target(p.Target)
	if eb != nil {
		return nil, eb
	}
	if a.AgentStatus == herdr.StatusBlocked {
		return nil, &errorBody{Code: herdr.CodeAgentBlocked, Message: "agent " + p.Target + " is waiting at an approval or question dialog"}
	}
	s.logInput(Input{Method: "agent.prompt", PaneID: a.PaneID, Text: p.Text})
	if a.AgentStatus != herdr.StatusWorking && !s.holdTurns {
		s.setStatus(a, herdr.StatusWorking)
		s.endTurnAfter(a)
	}
	return map[string]any{"type": "agent_prompted", "agent": a}, nil
}

// sendKeys checks every key before acting, like herdr. On a blocked agent a
// digit picks an option and it goes back to working. esc (or ctrl+c)
// dismisses a dialog or interrupts a turn, leaving the agent idle.
func (s *Server) sendKeys(raw json.RawMessage) (any, *errorBody) {
	var p struct {
		Target string   `json:"target"`
		Keys   []string `json:"keys"`
	}
	if eb := decode(raw, &p, "target", "keys"); eb != nil {
		return nil, eb
	}
	a, eb := s.target(p.Target)
	if eb != nil {
		return nil, eb
	}
	if err := herdr.ValidateKeys(p.Keys); err != nil {
		return nil, invalid("invalid request: %v", err)
	}
	s.logInput(Input{Method: "agent.send_keys", PaneID: a.PaneID, Keys: slices.Clone(p.Keys)})
	if _, ok := s.trust[a.PaneID]; ok {
		s.trustKeys(a, p.Keys)
		return map[string]any{"type": "ok"}, nil
	}
	if f := s.askFor(a); f != nil {
		s.askKeys(a, f, p.Keys)
		return map[string]any{"type": "ok"}, nil
	}
	for _, k := range p.Keys {
		isDigit := len(k) == 1 && k[0] >= '0' && k[0] <= '9'
		stop := k == "esc" || k == "escape" || k == "ctrl+c"
		switch {
		case a.AgentStatus == herdr.StatusBlocked && isDigit && !s.sticky[a.PaneID]:
			s.setStatus(a, herdr.StatusWorking)
		case stop && (a.AgentStatus == herdr.StatusBlocked || a.AgentStatus == herdr.StatusWorking):
			s.setStatus(a, herdr.StatusIdle)
		}
	}
	return map[string]any{"type": "ok"}, nil
}

func (s *Server) sendText(raw json.RawMessage) (any, *errorBody) {
	var p struct {
		PaneID string `json:"pane_id"`
		Text   string `json:"text"`
	}
	if eb := decode(raw, &p, "pane_id", "text"); eb != nil {
		return nil, eb
	}
	a, err := s.agent(p.PaneID)
	if err != nil {
		return nil, &errorBody{Code: herdr.CodePaneNotFound, Message: "pane " + p.PaneID + " not found"}
	}
	s.logInput(Input{Method: "pane.send_text", PaneID: a.PaneID, Text: p.Text})
	if f := s.askFor(a); f != nil {
		s.askText(f, p.Text)
	}
	return map[string]any{"type": "ok"}, nil
}

// agentName is herdr's rule for agent names (from herdr's agent guide).
var agentName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// rename enforces herdr's name rule and uniqueness. Unverified: the error
// code herdr uses for a bad or taken name; invalid_request is a guess.
// No pane.updated is emitted; whether herdr emits one is not known.
func (s *Server) rename(raw json.RawMessage) (any, *errorBody) {
	var p struct {
		Target string  `json:"target"`
		Name   *string `json:"name"`
	}
	if eb := decode(raw, &p, "target"); eb != nil {
		return nil, eb
	}
	a, eb := s.target(p.Target)
	if eb != nil {
		return nil, eb
	}
	name := ""
	if p.Name != nil {
		name = *p.Name
	}
	if name != "" {
		if !agentName.MatchString(name) {
			return nil, invalid("invalid request: agent name %q must match [a-z][a-z0-9_-]{0,31}", name)
		}
		for _, o := range s.fx.Agents {
			if o.PaneID != a.PaneID && o.Name == name {
				return nil, invalid("invalid request: agent name %q is already in use", name)
			}
		}
	}
	s.logInput(Input{Method: "agent.rename", PaneID: a.PaneID, Name: name})
	a.Name = name
	a.Revision++
	return map[string]any{"type": "agent_info", "agent": a}, nil
}

// agentFocus makes the agent the focused pane and marks it seen: done becomes idle.
func (s *Server) agentFocus(raw json.RawMessage) (any, *errorBody) {
	var p struct {
		Target string `json:"target"`
	}
	if eb := decode(raw, &p, "target"); eb != nil {
		return nil, eb
	}
	a, eb := s.target(p.Target)
	if eb != nil {
		return nil, eb
	}
	s.logInput(Input{Method: "agent.focus", PaneID: a.PaneID})
	s.focus(a)
	if a.AgentStatus == herdr.StatusDone {
		s.setStatus(a, herdr.StatusIdle)
	}
	return map[string]any{"type": "agent_info", "agent": a}, nil
}
