package start_test

import (
	"context"
	"errors"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/herdr"
	"github.com/tsoporan/herdr-pocket/internal/queue"
	"github.com/tsoporan/herdr-pocket/internal/start"
)

// fakeHerdr answers the runner's three calls from scripted results and
// records each call.
type fakeHerdr struct {
	script
	mu    sync.Mutex
	calls []string // "tab.create ws cwd label", "agent.start name kind pane", "prompt pane text"
	// agents are the panes an agent.start succeeded on, which the rig's
	// hub lists (unless the script says unlisted), as the real hub would.
	agents map[string]bool
	// names are the names agent.start was asked for, by pane: herdr's
	// once the hub lists that pane.
	names map[string]string
}

// script is what the fake answers.
type script struct {
	tabErr error
	starts []error // one per agent.start, the last repeating; nil means started
	prompt error
	// startDelay is how long agent.start takes (fake time under synctest).
	startDelay time.Duration
	// unlisted: the hub never lists a started agent.
	unlisted bool
	// launched: agent.start succeeded and the error in starts came from
	// the wait after it, so the agent comes back with it. Without it, as
	// with the real client, only nil, agent_not_ready and timeout (both
	// from the wait) come back with the agent.
	launched bool
}

// listed reports whether the rig's hub lists an agent herdr started on pane.
func (f *fakeHerdr) listed(pane string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.unlisted && f.agents[pane]
}

// started is the name agent.start was asked for, by pane.
func (f *fakeHerdr) started() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.names)
}

func (f *fakeHerdr) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeHerdr) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeHerdr) CreateTab(_ context.Context, ws, cwd, label string) (herdr.Tab, herdr.Pane, error) {
	f.record("tab.create " + ws + " " + cwd + " " + label)
	if f.tabErr != nil {
		return herdr.Tab{}, herdr.Pane{}, f.tabErr
	}
	reported := cwd
	if reported == "" {
		reported = "/srv/invented/active"
	}
	return herdr.Tab{TabID: ws + ":t9"}, herdr.Pane{PaneID: ws + ":p9", TabID: ws + ":t9", Cwd: reported}, nil
}

func (f *fakeHerdr) StartAgent(_ context.Context, name, kind, pane string, timeout time.Duration) (herdr.Agent, error) {
	f.mu.Lock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, "agent.start ") {
			n++
		}
	}
	var err error
	if len(f.starts) > 0 {
		err = f.starts[min(n, len(f.starts)-1)]
	}
	f.mu.Unlock()
	f.record("agent.start " + name + " " + kind + " " + pane)
	f.mu.Lock()
	if f.names == nil {
		f.names = map[string]string{}
	}
	f.names[pane] = name
	f.mu.Unlock()
	time.Sleep(f.startDelay)
	if err == nil {
		f.mu.Lock()
		if f.agents == nil {
			f.agents = map[string]bool{}
		}
		f.agents[pane] = true
		f.mu.Unlock()
	}
	var herr *herdr.Error
	fromWait := errors.As(err, &herr) && (herr.Code == herdr.CodeAgentNotReady || herr.Code == herdr.CodeTimeout)
	if err != nil && !fromWait && !f.launched {
		return herdr.Agent{}, err // refused by agent.start, or never answered
	}
	return herdr.Agent{PaneID: pane, Name: name, Agent: kind}, err
}

func (f *fakeHerdr) PromptWait(_ context.Context, target, text string, until []herdr.Status, _ time.Duration) (herdr.Agent, error) {
	f.record("prompt " + target + " " + text)
	if !slices.Equal(until, []herdr.Status{herdr.StatusWorking, herdr.StatusBlocked}) {
		return herdr.Agent{}, errors.New("unexpected until")
	}
	return herdr.Agent{PaneID: target}, f.prompt
}

func herr(code string) error { return &herdr.Error{Code: code, Message: "invented"} }

type step struct {
	method string
	pane   string
	size   int
	err    error
}

