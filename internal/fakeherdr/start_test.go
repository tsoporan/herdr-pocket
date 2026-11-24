package fakeherdr

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/answer"
	"github.com/tsoporan/herdr-pocket/internal/herdr"
)

// Everything here talks to fake-herdr on a temp socket; names are invented.

func code(err error) string {
	var herr *herdr.Error
	if errors.As(err, &herr) {
		return herr.Code
	}
	return ""
}

func ctx5(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// collect subscribes to subs, runs act and returns the events it causes,
// read until a quiet 200 ms.
func collect(t *testing.T, srv *Server, subs []herdr.Subscription, act func()) []herdr.Event {
	t.Helper()
	stream, err := herdr.NewClient(srv.Addr()).Subscribe(ctx5(t), subs)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	evs := make(chan herdr.Event, 16)
	go func() {
		defer close(evs)
		for {
			ev, err := stream.Next()
			if err != nil {
				return
			}
			evs <- ev
		}
	}()
	act()
	var got []herdr.Event
	for {
		select {
		case ev, ok := <-evs:
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-time.After(200 * time.Millisecond):
			return got
		}
	}
}

func paramKeys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return slices.Sorted(maps.Keys(m))
}

func inputs(srv *Server, method string) []Input {
	var out []Input
	for _, in := range srv.InputLog() {
		if in.Method == method {
			out = append(out, in)
		}
	}
	return out
}

func TestCreateTab(t *testing.T) {
	srv := start(t)
	c := herdr.NewClient(srv.Addr())
	var tab herdr.Tab
	var pane herdr.Pane
	evs := collect(t, srv, herdr.GlobalSubscriptions(), func() {
		var err error
		if tab, pane, err = c.CreateTab(ctx5(t), "w1", "", "fix-login"); err != nil {
			t.Fatal(err)
		}
	})
	// w1's active tab is w1:t2, whose agent works in /home/dev/src/api.
	if tab.TabID != "w1:t4" || tab.Number != 4 || tab.Label != "fix-login" || tab.Focused || tab.PaneCount != 1 ||
		pane.PaneID != "w1:p4" || pane.TabID != "w1:t4" || pane.Cwd != "/home/dev/src/api" ||
		pane.AgentStatus != herdr.StatusUnknown || pane.Agent != "" || pane.TerminalID == "" {
		t.Fatalf("tab.create: %+v %+v", tab, pane)
	}
	if got := kinds(evs); got != "tab.created pane.created" {
		t.Fatalf("events: %q", got)
	}
	var te herdr.TabEvent
	var pe herdr.PaneEvent
	if evs[0].Decode(&te) != nil || te.Tab == nil || te.Tab.TabID != "w1:t4" ||
		evs[1].Decode(&pe) != nil || pe.Pane.PaneID != "w1:p4" || pe.Pane.Agent != "" {
		t.Fatalf("event payloads: %s %s", evs[0].Data, evs[1].Data)
	}

	// A shell pane is no agent, but its tab is listed and counted.
	agents, _ := c.Agents(ctx5(t))
	if slices.ContainsFunc(agents, func(a herdr.Agent) bool { return a.PaneID == "w1:p4" }) {
		t.Error("a shell pane is in agent.list")
	}
	tabs, _ := c.Tabs(ctx5(t), "w1")
	if len(tabs) != 4 || tabs[3].TabID != "w1:t4" {
		t.Errorf("tab.list: %+v", tabs)
	}
	ws, _ := c.Workspaces(ctx5(t))
	if ws[0].TabCount != 4 || ws[0].PaneCount != 4 {
		t.Errorf("workspace counts: %+v", ws[0])
	}

	// An explicit cwd wins; ids keep counting.
	if _, pane, err := c.CreateTab(ctx5(t), "w1", "/home/dev/src/api/v2", "second"); err != nil || pane.PaneID != "w1:p5" || pane.Cwd != "/home/dev/src/api/v2" {
		t.Fatalf("second tab: %+v %v", pane, err)
	}
	if _, _, err := c.CreateTab(ctx5(t), "w9", "", "nowhere"); code(err) != herdr.CodeWorkspaceNotFound {
		t.Fatalf("unknown workspace: %v", err)
	}

	in := inputs(srv, "tab.create")
	if len(in) != 2 || in[0].PaneID != "w1:p4" || in[0].Label != "fix-login" || in[0].Cwd != "/home/dev/src/api" {
		t.Fatalf("logged: %+v", in)
	}
	if got := strings.Join(paramKeys(t, in[0].Params), " "); got != "focus label workspace_id" {
		t.Errorf("tab.create params: %s", got)
	}
}

