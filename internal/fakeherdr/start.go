package fakeherdr

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/herdr"
)

// tab.create, agent.start, agent.wait and agent.prompt with wait, modelled on
// herdr 0.9.5 as checked in an isolated session on 2026-11-22, and over the
// raw socket on 2026-11-23. The
// fake models the socket, not the herdr CLI: agent.start answers at once and
// agent.wait does the waiting. Where the probe didn't look, the comment says
// so.

// StartOutcome is how a started agent of a kind comes up (SetStartOutcome).
// agent.start answers agent_started at once whatever the outcome; the
// outcome is what agent.wait then sees.
type StartOutcome string

const (
	// StartReady: after the start delay the agent is idle and
	// interactive_ready, launch_pending gone.
	StartReady StartOutcome = "ready"
	// StartNotReady: after the start delay the agent is blocked at Claude
	// Code's trust dialog, launch_pending still true (the CLI calls this
	// agent_not_ready). The name stays the agent's.
	StartNotReady StartOutcome = "not_ready"
	// StartTimeout: the executable isn't installed. The agent stays listed
	// under its name, unknown and launch pending, so agent.wait times out
	// after its timeout_ms and the name stays taken.
	StartTimeout StartOutcome = "timeout"
)

var validOutcome = map[StartOutcome]bool{StartReady: true, StartNotReady: true, StartTimeout: true}

const (
	defaultStartDelay = 40 * time.Millisecond // real herdr: 3.9 s to idle or blocked
	defaultStall      = 5 * time.Second       // herdr's stall window for a waiting prompt
	// defaultTimeoutMS is agent.start's timeout_ms when omitted (30 s, per
	// herdr's agent guide). The same default for agent.wait and a waiting
	// prompt is a guess.
	defaultTimeoutMS = 30000
)

// SetStartOutcome sets what agent.start does for kind; StartReady is the
// default for every kind.
func (s *Server) SetStartOutcome(kind string, o StartOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outcomes[kind] = o
}

// SetStartDelay sets how long a started agent takes to come up, idle or
// blocked (default 40 ms; real herdr takes about 3.9 s). agent.start itself
// answers at once. The timeout outcome never comes up, so agent.wait on it
// runs to its own timeout_ms (or SetWaitLimit).
func (s *Server) SetStartDelay(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startDelay = d
}

// SetWaitLimit caps how long agent.wait waits before it answers timeout,
// whatever timeout_ms says (0, the default, removes the cap). herdr has no
// such cap: it is for tests of the timeout outcome, which would otherwise
// wait out agent.start's timeout_ms (more than 3 s).
func (s *Server) SetWaitLimit(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waitLimit = d
}

// SetStallWindow sets how long a waiting prompt to a non-working agent waits
// to see working or blocked before agent_prompt_stalled (herdr: 5 s).
func (s *Server) SetStallWindow(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stall = d
}

// trustScreen is Claude Code's folder trust dialog, from trust.blocked.txt.
// trustPath, the invented folder in that file, is replaced with the pane's cwd.
const trustPath = "/home/dev/src/new-project"

func trustScreen(cwd string) string {
	b, err := screenFS.ReadFile("testdata/screens/trust.blocked.txt")
	if err != nil {
		panic("fakeherdr: embedded screen: " + err.Error())
	}
	return strings.ReplaceAll(string(b), trustPath, cwd)
}

// shell finds a shell pane (one with no agent). Callers hold s.mu.
func (s *Server) shell(paneID string) int {
	return slices.IndexFunc(s.shells, func(p herdr.Pane) bool { return p.PaneID == paneID })
}

// nextID returns the next herdr-shaped id in a workspace, such as w1:p9 or
// w1:tA: the workspace id, the letter and the next number after the ones in
// use. herdr writes 10 as A (seen up to F on 2026-11-22); base 36 past that
// is a guess. Callers hold s.mu.
func nextID(workspaceID, letter string, used []string) string {
	n := int64(0)
	for _, id := range used {
		rest, ok := strings.CutPrefix(id, workspaceID+":"+letter)
		if !ok {
			continue
		}
		if v, err := strconv.ParseInt(rest, 36, 64); err == nil {
			n = max(n, v)
		}
	}
	return workspaceID + ":" + letter + strings.ToUpper(strconv.FormatInt(n+1, 36))
}

