package herdr_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/tsoporan/herdr-pocket/internal/fakeherdr"
	"github.com/tsoporan/herdr-pocket/internal/herdr"
)

// Every test here talks to fake-herdr on a temp socket; nothing sends input
// to a real herdr.

func errCode(err error) string {
	var herr *herdr.Error
	if errors.As(err, &herr) {
		return herr.Code
	}
	return ""
}

func TestRead(t *testing.T) {
	_, c := startFake(t)
	ctx := testContext(t)

	r, err := c.Read(ctx, "w1:p1", herdr.SourceVisible, 0)
	if err != nil {
		t.Fatal(err)
	}
	if r.PaneID != "w1:p1" || r.TabID != "w1:t1" || r.Source != herdr.SourceVisible || r.Format != "text" ||
		!strings.Contains(r.Text, "Do you want to proceed?") {
		t.Errorf("visible read of w1:p1: %+v", r)
	}

	r, err = c.Read(ctx, "Add rate limiting to /v2", herdr.SourceRecent, 3)
	if err != nil {
		t.Fatal(err)
	}
	if r.PaneID != "w1:p2" || !r.Truncated || strings.Count(r.Text, "\n") != 3 {
		t.Errorf("recent 3-line read by name: %+v", r)
	}

	if _, err := c.Read(ctx, "w9:p9", herdr.SourceVisible, 0); errCode(err) != herdr.CodeAgentNotFound {
		t.Errorf("read of a missing agent: %v, want %s", err, herdr.CodeAgentNotFound)
	}
	if _, err := c.Read(ctx, "w1:p1", "screen", 0); errCode(err) != herdr.CodeInvalidRequest {
		t.Errorf("read with a bad source: %v, want %s", err, herdr.CodeInvalidRequest)
	}
}

func TestPrompt(t *testing.T) {
	srv, c := startFake(t)
	ctx := testContext(t)

	if err := c.Prompt(ctx, "w1:p1", "carry on"); errCode(err) != herdr.CodeAgentBlocked {
		t.Fatalf("prompt to a blocked agent: %v, want %s", err, herdr.CodeAgentBlocked)
	}
	if err := c.Prompt(ctx, "w1:p3", "bump the minor versions"); err != nil {
		t.Fatal(err)
	}
	want := []fakeherdr.Input{{Method: "agent.prompt", PaneID: "w1:p3", Text: "bump the minor versions"}}
	if got := srv.InputLog(); !equalInputs(got, want) {
		t.Errorf("input log = %+v, want %+v (the blocked prompt must not be logged)", got, want)
	}
	if s := status(t, c, "w1:p3"); s != herdr.StatusWorking {
		t.Errorf("idle agent after a prompt: %s, want working", s)
	}
}