// With no pane in the active tab, the tab opens in the fake's own folder.
func TestCreateTabOwnFolder(t *testing.T) {
	srv, err := Listen(tempSocket(t), Fixture{Workspaces: []herdr.Workspace{{WorkspaceID: "w1", Label: "empty"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	wd, _ := os.Getwd()
	_, pane, err := herdr.NewClient(srv.Addr()).CreateTab(ctx5(t), "w1", "", "x")
	if err != nil || pane.Cwd != wd || pane.PaneID != "w1:p1" {
		t.Fatalf("%+v %v, want cwd %s", pane, err, wd)
	}
}

// Pane ids count like herdr's: w1:p9, then w1:pA.
func TestNextID(t *testing.T) {
	if got := nextID("w1", "p", []string{"w1:p9", "w2:pF", "w1:p3"}); got != "w1:pA" {
		t.Errorf("after w1:p9: %s", got)
	}
	if got := nextID("w2", "t", nil); got != "w2:t1" {
		t.Errorf("first: %s", got)
	}
}

// shellPane opens a tab in w2 and returns its root pane.
func shellPane(t *testing.T, c *herdr.Client) string {
	t.Helper()
	_, pane, err := c.CreateTab(ctx5(t), "w2", "", "job")
	if err != nil {
		t.Fatal(err)
	}
	return pane.PaneID
}

func TestStartReady(t *testing.T) {
	srv := start(t)
	c := herdr.NewClient(srv.Addr())
	pane := shellPane(t, c)
	var a herdr.Agent
	evs := collect(t, srv, herdr.GlobalSubscriptions(), func() {
		var err error
		if a, err = c.StartAgent(ctx5(t), "fix-login", "claude", pane, 30*time.Second); err != nil {
			t.Fatal(err)
		}
	})
	if a.PaneID != pane || a.Name != "fix-login" || a.Agent != "claude" || a.AgentStatus != herdr.StatusIdle || a.Cwd != "/home/dev/src/infra" ||
		a.LaunchPending || !a.InteractiveReady {
		t.Fatalf("agent.wait: %+v", a)
	}
	if srv.WaitCount() != 1 {
		t.Errorf("agent.wait sent %d times", srv.WaitCount())
	}
	// Nothing on the stream says startup ended: pane.updated still says unknown.
	if got := kinds(evs); got != "pane.agent_detected pane.updated" {
		t.Fatalf("events: %q", got)
	}
	var det herdr.AgentDetected
	var pe herdr.PaneEvent
	if evs[0].Decode(&det) != nil || det.PaneID != pane || det.Agent != "claude" || det.Released ||
		evs[1].Decode(&pe) != nil || pe.Pane.Agent != "claude" || pe.Pane.AgentStatus != herdr.StatusUnknown {
		t.Fatalf("payloads: %s %s", evs[0].Data, evs[1].Data)
	}
	agents, _ := c.Agents(ctx5(t))
	i := slices.IndexFunc(agents, func(x herdr.Agent) bool { return x.PaneID == pane })
	if i < 0 || agents[i].Name != "fix-login" || agents[i].AgentStatus != herdr.StatusIdle {
		t.Fatalf("agent.list: %+v", agents)
	}
	in := inputs(srv, "agent.start")
	if len(in) != 1 || in[0].PaneID != pane || in[0].Name != "fix-login" || in[0].Kind != "claude" {
		t.Fatalf("logged: %+v", in)
	}
	if got := strings.Join(paramKeys(t, in[0].Params), " "); got != "kind name pane_id timeout_ms" {
		t.Errorf("agent.start params: %s", got)
	}
	// The agent can be prompted under its name.
	if err := c.Prompt(ctx5(t), "fix-login", "invented task"); err != nil {
		t.Fatal(err)
	}
}

// Each refusal comes before anything is typed, in herdr's order: name
// pattern, live name, the pane, timeout_ms.
func TestStartRefusals(t *testing.T) {
	srv := start(t)
	c := herdr.NewClient(srv.Addr())
	pane := shellPane(t, c)
	other := shellPane(t, c)
	if _, err := c.StartAgent(ctx5(t), "taken", "claude", other, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	before := len(srv.InputLog())
	for _, tc := range []struct {
		name, kind, pane string
		timeout          time.Duration
		want             string
	}{
		{"Bad Name", "claude", "w1:p1", 30 * time.Second, herdr.CodeInvalidAgentName}, // before the busy pane
		{"9lives", "claude", pane, 30 * time.Second, herdr.CodeInvalidAgentName},
		{strings.Repeat("a", 33), "claude", pane, 30 * time.Second, herdr.CodeInvalidAgentName},
		{"taken", "claude", "w1:p1", 30 * time.Second, herdr.CodeAgentNameTaken}, // before the busy pane
		{"fresh", "claude", "w1:p1", 30 * time.Second, herdr.CodeAgentPaneBusy},  // an agent pane
		{"fresh", "claude", other, 30 * time.Second, herdr.CodeAgentPaneBusy},    // now hosts "taken"
		{"fresh", "claude", "w1:p99", 30 * time.Second, herdr.CodePaneNotFound},
		{"fresh", "claude", pane, 3 * time.Second, herdr.CodeInvalidRequest},
		{"fresh", "claude", pane, 301 * time.Second, herdr.CodeInvalidRequest},
		{"fresh", "nonesuch", pane, 30 * time.Second, herdr.CodeInvalidRequest},
	} {
		if _, err := c.StartAgent(ctx5(t), tc.name, tc.kind, tc.pane, tc.timeout); code(err) != tc.want {
			t.Errorf("start %q %s in %s (%s): %v, want %s", tc.name, tc.kind, tc.pane, tc.timeout, err, tc.want)
		}
	}
	if got := srv.InputLog()[before:]; len(got) != 0 {
		t.Errorf("refusals were logged: %+v", got)
	}
	if n := srv.WaitCount(); n != 1 { // the first start's only
		t.Errorf("agent.wait sent %d times; a refused agent.start sends none", n)
	}
}

// agent.start answers at once, as herdr does over the socket: the agent is
// listed under its name, unknown and launch pending, so its pane and name
// are taken before it is up.
func TestStartAnswersAtOnce(t *testing.T) {
	srv := start(t)
	c := herdr.NewClient(srv.Addr())
	pane, other := shellPane(t, c), shellPane(t, c)
	srv.SetStartDelay(time.Minute)
	var r struct {
		Type  string      `json:"type"`
		Agent herdr.Agent `json:"agent"`
		Argv  []string    `json:"argv"`
	}
	began := time.Now()
	params := map[string]any{"name": "slow", "kind": "claude", "pane_id": pane, "timeout_ms": 30000}
	if err := c.Call(ctx5(t), "agent.start", params, &r); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > time.Second {
		t.Errorf("agent.start took %s; herdr answers at once", took)
	}
	if r.Type != "agent_started" || r.Agent.PaneID != pane || r.Agent.Name != "slow" || r.Agent.AgentStatus != herdr.StatusUnknown ||
		!r.Agent.LaunchPending || r.Agent.InteractiveReady || r.Agent.StateChangeSeq != 0 || !slices.Equal(r.Argv, []string{"claude"}) {
		t.Fatalf("agent_started: %+v", r)
	}
	agents, _ := c.Agents(ctx5(t))
	if i := slices.IndexFunc(agents, func(x herdr.Agent) bool { return x.PaneID == pane }); i < 0 || agents[i].Name != "slow" || !agents[i].LaunchPending {
		t.Fatalf("agent.list: %+v", agents)
	}
	if _, err := c.StartAgent(ctx5(t), "slow", "claude", other, 30*time.Second); code(err) != herdr.CodeAgentNameTaken {
		t.Errorf("same name meanwhile: %v", err)
	}
	if _, err := c.StartAgent(ctx5(t), "quick", "claude", pane, 30*time.Second); code(err) != herdr.CodeAgentPaneBusy {
		t.Errorf("same pane meanwhile: %v", err)
	}
	if srv.WaitCount() != 0 {
		t.Errorf("agent.wait sent %d times", srv.WaitCount())
	}
}

// agent.wait answers as soon as the status is in until, at once when it
// already is, and times out with herdr's message otherwise.
func TestAgentWait(t *testing.T) {
	srv := start(t)
	c := herdr.NewClient(srv.Addr())
	wait := func(target string, until []herdr.Status, ms int) (herdr.Agent, error) {
		var r struct {
			Type  string      `json:"type"`
			Agent herdr.Agent `json:"agent"`
		}
		err := c.Call(ctx5(t), "agent.wait", map[string]any{"target": target, "until": until, "timeout_ms": ms}, &r)
		if err == nil && r.Type != "agent_info" {
			t.Errorf("type %q", r.Type)
		}
		return r.Agent, err
	}
	// w1:p1 is blocked in the studio fixture; by name or pane.
	if a, err := wait("w1:p1", []herdr.Status{herdr.StatusBlocked}, 5000); err != nil || a.PaneID != "w1:p1" {
		t.Fatalf("already blocked: %+v %v", a, err)
	}
	time.AfterFunc(50*time.Millisecond, func() { _ = srv.SetStatus("w1:p3", herdr.StatusDone) })
	if a, err := wait("w1:p3", []herdr.Status{herdr.StatusDone}, 5000); err != nil || a.AgentStatus != herdr.StatusDone {
		t.Fatalf("until done: %+v %v", a, err)
	}
	_, err := wait("w1:p3", []herdr.Status{herdr.StatusWorking}, 100)
	var herr *herdr.Error
	if !errors.As(err, &herr) || herr.Code != herdr.CodeTimeout || herr.Message != "timed out waiting for agent status" {
		t.Fatalf("timeout: %v", err)
	}
	if _, err := wait("w1:p99", []herdr.Status{herdr.StatusIdle}, 100); code(err) != herdr.CodeAgentNotFound {
		t.Errorf("unknown target: %v", err)
	}
	if _, err := wait("w1:p3", []herdr.Status{"asleep"}, 100); code(err) != herdr.CodeInvalidRequest {
		t.Errorf("bad status: %v", err)
	}
	if n := srv.WaitCount(); n != 5 {
		t.Errorf("WaitCount %d, want 5", n)
	}
}

func TestStartNotReady(t *testing.T) {
	srv := start(t)
	c := herdr.NewClient(srv.Addr())
	srv.SetStartOutcome("claude", StartNotReady)
	pane := shellPane(t, c)
	evs := collect(t, srv, herdr.GlobalSubscriptions(), func() {
		if _, err := c.StartAgent(ctx5(t), "untrusted", "claude", pane, 30*time.Second); code(err) != herdr.CodeAgentNotReady {
			t.Fatalf("start: %v, want %s", err, herdr.CodeAgentNotReady)
		}
	})
	if got := kinds(evs); got != "pane.agent_detected pane.updated" {
		t.Fatalf("events: %q", got)
	}
	if a := agentOf(t, c, pane); a.AgentStatus != herdr.StatusBlocked || !a.LaunchPending {
		t.Fatalf("agent %+v, want blocked and launch pending", a)
	}
	r, err := c.Read(ctx5(t), "untrusted", herdr.SourceVisible, 0)
	if err != nil || !strings.Contains(r.Text, "Accessing workspace:") || !strings.Contains(r.Text, "/home/dev/src/infra") ||
		!strings.Contains(r.Text, "❯ No, exit") {
		t.Fatalf("screen: %q %v", r.Text, err)
	}
	// The name stays the agent's; a prompt is refused while it asks.
	other := shellPane(t, c)
	if _, err := c.StartAgent(ctx5(t), "untrusted", "codex", other, 30*time.Second); code(err) != herdr.CodeAgentNameTaken {
		t.Errorf("name while not ready: %v", err)
	}
	if err := c.Prompt(ctx5(t), "untrusted", "invented task"); code(err) != herdr.CodeAgentBlocked {
		t.Errorf("prompt while not ready: %v", err)
	}
	// A digit does nothing on the unnumbered dialog; Down + Enter trusts it.
	if err := c.SendKeys(ctx5(t), "untrusted", []string{"2"}); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, c, pane); st != herdr.StatusBlocked {
		t.Fatalf("after a digit: %s", st)
	}
	if err := c.SendKeys(ctx5(t), "untrusted", []string{"down", "enter"}); err != nil {
		t.Fatal(err)
	}
	if a := agentOf(t, c, pane); a.AgentStatus != herdr.StatusIdle || a.LaunchPending || !a.InteractiveReady {
		t.Fatalf("after down, enter: %+v, want idle and ready", a)
	}
	if err := c.Prompt(ctx5(t), "untrusted", "invented task"); err != nil {
		t.Fatal(err)
	}
}

// Enter on the default "No, exit" ends the agent: the pane is a shell again.
func TestTrustRefused(t *testing.T) {
	srv := start(t)
	c := herdr.NewClient(srv.Addr())
	srv.SetStartOutcome("claude", StartNotReady)
	pane := shellPane(t, c)
	if _, err := c.StartAgent(ctx5(t), "untrusted", "claude", pane, 30*time.Second); code(err) != herdr.CodeAgentNotReady {
		t.Fatal(err)
	}
	evs := collect(t, srv, herdr.GlobalSubscriptions(), func() {
		if err := c.SendKeys(ctx5(t), pane, []string{"enter"}); err != nil {
			t.Fatal(err)
		}
	})
	var det herdr.AgentDetected
	if len(evs) != 1 || evs[0].Decode(&det) != nil || !det.Released || det.PaneID != pane {
		t.Fatalf("events: %q %v", kinds(evs), evs)
	}
	agents, _ := c.Agents(ctx5(t))
	if slices.ContainsFunc(agents, func(a herdr.Agent) bool { return a.PaneID == pane }) {
		t.Fatal("the agent is still listed")
	}
	srv.SetStartOutcome("claude", StartReady)
	if _, err := c.StartAgent(ctx5(t), "untrusted", "claude", pane, 30*time.Second); err != nil {
		t.Fatalf("start again in the shell: %v", err)
	}
}

// A kind whose executable isn't installed: agent.start succeeds, nothing
// comes up, and agent.wait times out. The agent stays listed under its name,
// unknown and launch pending, so the name and the pane stay taken.
func TestStartTimeout(t *testing.T) {
	srv := start(t)
	c := herdr.NewClient(srv.Addr())
	srv.SetStartOutcome("kiro", StartTimeout)
	srv.SetWaitLimit(100 * time.Millisecond)
	pane := shellPane(t, c)
	evs := collect(t, srv, herdr.GlobalSubscriptions(), func() {
		a, err := c.StartAgent(ctx5(t), "kiro-one", "kiro", pane, 30*time.Second)
		if code(err) != herdr.CodeTimeout || a.PaneID != pane {
			t.Fatalf("start: %+v %v, want timeout with the launched agent", a, err)
		}
	})
	if len(evs) != 0 {
		t.Fatalf("events: %q", kinds(evs))
	}
	if len(inputs(srv, "agent.start")) != 1 || srv.WaitCount() != 1 {
		t.Errorf("agent.start logged %d, agent.wait sent %d", len(inputs(srv, "agent.start")), srv.WaitCount())
	}
	if a := agentOf(t, c, pane); a.Name != "kiro-one" || a.AgentStatus != herdr.StatusUnknown || !a.LaunchPending {
		t.Fatalf("after the timeout: %+v", a)
	}
	if _, err := c.StartAgent(ctx5(t), "kiro-one", "claude", shellPane(t, c), 30*time.Second); code(err) != herdr.CodeAgentNameTaken {
		t.Errorf("same name again: %v", err)
	}
	if _, err := c.StartAgent(ctx5(t), "kiro-two", "claude", pane, 30*time.Second); code(err) != herdr.CodeAgentPaneBusy {
		t.Errorf("same pane again: %v", err)
	}
}

func agentOf(t *testing.T, c *herdr.Client, pane string) herdr.Agent {
	t.Helper()
	agents, err := c.Agents(ctx5(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if a.PaneID == pane {
			return a
		}
	}
	t.Fatalf("no agent %s", pane)
	return herdr.Agent{}
}

func statusOf(t *testing.T, c *herdr.Client, pane string) herdr.Status {
	t.Helper()
	agents, err := c.Agents(ctx5(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if a.PaneID == pane {
			return a.AgentStatus
		}
	}
	t.Fatalf("no agent %s", pane)
	return ""
}

func TestPromptWait(t *testing.T) {
	working := []herdr.Status{herdr.StatusWorking, herdr.StatusBlocked}

	t.Run("until working", func(t *testing.T) {
		srv := start(t)
		a, err := herdr.NewClient(srv.Addr()).PromptWait(ctx5(t), "w1:p3", "invented", working, 10*time.Second)
		if err != nil || a.AgentStatus != herdr.StatusWorking {
			t.Fatalf("%+v %v", a, err)
		}
	})
	t.Run("already working", func(t *testing.T) {
		srv := start(t)
		a, err := herdr.NewClient(srv.Addr()).PromptWait(ctx5(t), "w1:p2", "invented", working, 10*time.Second)
		if err != nil || a.AgentStatus != herdr.StatusWorking {
			t.Fatalf("%+v %v", a, err)
		}
	})
	t.Run("whole turn", func(t *testing.T) {
		// The turn is over before the waiter looks; it must still count.
		srv := start(t)
		srv.SetTurn(time.Millisecond)
		a, err := herdr.NewClient(srv.Addr()).PromptWait(ctx5(t), "w1:p3", "invented", nil, 10*time.Second)
		if err != nil || a.AgentStatus != herdr.StatusIdle {
			t.Fatalf("%+v %v", a, err)
		}
	})
	t.Run("stalled", func(t *testing.T) {
		srv := start(t)
		srv.HoldTurns()
		srv.SetStallWindow(50 * time.Millisecond)
		if _, err := herdr.NewClient(srv.Addr()).PromptWait(ctx5(t), "w1:p3", "invented", working, 10*time.Second); code(err) != herdr.CodeAgentPromptStalled {
			t.Fatalf("%v, want stalled", err)
		}
		if len(inputs(srv, "agent.prompt")) != 1 {
			t.Error("a stalled prompt was still typed, so it must be logged")
		}
	})
	t.Run("blocked counts as started", func(t *testing.T) {
		srv := start(t)
		srv.HoldTurns()
		srv.SetStallWindow(time.Second)
		time.AfterFunc(50*time.Millisecond, func() { _ = srv.SetStatus("w1:p3", herdr.StatusBlocked) })
		a, err := herdr.NewClient(srv.Addr()).PromptWait(ctx5(t), "w1:p3", "invented", nil, 10*time.Second)
		if err != nil || a.AgentStatus != herdr.StatusBlocked {
			t.Fatalf("%+v %v", a, err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		srv := start(t) // no SetTurn: the agent works until told
		if _, err := herdr.NewClient(srv.Addr()).PromptWait(ctx5(t), "w1:p3", "invented", []herdr.Status{herdr.StatusDone}, 100*time.Millisecond); code(err) != herdr.CodeTimeout {
			t.Fatalf("%v, want timeout", err)
		}
	})
	t.Run("blocked agent", func(t *testing.T) {
		srv := start(t)
		if _, err := herdr.NewClient(srv.Addr()).PromptWait(ctx5(t), "w1:p1", "invented", working, 10*time.Second); code(err) != herdr.CodeAgentBlocked {
			t.Fatalf("%v, want agent_blocked", err)
		}
		if len(inputs(srv, "agent.prompt")) != 0 {
			t.Error("a refused prompt was logged")
		}
	})
}

// Close ends a wait instead of hanging on it.
func TestCloseEndsWaits(t *testing.T) {
	srv, err := Listen(tempSocket(t), Studio())
	if err != nil {
		t.Fatal(err)
	}
	c := herdr.NewClient(srv.Addr())
	go func() {
		_, _ = c.PromptWait(context.Background(), "w1:p3", "invented", []herdr.Status{herdr.StatusDone}, time.Minute)
	}()
	time.Sleep(50 * time.Millisecond)
	closed := make(chan struct{})
	go func() { srv.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close hung on a waiting prompt")
	}
}

func TestScriptStartSteps(t *testing.T) {
	steps, err := ParseScript([]byte(`[{"after": "0s", "start_delay": "1ms"}, {"after": "0s", "kind": "codex", "start": "not_ready"}]`))
	if err != nil {
		t.Fatal(err)
	}
	srv := start(t)
	if err := srv.Run(context.Background(), steps, nil); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	o, d := srv.outcomes["codex"], srv.startDelay
	srv.mu.Unlock()
	if o != StartNotReady || d != time.Millisecond {
		t.Fatalf("outcome %q delay %s", o, d)
	}
	for _, bad := range []string{
		`[{"after": "0s", "start": "not_ready"}]`,
		`[{"after": "0s", "kind": "claude"}]`,
		`[{"after": "0s", "kind": "claude", "start": "maybe"}]`,
		`[{"after": "0s", "kind": "nonesuch", "start": "ready"}]`,
		`[{"after": "0s", "pane_id": "w1:p1", "start_delay": "1s"}]`,
		`[{"after": "0s", "pane_id": "w1:p1", "status": "idle", "kind": "claude", "start": "ready"}]`,
	} {
		if _, err := ParseScript([]byte(bad)); err == nil {
			t.Errorf("ParseScript(%s): want an error", bad)
		}
	}
}

// The plan's "check, don't change": what the answer sheet makes of the trust
// dialog. Parse finds no numbered options, so it is not a question the sheet
// can answer (the keys menu on the agent view can); DialogOpen sees the
// footer, so nothing is typed into it.
func TestTrustDialogForTheAnswerSheet(t *testing.T) {
	screen := trustScreen("/home/dev/src/infra")
	if got := answer.Parse(screen); got.Kind != answer.KindUnknown {
		t.Errorf("answer.Parse: %+v, want unknown (the parser needs numbered options)", got)
	}
	if !answer.DialogOpen(screen) {
		t.Error("answer.DialogOpen: false, want true")
	}
}