// rig is a runner on a fake herdr and a fake hub.
type rig struct {
	h       *fakeHerdr
	r       *start.Runner
	mu      sync.Mutex
	live    map[string]bool // live agent names
	panes   map[string]bool // panes the hub lists
	hasPane int             // HasPane calls
	relists int             // Relist calls
	lineUps []string        // "login pane text"
	lineErr error
	steps   []step
	changes int
}

var owner = queue.Author{Login: "owner@example.invalid", UserID: 1, Node: "phone"}

func newRig(t *testing.T, h *fakeHerdr) *rig {
	t.Helper()
	g := &rig{h: h, live: map[string]bool{"taken-one": true}, panes: map[string]bool{}}
	g.r = start.New(start.Config{
		Herdr: h,
		Kinds: []string{"claude", "codex"},
		Workspace: func(id string) (string, bool) {
			switch id {
			case "w1":
				return "/srv/invented/alpha", true
			case "w2":
				return "", true
			}
			return "", false
		},
		// As the hub's: a live name, or the name herdr gave a pane the hub lists.
		NameLive: func(n string) bool {
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.live[n] {
				return true
			}
			for p, name := range h.started() {
				if name == n && (g.panes[p] || h.listed(p)) {
					return true
				}
			}
			return false
		},
		HasPane: func(p string) bool {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.hasPane++
			return g.panes[p] || h.listed(p)
		},
		Relist: func() { g.mu.Lock(); g.relists++; g.mu.Unlock() },
		LineUp: func(by queue.Author, p, text string) error {
			g.mu.Lock()
			defer g.mu.Unlock()
			if !g.panes[p] {
				return errors.New("lined up before the hub listed the pane")
			}
			if by.UserID != owner.UserID {
				return errors.New("author lost its user id")
			}
			g.lineUps = append(g.lineUps, by.Login+" "+p+" "+text)
			return g.lineErr
		},
		OnChange: func() { g.mu.Lock(); g.changes++; g.mu.Unlock() },
		OnStep: func(j start.Job, method string, payload []byte, err error) {
			g.mu.Lock()
			g.steps = append(g.steps, step{method, j.PaneID, len(payload), err})
			g.mu.Unlock()
		},
		PaneWait:  2 * time.Second,
		BusyRetry: 500 * time.Millisecond,
	})
	return g
}

