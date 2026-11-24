package main

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/coder/websocket/wsjson"

	"github.com/tsoporan/herdr-pocket/internal/fakeherdr"
	"github.com/tsoporan/herdr-pocket/internal/hub"
)

func TestParseKinds(t *testing.T) {
	got, err := parseKinds("claude, codex,gemini,kiro")
	want := []hub.StartKind{{Kind: "claude", Label: "Claude Code"}, {Kind: "codex", Label: "Codex"}, {Kind: "gemini", Label: "Gemini CLI"}, {Kind: "kiro", Label: "Kiro"}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("parseKinds %+v, %v", got, err)
	}
	for _, bad := range []string{"", "claude,", "claude,claude", "invented-kind", "Claude"} {
		if _, err := parseKinds(bad); !errors.Is(err, errUsage) {
			t.Errorf("parseKinds(%q): %v, want a usage error", bad, err)
		}
	}
	if err := dispatch(t.Context(), []string{"--kinds", "invented-kind"}, nil); !errors.Is(err, errUsage) {
		t.Errorf("serve --kinds invented-kind: %v", err)
	}
}

// The serve path wires the runner to the queue with the asker's whole
// identity: the queue checks the author's role again before it sends, and
// the node's owner is known by user id, not login.
func TestServeStartLinesUp(t *testing.T) {
	fake, stateDir := queueHerdr(t)
	fake.SetStartOutcome("claude", fakeherdr.StartNotReady)
	s := startServe(t, ownerNode(nil), "--state-dir", stateDir, "--herdr-socket", fake.Addr())
	c, _, err := s.dial(t, owner, testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	snap := readJSON[hub.Snapshot](t, c)
	if want := []hub.StartKind{{Kind: "claude", Label: "Claude Code"}, {Kind: "codex", Label: "Codex"}}; !slices.Equal(snap.StartKinds, want) {
		t.Fatalf("start_kinds %+v", snap.StartKinds)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	msg := map[string]any{"type": "start", "id": "s1", "job_id": "job-00001", "workspace_id": "w1", "kind": "claude", "name": "fix-login", "task": "invented lined-up task"}
	if err := wsjson.Write(ctx, c, msg); err != nil {
		t.Fatal(err)
	}
	var pane string
	for pane == "" {
		var m struct {
			Type   string `json:"type"`
			OK     bool   `json:"ok"`
			Starts *[]struct {
				State  string `json:"state"`
				PaneID string `json:"pane_id"`
			} `json:"starts"`
		}
		if err := wsjson.Read(ctx, c, &m); err != nil {
			t.Fatal(err)
		}
		if m.Type == "ack" && !m.OK {
			t.Fatalf("start refused: %+v", m)
		}
		if m.Starts != nil && len(*m.Starts) == 1 && (*m.Starts)[0].State == "asks_first" {
			pane = (*m.Starts)[0].PaneID
		}
	}
	if err := fake.SetStatus(pane, "idle"); err != nil {
		t.Fatal(err)
	}
	for !slices.ContainsFunc(fake.InputLog(), func(in fakeherdr.Input) bool {
		return in.Method == "agent.prompt" && in.PaneID == pane && in.Text == "invented lined-up task"
	}) {
		if ctx.Err() != nil {
			t.Fatalf("the lined-up task was never sent: %+v", fake.InputLog())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.stop(); err != nil {
		t.Fatal(err)
	}
}
