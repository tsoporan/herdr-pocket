package hub_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/fakeherdr"
	"github.com/tsoporan/herdr-pocket/internal/herdr"
	"github.com/tsoporan/herdr-pocket/internal/hub"
	"github.com/tsoporan/herdr-pocket/internal/transcript"
)

func fake(t *testing.T) *fakeherdr.Server {
	t.Helper()
	return fakeWith(t, fakeherdr.Studio())
}

// claudeFixtures holds the fixture agents' transcripts. Tests never read
// ~/.claude.
const claudeFixtures = "../fakeherdr/testdata/claude"

func newHub(srv *fakeherdr.Server) *hub.Hub {
	return hub.New(herdr.NewClient(srv.Addr()), "studio", transcript.NewReader(claudeFixtures))
}

func fakeWith(t *testing.T, fx fakeherdr.Fixture) *fakeherdr.Server {
	t.Helper()
	dir, err := os.MkdirTemp("", "hh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	srv, err := fakeherdr.Listen(filepath.Join(dir, "h.sock"), fx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

func TestStartRefusesOldProtocol(t *testing.T) {
	srv := fake(t)
	srv.SetProtocol(21)
	err := newHub(srv).Start(t.Context())
	if !errors.Is(err, herdr.ErrProtocolTooOld) {
		t.Fatalf("Start: %v, want ErrProtocolTooOld", err)
	}
}

func TestEmptyInventoryMarshalsAsLists(t *testing.T) {
	srv := fakeWith(t, fakeherdr.Fixture{})
	h := newHub(srv)
	if err := h.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	snap, sub, err := h.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if snap.Agents == nil || snap.Workspaces == nil {
		t.Fatal("snapshot lists must be [] not null")
	}
}

func TestSubscribeRefusedBeforeStart(t *testing.T) {
	_, _, err := hub.New(herdr.NewClient("/nonexistent"), "studio", nil).Subscribe()
	if !errors.Is(err, hub.ErrOffline) {
		t.Fatalf("Subscribe: %v, want ErrOffline", err)
	}
}

// subscribe retries until the hub is back online.
func subscribe(t *testing.T, h *hub.Hub) (hub.Snapshot, *hub.Subscription) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		snap, sub, err := h.Subscribe()
		if err == nil {
			t.Cleanup(sub.Close)
			return snap, sub
		}
		if time.Now().After(deadline) {
			t.Fatalf("hub still offline: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestHerdrLossClosesSubscribers(t *testing.T) {
	srv := fake(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h := newHub(srv)
	if err := h.Start(ctx); err != nil {
		t.Fatal(err)
	}
	_, sub := subscribe(t, h)
	srv.Close()
	select {
	case _, ok := <-sub.C:
		if ok {
			t.Fatal("got a patch, want the channel closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subscriber not closed after herdr went away")
	}
	if _, _, err := h.Subscribe(); !errors.Is(err, hub.ErrOffline) {
		t.Fatalf("Subscribe while herdr is down: %v, want ErrOffline", err)
	}
}

func TestLine(t *testing.T) {
	cases := []struct {
		a    herdr.Agent
		want string
	}{
		{herdr.Agent{AgentStatus: herdr.StatusBlocked}, "Wants to run a command"},
		{herdr.Agent{AgentStatus: herdr.StatusDone}, "Finished"},
		{herdr.Agent{AgentStatus: herdr.StatusWorking, TerminalTitleStripped: "Running tests"}, "Running tests"},
		{herdr.Agent{AgentStatus: herdr.StatusWorking}, "Working"},
		{herdr.Agent{AgentStatus: herdr.StatusIdle}, "Waiting for your next task"},
		{herdr.Agent{AgentStatus: herdr.StatusUnknown}, "Can't tell what it's doing"},
	}
	for _, c := range cases {
		if got := hub.Line(c.a, nil, time.Now()); got != c.want {
			t.Errorf("Line(%s) = %q, want %q", c.a.AgentStatus, got, c.want)
		}
	}
}

func TestReconnectResyncs(t *testing.T) {
	srv := fake(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h := newHub(srv)
	if err := h.Start(ctx); err != nil {
		t.Fatal(err)
	}
	_, lost := subscribe(t, h)

	// herdr restarts on the same socket with w4:p2 now working. The old
	// subscriber is closed; once the hub reconnects, a new one sees the
	// change in its snapshot and gets patches from the new streams.
	path := srv.Addr()
	srv.Close()
	for range lost.C {
	}
	fx := fakeherdr.Studio()
	for i := range fx.Agents {
		if fx.Agents[i].PaneID == "w4:p2" {
			fx.Agents[i].AgentStatus = herdr.StatusWorking
		}
	}
	again, err := fakeherdr.Listen(path, fx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { again.Close() })

	snap, sub := subscribe(t, h)
	if a := byID(snap.Agents)["w4:p2"]; a.Status != herdr.StatusWorking {
		t.Fatalf("w4:p2 is %q after herdr came back, want working", a.Status)
	}
	if err := again.SetStatus("w4:p2", herdr.StatusBlocked); err != nil {
		t.Fatal(err)
	}
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m := <-sub.C:
			if p, ok := m.(hub.Patch); ok && p.Agents["w4:p2"] != nil && p.Agents["w4:p2"].Status == herdr.StatusBlocked {
				return
			}
		case <-timeout:
			t.Fatal("no patch from the resubscribed stream")
		}
	}
}

func byID(agents []hub.Agent) map[string]hub.Agent {
	m := map[string]hub.Agent{}
	for _, a := range agents {
		m[a.PaneID] = a
	}
	return m
}

// The fixture transcripts give the mockup's row lines.
func TestLinesFromTranscripts(t *testing.T) {
	h := newHub(fake(t))
	if err := h.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	snap, _ := subscribe(t, h)
	agents := byID(snap.Agents)
	exact := map[string]string{
		"w1:p1": "Wants to run a command",
		"w2:p1": "Asks: staging or dev first?",
		"w3:p1": "Finished · PR #48 is ready",
		"w1:p3": "Waiting for your next task",
	}
	for id, want := range exact {
		if got := agents[id].Line; got != want {
			t.Errorf("%s line %q, want %q", id, got, want)
		}
	}
	prefix := map[string]string{
		"w1:p2": "Running the integration tests · ",
		"w4:p1": "Changing auth.md · ",
	}
	for id, want := range prefix {
		if got := agents[id].Line; !strings.HasPrefix(got, want) {
			t.Errorf("%s line %q, want prefix %q", id, got, want)
		}
	}
}

// Before the hub sees a status change, changed_at comes from the transcript;
// after one, it is the hub's own time.
func TestChangedAtFromTranscript(t *testing.T) {
	srv := fake(t)
	h := newHub(srv)
	if err := h.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	snap, sub := subscribe(t, h)
	agents := byID(snap.Agents)
	want := map[string]string{
		"w1:p2": "2026-11-21T09:10:01Z",   // working: the last typed prompt
		"w1:p1": "2026-11-21T09:02:50.9Z", // blocked: the pending call
		"w3:p1": "2026-11-21T07:41:11.5Z", // done: the last assistant record
	}
	for id, w := range want {
		at, _ := time.Parse(time.RFC3339Nano, w)
		if got := agents[id].ChangedAt; !got.Equal(at) {
			t.Errorf("%s changed_at %v, want %v", id, got, at)
		}
	}

	before := time.Now()
	if err := srv.SetStatus("w1:p2", herdr.StatusBlocked); err != nil {
		t.Fatal(err)
	}
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m := <-sub.C:
			p, ok := m.(hub.Patch)
			if a := p.Agents["w1:p2"]; ok && a != nil && a.Status == herdr.StatusBlocked {
				if a.ChangedAt.Before(before.Truncate(time.Millisecond)) {
					t.Errorf("changed_at %v after a seen change, want about now", a.ChangedAt)
				}
				return
			}
		case <-timeout:
			t.Fatal("no patch for the status change")
		}
	}
}