// run runs the runner until the test's bubble ends.
func (g *rig) run(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { g.r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func (g *rig) job(t *testing.T, id string) start.Job {
	t.Helper()
	for _, j := range g.r.List() {
		if j.ID == id {
			return j
		}
	}
	t.Fatalf("no job %s", id)
	return start.Job{}
}

func req(id string) start.Request {
	return start.Request{ID: id, WorkspaceID: "w1", Kind: "claude", Name: "fix-login", Task: "  invented task  "}
}

func TestEndings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		h      script
		state  start.State
		code   string
		step   string
		calls  int // herdr calls made
		lineUp bool
	}{
		{name: "done", state: start.Done, calls: 3},
		{name: "asks first", h: script{starts: []error{herr(herdr.CodeAgentNotReady)}}, state: start.AsksFirst, calls: 2, lineUp: true},
		{name: "blocked at the prompt", h: script{prompt: herr(herdr.CodeAgentBlocked)}, state: start.AsksFirst, calls: 3, lineUp: true},
		{name: "stalled", h: script{prompt: herr(herdr.CodeAgentPromptStalled)}, state: start.Stalled, code: herdr.CodeAgentPromptStalled, step: start.StepSent, calls: 3},
		// A waiting prompt submits before it waits: the task may have gone.
		{name: "prompt timeout", h: script{prompt: herr(herdr.CodeTimeout)}, state: start.Stalled, code: herdr.CodeTimeout, step: start.StepSent, calls: 3},
		{name: "not installed", h: script{starts: []error{herr(herdr.CodeTimeout)}}, state: start.Failed, code: herdr.CodeTimeout, step: start.StepStarted, calls: 2},
		{name: "name taken by herdr", h: script{starts: []error{herr(herdr.CodeAgentNameTaken)}}, state: start.Failed, code: herdr.CodeAgentNameTaken, step: start.StepStarted, calls: 2},
		{name: "no workspace", h: script{tabErr: herr(herdr.CodeWorkspaceNotFound)}, state: start.Failed, code: herdr.CodeWorkspaceNotFound, step: start.StepOpened, calls: 1},
		{name: "unreachable", h: script{tabErr: &net.OpError{Op: "dial", Err: errors.New("refused")}}, state: start.Failed, code: start.ErrorUnreachable, step: start.StepOpened, calls: 1},
		{name: "unsure at the start", h: script{starts: []error{errors.New("read: connection reset")}}, state: start.Unsure, code: start.ErrorUnsure, step: start.StepStarted, calls: 2},
		{name: "unsure at the prompt", h: script{prompt: errors.New("read: i/o timeout")}, state: start.Unsure, code: start.ErrorUnsure, step: start.StepSent, calls: 3},
		{name: "busy then started", h: script{starts: []error{herr(herdr.CodeAgentPaneBusy), herr(herdr.CodeAgentPaneBusy), nil}}, state: start.Done, calls: 5},
		{name: "busy every try", h: script{starts: []error{herr(herdr.CodeAgentPaneBusy)}}, state: start.Failed, code: herdr.CodeAgentPaneBusy, step: start.StepStarted, calls: 5},
		// The wait failed after agent.start succeeded: never started again,
		// even on a code that would otherwise be retried.
		{name: "wait failed after the start", h: script{starts: []error{herr(herdr.CodeAgentNotFound)}, launched: true}, state: start.Failed, code: herdr.CodeAgentNotFound, step: start.StepStarted, calls: 2},
		{name: "busy after the start", h: script{starts: []error{herr(herdr.CodeAgentPaneBusy)}, launched: true}, state: start.Failed, code: herdr.CodeAgentPaneBusy, step: start.StepStarted, calls: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := &fakeHerdr{script: tc.h}
				h.startDelay = 4 * time.Second
				g := newRig(t, h)
				g.panes["w1:p9"] = true
				g.run(t)
				j, err := g.r.Begin(owner, req("job-00001"))
				if err != nil || j.State != start.Running || j.Cwd != "/srv/invented/alpha" {
					t.Fatalf("Begin %+v, %v", j, err)
				}
				synctest.Wait()
				time.Sleep(time.Minute)
				synctest.Wait()
				j = g.job(t, "job-00001")
				if j.State != tc.state || j.Error != tc.code || j.FailedStep != tc.step || j.EndedAt == nil {
					t.Fatalf("job %+v", j)
				}
				calls := h.log()
				if len(calls) != tc.calls {
					t.Fatalf("calls %q", calls)
				}
				if calls[0] != "tab.create w1 /srv/invented/alpha fix-login" {
					t.Errorf("tab.create %q", calls[0])
				}
				for _, c := range calls[1:] {
					if strings.HasPrefix(c, "prompt ") && c != "prompt w1:p9 invented task" {
						t.Errorf("prompt %q: want the pane and the trimmed task", c)
					}
				}
				if tc.lineUp != (len(g.lineUps) == 1) || (tc.lineUp && g.lineUps[0] != "owner@example.invalid w1:p9 invented task") {
					t.Errorf("line-ups %q", g.lineUps)
				}
				if len(g.steps) != tc.calls {
					t.Errorf("audited steps %+v", g.steps)
				}
				for _, s := range g.steps {
					if (s.method == "prompt") != (s.size == len("invented task")) {
						t.Errorf("step %+v: only the prompt carries the task", s)
					}
				}
				if g.relists == 0 && len(calls) > 1 {
					t.Error("no re-list asked for after agent.start")
				}
				if tc.state == start.Done && (j.OpenedAt == nil || j.StartedAt == nil || j.SentAt == nil || j.PaneID != "w1:p9" || j.TabID != "w1:t9") {
					t.Errorf("done job %+v", j)
				}
			})
		})
	}
}