// createTab adds a tab with one shell pane. Unverified: herdr's code for an
// unknown workspace (CodeWorkspaceNotFound is a guess); that an omitted
// workspace_id means the focused workspace; and focus:true, which the fake
// doesn't act on (herdr-pocket never sends it).
func (s *Server) createTab(raw json.RawMessage) (any, *errorBody) {
	var p struct {
		WorkspaceID *string `json:"workspace_id"`
		Cwd         *string `json:"cwd"`
		Label       *string `json:"label"`
		Focus       bool    `json:"focus"`
	}
	if eb := decode(raw, &p); eb != nil {
		return nil, eb
	}
	wsID := ""
	if p.WorkspaceID != nil {
		wsID = *p.WorkspaceID
	} else if i := slices.IndexFunc(s.fx.Workspaces, func(w herdr.Workspace) bool { return w.Focused }); i >= 0 {
		wsID = s.fx.Workspaces[i].WorkspaceID
	}
	wi := slices.IndexFunc(s.fx.Workspaces, func(w herdr.Workspace) bool { return w.WorkspaceID == wsID })
	if wi < 0 {
		return nil, &errorBody{Code: herdr.CodeWorkspaceNotFound, Message: "workspace " + wsID + " not found"}
	}
	ws := &s.fx.Workspaces[wi]

	cwd := s.activeCwd(ws)
	if p.Cwd != nil && *p.Cwd != "" {
		cwd = *p.Cwd
	}
	var tabIDs, paneIDs []string
	number := 0
	for _, t := range s.fx.Tabs {
		tabIDs = append(tabIDs, t.TabID)
		if t.WorkspaceID == wsID {
			number = max(number, t.Number)
		}
	}
	for _, a := range s.fx.Agents {
		paneIDs = append(paneIDs, a.PaneID)
	}
	for _, sh := range s.shells {
		paneIDs = append(paneIDs, sh.PaneID)
	}
	label := ""
	if p.Label != nil {
		label = *p.Label
	}
	tab := herdr.Tab{
		TabID: nextID(wsID, "t", tabIDs), WorkspaceID: wsID, Number: number + 1, Label: label,
		PaneCount: 1, AgentStatus: herdr.StatusUnknown,
	}
	s.terms++
	pane := herdr.Pane{
		PaneID: nextID(wsID, "p", paneIDs), TabID: tab.TabID, WorkspaceID: wsID,
		TerminalID: fmt.Sprintf("term_fake%08x", s.terms), AgentStatus: herdr.StatusUnknown,
		Cwd: cwd, ForegroundCwd: cwd,
	}
	s.fx.Tabs = append(s.fx.Tabs, tab)
	s.shells = append(s.shells, pane)
	ws.TabCount++
	ws.PaneCount++
	s.logInput(Input{Method: "tab.create", PaneID: pane.PaneID, Label: label, Cwd: cwd, Params: slices.Clone(raw)})
	// Both at once, tab first (seen 2026-11-22).
	s.emitGlobal(herdr.EventTabCreated, "tab_created", map[string]any{"tab": tab})
	s.emitGlobal(herdr.EventPaneCreated, "pane_created", map[string]any{"pane": pane})
	return map[string]any{"type": "tab_created", "tab": tab, "root_pane": pane}, nil
}

// activeCwd is where a tab opens with cwd omitted: herdr uses the live cwd
// of the workspace's active pane. The fake takes the first pane of the
// active tab, else its own folder. Callers hold s.mu.
func (s *Server) activeCwd(ws *herdr.Workspace) string {
	for _, a := range s.fx.Agents {
		if a.TabID == ws.ActiveTabID {
			return a.Cwd
		}
	}
	for _, sh := range s.shells {
		if sh.TabID == ws.ActiveTabID {
			return sh.Cwd
		}
	}
	return s.dir
}

