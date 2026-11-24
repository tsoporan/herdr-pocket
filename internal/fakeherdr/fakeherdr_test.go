package fakeherdr

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/herdr"
)

func tempSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "h.sock")
}

func start(t *testing.T) *Server {
	t.Helper()
	srv, err := Listen(tempSocket(t), Studio())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

// rawConn sends one line and returns a reader for the replies.
func rawConn(t *testing.T, srv *Server, line string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("unix", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, line+"\n"); err != nil {
		t.Fatal(err)
	}
	return conn, bufio.NewReader(conn)
}

func TestOneRequestPerConnection(t *testing.T) {
	srv := start(t)
	_, r := rawConn(t, srv, `{"id":"1","method":"ping","params":{}}`)
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		ID     string     `json:"id"`
		Result herdr.Pong `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil || resp.ID != "1" || resp.Result.Protocol != 22 {
		t.Fatalf("ping: %s (%v)", line, err)
	}
	if _, err := r.ReadByte(); err != io.EOF {
		t.Fatalf("after the response: got %v, want EOF", err)
	}
}

func TestWriteAfterSubscribeEndsStream(t *testing.T) {
	srv := start(t)
	conn, r := rawConn(t, srv, `{"id":"1","method":"events.subscribe","params":{"subscriptions":[{"type":"pane.updated"}]}}`)
	line, err := r.ReadString('\n')
	if err != nil || line != `{"id":"1","result":{"type":"subscription_started"}}`+"\n" {
		t.Fatalf("ack: %q, %v", line, err)
	}
	if _, err := io.WriteString(conn, `{"id":"2","method":"ping","params":{}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadByte(); err != io.EOF {
		t.Fatalf("after a second request: got %v, want EOF", err)
	}
}

func TestRefusesRealSocket(t *testing.T) {
	path := tempSocket(t)
	t.Setenv("HERDR_SOCKET_PATH", path)
	if srv, err := Listen(path, Studio()); err == nil {
		srv.Close()
		t.Fatal("Listen on $HERDR_SOCKET_PATH: want an error")
	}
}

func TestScript(t *testing.T) {
	steps, err := ParseScript([]byte(`[
		{"after": "10ms", "pane_id": "w1:p2", "status": "blocked"},
		{"after": "20ms", "pane_id": "w1:p2", "title": "Waiting"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	srv := start(t)
	if err := srv.Run(context.Background(), steps, nil); err != nil {
		t.Fatal(err)
	}
	a, _ := srv.agent("w1:p2")
	if a.AgentStatus != herdr.StatusBlocked || a.TerminalTitleStripped != "Waiting" {
		t.Fatalf("after script: %+v", a)
	}
	if srv.fx.Workspaces[0].AgentStatus != herdr.StatusBlocked {
		t.Errorf("workspace roll-up: %s", srv.fx.Workspaces[0].AgentStatus)
	}

	for _, bad := range []string{
		`[{"after": "1s", "status": "blocked"}]`,
		`[{"after": "1s", "pane_id": "w1:p1"}]`,
		`[{"after": "1s", "pane_id": "w1:p1", "status": "asleep"}]`,
		`[{"after": "2s", "pane_id": "w1:p1", "status": "idle"}, {"after": "1s", "pane_id": "w1:p1", "status": "done"}]`,
		`[{"after": 5, "pane_id": "w1:p1", "status": "idle"}]`,
	} {
		if _, err := ParseScript([]byte(bad)); err == nil {
			t.Errorf("ParseScript(%s): want an error", bad)
		}
	}
}

func TestDemoScriptParses(t *testing.T) {
	data, err := os.ReadFile("testdata/demo-script.json")
	if err != nil {
		t.Fatal(err)
	}
	steps, err := ParseScript(data)
	if err != nil {
		t.Fatal(err)
	}
	srv := start(t)
	for _, st := range steps {
		if st.PaneID == "" {
			continue // a start step
		}
		if _, err := srv.agent(st.PaneID); err != nil {
			t.Error(err)
		}
	}
}

// Every pane-specific screen must name a fixture pane and a real status, or
// a typo would silently fall back to the generic screen.
func TestScreenNames(t *testing.T) {
	srv := start(t)
	entries, err := screenFS.ReadDir("testdata/screens")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		base := strings.TrimSuffix(e.Name(), ".txt")
		if base == "trust.blocked" {
			continue // loaded by name for a StartNotReady pane
		}
		pane, status, ok := strings.Cut(base, ".")
		if !ok {
			status, pane = base, ""
		}
		if !validStatus[herdr.Status(status)] {
			t.Errorf("%s: unknown status %q", e.Name(), status)
		}
		if pane != "" {
			if _, err := srv.agent(strings.Replace(pane, "-", ":", 1)); err != nil {
				t.Errorf("%s: %v", e.Name(), err)
			}
		}
	}
	for _, a := range Studio().Agents {
		if srv.screen(&a) == "" {
			t.Errorf("%s (%s) has no screen", a.PaneID, a.AgentStatus)
		}
	}
}

// focusEvents subscribes to the three focused types and returns what
// SetFocus(pane) emits, read until a quiet 200 ms.
func focusEvents(t *testing.T, srv *Server, pane string) []herdr.Event {
	t.Helper()
	c := herdr.NewClient(srv.Addr())
	stream, err := c.Subscribe(t.Context(), []herdr.Subscription{
		{Type: herdr.EventPaneFocused}, {Type: herdr.EventTabFocused}, {Type: herdr.EventWorkspaceFocused},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err := srv.SetFocus(pane); err != nil {
		t.Fatal(err)
	}
	evs := make(chan herdr.Event, 8)
	go func() {
		for {
			ev, err := stream.Next()
			if err != nil {
				close(evs)
				return
			}
			evs <- ev
		}
	}()
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

func kinds(evs []herdr.Event) string {
	var k []string
	for _, ev := range evs {
		k = append(k, ev.Kind)
	}
	return strings.Join(k, " ")
}

func TestSetFocusEmitsFocusedEvents(t *testing.T) {
	srv := start(t)
	// w1:p2 is focused in Studio; w1:p1 is another tab of the same workspace.
	evs := focusEvents(t, srv, "w1:p1")
	if got := kinds(evs); got != "tab.focused pane.focused" {
		t.Fatalf("same workspace: %q", got)
	}
	var ref herdr.PaneRef
	if err := evs[1].Decode(&ref); err != nil || ref.PaneID != "w1:p1" || ref.WorkspaceID != "w1" {
		t.Fatalf("pane.focused %s: %+v %v", evs[1].Data, ref, err)
	}
	if got := kinds(focusEvents(t, srv, "w2:p1")); got != "workspace.focused tab.focused pane.focused" {
		t.Fatalf("another workspace: %q", got)
	}
	if got := kinds(focusEvents(t, srv, "w2:p1")); got != "" {
		t.Fatalf("already focused: %q", got)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for _, a := range srv.fx.Agents {
		if a.Focused != (a.PaneID == "w2:p1") {
			t.Errorf("%s focused %t", a.PaneID, a.Focused)
		}
	}
	for _, w := range srv.fx.Workspaces {
		if w.Focused != (w.WorkspaceID == "w2") || (w.Focused && w.ActiveTabID != "w2:t1") {
			t.Errorf("workspace %+v", w)
		}
	}
	for _, tb := range srv.fx.Tabs {
		if tb.Focused != (tb.TabID == "w2:t1") {
			t.Errorf("tab %s focused %t", tb.TabID, tb.Focused)
		}
	}
}

func TestScriptFocus(t *testing.T) {
	steps, err := ParseScript([]byte(`[{"after": "1ms", "pane_id": "w3:p1", "focus": true}]`))
	if err != nil {
		t.Fatal(err)
	}
	srv := start(t)
	if err := srv.Run(context.Background(), steps, nil); err != nil {
		t.Fatal(err)
	}
	a, _ := srv.agent("w3:p1")
	if !a.Focused || a.AgentStatus != herdr.StatusDone {
		t.Fatalf("after a focus step: %+v (focus alone must not mark it seen)", a)
	}
}

// A prompt starts a turn and SetStatus ends it, so a test can drive an
// agent idle, working, idle; subscribers see both changes. With SetTurn the
// turn ends by itself.
func TestPromptTurn(t *testing.T) {
	srv := start(t)
	c := herdr.NewClient(srv.Addr())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := c.Subscribe(ctx, []herdr.Subscription{herdr.AgentStatusSubscription("w1:p3")})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	next := func(want herdr.Status) {
		t.Helper()
		ev, err := stream.Next()
		if err != nil {
			t.Fatal(err)
		}
		var p herdr.AgentStatusChanged
		if err := ev.Decode(&p); err != nil || p.PaneID != "w1:p3" || p.AgentStatus != want {
			t.Fatalf("event %+v (%v), want w1:p3 %s", p, err, want)
		}
	}

	if err := c.Prompt(ctx, "w1:p3", "invented first task"); err != nil {
		t.Fatal(err)
	}
	next(herdr.StatusWorking)
	if err := srv.SetStatus("w1:p3", herdr.StatusIdle); err != nil {
		t.Fatal(err)
	}
	next(herdr.StatusIdle)

	srv.SetTurn(20 * time.Millisecond)
	if err := c.Prompt(ctx, "w1:p3", "invented second task"); err != nil {
		t.Fatal(err)
	}
	next(herdr.StatusWorking)
	next(herdr.StatusIdle)

	// A status change during the turn wins over the timer.
	srv.SetTurn(50 * time.Millisecond)
	if err := c.Prompt(ctx, "w1:p3", "invented third task"); err != nil {
		t.Fatal(err)
	}
	next(herdr.StatusWorking)
	if err := srv.SetStatus("w1:p3", herdr.StatusBlocked); err != nil {
		t.Fatal(err)
	}
	next(herdr.StatusBlocked)
	time.Sleep(100 * time.Millisecond)
	srv.mu.Lock()
	a, _ := srv.agent("w1:p3")
	st := a.AgentStatus
	srv.mu.Unlock()
	if st != herdr.StatusBlocked {
		t.Fatalf("after the turn timer: %s, want blocked", st)
	}
}