func TestBusyRetrySpacing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &fakeHerdr{script: script{starts: []error{herr(herdr.CodeAgentPaneBusy), nil}}}
		g := newRig(t, h)
		g.run(t)
		t0 := time.Now()
		if _, err := g.r.Begin(owner, req("job-00001")); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if got := len(h.log()); got != 2 {
			t.Fatalf("before the retry: %q", h.log())
		}
		time.Sleep(499 * time.Millisecond)
		synctest.Wait()
		if got := len(h.log()); got != 2 {
			t.Fatalf("retried early: %q", h.log())
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if j := g.job(t, "job-00001"); j.State != start.Done || j.EndedAt.Sub(t0) != 500*time.Millisecond {
			t.Fatalf("job %+v", j)
		}
	})
}

// A started job stays running until the hub lists its agent, so its name
// is never free between the job ending and NameLive seeing the agent. The
// prompt goes to herdr either way.
func TestDoneWaitsForPane(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &fakeHerdr{script: script{unlisted: true}}
		g := newRig(t, h)
		g.run(t)
		if _, err := g.r.Begin(owner, req("job-00001")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if j := g.job(t, "job-00001"); j.State != start.Running || j.StartedAt == nil || len(h.log()) != 2 {
			t.Fatalf("job while waiting %+v, calls %q", j, h.log())
		}
		if _, err := g.r.Begin(owner, start.Request{ID: "job-00002", WorkspaceID: "w1", Kind: "claude", Name: "fix-login", Task: "x"}); !errors.Is(err, start.ErrNameTaken) {
			t.Fatalf("same name while waiting: %v", err)
		}
		g.mu.Lock()
		g.panes["w1:p9"] = true
		g.mu.Unlock()
		time.Sleep(time.Second)
		synctest.Wait()
		if j := g.job(t, "job-00001"); j.State != start.Done {
			t.Fatalf("job %+v", j)
		}

		// Never listed: sent after PaneWait anyway.
		h2 := &fakeHerdr{script: script{unlisted: true}}
		g2 := newRig(t, h2)
		g2.run(t)
		t0 := time.Now()
		if _, err := g2.r.Begin(owner, req("job-00003")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if j := g2.job(t, "job-00003"); j.State != start.Done || j.SentAt.Sub(t0) != 2*time.Second {
			t.Fatalf("never listed: %+v", j)
		}
	})
}

// The task is lined up only once the hub lists the new pane: the queue
// drops an item for a pane missing from the agent set it last saw.
func TestLineUpWaitsForPane(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &fakeHerdr{script: script{starts: []error{herr(herdr.CodeAgentNotReady)}}}
		g := newRig(t, h)
		g.run(t)
		if _, err := g.r.Begin(owner, req("job-00001")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		g.mu.Lock()
		polls, lined := g.hasPane, len(g.lineUps)
		g.mu.Unlock()
		if polls < 2 || lined != 0 {
			t.Fatalf("polls %d, line-ups %d", polls, lined)
		}
		if j := g.job(t, "job-00001"); j.State != start.Running || j.StartedAt == nil {
			t.Fatalf("job while waiting %+v", j)
		}
		g.mu.Lock()
		g.panes["w1:p9"] = true
		g.mu.Unlock()
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()
		if j := g.job(t, "job-00001"); j.State != start.AsksFirst || len(g.lineUps) != 1 {
			t.Fatalf("job %+v, line-ups %q", j, g.lineUps)
		}
	})
}

// The runner asks the hub to re-list after agent.start, and each second
// while it waits for the agent: herdr sends no event once the agent has
// its name, so nothing else would.
func TestWaitAsksForRelists(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &fakeHerdr{script: script{unlisted: true}}
		g := newRig(t, h)
		g.run(t)
		if _, err := g.r.Begin(owner, req("job-00001")); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		g.mu.Lock()
		after := g.relists
		g.mu.Unlock()
		time.Sleep(1500 * time.Millisecond)
		synctest.Wait()
		g.mu.Lock()
		later := g.relists
		g.mu.Unlock()
		if after != 1 || later != 2 {
			t.Fatalf("re-lists %d after agent.start, %d after 1.5 s; want 1, 2", after, later)
		}
	})
}