// emitGlobal writes a global event in herdr's wire shape. Callers hold s.mu.
func (s *Server) emitGlobal(kind, wire string, data map[string]any) {
	data["type"] = wire
	s.emit(func(sub *subscriber) bool { return sub.types[kind] }, map[string]any{"event": wire, "data": data})
}

// startAgent checks the request in herdr's order (name pattern, live name,
// the pane is an available shell, timeout_ms), each refusal before anything
// is typed. Then, as herdr 0.9.5 does over the socket (checked 2026-11-23),
// it answers at once with agent_started: the shell pane becomes an agent
// listed under its name, status unknown, launch_pending, so the name and the
// pane are taken from here on. The outcome lands after the start delay
// (landStart). Unverified: the codes for an unknown pane (pane_not_found),
// an out-of-range timeout_ms and an unknown kind (invalid_request); where
// the kind check falls in the order; and the refusal message texts.
func (s *Server) startAgent(raw json.RawMessage) (any, *errorBody) {
	var p struct {
		Name      string   `json:"name"`
		Kind      string   `json:"kind"`
		PaneID    string   `json:"pane_id"`
		TimeoutMS *int64   `json:"timeout_ms"`
		Args      []string `json:"args"`
	}
	if eb := decode(raw, &p, "name", "kind", "pane_id"); eb != nil {
		return nil, eb
	}
	if eb := s.checkStart(p.Name, p.Kind, p.PaneID, p.TimeoutMS); eb != nil {
		return nil, eb
	}
	s.logInput(Input{Method: "agent.start", PaneID: p.PaneID, Name: p.Name, Kind: p.Kind, Params: slices.Clone(raw)})
	i := s.shell(p.PaneID)
	sh := s.shells[i]
	s.shells = slices.Delete(s.shells, i, i+1)
	s.fx.Agents = append(s.fx.Agents, herdr.Agent{
		PaneID: sh.PaneID, TabID: sh.TabID, WorkspaceID: sh.WorkspaceID, TerminalID: sh.TerminalID,
		Name: p.Name, Agent: p.Kind, AgentStatus: herdr.StatusUnknown, Cwd: sh.Cwd, ForegroundCwd: sh.ForegroundCwd,
		Revision: sh.Revision, LaunchPending: true,
	})
	a := s.fx.Agents[len(s.fx.Agents)-1]
	s.rollUp(a.TabID, a.WorkspaceID)
	go s.landStart(a.PaneID, s.outcomes[p.Kind], s.startDelay)
	// herdr's argv is the executable it typed, which isn't always the
	// kind's name (kiro runs kiro-cli); the fake uses the kind.
	return map[string]any{"type": "agent_started", "agent": a, "argv": []string{p.Kind}}, nil
}

// landStart plays a started agent's launch, without the lock while it
// waits. herdr emits pane_agent_detected and pane_updated partway through
// (0.5 s and 3.5 s of 3.9 s), and nothing once startup ends, so the events
// come halfway here, while agent.list still shows the agent unknown and
// launch pending. At the end of the delay it becomes idle and ready, or
// blocked at the trust dialog, launch still pending. On StartTimeout (the
// executable isn't installed) nothing changes, ever: no events, and the
// agent stays unknown and launch pending under its name, as on herdr.
func (s *Server) landStart(paneID string, outcome StartOutcome, delay time.Duration) {
	if outcome == StartTimeout || !s.startWait(delay/2) {
		return
	}
	s.mu.Lock()
	if a, err := s.agent(paneID); err == nil && a.LaunchPending {
		s.emitGlobal(herdr.EventPaneAgentDetected, "pane_agent_detected",
			map[string]any{"pane_id": a.PaneID, "workspace_id": a.WorkspaceID, "agent": a.Agent})
		s.emitGlobal(herdr.EventPaneUpdated, "pane_updated", map[string]any{"pane": paneOf(a)})
	}
	s.mu.Unlock()
	if !s.startWait(delay - delay/2) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.agent(paneID)
	if err != nil || !a.LaunchPending {
		return
	}
	// Landed without pane.agent_status_changed, as the fake did before:
	// the probe saw no event once startup ended (it didn't subscribe to
	// the new pane, so that one is unverified). agent.wait still sees it.
	status := herdr.StatusIdle
	if outcome == StartNotReady {
		status = herdr.StatusBlocked
		s.screens[a.PaneID] = trustScreen(a.Cwd)
		s.trust[a.PaneID] = 0
	} else {
		a.LaunchPending, a.InteractiveReady = false, true
	}
	s.seq++
	a.AgentStatus, a.StateChangeSeq = status, s.seq
	s.rollUp(a.TabID, a.WorkspaceID)
	s.notify(a.PaneID, status)
}

