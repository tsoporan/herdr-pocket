package herdr_test

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/fakeherdr"
	"github.com/tsoporan/herdr-pocket/internal/herdr"
)

// keysOf returns the param names a logged tab.create or agent.start sent.
func keysOf(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return strings.Join(slices.Sorted(maps.Keys(m)), " ")
}

func logged(srv *fakeherdr.Server, method string) []fakeherdr.Input {
	var out []fakeherdr.Input
	for _, in := range srv.InputLog() {
		if in.Method == method {
			out = append(out, in)
		}
	}
	return out
}

// CreateTab always sends focus:false, never env, and omits an empty cwd.
func TestCreateTab(t *testing.T) {
	srv, c := startFake(t)
	ctx := testContext(t)
	tab, pane, err := c.CreateTab(ctx, "w2", "", "fix-login")
	if err != nil || tab.WorkspaceID != "w2" || tab.Label != "fix-login" || pane.TabID != tab.TabID || pane.Cwd != "/home/dev/src/infra" {
		t.Fatalf("CreateTab: %+v %+v %v", tab, pane, err)
	}
	if _, pane, err = c.CreateTab(ctx, "w2", "/home/dev/src/infra/sub", "second"); err != nil || pane.Cwd != "/home/dev/src/infra/sub" {
		t.Fatalf("CreateTab with cwd: %+v %v", pane, err)
	}
	in := logged(srv, "tab.create")
	if len(in) != 2 {
		t.Fatalf("logged %+v", in)
	}
	if got := keysOf(t, in[0].Params); got != "focus label workspace_id" {
		t.Errorf("params without cwd: %s", got)
	}
	if got := keysOf(t, in[1].Params); got != "cwd focus label workspace_id" {
		t.Errorf("params with cwd: %s", got)
	}
	var p struct{ Focus *bool }
	if err := json.Unmarshal(in[0].Params, &p); err != nil || p.Focus == nil || *p.Focus {
		t.Errorf("focus: %s", in[0].Params)
	}
	if _, _, err := c.CreateTab(ctx, "w9", "", "x"); errCode(err) != herdr.CodeWorkspaceNotFound {
		t.Errorf("unknown workspace: %v", err)
	}
}

// StartAgent is agent.start, answered at once, then agent.wait for the
// agent to come up, which may take past requestTimeout. It never sends args.
func TestStartAgent(t *testing.T) {
	defer herdr.SetRequestTimeout(50 * time.Millisecond)()
	srv, c := startFake(t)
	srv.SetStartDelay(200 * time.Millisecond)
	ctx := testContext(t)
	_, pane, err := c.CreateTab(ctx, "w1", "", "job")
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.StartAgent(ctx, "fix-login", "claude", pane.PaneID, 30*time.Second)
	if err != nil || a.PaneID != pane.PaneID || a.Name != "fix-login" || a.AgentStatus != herdr.StatusIdle || a.LaunchPending {
		t.Fatalf("StartAgent: %+v %v", a, err)
	}
	in := logged(srv, "agent.start")
	if len(in) != 1 || keysOf(t, in[0].Params) != "kind name pane_id timeout_ms" {
		t.Fatalf("params: %+v", in)
	}
	var p struct {
		TimeoutMS int64 `json:"timeout_ms"`
	}
	if err := json.Unmarshal(in[0].Params, &p); err != nil || p.TimeoutMS != 30000 {
		t.Errorf("timeout_ms: %s", in[0].Params)
	}
	if n := srv.WaitCount(); n != 1 {
		t.Errorf("agent.wait sent %d times", n)
	}

	// Refused by agent.start: no agent back, and no agent.wait.
	for _, tc := range []struct {
		name, pane string
		want       string
	}{
		{"Fix Login", pane.PaneID, herdr.CodeInvalidAgentName},
		{"fix-login", pane.PaneID, herdr.CodeAgentNameTaken},
		{"other", pane.PaneID, herdr.CodeAgentPaneBusy},
	} {
		if a, err := c.StartAgent(ctx, tc.name, "claude", tc.pane, 30*time.Second); errCode(err) != tc.want || a.PaneID != "" {
			t.Errorf("StartAgent %q: %+v %v, want %s and no agent", tc.name, a, err, tc.want)
		}
	}
	if n := srv.WaitCount(); n != 1 {
		t.Errorf("agent.wait sent %d times after refusals, want still 1", n)
	}

	// Blocked at startup: agent_not_ready, made here from agent.wait.
	_, pane, _ = c.CreateTab(ctx, "w1", "", "job2")
	srv.SetStartOutcome("claude", fakeherdr.StartNotReady)
	if a, err := c.StartAgent(ctx, "asks-first", "claude", pane.PaneID, 30*time.Second); errCode(err) != herdr.CodeAgentNotReady ||
		a.PaneID != pane.PaneID || a.AgentStatus != herdr.StatusBlocked {
		t.Errorf("not ready: %+v %v", a, err)
	}

	// Not installed: the wait times out; the agent was launched, so it
	// comes back with the error, and its name stays taken.
	_, pane, _ = c.CreateTab(ctx, "w1", "", "job3")
	srv.SetStartOutcome("kiro", fakeherdr.StartTimeout)
	srv.SetWaitLimit(100 * time.Millisecond)
	if a, err := c.StartAgent(ctx, "no-kiro", "kiro", pane.PaneID, 30*time.Second); errCode(err) != herdr.CodeTimeout || a.PaneID != pane.PaneID {
		t.Errorf("timeout: %+v %v", a, err)
	}
	_, other, _ := c.CreateTab(ctx, "w1", "", "job4")
	if _, err := c.StartAgent(ctx, "no-kiro", "claude", other.PaneID, 30*time.Second); errCode(err) != herdr.CodeAgentNameTaken {
		t.Errorf("name after the timeout: %v", err)
	}
}