// A job that ended after herdr may have taken its name keeps the name from
// a new start for a while, so a quick retry can't open a second tab only to
// be refused agent_name_taken.
func TestNameHeld(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    script
		held bool
	}{
		{"lost pane", script{starts: []error{herr(herdr.CodeAgentNotReady)}}, true},
		{"unsure at the start", script{starts: []error{errors.New("read: connection reset")}}, true},
		{"timeout at the start", script{starts: []error{herr(herdr.CodeTimeout)}}, true},
		{"wait failed after the start", script{starts: []error{herr(herdr.CodeAgentNotFound)}, launched: true}, true},
		{"refused by herdr", script{starts: []error{herr(herdr.CodeAgentPaneBusy)}}, false},
		{"name taken by herdr", script{starts: []error{herr(herdr.CodeAgentNameTaken)}}, false},
		{"no tab", script{tabErr: herr(herdr.CodeWorkspaceNotFound)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				g := newRig(t, &fakeHerdr{script: tc.h})
				g.run(t)
				if _, err := g.r.Begin(owner, req("job-00001")); err != nil {
					t.Fatal(err)
				}
				time.Sleep(time.Minute)
				synctest.Wait()
				if j := g.job(t, "job-00001"); j.State == start.Running {
					t.Fatalf("still running: %+v", j)
				}
				_, err := g.r.Begin(owner, req("job-00002"))
				if held := errors.Is(err, start.ErrNameTaken); held != tc.held {
					t.Fatalf("retry a minute on: %v, want held %v", err, tc.held)
				}
				if !tc.held {
					return
				}
				time.Sleep(2 * time.Minute)
				if _, err := g.r.Begin(owner, req("job-00003")); err != nil {
					t.Fatalf("retry three minutes on: %v", err)
				}
			})
		})
	}
}

// A runner stopping while an asking agent's pane is awaited ends the job
// stopped, not lost_pane.
func TestLineUpStopped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &fakeHerdr{script: script{starts: []error{herr(herdr.CodeAgentNotReady)}}}
		g := newRig(t, h)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { g.r.Run(ctx); close(done) }()
		if _, err := g.r.Begin(owner, req("job-00001")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		cancel()
		<-done
		if j := g.job(t, "job-00001"); j.State != start.Failed || j.Error != start.ErrorStopped || j.FailedStep != start.StepSent {
			t.Fatalf("job %+v", j)
		}
	})
}

func TestLostPane(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &fakeHerdr{script: script{starts: []error{herr(herdr.CodeAgentNotReady)}}}
		g := newRig(t, h)
		g.run(t)
		if _, err := g.r.Begin(owner, req("job-00001")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if j := g.job(t, "job-00001"); j.State != start.Failed || j.Error != start.ErrorLostPane || j.FailedStep != start.StepSent || len(g.lineUps) != 0 {
			t.Fatalf("job %+v, line-ups %q", j, g.lineUps)
		}
	})
}

func TestLineUpFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &fakeHerdr{script: script{starts: []error{herr(herdr.CodeAgentNotReady)}}}
		g := newRig(t, h)
		g.panes["w1:p9"] = true
		g.lineErr = errors.New("queue: saving: invented")
		g.run(t)
		if _, err := g.r.Begin(owner, req("job-00001")); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if j := g.job(t, "job-00001"); j.State != start.Failed || j.Error != start.ErrorQueueFailed {
			t.Fatalf("job %+v", j)
		}
	})
}

// A resend or double tap with the same id returns the same job and opens
// no second tab.
func TestIdempotent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &fakeHerdr{script: script{startDelay: 4 * time.Second}}
		g := newRig(t, h)
		g.run(t)
		first, err := g.r.Begin(owner, req("job-00001"))
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait() // mid-start: its name is a running job's
		again, err := g.r.Begin(owner, req("job-00001"))
		if err != nil || again.ID != first.ID || !again.Began.Equal(first.Began) {
			t.Fatalf("resend %+v, %v", again, err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if _, err := g.r.Begin(owner, req("job-00001")); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		tabs := 0
		for _, c := range h.log() {
			if strings.HasPrefix(c, "tab.create") {
				tabs++
			}
		}
		if tabs != 1 || len(g.r.List()) != 1 {
			t.Fatalf("tabs %d, jobs %+v", tabs, g.r.List())
		}
	})
}

