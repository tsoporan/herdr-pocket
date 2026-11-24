package herdr_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/fakeherdr"
	"github.com/tsoporan/herdr-pocket/internal/herdr"
)

// startFake serves the studio fixture on a fresh temp socket. The path is
// kept short because unix socket paths are limited to about 108 bytes.
func startFake(t *testing.T) (*fakeherdr.Server, *herdr.Client) {
	t.Helper()
	dir, err := os.MkdirTemp("", "hp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	srv, err := fakeherdr.Listen(filepath.Join(dir, "h.sock"), fakeherdr.Studio())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv, herdr.NewClient(srv.Addr())
}

func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestListCalls(t *testing.T) {
	_, c := startFake(t)
	ctx := testContext(t)

	agents, err := c.Agents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[herdr.Status]int{}
	for _, a := range agents {
		counts[a.AgentStatus]++
		if a.PaneID == "" || a.TabID == "" || a.WorkspaceID == "" || a.TerminalID == "" || a.Name == "" {
			t.Errorf("agent missing a required field: %+v", a)
		}
	}
	want := map[herdr.Status]int{herdr.StatusBlocked: 2, herdr.StatusDone: 1, herdr.StatusWorking: 4, herdr.StatusIdle: 2}
	if len(agents) != 9 || len(counts) != len(want) {
		t.Fatalf("got %d agents with statuses %v, want 9 with %v", len(agents), counts, want)
	}
	for s, n := range want {
		if counts[s] != n {
			t.Errorf("%s: got %d agents, want %d", s, counts[s], n)
		}
	}
	a := agents[1]
	if a.Name != "Add rate limiting to /v2" || a.TerminalTitleStripped != "Running tests" ||
		a.Agent != "claude" || a.AgentSession == nil || a.Tokens["ctx"] == "" || a.Cwd != "/home/dev/src/api" {
		t.Errorf("unexpected record for %s: %+v", a.PaneID, a)
	}

	workspaces, err := c.Workspaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, w := range workspaces {
		labels = append(labels, w.Label)
	}
	if got := len(labels); got != 4 || labels[0] != "api" || labels[3] != "docs" {
		t.Errorf("workspace labels = %v, want [api infra web docs]", labels)
	}

	tabs, err := c.Tabs(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	apiTabs, err := c.Tabs(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tabs) != 9 || len(apiTabs) != 3 {
		t.Errorf("got %d tabs and %d in w1, want 9 and 3", len(tabs), len(apiTabs))
	}
}

func TestErrorCarriesCode(t *testing.T) {
	_, c := startFake(t)
	ctx := testContext(t)

	err := c.Call(ctx, "pane.split", map[string]string{"pane_id": "w1:p1"}, nil)
	var herr *herdr.Error
	if !errors.As(err, &herr) || herr.Code != herdr.CodeInvalidRequest {
		t.Fatalf("unsupported method: got %v, want a *herdr.Error with code %s", err, herdr.CodeInvalidRequest)
	}

	_, err = c.Subscribe(ctx, []herdr.Subscription{{Type: herdr.EventPaneAgentStatusChanged}})
	if !errors.As(err, &herr) || herr.Code != herdr.CodeInvalidRequest {
		t.Fatalf("status subscription without pane_id: got %v, want code %s", err, herdr.CodeInvalidRequest)
	}

	_, err = c.Subscribe(ctx, []herdr.Subscription{herdr.AgentStatusSubscription("w1:p1"), herdr.AgentStatusSubscription("w9:p9")})
	if !errors.As(err, &herr) || herr.Code != herdr.CodePaneNotFound {
		t.Fatalf("status subscription for a missing pane: got %v, want code %s", err, herdr.CodePaneNotFound)
	}
}

func TestProtocolCheck(t *testing.T) {
	srv, c := startFake(t)
	ctx := testContext(t)

	p, err := c.CheckProtocol(ctx)
	if err != nil || p.Protocol != 22 {
		t.Fatalf("protocol 22: got %+v, %v", p, err)
	}

	srv.SetProtocol(21)
	_, err = c.CheckProtocol(ctx)
	if !errors.Is(err, herdr.ErrProtocolTooOld) {
		t.Fatalf("protocol 21: got %v, want ErrProtocolTooOld", err)
	}
	t.Log(err)
}

func TestStatusChangeOnStream(t *testing.T) {
	srv, c := startFake(t)
	ctx := testContext(t)

	subs := append(herdr.GlobalSubscriptions(), herdr.AgentStatusSubscription("w1:p2"))
	stream, err := c.Subscribe(ctx, subs)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	// w2:p2 isn't subscribed, so only the w1:p2 change may arrive.
	if err := srv.SetStatus("w2:p2", herdr.StatusIdle); err != nil {
		t.Fatal(err)
	}
	if err := srv.SetStatus("w1:p2", herdr.StatusBlocked); err != nil {
		t.Fatal(err)
	}
	ev, err := stream.Next()
	if err != nil {
		t.Fatal(err)
	}
	var sc herdr.AgentStatusChanged
	if err := ev.Decode(&sc); err != nil {
		t.Fatal(err)
	}
	if ev.Kind != herdr.EventPaneAgentStatusChanged || sc.PaneID != "w1:p2" || sc.WorkspaceID != "w1" || sc.AgentStatus != herdr.StatusBlocked {
		t.Fatalf("got %s %+v, want w1:p2 blocked", ev.Kind, sc)
	}

	// A title change arrives as a global event, underscore name normalised.
	if err := srv.SetTitle("w1:p2", "Waiting on approval"); err != nil {
		t.Fatal(err)
	}
	ev, err = stream.Next()
	if err != nil {
		t.Fatal(err)
	}
	var pe herdr.PaneEvent
	if err := ev.Decode(&pe); err != nil {
		t.Fatal(err)
	}
	if ev.Kind != herdr.EventPaneUpdated || pe.Pane.PaneID != "w1:p2" || pe.Pane.TerminalTitleStripped != "Waiting on approval" {
		t.Fatalf("got %s %+v, want pane.updated for w1:p2", ev.Kind, pe.Pane)
	}

	agents, err := c.Agents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if agents[1].AgentStatus != herdr.StatusBlocked || agents[1].StateChangeSeq <= 115 {
		t.Errorf("agent.list after change: %+v", agents[1])
	}
}

func TestStreamEndsOnCancel(t *testing.T) {
	_, c := startFake(t)
	ctx, cancel := context.WithCancel(testContext(t))
	stream, err := c.Subscribe(ctx, herdr.GlobalSubscriptions())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	cancel()
	if _, err := stream.Next(); err == nil {
		t.Fatal("Next after cancel: want an error")
	}
}

// A socket that accepts and never answers must fail requests, not hang them.
func TestSilentHerdrTimesOut(t *testing.T) {
	defer herdr.SetRequestTimeout(100 * time.Millisecond)()
	dir, err := os.MkdirTemp("", "hp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		var conns []net.Conn
		for {
			conn, err := ln.Accept()
			if err != nil {
				for _, c := range conns {
					c.Close()
				}
				return
			}
			conns = append(conns, conn)
		}
	}()
	c := herdr.NewClient(ln.Addr().String())
	ctx := testContext(t)
	if _, err := c.Ping(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Ping: %v, want a deadline error", err)
	}
	if _, err := c.Subscribe(ctx, herdr.GlobalSubscriptions()); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("Subscribe: %v, want a deadline error", err)
	}
}

// The ack deadline must not outlive the ack: a quiet stream stays open.
func TestQuietStreamOutlivesTimeout(t *testing.T) {
	defer herdr.SetRequestTimeout(50 * time.Millisecond)()
	srv, c := startFake(t)
	stream, err := c.Subscribe(testContext(t), []herdr.Subscription{herdr.AgentStatusSubscription("w1:p2")})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	time.Sleep(150 * time.Millisecond)
	if err := srv.SetStatus("w1:p2", herdr.StatusBlocked); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Next(); err != nil {
		t.Fatalf("Next after a quiet spell: %v", err)
	}
}

func TestResolveSocket(t *testing.T) {
	// Fake paths only; nothing here dials.
	t.Setenv("HERDR_SOCKET_PATH", "/nonexistent/env.sock")
	t.Setenv("HOME", "/nonexistent/home")
	for _, tc := range []struct{ flag, env, want string }{
		{"/nonexistent/flag.sock", "/nonexistent/env.sock", "/nonexistent/flag.sock"},
		{"", "/nonexistent/env.sock", "/nonexistent/env.sock"},
		{"", "", "/nonexistent/home/.config/herdr/herdr.sock"},
	} {
		t.Setenv("HERDR_SOCKET_PATH", tc.env)
		got, err := herdr.ResolveSocket(tc.flag)
		if err != nil || got != tc.want {
			t.Errorf("ResolveSocket(%q) with env %q = %q, %v; want %q", tc.flag, tc.env, got, err, tc.want)
		}
	}
}
