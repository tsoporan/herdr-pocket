// Package fakeherdr serves herdr's socket protocol from a fixture, for tests,
// UI work and screenshots. It mirrors the real server's connection rules
// (probed on herdr 0.9.5, protocol 22):
//
//   - one request per connection: one response line, then the connection closes;
//   - events.subscribe answers subscription_started and then only streams;
//     anything the client writes after that ends the stream;
//   - unknown methods and a pane.agent_status_changed subscription without
//     pane_id get invalid_request; one naming a pane that doesn't exist gets
//     pane_not_found, which fails the whole subscribe.
//
// It serves ping, agent.list, workspace.list, tab.list and events.subscribe,
// plus agent.read and the input methods (agent.prompt, agent.send_keys,
// pane.send_text, agent.rename, agent.focus; see input.go), and tab.create
// and agent.start (see start.go). Tests drive changes with SetStatus,
// SetTitle, SetFocus and SetScreen, set what agent.start does with
// SetStartOutcome and SetStartDelay, and read what was sent with InputLog. A
// prompt starts a turn (idle or done to working), unless HoldTurns;
// SetStatus, or SetTurn's timer, ends it. A tab.create root pane is a shell
// pane: it isn't in agent.list until agent.start puts an agent in it.
// cmd/fake-herdr replays changes from a script (see Step).
package fakeherdr

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/herdr"
)

// Fixture is the state the fake serves.
type Fixture struct {
	Workspaces []herdr.Workspace `json:"workspaces"`
	Tabs       []herdr.Tab       `json:"tabs"`
	Agents     []herdr.Agent     `json:"agents"`
}

//go:embed testdata/studio.json
var studioJSON []byte

// Studio returns the mockup fixture: nine agents in workspaces api, infra,
// web and docs (2 blocked, 1 done, 4 working, 2 idle). w4:p2 is a Codex agent
// with no transcript, so it is terminal-source; the rest are Claude Code
// agents with transcripts under testdata/claude. All names are invented.
func Studio() Fixture {
	var fx Fixture
	if err := json.Unmarshal(studioJSON, &fx); err != nil {
		panic(fmt.Sprintf("fakeherdr: bad embedded fixture: %v", err))
	}
	return fx
}

// Version is the herdr version the fake reports in ping.
const Version = "0.9.5"

// globalTypes are the subscription types that need no pane_id (from the
// protocol 22 Subscription schema).
var globalTypes = map[string]bool{
	"layout.updated": true, "worktree.created": true, "worktree.opened": true, "worktree.removed": true,
	"pane.moved": true, "pane.exited": true,
}

func init() {
	for _, s := range herdr.GlobalSubscriptions() {
		globalTypes[s.Type] = true
	}
}

// Server is a running fake herdr.
type Server struct {
	ln        net.Listener
	wg        sync.WaitGroup
	mu        sync.Mutex
	protocol  int
	fx        Fixture
	seq       uint64
	conns     map[net.Conn]struct{}
	subs      map[*subscriber]struct{}
	screens   map[string]string // SetScreen overrides, by pane id
	sticky    map[string]bool   // KeepBlocked panes: a digit doesn't unblock them
	inputs    []Input
	onInput   func(Input)
	turn      time.Duration // SetTurn; 0: a prompted agent works until SetStatus
	holdTurns bool          // HoldTurns: a prompt doesn't start a turn

	shells     []herdr.Pane            // panes with no agent (tab.create's root panes); not in agent.list
	outcomes   map[string]StartOutcome // SetStartOutcome, by kind; StartReady when unset
	startDelay time.Duration           // SetStartDelay
	stall      time.Duration           // SetStallWindow
	trust      map[string]int          // panes at the trust dialog: the selected option (0 "No, exit", 1 "Yes")
	asks       map[string]*askFlow     // panes in a scripted AskUserQuestion (ask.go)
	watchers   map[*watcher]struct{}   // waiting prompts and agent.waits
	waits      int                     // agent.wait requests served (WaitCount)
	waitLimit  time.Duration           // SetWaitLimit; 0: agent.wait runs to its timeout_ms
	terms      uint64                  // terminal ids handed out
	dir        string                  // the fake's own folder: tab.create's last cwd fallback
	done       chan struct{}           // closed by Close, ending every wait
}

type subscriber struct {
	conn  net.Conn
	types map[string]bool // global types
	panes map[string]bool // panes subscribed for pane.agent_status_changed
}

