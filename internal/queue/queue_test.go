package queue

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/herdr"
)

const owner = "owner@example.com"

var start = time.Date(2026, 11, 22, 15, 0, 0, 0, time.UTC)

// clock is a fake Now.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

type prompt struct{ pane, text string }

type sent struct {
	it  Item
	err error
}

// rig is a Queue on a temp state dir with a fake clock and a fake Prompt.
type rig struct {
	t   *testing.T
	dir string
	clk *clock
	q   *Queue

	mu      sync.Mutex
	prompts []prompt
	errs    []error // returned by the next Prompts, in order
	hook    func(paneID string)
	audit   []sent
	changes int
	revoked map[string]bool // logins CanSend refuses
	busy    map[string]bool // panes Clear says show a dialog
	clears  int             // Clear calls
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, dir: t.TempDir(), clk: &clock{t: start}}
	r.q = r.open()
	return r
}

func (r *rig) open() *Queue {
	r.t.Helper()
	q, err := Open(r.dir, Config{
		Now:      r.clk.Now,
		Prompt:   r.prompt,
		OnChange: func() { r.mu.Lock(); r.changes++; r.mu.Unlock() },
		OnSent:   func(it Item, err error) { r.mu.Lock(); r.audit = append(r.audit, sent{it, err}); r.mu.Unlock() },
		CanSend:  func(a Author) bool { r.mu.Lock(); defer r.mu.Unlock(); return !r.revoked[a.Login] },
		Clear: func(_ context.Context, pane string) bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.clears++
			return !r.busy[pane]
		},
		Logf: r.t.Logf,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return q
}

func (r *rig) prompt(_ context.Context, pane, text string) error {
	r.mu.Lock()
	hook := r.hook
	r.mu.Unlock()
	if hook != nil {
		hook(pane)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prompts = append(r.prompts, prompt{pane, text})
	if len(r.errs) > 0 {
		err := r.errs[0]
		r.errs = r.errs[1:]
		return err
	}
	return nil
}

// at sets the clock to start+d, runs one step and returns the prompts sent.
func (r *rig) at(d time.Duration) []prompt {
	r.t.Helper()
	r.clk.Set(start.Add(d))
	r.q.step(r.t.Context())
	r.mu.Lock()
	defer r.mu.Unlock()
	got := r.prompts
	r.prompts = nil
	return got
}

// observe sets the clock to start+d and observes agents.
func (r *rig) observe(d time.Duration, agents ...Agent) {
	r.clk.Set(start.Add(d))
	r.q.Observe(agents)
}

func (r *rig) add(t Target, text string) Item {
	r.t.Helper()
	it, err := r.q.Add(Author{Login: owner}, t, text)
	if err != nil {
		r.t.Fatal(err)
	}
	return it
}

// item returns the item's current state from List.
func (r *rig) item(id string) Item {
	r.t.Helper()
	for _, it := range r.q.List() {
		if it.ID == id {
			return it
		}
	}
	r.t.Fatalf("item %s not listed", id)
	return Item{}
}

func agent(pane, status string, seq uint64) Agent {
	return Agent{PaneID: pane, WorkspaceID: "orchard", Status: status, Seq: seq}
}

func toPane(p string) Target { return Target{PaneID: p} }

func wantPrompts(t *testing.T, got []prompt, want ...prompt) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("prompts %v, want %v", got, want)
	}
}

func TestSendsAfterSettleWhenIdle(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "idle", 1))
	it := r.add(toPane("w1-p1"), "  run the tests  ")
	if it.Text != "run the tests" || it.State != Waiting || it.By != owner {
		t.Fatalf("added %+v", it)
	}
	wantPrompts(t, r.at(4*time.Second))
	wantPrompts(t, r.at(5*time.Second), prompt{"w1-p1", "run the tests"})
	got := r.item(it.ID)
	if got.State != Sent || got.SentTo != "w1-p1" || got.SentAt == nil || !got.SentAt.Equal(start.Add(5*time.Second)) {
		t.Fatalf("after send %+v", got)
	}
	if len(r.audit) != 1 || r.audit[0].err != nil || r.audit[0].it.SentTo != "w1-p1" || r.audit[0].it.By != owner {
		t.Fatalf("audit %+v", r.audit)
	}
}