// PromptWait sends wait {until, timeout_ms} and waits past requestTimeout.
func TestPromptWait(t *testing.T) {
	defer herdr.SetRequestTimeout(50 * time.Millisecond)()
	srv, c := startFake(t)
	srv.SetTurn(200 * time.Millisecond)
	ctx := testContext(t)

	a, err := c.PromptWait(ctx, "w1:p3", "invented task", nil, 10*time.Second)
	if err != nil || a.AgentStatus != herdr.StatusIdle {
		t.Fatalf("whole turn: %+v %v", a, err)
	}
	a, err = c.PromptWait(ctx, "w1:p3", "invented task", []herdr.Status{herdr.StatusWorking, herdr.StatusBlocked}, 10*time.Second)
	if err != nil || a.AgentStatus != herdr.StatusWorking {
		t.Fatalf("until working: %+v %v", a, err)
	}
	want := []fakeherdr.Input{
		{Method: "agent.prompt", PaneID: "w1:p3", Text: "invented task"},
		{Method: "agent.prompt", PaneID: "w1:p3", Text: "invented task"},
	}
	if got := srv.InputLog(); !equalInputs(got, want) {
		t.Errorf("inputs %+v", got)
	}

	srv.HoldTurns()
	srv.SetStallWindow(30 * time.Millisecond)
	if _, err := c.PromptWait(ctx, "w4:p2", "invented task", nil, 10*time.Second); errCode(err) != herdr.CodeAgentPromptStalled {
		t.Errorf("stalled: %v", err)
	}
	if _, err := c.PromptWait(ctx, "w1:p2", "invented task", []herdr.Status{herdr.StatusDone}, 100*time.Millisecond); errCode(err) != herdr.CodeTimeout {
		t.Errorf("timeout: %v", err)
	}
	if _, err := c.PromptWait(ctx, "w1:p1", "invented task", nil, 10*time.Second); errCode(err) != herdr.CodeAgentBlocked {
		t.Errorf("blocked: %v", err)
	}
}

func TestKinds(t *testing.T) {
	if len(herdr.Kinds) != 24 || !slices.Contains(herdr.Kinds, "claude") || !slices.Contains(herdr.Kinds, "codex") {
		t.Errorf("Kinds: %v", herdr.Kinds)
	}
}