// paneOf is an agent's pane record, as pane.updated carries it.
func paneOf(a *herdr.Agent) herdr.Pane {
	return herdr.Pane{
		PaneID: a.PaneID, TabID: a.TabID, WorkspaceID: a.WorkspaceID, TerminalID: a.TerminalID,
		Agent: a.Agent, DisplayAgent: a.DisplayAgent, AgentStatus: a.AgentStatus, AgentSession: a.AgentSession,
		Title: a.Title, TerminalTitle: a.TerminalTitle, TerminalTitleStripped: a.TerminalTitleStripped,
		Cwd: a.Cwd, ForegroundCwd: a.ForegroundCwd, Focused: a.Focused, Tokens: a.Tokens,
		StateLabels: a.StateLabels, Revision: a.Revision,
	}
}

// waitAgent serves agent.wait {target, until, timeout_ms}: agent_info as
// soon as the agent's status is in until, at once if it already is, else
// timeout ("timed out waiting for agent status", as herdr 0.9.5 answered on
// 2026-11-23) after timeout_ms. Unverified: herdr's limits on timeout_ms
// and its default, whether until may be empty, and what it answers when
// the agent leaves mid-wait (the fake waits on to the timeout).
func (s *Server) waitAgent(raw json.RawMessage) (json.RawMessage, *errorBody) {
	s.mu.Lock()
	s.waits++
	s.mu.Unlock()
	var p struct {
		Target    string         `json:"target"`
		Until     []herdr.Status `json:"until"`
		TimeoutMS *int64         `json:"timeout_ms"`
	}
	if eb := decode(raw, &p, "target", "until"); eb != nil {
		return nil, eb
	}
	until := map[herdr.Status]bool{}
	for _, st := range p.Until {
		if !validStatus[st] {
			return nil, invalid("invalid request: unknown variant `%s`", st)
		}
		until[st] = true
	}
	timeout := time.Duration(defaultTimeoutMS) * time.Millisecond
	if p.TimeoutMS != nil {
		timeout = time.Duration(*p.TimeoutMS) * time.Millisecond
	}
	s.mu.Lock()
	if s.waitLimit > 0 {
		timeout = min(timeout, s.waitLimit)
	}
	a, eb := s.target(p.Target)
	if eb != nil {
		s.mu.Unlock()
		return nil, eb
	}
	if until[a.AgentStatus] {
		defer s.mu.Unlock()
		return s.marshal(map[string]any{"type": "agent_info", "agent": a})
	}
	w := &watcher{pane: a.PaneID, ch: make(chan herdr.Status, 64)}
	s.watchers[w] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.watchers, w)
		s.mu.Unlock()
	}()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case st := <-w.ch:
			if !until[st] {
				continue
			}
			s.mu.Lock()
			a, err := s.agent(w.pane)
			if err != nil {
				s.mu.Unlock()
				return nil, &errorBody{Code: herdr.CodeAgentNotFound, Message: "agent target " + w.pane + " not found"}
			}
			v, eb := s.marshal(map[string]any{"type": "agent_info", "agent": a})
			s.mu.Unlock()
			return v, eb
		case <-deadline.C:
			return nil, &errorBody{Code: herdr.CodeTimeout, Message: "timed out waiting for agent status"}
		case <-s.done:
			return nil, &errorBody{Code: "internal", Message: "fake-herdr is stopping"}
		}
	}
}

// WaitCount is how many agent.wait requests the fake has had, refused or
// not. agent.wait types nothing, so it is not in InputLog.
func (s *Server) WaitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waits
}

// startWait waits d, or reports false when the fake is stopping.
func (s *Server) startWait(d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-s.done:
		return false
	}
}