// herdr can call an agent idle while Claude Code's question is still open; the
// screen check holds the item, rechecks, and sends once the dialog is gone.
func TestHeldWhileADialogIsOpen(t *testing.T) {
	r := newRig(t)
	r.busy = map[string]bool{"w1-p1": true}
	r.observe(0, agent("w1-p1", "idle", 1))
	it := r.add(toPane("w1-p1"), "the next task")
	r.clk.Set(start.Add(5 * time.Second))
	next := r.q.step(r.t.Context())
	if !next.Equal(start.Add(10 * time.Second)) {
		t.Fatalf("next wake %v, want the recheck at +10s", next)
	}
	wantPrompts(t, r.at(10*time.Second))
	if got := r.item(it.ID); got.State != Waiting || got.SentTo != "" {
		t.Fatalf("held item %+v", got)
	}
	r.mu.Lock()
	r.busy = nil
	r.mu.Unlock()
	wantPrompts(t, r.at(15*time.Second), prompt{"w1-p1", "the next task"})
	if got := r.item(it.ID); got.State != Sent {
		t.Fatalf("after the dialog closed %+v", got)
	}
}

func TestRevokedAuthorIsNotSent(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "idle", 1))
	first := r.add(toPane("w1-p1"), "lined up before the change")
	second := r.add(toPane("w1-p1"), "also lined up")
	if a := first.Author; a == nil || a.Login != owner {
		t.Fatalf("author %+v", a)
	}
	// Access changes (say, a restart without that --allow) before its turn.
	r.mu.Lock()
	r.revoked = map[string]bool{owner: true}
	r.mu.Unlock()
	wantPrompts(t, r.at(5*time.Second)) // one item per agent per step
	wantPrompts(t, r.at(6*time.Second))
	for _, id := range []string{first.ID, second.ID} {
		if got := r.item(id); got.State != Failed || got.Error != ErrorForbidden {
			t.Fatalf("item %s: %+v", id, got)
		}
	}
	if len(r.audit) != 0 {
		t.Fatalf("audited a send that never happened: %+v", r.audit)
	}
}

func TestItemWithoutAuthorIsNotSent(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "idle", 1))
	it := r.add(toPane("w1-p1"), "from an older file")
	r.q.mu.Lock()
	r.q.findLocked(it.ID).Author = nil
	r.q.mu.Unlock()
	wantPrompts(t, r.at(5*time.Second))
	if got := r.item(it.ID); got.State != Failed || got.Error != ErrorForbidden {
		t.Fatalf("%+v", got)
	}
}

func TestDoneCountsAsFreeForItsOwnItems(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "done", 1))
	r.add(toPane("w1-p1"), "next step")
	wantPrompts(t, r.at(5*time.Second), prompt{"w1-p1", "next step"})
}

func TestNotWhileWorkingOrBlocked(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "working", 1))
	r.add(toPane("w1-p1"), "later")
	wantPrompts(t, r.at(time.Minute))
	r.observe(time.Minute, agent("w1-p1", "blocked", 2))
	wantPrompts(t, r.at(2*time.Minute))
	r.observe(2*time.Minute, agent("w1-p1", "unknown", 3))
	wantPrompts(t, r.at(3*time.Minute))
	r.observe(3*time.Minute, agent("w1-p1", "idle", 4))
	wantPrompts(t, r.at(3*time.Minute+4*time.Second))
	wantPrompts(t, r.at(3*time.Minute+5*time.Second), prompt{"w1-p1", "later"})
}

func TestSettleRestartsOnNewSeq(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "idle", 1))
	r.add(toPane("w1-p1"), "later")
	r.observe(3*time.Second, agent("w1-p1", "idle", 2)) // a change the hub saw
	wantPrompts(t, r.at(5*time.Second))
	wantPrompts(t, r.at(8*time.Second), prompt{"w1-p1", "later"})
}

func TestDeskHoldsThenSends(t *testing.T) {
	r := newRig(t)
	a := agent("w1-p1", "idle", 1)
	a.Focused = true
	r.q.FocusEvent(start)
	r.observe(0, a)
	r.add(toPane("w1-p1"), "later")
	r.clk.Set(start.Add(5 * time.Second))
	if next := r.q.step(t.Context()); !next.Equal(start.Add(5 * time.Minute)) {
		t.Fatalf("next wake %v, want the end of the desk window", next)
	}
	wantPrompts(t, r.at(4*time.Minute))
	wantPrompts(t, r.at(5*time.Minute), prompt{"w1-p1", "later"})
}

