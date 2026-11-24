package hub_test

import (
	"sync"
	"testing"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/fakeherdr"
	"github.com/tsoporan/herdr-pocket/internal/herdr"
	"github.com/tsoporan/herdr-pocket/internal/push"
)

// pushRecorder is a hub.Pusher that keeps what the hub fed it.
type pushRecorder struct {
	mu      sync.Mutex
	sets    [][]push.Agent
	focuses []time.Time
	expiry  []*time.Time
	changed chan struct{}
}

func newRecorder() *pushRecorder { return &pushRecorder{changed: make(chan struct{}, 64)} }

func (p *pushRecorder) note() {
	select {
	case p.changed <- struct{}{}:
	default:
	}
}

func (p *pushRecorder) Observe(agents []push.Agent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sets = append(p.sets, agents)
	p.note()
}

func (p *pushRecorder) FocusEvent(at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.focuses = append(p.focuses, at)
	p.note()
}

func (p *pushRecorder) SetExpiry(at *time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expiry = append(p.expiry, at)
	p.note()
}

// wait polls until ok accepts the recorder's state.
func (p *pushRecorder) wait(t *testing.T, what string, ok func(p *pushRecorder) bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		p.mu.Lock()
		done := ok(p)
		p.mu.Unlock()
		if done {
			return
		}
		select {
		case <-p.changed:
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// lastAgent is the pane's record in the latest Observe. Callers hold p.mu.
func (p *pushRecorder) lastAgent(pane string) (push.Agent, bool) {
	if len(p.sets) == 0 {
		return push.Agent{}, false
	}
	for _, a := range p.sets[len(p.sets)-1] {
		if a.PaneID == pane {
			return a, true
		}
	}
	return push.Agent{}, false
}

func TestHubFeedsPush(t *testing.T) {
	srv := fake(t)
	h := newHub(srv)
	p := newRecorder()
	at := time.Date(2027, 5, 20, 0, 0, 0, 0, time.UTC)
	h.SetKeyExpiry(&at)
	h.SetPusher(p) // before Start: it gets the expiry already set
	if err := h.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	p.wait(t, "the first agent set", func(p *pushRecorder) bool {
		return len(p.expiry) == 1 && p.expiry[0].Equal(at) && len(p.sets) > 0 && len(p.sets[len(p.sets)-1]) == 9
	})
	p.mu.Lock()
	a, _ := p.lastAgent("w1:p1")
	p.mu.Unlock()
	if a.Name == "" || a.Workspace != "api" || a.Status != string(herdr.StatusBlocked) || a.Seq == 0 || a.Focused {
		t.Fatalf("w1:p1 %+v", a)
	}

	// A status event moves the seq once.
	if err := srv.SetStatus("w1:p3", herdr.StatusBlocked); err != nil {
		t.Fatal(err)
	}
	var seq uint64
	p.wait(t, "w1:p3 blocked", func(p *pushRecorder) bool {
		a, ok := p.lastAgent("w1:p3")
		seq = a.Seq
		return ok && a.Status == string(herdr.StatusBlocked)
	})
	// Clicking that pane on the desk is a focus event, and its re-list
	// catches up on herdr's seq without moving the engine's: an alert held
	// for the desk window must not look stale.
	if err := srv.SetFocus("w1:p3"); err != nil {
		t.Fatal(err)
	}
	p.wait(t, "the focus event and w1:p3 focused", func(p *pushRecorder) bool {
		a, _ := p.lastAgent("w1:p3")
		return len(p.focuses) > 0 && a.Focused
	})
	p.mu.Lock()
	a, _ = p.lastAgent("w1:p3")
	p.mu.Unlock()
	if a.Seq != seq || a.Status != string(herdr.StatusBlocked) {
		t.Fatalf("after the focus re-list: %+v, want seq %d", a, seq)
	}

	// The app's focus_on_desktop goes through FocusEvent.
	p.mu.Lock()
	n := len(p.focuses)
	p.mu.Unlock()
	h.FocusEvent()
	p.wait(t, "FocusEvent", func(p *pushRecorder) bool { return len(p.focuses) == n+1 })

	// A new status moves the seq again.
	if err := srv.SetStatus("w1:p3", herdr.StatusWorking); err != nil {
		t.Fatal(err)
	}
	p.wait(t, "w1:p3 working", func(p *pushRecorder) bool {
		a, _ := p.lastAgent("w1:p3")
		return a.Status == string(herdr.StatusWorking) && a.Seq != seq
	})

	h.SetKeyExpiry(nil)
	p.wait(t, "expiry off", func(p *pushRecorder) bool { return len(p.expiry) == 2 && p.expiry[1] == nil })
}

// The push title is the agent's herdr name, else its kind, never the
// terminal title: the program in the pane sets that (Claude Code puts its
// current task there), and a push carries no screen text.
func TestPushNameSkipsTerminalTitle(t *testing.T) {
	fx := fakeherdr.Studio()
	fx.Agents[0].Name = ""
	fx.Agents[0].DisplayAgent = ""
	srv := fakeWith(t, fx)
	h := newHub(srv)
	p := newRecorder()
	h.SetPusher(p)
	if err := h.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	pane := fx.Agents[0].PaneID
	p.wait(t, "the first agent set", func(p *pushRecorder) bool { _, ok := p.lastAgent(pane); return ok })
	p.mu.Lock()
	a, _ := p.lastAgent(pane)
	p.mu.Unlock()
	if fx.Agents[0].TerminalTitleStripped == "" || a.Name != fx.Agents[0].Agent {
		t.Fatalf("push name %q, want the agent kind %q (terminal title %q)", a.Name, fx.Agents[0].Agent, fx.Agents[0].TerminalTitleStripped)
	}
}