// checkStart is startAgent's validation. Callers hold s.mu.
func (s *Server) checkStart(name, kind, paneID string, timeoutMS *int64) *errorBody {
	if !agentName.MatchString(name) {
		return &errorBody{Code: herdr.CodeInvalidAgentName, Message: fmt.Sprintf("agent name %q must match [a-z][a-z0-9_-]{0,31}", name)}
	}
	if slices.ContainsFunc(s.fx.Agents, func(a herdr.Agent) bool { return a.Name == name }) {
		return &errorBody{Code: herdr.CodeAgentNameTaken, Message: fmt.Sprintf("agent name %q is already in use", name)}
	}
	if _, err := s.agent(paneID); err == nil {
		return &errorBody{Code: herdr.CodeAgentPaneBusy, Message: "pane " + paneID + " is not an available shell"}
	}
	if s.shell(paneID) < 0 {
		return &errorBody{Code: herdr.CodePaneNotFound, Message: "pane " + paneID + " not found"}
	}
	// The schema: "Values must be greater than 3000 and at most 300000."
	if timeoutMS != nil && (*timeoutMS <= 3000 || *timeoutMS > 300000) {
		return invalid("invalid request: timeout_ms must be greater than 3000 and at most 300000")
	}
	if !slices.Contains(herdr.Kinds, kind) {
		return invalid("invalid request: unknown agent kind `%s`", kind)
	}
	return nil
}

// marshal encodes v. Callers hold s.mu, so v is a consistent copy.
func (s *Server) marshal(v any) (json.RawMessage, *errorBody) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, &errorBody{Code: "internal", Message: err.Error()}
	}
	return b, nil
}

// watcher receives every status change of one pane, in order, so a waiting
// prompt sees a working that a short turn has already ended, and agent.wait
// sees a started agent come up.
type watcher struct {
	pane string
	ch   chan herdr.Status
}

// notify passes a status change to the pane's watchers. Callers hold s.mu.
func (s *Server) notify(pane string, status herdr.Status) {
	for w := range s.watchers {
		if w.pane == pane {
			select {
			case w.ch <- status:
			default: // a waiter this far behind has long timed out
			}
		}
	}
}

// promptMethod serves agent.prompt: without wait as before (prompt), with
// wait it sends and then waits without the lock.
func (s *Server) promptMethod(raw json.RawMessage) (json.RawMessage, *errorBody) {
	var p struct {
		Wait *struct {
			Until     []herdr.Status `json:"until"`
			TimeoutMS *int64         `json:"timeout_ms"`
		} `json:"wait"`
	}
	if eb := decode(raw, &p); eb != nil {
		return nil, eb
	}
	s.mu.Lock()
	if p.Wait == nil {
		defer s.mu.Unlock()
		v, eb := s.prompt(raw)
		if eb != nil {
			return nil, eb
		}
		return s.marshal(v)
	}
	until := map[herdr.Status]bool{herdr.StatusIdle: true, herdr.StatusDone: true, herdr.StatusBlocked: true}
	if len(p.Wait.Until) > 0 {
		until = map[herdr.Status]bool{}
		for _, st := range p.Wait.Until {
			if !validStatus[st] {
				s.mu.Unlock()
				return nil, invalid("invalid request: unknown variant `%s`", st)
			}
			until[st] = true
		}
	}
	timeout := time.Duration(defaultTimeoutMS) * time.Millisecond
	if p.Wait.TimeoutMS != nil {
		timeout = time.Duration(*p.Wait.TimeoutMS) * time.Millisecond
	}
	// Watch before prompt changes the status, so no change is missed.
	w := &watcher{ch: make(chan herdr.Status, 64)}
	var target struct {
		Target string `json:"target"`
	}
	_ = json.Unmarshal(raw, &target)
	if a, eb := s.target(target.Target); eb == nil {
		w.pane = a.PaneID
	}
	s.watchers[w] = struct{}{}
	before := herdr.StatusUnknown
	if a, err := s.agent(w.pane); err == nil {
		before = a.AgentStatus
	}
	_, eb := s.prompt(raw)
	stall := s.stall
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.watchers, w)
		s.mu.Unlock()
	}()
	if eb != nil {
		return nil, eb
	}
	return s.waitPrompt(w, before, until, timeout, stall)
}