func TestSendKeysAnswersBlocked(t *testing.T) {
	srv, c := startFake(t)
	ctx := testContext(t)
	stream, err := c.Subscribe(ctx, []herdr.Subscription{herdr.AgentStatusSubscription("w1:p1")})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	if err := c.SendKeys(ctx, "w1:p1", []string{"1"}); err != nil {
		t.Fatal(err)
	}
	ev, err := stream.Next()
	if err != nil {
		t.Fatal(err)
	}
	var sc herdr.AgentStatusChanged
	if err := ev.Decode(&sc); err != nil || sc.PaneID != "w1:p1" || sc.AgentStatus != herdr.StatusWorking {
		t.Fatalf("after answering: %+v, %v; want w1:p1 working", sc, err)
	}
	r, err := c.Read(ctx, "w1:p1", herdr.SourceVisible, 0)
	if err != nil || strings.Contains(r.Text, "Do you want to proceed?") {
		t.Errorf("screen after answering still shows the prompt (%v): %q", err, r.Text)
	}
	if got := srv.InputLog(); !equalInputs(got, []fakeherdr.Input{{Method: "agent.send_keys", PaneID: "w1:p1", Keys: []string{"1"}}}) {
		t.Errorf("input log = %+v", got)
	}

	// esc dismisses a dialog and interrupts a turn.
	if err := c.SendKeys(ctx, "w2:p1", []string{"esc"}); err != nil {
		t.Fatal(err)
	}
	if err := c.SendKeys(ctx, "w1:p1", []string{"esc"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"w2:p1", "w1:p1"} {
		if s := status(t, c, p); s != herdr.StatusIdle {
			t.Errorf("%s after esc: %s, want idle", p, s)
		}
	}
}

func TestSendKeysRefusesUnknownKeys(t *testing.T) {
	srv, c := startFake(t)
	ctx := testContext(t)

	for _, keys := range [][]string{nil, {"Escape"}, {"1", "return"}, {"ctrl+z"}, {"10"}} {
		if err := c.SendKeys(ctx, "w1:p1", keys); err == nil {
			t.Errorf("SendKeys(%q): want an error", keys)
		}
	}
	// The fake refuses on its own too, so a raw call can't slip past.
	err := c.Call(ctx, "agent.send_keys", map[string]any{"target": "w1:p1", "keys": []string{"1", "f13"}}, nil)
	if errCode(err) != herdr.CodeInvalidRequest {
		t.Errorf("raw send_keys with an unknown key: %v, want %s", err, herdr.CodeInvalidRequest)
	}
	if got := srv.InputLog(); len(got) != 0 {
		t.Errorf("refused keys reached the log: %+v", got)
	}
	if s := status(t, c, "w1:p1"); s != herdr.StatusBlocked {
		t.Errorf("w1:p1 after refused keys: %s, want blocked", s)
	}
}

func TestValidKey(t *testing.T) {
	for _, k := range []string{"esc", "escape", "enter", "ctrl+c", "up", "down", "left", "right", "0", "9"} {
		if !herdr.ValidKey(k) {
			t.Errorf("ValidKey(%q) = false", k)
		}
	}
	for _, k := range []string{"", "Esc", "ESC", "ctrl+C", "a", "10", " 1", "return", "ctrl+c "} {
		if herdr.ValidKey(k) {
			t.Errorf("ValidKey(%q) = true", k)
		}
	}
}

func TestTextRenameFocus(t *testing.T) {
	srv, c := startFake(t)
	ctx := testContext(t)

	if err := c.SendText(ctx, "w1:p2", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := c.SendText(ctx, "w9:p9", "x"); errCode(err) != herdr.CodePaneNotFound {
		t.Errorf("send_text to a missing pane: %v, want %s", err, herdr.CodePaneNotFound)
	}
	if err := c.Rename(ctx, "w1:p2", "ratelimit"); err != nil {
		t.Fatal(err)
	}
	if err := c.Rename(ctx, "w1:p3", "ratelimit"); errCode(err) != herdr.CodeInvalidRequest {
		t.Errorf("rename to a taken name: %v", err)
	}
	if err := c.Rename(ctx, "w1:p3", "Bump deps"); errCode(err) != herdr.CodeInvalidRequest {
		t.Errorf("rename to an invalid name: %v", err)
	}
	if err := c.Focus(ctx, "ratelimit"); err != nil {
		t.Fatal(err)
	}
	if err := c.Focus(ctx, "w3:p1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Rename(ctx, "w1:p2", ""); err != nil {
		t.Fatal(err)
	}

	want := []fakeherdr.Input{
		{Method: "pane.send_text", PaneID: "w1:p2", Text: "hello"},
		{Method: "agent.rename", PaneID: "w1:p2", Name: "ratelimit"},
		{Method: "agent.focus", PaneID: "w1:p2"},
		{Method: "agent.focus", PaneID: "w3:p1"},
		{Method: "agent.rename", PaneID: "w1:p2"},
	}
	if got := srv.InputLog(); !equalInputs(got, want) {
		t.Errorf("input log = %+v\nwant %+v", got, want)
	}
	agents, err := c.Agents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if a.Focused != (a.PaneID == "w3:p1") {
			t.Errorf("%s focused = %v", a.PaneID, a.Focused)
		}
		if a.PaneID == "w3:p1" && a.AgentStatus != herdr.StatusIdle {
			t.Errorf("done agent after focus: %s, want idle (seen)", a.AgentStatus)
		}
		if a.PaneID == "w1:p2" && a.Name != "" {
			t.Errorf("w1:p2 name after clearing: %q", a.Name)
		}
	}
}

func TestSetScreen(t *testing.T) {
	srv, c := startFake(t)
	ctx := testContext(t)
	before, err := c.Read(ctx, "w1:p1", herdr.SourceVisible, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.SetScreen("w1:p1", "something else\n"); err != nil {
		t.Fatal(err)
	}
	after, err := c.Read(ctx, "w1:p1", herdr.SourceVisible, 0)
	if err != nil || after.Text != "something else\n" || after.Revision <= before.Revision {
		t.Errorf("after SetScreen: %+v, %v", after, err)
	}
	// A status change drops the override.
	if err := srv.SetStatus("w1:p1", herdr.StatusWorking); err != nil {
		t.Fatal(err)
	}
	if r, _ := c.Read(ctx, "w1:p1", herdr.SourceVisible, 0); r.Text == "something else\n" {
		t.Error("override survived a status change")
	}
}

func status(t *testing.T, c *herdr.Client, paneID string) herdr.Status {
	t.Helper()
	agents, err := c.Agents(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if a.PaneID == paneID {
			return a.AgentStatus
		}
	}
	t.Fatalf("no agent %s", paneID)
	return ""
}

func equalInputs(a, b []fakeherdr.Input) bool {
	return slices.EqualFunc(a, b, func(x, y fakeherdr.Input) bool {
		return x.Method == y.Method && x.PaneID == y.PaneID && x.Text == y.Text && x.Name == y.Name && slices.Equal(x.Keys, y.Keys)
	})
}