func TestDeskNeedsFocus(t *testing.T) {
	r := newRig(t)
	r.q.FocusEvent(start)
	r.observe(0, agent("w1-p1", "idle", 1)) // not the focused pane
	r.add(toPane("w1-p1"), "later")
	wantPrompts(t, r.at(5*time.Second), prompt{"w1-p1", "later"})
}

func TestOneInFlightThenPickedUp(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "idle", 1))
	first := r.add(toPane("w1-p1"), "first")
	second := r.add(toPane("w1-p1"), "second")
	wantPrompts(t, r.at(5*time.Second), prompt{"w1-p1", "first"})
	wantPrompts(t, r.at(9*time.Second)) // still idle, first not picked up yet
	r.observe(9*time.Second, agent("w1-p1", "working", 2))
	got := r.item(first.ID)
	if got.State != PickedUp || got.PickedUpAt == nil || !got.PickedUpAt.Equal(start.Add(9*time.Second)) {
		t.Fatalf("first %+v", got)
	}
	wantPrompts(t, r.at(time.Minute))
	r.observe(time.Minute, agent("w1-p1", "done", 3))
	wantPrompts(t, r.at(time.Minute+5*time.Second), prompt{"w1-p1", "second"})
	if r.item(second.ID).State != Sent {
		t.Fatalf("second %+v", r.item(second.ID))
	}
}

func TestPickedUpWhenSeenWorkingDuringPrompt(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "idle", 1))
	it := r.add(toPane("w1-p1"), "go")
	r.clk.Set(start.Add(5 * time.Second))
	r.hook = func(string) { r.q.Observe([]Agent{agent("w1-p1", "working", 2)}) }
	wantPrompts(t, r.at(5*time.Second), prompt{"w1-p1", "go"})
	if got := r.item(it.ID); got.State != PickedUp {
		t.Fatalf("state %s, want picked_up", got.State)
	}
}

func TestBlockedCountsAsPickedUp(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "idle", 1))
	it := r.add(toPane("w1-p1"), "go")
	r.at(5 * time.Second)
	r.observe(6*time.Second, agent("w1-p1", "blocked", 2))
	if got := r.item(it.ID); got.State != PickedUp {
		t.Fatalf("state %s, want picked_up", got.State)
	}
}

func TestStalledAfterTenSeconds(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "idle", 1))
	first := r.add(toPane("w1-p1"), "first")
	r.add(toPane("w1-p1"), "second")
	r.at(5 * time.Second)
	r.clk.Set(start.Add(5 * time.Second))
	if next := r.q.step(t.Context()); !next.Equal(start.Add(15 * time.Second)) {
		t.Fatalf("next wake %v, want the stall time", next)
	}
	wantPrompts(t, r.at(14*time.Second))
	if r.item(first.ID).State != Sent {
		t.Fatal("stalled early")
	}
	// The stall frees the agent, so the next item goes (as the plan says).
	wantPrompts(t, r.at(15*time.Second), prompt{"w1-p1", "second"})
	if got := r.item(first.ID); got.State != Stalled || got.EndedAt == nil {
		t.Fatalf("first %+v", got)
	}
	// Seen working later: a stalled item stays stalled.
	r.observe(16*time.Second, agent("w1-p1", "working", 2))
	if r.item(first.ID).State != Stalled {
		t.Fatal("stalled item changed")
	}
}

func TestAgentBlockedPutsBackOnce(t *testing.T) {
	r := newRig(t)
	blocked := &herdr.Error{Code: herdr.CodeAgentBlocked, Message: "agent is blocked"}
	r.errs = []error{blocked, blocked}
	r.observe(0, agent("w1-p1", "idle", 1))
	it := r.add(toPane("w1-p1"), "go")
	wantPrompts(t, r.at(5*time.Second), prompt{"w1-p1", "go"})
	if got := r.item(it.ID); got.State != Waiting || got.Error != "" {
		t.Fatalf("after the first refusal %+v", got)
	}
	// Not retried until the agent's state changes.
	wantPrompts(t, r.at(6*time.Second))
	r.observe(6*time.Second, agent("w1-p1", "blocked", 2))
	wantPrompts(t, r.at(20*time.Second))
	r.observe(20*time.Second, agent("w1-p1", "idle", 3))
	wantPrompts(t, r.at(25*time.Second), prompt{"w1-p1", "go"})
	got := r.item(it.ID)
	if got.State != Failed || got.Error != herdr.CodeAgentBlocked || got.SentTo != "w1-p1" {
		t.Fatalf("after the second refusal %+v", got)
	}
	if len(r.audit) != 2 || !errors.Is(r.audit[1].err, blocked) {
		t.Fatalf("audit %+v", r.audit)
	}
}