// waitPrompt waits for a prompted agent: from a non-working state it must
// show working or blocked within the stall window, then reach a status in
// until, all within timeout.
func (s *Server) waitPrompt(w *watcher, before herdr.Status, until map[herdr.Status]bool, timeout, stall time.Duration) (json.RawMessage, *errorBody) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	started := before == herdr.StatusWorking
	var stallC <-chan time.Time
	if !started {
		t := time.NewTimer(stall)
		defer t.Stop()
		stallC = t.C
	}
	result := func() (json.RawMessage, *errorBody) {
		s.mu.Lock()
		defer s.mu.Unlock()
		a, err := s.agent(w.pane)
		if err != nil {
			return nil, &errorBody{Code: herdr.CodeAgentNotFound, Message: "agent target " + w.pane + " not found"}
		}
		return s.marshal(map[string]any{"type": "agent_prompted", "agent": a})
	}
	if started && until[herdr.StatusWorking] {
		return result()
	}
	for {
		select {
		case st := <-w.ch:
			if !started && (st == herdr.StatusWorking || st == herdr.StatusBlocked) {
				started, stallC = true, nil
			}
			if started && until[st] {
				return result()
			}
		case <-stallC:
			return nil, &errorBody{Code: herdr.CodeAgentPromptStalled, Message: "agent " + w.pane + " did not start working after the prompt"}
		case <-deadline.C:
			return nil, &errorBody{Code: herdr.CodeTimeout, Message: "timed out waiting for agent " + w.pane}
		case <-s.done:
			return nil, &errorBody{Code: "internal", Message: "fake-herdr is stopping"}
		}
	}
}

// trustKeys answers the trust dialog, whose options are unnumbered: up and
// down move the selection, enter picks it. "Yes, I trust this folder" leaves
// the agent idle under the same name (seen 2026-11-22); "No, exit" (the
// default) and esc end Claude Code, so the pane is a shell again. Unverified:
// what esc does; the event herdr emits when the agent leaves (the fake
// sends pane_agent_detected with released); and that "Yes" clears
// launch_pending and sets interactive_ready, as a ready start does.
// Callers hold s.mu.
func (s *Server) trustKeys(a *herdr.Agent, keys []string) {
	for _, k := range keys {
		sel, ok := s.trust[a.PaneID]
		if !ok {
			return
		}
		switch k {
		case "down":
			s.trust[a.PaneID] = 1
		case "up":
			s.trust[a.PaneID] = 0
		case "enter":
			delete(s.trust, a.PaneID)
			if sel == 1 {
				a.LaunchPending, a.InteractiveReady = false, true
				s.setStatus(a, herdr.StatusIdle)
				return
			}
			s.release(a)
			return
		case "esc", "escape", "ctrl+c":
			delete(s.trust, a.PaneID)
			s.release(a)
			return
		}
	}
}

// release turns an agent pane back into a shell pane. Callers hold s.mu.
func (s *Server) release(a *herdr.Agent) {
	delete(s.screens, a.PaneID)
	pane := herdr.Pane{
		PaneID: a.PaneID, TabID: a.TabID, WorkspaceID: a.WorkspaceID, TerminalID: a.TerminalID,
		AgentStatus: herdr.StatusUnknown, Cwd: a.Cwd, ForegroundCwd: a.ForegroundCwd, Revision: a.Revision,
	}
	final := a.AgentStatus
	i := slices.IndexFunc(s.fx.Agents, func(x herdr.Agent) bool { return x.PaneID == a.PaneID })
	s.fx.Agents = slices.Delete(s.fx.Agents, i, i+1)
	s.shells = append(s.shells, pane)
	s.rollUp(pane.TabID, pane.WorkspaceID)
	s.emitGlobal(herdr.EventPaneAgentDetected, "pane_agent_detected",
		map[string]any{"pane_id": pane.PaneID, "workspace_id": pane.WorkspaceID, "final_status": final, "released": true})
}
