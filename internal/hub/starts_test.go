package hub_test

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/fakeherdr"
	"github.com/tsoporan/herdr-pocket/internal/hub"
	"github.com/tsoporan/herdr-pocket/internal/start"
)

type startsRecorder struct {
	mu   sync.Mutex
	jobs []start.Job
}

func (s *startsRecorder) List() []start.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]start.Job(nil), s.jobs...)
}

func nextStartsPatch(t *testing.T, sub *hub.Subscription) hub.Patch {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case m, ok := <-sub.C:
			if !ok {
				t.Fatal("subscription closed")
			}
			if p, isPatch := m.(hub.Patch); isPatch && p.Starts != nil {
				return p
			}
		case <-deadline:
			t.Fatal("timed out waiting for a starts patch")
		}
	}
}

func TestHubStarts(t *testing.T) {
	fx := fakeherdr.Studio()
	// w2's two agents split between two folders: the tie goes to the
	// smaller path.
	for i, a := range fx.Agents {
		if a.PaneID == "w2:p2" {
			fx.Agents[i].Cwd = "/home/dev/src/aaa"
		}
	}
	srv := fakeWith(t, fx)
	h := newHub(srv)
	rec := &startsRecorder{}
	h.SetStarts(rec, []hub.StartKind{{Kind: "claude", Label: "Claude Code"}})
	if err := h.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	snap, sub, err := h.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	b, _ := json.Marshal(snap)
	for _, want := range []string{`"starts":[]`, `"start_kinds":[{"kind":"claude","label":"Claude Code"}]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("snapshot lacks %s: %s", want, b)
		}
	}
	paths := map[string]hub.Workspace{}
	for _, w := range snap.Workspaces {
		paths[w.WorkspaceID] = w
	}
	if w := paths["w1"]; w.Path != "/home/dev/src/api" || w.AgentCount != 3 {
		t.Errorf("w1 %+v", w)
	}
	if w := paths["w2"]; w.Path != "/home/dev/src/aaa" || w.AgentCount != 2 {
		t.Errorf("w2 %+v", w)
	}
	if p, ok := h.WorkspacePath("w1"); !ok || p != "/home/dev/src/api" {
		t.Errorf("WorkspacePath(w1) %q %v", p, ok)
	}
	if _, ok := h.WorkspacePath("w404"); ok {
		t.Error("WorkspacePath of an unknown workspace")
	}
	if !h.NameLive("Refactor auth middleware") || h.NameLive("refactor") {
		t.Error("NameLive")
	}

	began := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	rec.mu.Lock()
	rec.jobs = []start.Job{{ID: "job-00001", By: "owner@example.invalid", WorkspaceID: "w1", Kind: "claude", Name: "fix-login", State: start.Running, Began: began}}
	rec.mu.Unlock()
	h.StartsChanged()
	p := nextStartsPatch(t, sub)
	if len(*p.Starts) != 1 || (*p.Starts)[0].ID != "job-00001" || len(p.Agents) != 0 {
		t.Fatalf("patch %+v", p)
	}
	if snap, sub2, err := h.Subscribe(); err != nil || len(snap.Starts) != 1 {
		t.Fatalf("snapshot starts %+v, %v", snap.Starts, err)
	} else {
		sub2.Close()
	}

	rec.mu.Lock()
	rec.jobs = nil
	rec.mu.Unlock()
	h.StartsChanged()
	p = nextStartsPatch(t, sub)
	if b, _ := json.Marshal(p); !strings.Contains(string(b), `"starts":[]`) {
		t.Errorf("emptied starts patch %s", b)
	}
}

// Without a runner the snapshot still carries empty lists, never null.
func TestHubStartsOff(t *testing.T) {
	h := newHub(fake(t))
	if err := h.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	snap, sub, err := h.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	b, _ := json.Marshal(snap)
	if !strings.Contains(string(b), `"starts":[]`) || !strings.Contains(string(b), `"start_kinds":[]`) {
		t.Errorf("snapshot %s", b)
	}
}