func TestOtherRefusalFails(t *testing.T) {
	r := newRig(t)
	r.errs = []error{&herdr.Error{Code: herdr.CodePaneNotFound, Message: "no pane"}}
	r.observe(0, agent("w1-p1", "idle", 1))
	it := r.add(toPane("w1-p1"), "go")
	r.add(toPane("w1-p1"), "then this")
	r.at(5 * time.Second)
	if got := r.item(it.ID); got.State != Failed || got.Error != herdr.CodePaneNotFound {
		t.Fatalf("%+v", got) // the code, never herdr's message or a Go error
	}
	if len(r.q.wake) != 1 {
		t.Fatal("Run is not woken for the next item")
	}
	// A failed item isn't in flight; the next one goes.
	wantPrompts(t, r.at(6*time.Second), prompt{"w1-p1", "then this"})
}

// Stopping during a Prompt doesn't cancel it: herdr may already have it, so
// its result counts and the item isn't sent again after a restart.
func TestShutdownDuringPromptKeepsItsResult(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "idle", 1))
	it := r.add(toPane("w1-p1"), "go")
	ctx, cancel := context.WithCancel(t.Context())
	r.q.cfg.Prompt = func(ctx context.Context, _, _ string) error {
		cancel()
		return ctx.Err() // nil: the Prompt's context outlives Run's
	}
	r.clk.Set(start.Add(5 * time.Second))
	r.q.step(ctx)
	if got := r.item(it.ID); got.State != Sent {
		t.Fatalf("%+v", got)
	}
}

func TestRecheckBeforeEachPrompt(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "idle", 1), agent("w1-p2", "idle", 1))
	r.add(toPane("w1-p1"), "one")
	r.add(toPane("w1-p2"), "two")
	r.clk.Set(start.Add(5 * time.Second))
	// While the first Prompt is under way, the second agent starts working.
	r.hook = func(pane string) {
		if pane == "w1-p1" {
			r.q.Observe([]Agent{agent("w1-p1", "idle", 1), agent("w1-p2", "working", 2)})
		}
	}
	wantPrompts(t, r.at(5*time.Second), prompt{"w1-p1", "one"})
}

func TestClosedPaneDropsItsItems(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "idle", 1), agent("w1-p2", "working", 1))
	sentIt := r.add(toPane("w1-p1"), "sent one")
	waitIt := r.add(toPane("w1-p2"), "waiting one")
	wsIt := r.add(Target{WorkspaceID: "orchard"}, "for anyone")
	r.at(5 * time.Second) // sentIt goes to w1-p1; wsIt can't: w1-p1 has its own item
	if r.item(sentIt.ID).State != Sent || r.item(wsIt.ID).State != Waiting {
		t.Fatalf("setup: %+v", r.q.List())
	}
	r.observe(6 * time.Second) // both panes closed
	for _, id := range []string{sentIt.ID, waitIt.ID} {
		if got := r.item(id); got.State != Dropped || got.EndedAt == nil {
			t.Fatalf("%+v", got)
		}
	}
	if r.item(wsIt.ID).State != Waiting {
		t.Fatal("a workspace item was dropped with a pane")
	}
	// Dropped items leave List after 24 h and the file after 7 days.
	r.at(25 * time.Hour)
	if len(r.q.List()) != 1 {
		t.Fatalf("listed %+v", r.q.List())
	}
	if len(r.q.items) != 3 {
		t.Fatalf("kept %d items before 7 days", len(r.q.items))
	}
	r.at(6*time.Second + 7*24*time.Hour)
	if len(r.q.items) != 1 || r.q.items[0].ID != wsIt.ID {
		t.Fatalf("kept %+v", r.q.items)
	}
}