// Listen serves fx on the unix socket at socketPath. It refuses the real
// herdr socket ($HERDR_SOCKET_PATH or ~/.config/herdr/herdr.sock), and
// removes a stale socket file that nothing answers on.
func Listen(socketPath string, fx Fixture) (*Server, error) {
	if err := refuseRealSocket(socketPath); err != nil {
		return nil, err
	}
	if err := removeStale(socketPath); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("fakeherdr: %w", err)
	}
	s := &Server{
		ln:       ln,
		protocol: herdr.MinProtocol,
		fx:       fx,
		conns:    map[net.Conn]struct{}{},
		subs:     map[*subscriber]struct{}{},
		screens:  map[string]string{},
		sticky:   map[string]bool{},

		outcomes:   map[string]StartOutcome{},
		startDelay: defaultStartDelay,
		stall:      defaultStall,
		trust:      map[string]int{},
		asks:       map[string]*askFlow{},
		watchers:   map[*watcher]struct{}{},
		dir:        "/",
		done:       make(chan struct{}),
	}
	if wd, err := os.Getwd(); err == nil {
		s.dir = wd
	}
	for _, a := range fx.Agents {
		s.seq = max(s.seq, a.StateChangeSeq)
	}
	s.wg.Go(s.accept)
	return s, nil
}

func refuseRealSocket(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("fakeherdr: %w", err)
	}
	real := []string{os.Getenv("HERDR_SOCKET_PATH")}
	if home, err := os.UserHomeDir(); err == nil {
		real = append(real, filepath.Join(home, ".config", "herdr", "herdr.sock"))
	}
	for _, r := range real {
		if r != "" && filepath.Clean(r) == abs {
			return fmt.Errorf("fakeherdr: refusing to listen on the real herdr socket %s; pass a temp path", path)
		}
	}
	return nil
}

func removeStale(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("fakeherdr: %w", err)
	}
	if info.Mode().Type() != os.ModeSocket {
		return fmt.Errorf("fakeherdr: %s exists and is not a socket", path)
	}
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		c.Close()
		return fmt.Errorf("fakeherdr: something is already listening on %s", path)
	}
	return os.Remove(path)
}

// Addr returns the socket path.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// SetProtocol changes the protocol reported by ping, for the refusal test.
func (s *Server) SetProtocol(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.protocol = n
}

// Close stops listening, ends every connection and waits for them.
func (s *Server) Close() error {
	err := s.ln.Close()
	s.mu.Lock()
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return err
}

func (s *Server) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Go(func() {
			defer func() {
				s.mu.Lock()
				delete(s.conns, conn)
				s.mu.Unlock()
				conn.Close()
			}()
			s.handle(conn)
		})
	}
}

type request struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (s *Server) handle(conn net.Conn) {
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		writeLine(conn, map[string]any{"id": "", "error": invalid("invalid request: %v", err)})
		return
	}
	if req.Method == "events.subscribe" {
		s.subscribe(conn, r, req)
		return
	}
	var result json.RawMessage
	var eb *errorBody
	if h, ok := waitingMethods[req.Method]; ok {
		result, eb = h(s, req.Params)
	} else {
		result, eb = s.result(req)
	}
	if eb != nil {
		writeLine(conn, map[string]any{"id": req.ID, "error": eb})
		return
	}
	writeLine(conn, map[string]any{"id": req.ID, "result": result})
}

// result answers a one-shot request. It marshals under the lock so the
// response is a consistent copy of the state.
func (s *Server) result(req request) (json.RawMessage, *errorBody) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var v any
	switch req.Method {
	case "ping":
		v = map[string]any{"type": "pong", "version": Version, "protocol": s.protocol,
			"capabilities": map[string]any{"live_handoff": false}}
	case "agent.list":
		v = map[string]any{"type": "agent_list", "agents": s.fx.Agents}
	case "workspace.list":
		v = map[string]any{"type": "workspace_list", "workspaces": s.fx.Workspaces}
	case "tab.list":
		var p struct {
			WorkspaceID string `json:"workspace_id"`
		}
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &p); err != nil {
				return nil, invalid("invalid request: %v", err)
			}
		}
		tabs := []herdr.Tab{}
		for _, t := range s.fx.Tabs {
			if p.WorkspaceID == "" || t.WorkspaceID == p.WorkspaceID {
				tabs = append(tabs, t)
			}
		}
		v = map[string]any{"type": "tab_list", "tabs": tabs}
	default:
		h, ok := inputMethods[req.Method]
		if !ok {
			return nil, invalid("invalid request: unknown variant `%s` (fake-herdr serves ping, agent.list, workspace.list, tab.list, tab.create, events.subscribe, agent.read, agent.start, agent.wait, agent.prompt, agent.send_keys, pane.send_text, agent.rename, agent.focus)", req.Method)
		}
		var eb *errorBody
		if v, eb = h(s, req.Params); eb != nil {
			return nil, eb
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, &errorBody{Code: "internal", Message: err.Error()}
	}
	return b, nil
}