// Every refusal comes before any herdr call.
func TestRefusals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &fakeHerdr{script: script{startDelay: 4 * time.Second}}
		g := newRig(t, h)
		g.run(t)
		if _, err := g.r.Begin(owner, req("running-1")); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		before := len(h.log())
		long := strings.Repeat("x", 4001)
		for _, tc := range []struct {
			name string
			by   queue.Author
			f    func(*start.Request)
			want error
		}{
			{"short id", owner, func(r *start.Request) { r.ID = "short" }, start.ErrInvalid},
			{"bad id", owner, func(r *start.Request) { r.ID = "job/../00001" }, start.ErrInvalid},
			{"no login", queue.Author{}, func(*start.Request) {}, start.ErrInvalid},
			{"empty task", owner, func(r *start.Request) { r.Task = " \n\t" }, start.ErrInvalid},
			{"bad utf-8", owner, func(r *start.Request) { r.Task = "\xff" }, start.ErrInvalid},
			{"too long", owner, func(r *start.Request) { r.Task = long }, start.ErrTooLong},
			{"workspace", owner, func(r *start.Request) { r.WorkspaceID = "w404" }, start.ErrUnknownWorkspace},
			{"kind", owner, func(r *start.Request) { r.Kind = "gemini" }, start.ErrUnknownKind},
			{"name pattern", owner, func(r *start.Request) { r.Name = "Fix Login" }, start.ErrInvalidName},
			{"name too long", owner, func(r *start.Request) { r.Name = "a" + strings.Repeat("b", 32) }, start.ErrInvalidName},
			{"live name", owner, func(r *start.Request) { r.Name = "taken-one" }, start.ErrNameTaken},
			{"running job name", owner, func(*start.Request) {}, start.ErrNameTaken},
		} {
			r := req("job-" + strings.ReplaceAll(tc.name, " ", "-"))
			tc.f(&r)
			if _, err := g.r.Begin(tc.by, r); !errors.Is(err, tc.want) {
				t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
			}
		}
		synctest.Wait()
		if got := h.log(); len(got) != before {
			t.Fatalf("a refusal reached herdr: %q", got[before:])
		}
		if n := len(g.r.List()); n != 1 {
			t.Fatalf("refused requests kept as jobs: %d", n)
		}
		// 4000 bytes after trimming is accepted, and an empty workspace path
		// leaves the folder to herdr.
		r := start.Request{ID: "job-at-limit", WorkspaceID: "w2", Kind: "codex", Name: "at-limit", Task: " " + long[:4000] + " "}
		if j, err := g.r.Begin(owner, r); err != nil || j.Cwd != "" {
			t.Fatalf("at the limit: %+v, %v", j, err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if j := g.job(t, "job-at-limit"); j.Cwd != "/srv/invented/active" || j.State != start.Done {
			t.Errorf("cwd from herdr: %+v", j)
		}
	})
}

func TestKeeps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &fakeHerdr{}
		g := newRig(t, h)
		g.run(t)
		for i := range 22 {
			r := req("job-" + string(rune('a'+i)) + "-0000")
			r.Name = "agent-" + string(rune('a'+i))
			if _, err := g.r.Begin(owner, r); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
		}
		jobs := g.r.List()
		if len(jobs) != 20 || jobs[0].ID != "job-c-0000" || jobs[19].ID != "job-v-0000" {
			t.Fatalf("kept %d, first %s", len(jobs), jobs[0].ID)
		}
		time.Sleep(time.Hour)
		if n := len(g.r.List()); n != 0 {
			t.Fatalf("kept %d after an hour", n)
		}
	})
}

// A job begun with the runner stopping makes no herdr call.
func TestStopped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &fakeHerdr{}
		g := newRig(t, h)
		if _, err := g.r.Begin(owner, req("job-00001")); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		g.r.Run(ctx)
		if len(h.log()) != 0 {
			t.Fatalf("calls %q", h.log())
		}
	})
}