func TestWorkspaceItemGoesToLongestIdle(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "done", 1)) // done is never free for a workspace item
	r.observe(time.Second, agent("w1-p1", "done", 1), agent("w1-p2", "idle", 1))
	r.observe(2*time.Second, agent("w1-p1", "done", 1), agent("w1-p2", "idle", 1), agent("w1-p3", "idle", 1),
		Agent{PaneID: "w2-p1", WorkspaceID: "meadow", Status: "idle", Seq: 1})
	it := r.add(Target{WorkspaceID: "orchard"}, "anyone")
	wantPrompts(t, r.at(10*time.Second), prompt{"w1-p2", "anyone"})
	if got := r.item(it.ID); got.SentTo != "w1-p2" || got.Target.WorkspaceID != "orchard" {
		t.Fatalf("%+v", got)
	}
	if r.audit[0].it.SentTo != "w1-p2" {
		t.Fatalf("audit %+v", r.audit)
	}
}

func TestWorkspaceItemsSpreadAndSkipBusyAgents(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "idle", 1), agent("w1-p2", "idle", 1), agent("w1-p3", "idle", 1), agent("w1-p4", "idle", 1))
	r.add(toPane("w1-p1"), "own") // w1-p1 has its own item
	a := agent("w1-p2", "idle", 1)
	a.Focused = true // w1-p2 is at the desk
	r.q.FocusEvent(start)
	r.observe(0, agent("w1-p1", "idle", 1), a, agent("w1-p3", "idle", 1), agent("w1-p4", "idle", 1))
	r.add(Target{WorkspaceID: "orchard"}, "task a")
	r.add(Target{WorkspaceID: "orchard"}, "task b")
	r.add(Target{WorkspaceID: "orchard"}, "task c")
	// Equal idle time: ties go by pane id. One item per agent per step.
	wantPrompts(t, r.at(5*time.Second), prompt{"w1-p1", "own"}, prompt{"w1-p3", "task a"}, prompt{"w1-p4", "task b"})
}

func TestWorkspaceItemWaitsForAFreeAgent(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "done", 1))
	it := r.add(Target{WorkspaceID: "orchard"}, "anyone")
	wantPrompts(t, r.at(time.Minute))
	r.observe(time.Minute, agent("w1-p1", "idle", 2))
	wantPrompts(t, r.at(time.Minute+5*time.Second), prompt{"w1-p1", "anyone"})
	if r.item(it.ID).State != Sent {
		t.Fatal("not sent")
	}
}

func TestPersistsAcrossRestart(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "idle", 1))
	first := r.add(toPane("w1-p1"), "first")
	second := r.add(toPane("w1-p1"), "second")
	r.at(5 * time.Second)

	dir := filepath.Join(r.dir, dirName)
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir: %v %v", fi.Mode(), err)
	}
	if fi, err := os.Stat(filepath.Join(dir, fileName)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", fi.Mode(), err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, fileName+".*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}

	r.q = r.open()
	r.at(time.Minute) // Run's first step comes before the hub's first Observe
	if got := r.item(first.ID); got.State != Sent || got.SentTo != "w1-p1" || got.Text != "first" {
		t.Fatalf("first after restart %+v", got)
	}
	if got := r.item(second.ID); got.State != Waiting || got.Text != "second" {
		t.Fatalf("second after restart %+v", got)
	}
	r.observe(time.Minute, agent("w1-p1", "working", 1))
	if r.item(first.ID).State != PickedUp {
		t.Fatal("not picked up after restart")
	}
}

func TestCorruptFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, dirName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dirName, fileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Config{Prompt: func(context.Context, string, string) error { return nil }}); err == nil {
		t.Fatal("opened a corrupt queue.json")
	}
}

func TestAddLimits(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "working", 1))
	for _, tc := range []struct {
		name  string
		login string
		t     Target
		text  string
		want  error
	}{
		{"blank", owner, toPane("w1-p1"), " \n\t ", ErrInvalid},
		{"too long", owner, toPane("w1-p1"), strings.Repeat("a", 4001), ErrInvalid},
		{"not utf-8", owner, toPane("w1-p1"), "bad \xff", ErrInvalid},
		{"no target", owner, Target{}, "x", ErrInvalid},
		{"two targets", owner, Target{PaneID: "w1-p1", WorkspaceID: "orchard"}, "x", ErrInvalid},
		{"no login", "", toPane("w1-p1"), "x", ErrInvalid},
		{"unknown pane", owner, toPane("w9-p9"), "x", ErrNotFound},
	} {
		if _, err := r.q.Add(Author{Login: tc.login}, tc.t, tc.text); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	if _, err := r.q.Add(Author{Login: owner}, toPane("w1-p1"), "  "+strings.Repeat("a", 4000)+"\n"); err != nil {
		t.Fatalf("4000 bytes after trim: %v", err)
	}
	for range 19 {
		r.add(toPane("w1-p1"), "more")
	}
	if _, err := r.q.Add(Author{Login: owner}, toPane("w1-p1"), "one too many"); !errors.Is(err, ErrFull) {
		t.Fatalf("21st: %v", err)
	}
	// The limit is per target.
	r.add(Target{WorkspaceID: "orchard"}, "elsewhere")
	if len(r.q.List()) != 21 {
		t.Fatalf("listed %d", len(r.q.List()))
	}
}