func (s *Server) subscribe(conn net.Conn, r *bufio.Reader, req request) {
	var p struct {
		Subscriptions *[]herdr.Subscription `json:"subscriptions"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Subscriptions == nil {
		writeLine(conn, map[string]any{"id": req.ID, "error": invalid("invalid request: missing field `subscriptions`")})
		return
	}
	sub := &subscriber{conn: conn, types: map[string]bool{}, panes: map[string]bool{}}
	for _, x := range *p.Subscriptions {
		switch {
		case x.Type == herdr.EventPaneAgentStatusChanged && x.PaneID == "":
			writeLine(conn, map[string]any{"id": req.ID, "error": invalid("invalid request: missing field `pane_id`")})
			return
		case x.Type == herdr.EventPaneAgentStatusChanged:
			if !s.hasPane(x.PaneID) {
				writeLine(conn, map[string]any{"id": req.ID, "error": &errorBody{Code: herdr.CodePaneNotFound, Message: "pane " + x.PaneID + " not found"}})
				return
			}
			sub.panes[x.PaneID] = true
		case globalTypes[x.Type]:
			sub.types[x.Type] = true
		default:
			writeLine(conn, map[string]any{"id": req.ID, "error": invalid("invalid request: unknown or unsupported subscription type `%s`", x.Type)})
			return
		}
	}
	// Register and ack under the lock, so no event can precede the ack and
	// none emitted after Subscribe returns is missed.
	s.mu.Lock()
	s.subs[sub] = struct{}{}
	writeLine(conn, map[string]any{"id": req.ID, "result": map[string]string{"type": "subscription_started"}})
	s.mu.Unlock()

	// Block until the client writes anything or hangs up; either ends the
	// stream, as with real herdr.
	_, _ = r.ReadByte()
	s.mu.Lock()
	delete(s.subs, sub)
	s.mu.Unlock()
}

func invalid(format string, args ...any) *errorBody {
	return &errorBody{Code: herdr.CodeInvalidRequest, Message: fmt.Sprintf(format, args...)}
}

func writeLine(conn net.Conn, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(append(b, '\n')); err != nil {
		conn.Close()
	}
}

func (s *Server) hasPane(paneID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.agent(paneID)
	return err == nil || s.shell(paneID) >= 0
}

// agent finds an agent pane. Callers hold s.mu.
func (s *Server) agent(paneID string) (*herdr.Agent, error) {
	i := slices.IndexFunc(s.fx.Agents, func(a herdr.Agent) bool { return a.PaneID == paneID })
	if i < 0 {
		return nil, fmt.Errorf("fakeherdr: no agent pane %q", paneID)
	}
	return &s.fx.Agents[i], nil
}

// SetStatus changes an agent's status and emits pane.agent_status_changed to
// the streams subscribed to that pane. Like herdr, it emits no pane.updated
// for a status change.
func (s *Server) SetStatus(paneID string, status herdr.Status) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.agent(paneID)
	if err != nil {
		return err
	}
	s.setStatus(a, status)
	return nil
}

// setStatus is SetStatus for a found agent. It also drops any screen set
// with SetScreen, so agent.read shows the new state. Callers hold s.mu.
func (s *Server) setStatus(a *herdr.Agent, status herdr.Status) {
	delete(s.screens, a.PaneID)
	delete(s.asks, a.PaneID)
	s.seq++
	a.AgentStatus = status
	a.StateChangeSeq = s.seq
	if status == herdr.StatusDone {
		a.CompletionSeq = s.seq
	}
	s.rollUp(a.TabID, a.WorkspaceID)
	s.notify(a.PaneID, status)
	ev := herdr.AgentStatusChanged{PaneID: a.PaneID, WorkspaceID: a.WorkspaceID, AgentStatus: status, Agent: a.Agent}
	s.emit(func(sub *subscriber) bool { return sub.panes[a.PaneID] },
		map[string]any{"event": herdr.EventPaneAgentStatusChanged, "data": ev})
}

// SetTitle changes an agent's terminal title and emits pane.updated with the
// full pane record to global pane.updated subscribers. Unverified: real herdr
// does emit pane_updated (seen on 2026-11-20), but no title changed during
// the probe, so that a title change triggers it is assumed.
func (s *Server) SetTitle(paneID, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.agent(paneID)
	if err != nil {
		return err
	}
	a.TerminalTitle = title
	a.TerminalTitleStripped = title
	a.Revision++
	s.emit(func(sub *subscriber) bool { return sub.types[herdr.EventPaneUpdated] },
		map[string]any{"event": "pane_updated", "data": map[string]any{"type": "pane_updated", "pane": paneOf(a)}})
	return nil
}

// statusRank orders statuses for the tab and workspace roll-up: the one that
// most needs the user wins.
var statusRank = map[herdr.Status]int{
	herdr.StatusBlocked: 0, herdr.StatusDone: 1, herdr.StatusWorking: 2, herdr.StatusIdle: 3, herdr.StatusUnknown: 4,
}

// rollUp recomputes agent_status on a tab and workspace from their agents.
func (s *Server) rollUp(tabID, workspaceID string) {
	roll := func(match func(herdr.Agent) bool) herdr.Status {
		best := herdr.StatusUnknown
		for _, a := range s.fx.Agents {
			if match(a) && statusRank[a.AgentStatus] < statusRank[best] {
				best = a.AgentStatus
			}
		}
		return best
	}
	for i := range s.fx.Tabs {
		if s.fx.Tabs[i].TabID == tabID {
			s.fx.Tabs[i].AgentStatus = roll(func(a herdr.Agent) bool { return a.TabID == tabID })
		}
	}
	for i := range s.fx.Workspaces {
		if s.fx.Workspaces[i].WorkspaceID == workspaceID {
			s.fx.Workspaces[i].AgentStatus = roll(func(a herdr.Agent) bool { return a.WorkspaceID == workspaceID })
		}
	}
}

// emit writes one event line to every matching subscriber. Callers hold s.mu.
func (s *Server) emit(match func(*subscriber) bool, line any) {
	for sub := range s.subs {
		if match(sub) {
			writeLine(sub.conn, line)
		}
	}
}

// SetFocus makes an agent's pane the focused one, as a click in a herdr
// client would, and emits pane.focused, plus tab.focused and
// workspace.focused when those change too. It doesn't mark the agent seen
// (agent.focus does).
func (s *Server) SetFocus(paneID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.agent(paneID)
	if err != nil {
		return err
	}
	s.focus(a)
	return nil
}

// focus moves the global focus to a's pane, tab and workspace, and emits a
// focused event for each that changed, in herdr's global wire shape. herdr
// keeps one focused pane (checked read-only on 2026-11-22: agent.list marks
// exactly one agent focused, in the focused tab and workspace). Unverified:
// the order real herdr emits the three in. Callers hold s.mu.
func (s *Server) focus(a *herdr.Agent) {
	paneChanged := !a.Focused
	for i := range s.fx.Agents {
		s.fx.Agents[i].Focused = &s.fx.Agents[i] == a
	}
	tabChanged := false
	for i := range s.fx.Tabs {
		t := &s.fx.Tabs[i]
		focused := t.TabID == a.TabID
		tabChanged = tabChanged || (focused && !t.Focused)
		t.Focused = focused
	}
	wsChanged := false
	for i := range s.fx.Workspaces {
		w := &s.fx.Workspaces[i]
		focused := w.WorkspaceID == a.WorkspaceID
		wsChanged = wsChanged || (focused && !w.Focused)
		w.Focused = focused
		if focused {
			w.ActiveTabID = a.TabID
		}
	}
	global := func(kind, wire string, data map[string]any) {
		data["type"] = wire
		s.emit(func(sub *subscriber) bool { return sub.types[kind] }, map[string]any{"event": wire, "data": data})
	}
	if wsChanged {
		global(herdr.EventWorkspaceFocused, "workspace_focused", map[string]any{"workspace_id": a.WorkspaceID})
	}
	if tabChanged {
		global(herdr.EventTabFocused, "tab_focused", map[string]any{"tab_id": a.TabID, "workspace_id": a.WorkspaceID})
	}
	if paneChanged {
		global(herdr.EventPaneFocused, "pane_focused", map[string]any{"pane_id": a.PaneID, "workspace_id": a.WorkspaceID})
	}
}