func TestOnlyWaitingItemsCanBeRemoved(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "idle", 1))
	first := r.add(toPane("w1-p1"), "first")
	second := r.add(toPane("w1-p1"), "second")
	r.at(5 * time.Second)
	if err := r.q.Remove("friend@example.com", first.ID); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("removing a sent item: %v", err)
	}
	if err := r.q.Remove("friend@example.com", "no-such-item"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removing an unknown item: %v", err)
	}
	before := r.changes
	<-r.q.wake // drain
	if err := r.q.Remove("friend@example.com", second.ID); err != nil {
		t.Fatal(err)
	}
	if len(r.q.wake) != 1 {
		t.Fatal("Remove did not wake Run")
	}
	if r.changes != before+1 {
		t.Fatal("no OnChange after Remove")
	}
	if len(r.q.List()) != 1 {
		t.Fatalf("listed %+v", r.q.List())
	}
	r.q = r.open()
	if len(r.q.List()) != 1 {
		t.Fatal("removal not saved")
	}
}

func TestTrimKeepsTwentyFinishedPerTarget(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "idle", 1))
	seq := uint64(1)
	var ids []string
	for i := range 22 {
		ids = append(ids, r.add(toPane("w1-p1"), fmt.Sprintf("task %d", i)).ID)
		d := time.Duration(i) * time.Minute
		seq++
		r.observe(d, agent("w1-p1", "idle", seq))
		wantPrompts(t, r.at(d+5*time.Second), prompt{"w1-p1", fmt.Sprintf("task %d", i)})
		seq++
		r.observe(d+6*time.Second, agent("w1-p1", "working", seq))
	}
	r.at(23 * time.Minute)
	if len(r.q.items) != 20 || r.q.items[0].ID != ids[2] {
		t.Fatalf("kept %d items, first %s", len(r.q.items), r.q.items[0].ID)
	}
}

// A slow herdr must not stall the hub: Observe, FocusEvent, List and Add
// return while a Prompt is stuck, and the stuck item can't be removed.
func TestSlowPromptDoesNotBlockTheHub(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "idle", 1), agent("w1-p2", "working", 1))
	it := r.add(toPane("w1-p1"), "slow")
	release := make(chan struct{})
	inPrompt := make(chan struct{})
	r.hook = func(string) { close(inPrompt); <-release }
	r.clk.Set(start.Add(5 * time.Second))
	stepped := make(chan struct{})
	go func() { r.q.step(t.Context()); close(stepped) }()
	<-inPrompt

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.q.Observe([]Agent{agent("w1-p1", "idle", 1), agent("w1-p2", "idle", 2)})
		r.q.FocusEvent(start.Add(5 * time.Second))
		_ = r.q.List()
		if _, err := r.q.Add(Author{Login: owner}, toPane("w1-p2"), "meanwhile"); err != nil {
			t.Error(err)
		}
		if err := r.q.Remove(owner, it.ID); !errors.Is(err, ErrNotWaiting) {
			t.Errorf("removing an item mid-send: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the hub-facing calls blocked on a slow Prompt")
	}
	close(release)
	<-stepped
	if r.item(it.ID).State != Sent {
		t.Fatal("not sent")
	}
}

// Run sends once the settle time passes, woken by its timer, and reports
// the change.
func TestRunSendsAndNotifies(t *testing.T) {
	r := newRig(t)
	r.clk.Set(time.Now())
	r.q = r.open() // started now, for the hold after Open
	r.q.cfg.Settle = 20 * time.Millisecond
	r.q.cfg.Now = time.Now // Run's timer runs on the real clock here
	got := make(chan prompt, 1)
	r.q.cfg.Prompt = func(_ context.Context, pane, text string) error { got <- prompt{pane, text}; return nil }
	r.q.Observe([]Agent{agent("w1-p1", "idle", 1)})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go r.q.Run(ctx)
	r.add(toPane("w1-p1"), "go")
	select {
	case p := <-got:
		if p != (prompt{"w1-p1", "go"}) {
			t.Fatalf("prompt %+v", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run never sent")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.mu.Lock()
		n := r.changes
		r.mu.Unlock()
		if n >= 2 { // the Add, then the send
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no OnChange after the send")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A restart from the desk: herdr has sent no focus event yet, but the
// focused pane may be in use, so it is held for the desk time.
func TestFocusedAtStartIsHeld(t *testing.T) {
	r := newRig(t)
	a := agent("w1-p1", "idle", 1)
	a.Focused = true
	r.observe(0, a)
	r.add(toPane("w1-p1"), "later")
	wantPrompts(t, r.at(6*time.Second))
	wantPrompts(t, r.at(4*time.Minute))
	wantPrompts(t, r.at(5*time.Minute), prompt{"w1-p1", "later"})
}

// Which pane is focused arrives only with the hub's re-list after a focus
// event, so every pane is held briefly after one.
func TestEveryPaneHeldJustAfterAFocusEvent(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "idle", 1)) // not focused, as last listed
	r.add(toPane("w1-p1"), "later")
	r.q.FocusEvent(start.Add(4500 * time.Millisecond))
	r.clk.Set(start.Add(5 * time.Second))
	if next := r.q.step(t.Context()); !next.Equal(start.Add(5500 * time.Millisecond)) {
		t.Fatalf("next wake %v, want the end of the re-list hold", next)
	}
	wantPrompts(t, r.at(5500*time.Millisecond), prompt{"w1-p1", "later"})
}

// The re-check compares the whole agent: a pane that moved to another
// workspace mid-step doesn't get that workspace's item.
func TestRecheckComparesTheWorkspace(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "idle", 1), agent("w1-p2", "idle", 1))
	r.add(Target{WorkspaceID: "orchard"}, "task a")
	r.add(Target{WorkspaceID: "orchard"}, "task b")
	r.hook = func(pane string) {
		if pane == "w1-p1" {
			moved := agent("w1-p2", "idle", 1)
			moved.WorkspaceID = "meadow"
			r.q.Observe([]Agent{agent("w1-p1", "idle", 1), moved})
		}
	}
	wantPrompts(t, r.at(5*time.Second), prompt{"w1-p1", "task a"})
}

func dialRefused() error {
	return fmt.Errorf("herdr agent.prompt: %w", &net.OpError{Op: "dial", Net: "unix", Err: syscall.ECONNREFUSED})
}

// herdr unreachable: the item stays waiting with no error, nothing more
// goes until a growing backoff passes, and the list doesn't drain.
func TestUnreachableHerdrKeepsItemsWaiting(t *testing.T) {
	r := newRig(t)
	r.errs = []error{dialRefused(), dialRefused()}
	r.observe(0, agent("w1-p1", "idle", 1), agent("w1-p2", "idle", 1))
	first := r.add(toPane("w1-p1"), "first")
	r.add(toPane("w1-p1"), "second")
	other := r.add(toPane("w1-p2"), "other")
	// The first dial fails; the step sends nothing more.
	wantPrompts(t, r.at(5*time.Second), prompt{"w1-p1", "first"})
	for _, it := range r.q.List() {
		if it.State != Waiting || it.Error != "" || it.SentTo != "" {
			t.Fatalf("after the failed dial %+v", it)
		}
	}
	r.clk.Set(start.Add(6 * time.Second))
	if next := r.q.step(t.Context()); !next.Equal(start.Add(7 * time.Second)) {
		t.Fatalf("next wake %v, want the end of the backoff", next)
	}
	wantPrompts(t, r.at(6*time.Second))
	wantPrompts(t, r.at(7*time.Second), prompt{"w1-p1", "first"}) // fails again: 4 s
	wantPrompts(t, r.at(10*time.Second))
	wantPrompts(t, r.at(11*time.Second), prompt{"w1-p1", "first"}, prompt{"w1-p2", "other"})
	if r.item(first.ID).State != Sent || r.item(other.ID).State != Sent {
		t.Fatalf("after herdr came back %+v", r.q.List())
	}
	if len(r.audit) != 4 {
		t.Fatalf("audit %+v", r.audit)
	}
}

// An error after the request was written may mean herdr has it: the item
// counts as sent, stalls, and is never sent again. herdr didn't answer, so
// nothing else goes until the backoff passes.
func TestErrorAfterTheRequestCountsAsSent(t *testing.T) {
	r := newRig(t)
	r.errs = []error{errors.New("herdr agent.prompt: read response: i/o timeout")}
	r.observe(0, agent("w1-p1", "idle", 1), agent("w1-p2", "idle", 1))
	first := r.add(toPane("w1-p1"), "first")
	r.add(toPane("w1-p2"), "other")
	wantPrompts(t, r.at(5*time.Second), prompt{"w1-p1", "first"})
	if got := r.item(first.ID); got.State != Sent || got.Error != "" {
		t.Fatalf("%+v", got)
	}
	wantPrompts(t, r.at(6*time.Second))
	wantPrompts(t, r.at(7*time.Second), prompt{"w1-p2", "other"})
	wantPrompts(t, r.at(15*time.Second))
	if r.item(first.ID).State != Stalled {
		t.Fatalf("%+v", r.item(first.ID))
	}
	wantPrompts(t, r.at(time.Minute))
}

// The file has the item as sent while its Prompt runs: a crash then (no
// chance to save the result) restarts with it sent, and it stalls instead
// of going again.
func TestSavedAsSentBeforeThePrompt(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "idle", 1), agent("w1-p2", "idle", 1))
	one := r.add(toPane("w1-p1"), "one")
	two := r.add(toPane("w1-p2"), "two")
	var mid []byte
	r.hook = func(pane string) {
		if pane != "w1-p2" {
			return
		}
		b, err := os.ReadFile(filepath.Join(r.dir, dirName, fileName))
		if err != nil {
			t.Error(err)
		}
		mid = b
	}
	wantPrompts(t, r.at(5*time.Second), prompt{"w1-p1", "one"}, prompt{"w1-p2", "two"})

	// Restart from the file as it was during w1-p2's Prompt.
	r2 := &rig{t: t, dir: t.TempDir(), clk: r.clk}
	if err := os.MkdirAll(filepath.Join(r2.dir, dirName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r2.dir, dirName, fileName), mid, 0o600); err != nil {
		t.Fatal(err)
	}
	r2.q = r2.open()
	for _, id := range []string{one.ID, two.ID} {
		if got := r2.item(id); got.State != Sent || got.SentAt == nil || got.SentTo == "" {
			t.Fatalf("on disk mid-Prompt %+v", got)
		}
	}
	r2.observe(6*time.Second, agent("w1-p1", "idle", 1), agent("w1-p2", "idle", 1))
	wantPrompts(t, r2.at(6*time.Minute))
	if r2.item(two.ID).State != Stalled {
		t.Fatalf("%+v", r2.item(two.ID))
	}
}

// A closed pane's finished items go after 7 days, whatever their state.
func TestClosedPaneFinishedItemsAreForgotten(t *testing.T) {
	r := newRig(t)
	r.observe(0, agent("w1-p1", "idle", 1))
	r.add(toPane("w1-p1"), "go")
	r.at(5 * time.Second)
	r.observe(6*time.Second, agent("w1-p1", "working", 2))
	r.observe(time.Minute) // the pane closed
	r.at(time.Minute + 6*24*time.Hour)
	if len(r.q.items) != 1 {
		t.Fatalf("kept %d items before 7 days", len(r.q.items))
	}
	r.at(6*time.Second + 7*24*time.Hour)
	if len(r.q.items) != 0 {
		t.Fatalf("kept %+v", r.q.items)
	}
}

// The real client's error when herdr's socket is gone is a failed dial: the
// item stays waiting.
func TestMissingSocketKeepsItemWaiting(t *testing.T) {
	r := newRig(t)
	r.q.cfg.Prompt = herdr.NewClient(filepath.Join(t.TempDir(), "none.sock")).Prompt
	r.observe(0, agent("w1-p1", "idle", 1))
	it := r.add(toPane("w1-p1"), "go")
	r.at(5 * time.Second)
	if got := r.item(it.ID); got.State != Waiting || got.Error != "" || got.SentTo != "" {
		t.Fatalf("%+v", got)
	}
	if r.q.holdUntil.IsZero() {
		t.Fatal("no backoff after a failed dial")
	}
}
