package queue

import (
	"cmp"
	"context"
	"errors"
	"net"
	"slices"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/herdr"
)

// The rules:
//
//   - A pane item goes when its agent is idle or done, has been in that
//     (status, seq) for the settle time, isn't at the desk (focused, and a
//     focus event within the desk time), and has no item in flight (sent,
//     or with its Prompt under way). The oldest waiting item goes first.
//   - A workspace item goes to a free agent there: idle only, settled, not
//     at the desk, nothing in flight and no waiting items of its own; the
//     longest idle first (ties by pane id). An agent gets one item per step.
//   - Just after any focus event every pane is held, until the hub has
//     re-listed which one is focused. Startup counts as a focus event, so a
//     restart from the desk never types into the pane in use.
//   - The state is checked again right before each Prompt, and the item is
//     saved as sent before it, so a crash mid-Prompt never sends it twice.
//   - Sent becomes picked_up when the agent is seen working or blocked, or
//     stalled after the stall time. Only herdr's own refusal (a
//     *herdr.Error) is failed, except that agent_blocked puts the item back
//     to waiting once. With no answer from herdr nothing more goes until a
//     backoff passes; after a failed dial the item stays waiting, but any
//     other error may have reached herdr, so it counts as sent and picks up
//     or stalls.
//   - A closed pane drops its waiting items and its item in flight.

const (
	idleWake = time.Hour // the longest Run sleeps; trimming rides on it
	// focusRelist holds every pane after a focus event: herdr says which
	// pane is focused only in the hub's re-list (relistWait, 150 ms) after.
	focusRelist = time.Second
	minBackoff  = 2 * time.Second // after herdr couldn't be reached
	maxBackoff  = time.Minute
)

const (
	statusIdle    = "idle"
	statusDone    = "done"
	statusWorking = "working"
	statusBlocked = "blocked"
)

func active(status string) bool { return status == statusWorking || status == statusBlocked }

// Observe records the full current agent set. Call it after every hub change
// and only once the agents have been listed: a pane missing from the set has
// closed, and its waiting items are dropped for good, so an empty set from a
// herdr that is merely unreachable would drop every pane's queue. It does no
// I/O and never blocks on a send.
func (q *Queue) Observe(agents []Agent) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.cfg.Now()
	seen := make(map[string]bool, len(agents))
	for _, a := range agents {
		seen[a.PaneID] = true
		p := q.panes[a.PaneID]
		if p == nil || p.agent.Status != a.Status || p.agent.Seq != a.Seq {
			p = &paneState{since: now}
			q.panes[a.PaneID] = p
		}
		p.agent = a
		if active(a.Status) {
			for _, m := range q.sending {
				if m.pane == a.PaneID {
					m.active = true
				}
			}
		}
	}
	for id := range q.panes {
		if !seen[id] {
			delete(q.panes, id)
		}
	}
	q.observed = true
	for _, it := range q.items {
		switch it.State {
		case Sent:
			if p := q.panes[it.SentTo]; p == nil {
				q.endLocked(it, Dropped, now)
			} else if active(p.agent.Status) {
				q.pickedUpLocked(it, now)
			}
		case Waiting:
			if it.Target.PaneID != "" && q.panes[it.Target.PaneID] == nil {
				q.endLocked(it, Dropped, now)
			}
		}
	}
	q.poke()
}

// FocusEvent records a herdr focus event, or an agent.focus from the app.
func (q *Queue) FocusEvent(at time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if at.After(q.lastFocus) {
		q.lastFocus = at
	}
	q.poke()
}

// Run sends items as their agents come free, until ctx ends.
func (q *Queue) Run(ctx context.Context) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-q.wake:
		}
		wait := idleWake
		if next := q.step(ctx); !next.IsZero() {
			wait = min(max(next.Sub(q.cfg.Now()), 0), idleWake)
		}
		t.Reset(wait)
	}
}

// poke wakes Run without blocking. Callers may hold mu.
func (q *Queue) poke() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *Queue) endLocked(it *Item, s State, now time.Time) {
	it.State = s
	it.EndedAt = &now
	delete(q.retried, it.ID)
	q.dirty = true
}

func (q *Queue) pickedUpLocked(it *Item, now time.Time) {
	it.PickedUpAt = &now
	q.endLocked(it, PickedUp, now)
}

// job is one decided send: the item, the pane, and the agent as it was
// when decided, for the re-check.
type job struct {
	id       string
	pane     string
	agent    Agent
	idleOnly bool // a workspace item: the agent must be idle, not done
}

// step marks stalls, trims, then sends what is due. It returns the earliest
// time something settling, held or sent becomes due (zero when none is).
func (q *Queue) step(ctx context.Context) time.Time {
	q.stepMu.Lock()
	defer q.stepMu.Unlock()

	q.mu.Lock()
	now := q.cfg.Now()
	var next time.Time
	soonest := func(t time.Time) {
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	for _, it := range q.items {
		// Before the first Observe an item sent before a restart may
		// already have been picked up.
		if it.State == Sent && q.observed {
			if due := it.SentAt.Add(q.cfg.Stall); now.Before(due) {
				soonest(due)
			} else {
				q.endLocked(it, Stalled, now)
			}
		}
	}
	q.trimLocked(now)
	var jobs []job
	if now.Before(q.holdUntil) {
		soonest(q.holdUntil) // herdr couldn't be reached
	} else {
		jobs = q.decideLocked(now, soonest)
	}
	q.mu.Unlock()

	for _, j := range jobs {
		if ctx.Err() != nil {
			break
		}
		due, ok := q.send(ctx, j)
		if !due.IsZero() {
			soonest(due)
		}
		if !ok {
			break // herdr didn't answer, or the list couldn't be saved
		}
	}
	saved, err := q.saveDirty()
	if err != nil {
		q.cfg.Logf("queue: saving %s: %v", fileName, err)
	}
	if saved {
		q.cfg.OnChange() // the list changed, saved or not
	}
	return next
}

// readyLocked reports whether the pane's agent may get an item now, and if
// not, when it might (zero when only a state change can make it ready).
func (q *Queue) readyLocked(p *paneState, now time.Time, idleOnly bool) (time.Time, bool) {
	switch s := p.agent.Status; {
	case s == statusIdle:
	case s == statusDone && !idleOnly:
	default:
		return time.Time{}, false
	}
	if settled := p.since.Add(q.cfg.Settle); now.Before(settled) {
		return settled, false
	}
	desk := focusRelist
	if p.agent.Focused {
		desk = q.cfg.Desk
	}
	if end := q.lastFocus.Add(desk); now.Before(end) {
		return end, false
	}
	return time.Time{}, true
}

// decideLocked picks the sends due now. Callers hold mu.
func (q *Queue) decideLocked(now time.Time, soonest func(time.Time)) []job {
	busy := map[string]bool{}       // panes with an item in flight
	ownWaiting := map[string]bool{} // panes with waiting items of their own
	for _, it := range q.items {
		switch {
		case it.State == Sent:
			busy[it.SentTo] = true
		case it.State == Waiting && it.Target.PaneID != "":
			ownWaiting[it.Target.PaneID] = true
		}
	}
	for _, m := range q.sending {
		busy[m.pane] = true
	}

	var jobs []job
	taken := map[string]bool{} // panes decided this step, sent to or not
	for _, it := range q.items {
		pane := it.Target.PaneID
		if it.State != Waiting || pane == "" || taken[pane] {
			continue
		}
		taken[pane] = true // only the oldest waiting item is considered
		p := q.panes[pane]
		if p == nil || busy[pane] || q.retried[it.ID] == p {
			continue
		}
		if due, ok := q.readyLocked(p, now, false); !ok {
			if !due.IsZero() {
				soonest(due)
			}
			continue
		}
		jobs = append(jobs, job{id: it.ID, pane: pane, agent: p.agent})
	}

	for _, it := range q.items {
		ws := it.Target.WorkspaceID
		if it.State != Waiting || ws == "" {
			continue
		}
		var pick *paneState
		for id, p := range q.panes {
			if p.agent.WorkspaceID != ws || busy[id] || ownWaiting[id] || taken[id] || q.retried[it.ID] == p {
				continue
			}
			if due, ok := q.readyLocked(p, now, true); !ok {
				if !due.IsZero() {
					soonest(due)
				}
				continue
			}
			if pick == nil || cmp.Or(p.since.Compare(pick.since), cmp.Compare(id, pick.agent.PaneID)) < 0 {
				pick = p
			}
		}
		if pick != nil {
			taken[pick.agent.PaneID] = true
			jobs = append(jobs, job{id: it.ID, pane: pick.agent.PaneID, agent: pick.agent, idleOnly: true})
		}
	}
	return jobs
}

// send re-checks a job, saves the item as sent, and prompts outside the
// lock. It returns when the item will stall, if it was sent, and false when
// the step should send nothing more.
func (q *Queue) send(ctx context.Context, j job) (time.Time, bool) {
	// The screen check reads herdr, so it runs before the lock; the re-check
	// under the lock below still decides whether this job is current.
	if q.cfg.Clear != nil && !q.cfg.Clear(ctx, j.pane) {
		q.mu.Lock()
		at := q.cfg.Now().Add(q.cfg.ClearRecheck)
		first := q.heldFor[j.pane] == ""
		q.heldFor[j.pane] = j.id
		q.mu.Unlock()
		if first {
			q.cfg.Logf("queue: item %s held: %s shows a dialog, or its screen couldn't be read", j.id, j.pane)
		}
		return at, true
	}
	q.mu.Lock()
	delete(q.heldFor, j.pane)
	it := q.findLocked(j.id)
	p := q.panes[j.pane]
	now := q.cfg.Now()
	if now.Before(q.holdUntil) {
		q.mu.Unlock()
		return time.Time{}, false
	}
	if it == nil || it.State != Waiting || p == nil || p.agent != j.agent {
		q.mu.Unlock()
		return time.Time{}, true
	}
	if _, ok := q.readyLocked(p, now, j.idleOnly); !ok {
		q.mu.Unlock()
		return time.Time{}, true
	}
	// The author's role is checked again now: access may have changed
	// (a restart with a different --allow) since the item was lined up.
	if q.cfg.CanSend != nil && (it.Author == nil || !q.cfg.CanSend(*it.Author)) {
		it.SentTo, it.Error = j.pane, ErrorForbidden
		q.endLocked(it, Failed, now)
		q.dirty = true
		q.mu.Unlock()
		q.cfg.Logf("queue: item %s not sent: %s may no longer send input", j.id, it.By)
		q.poke() // the agent's next item is due
		return time.Time{}, true
	}
	// The file has it as sent (snapshotLocked) from here on: after a crash
	// it picks up or stalls, and is never sent again.
	mark := &sendMark{pane: j.pane, at: now}
	q.sending[j.id] = mark
	q.dirty = true
	orig := *it
	q.mu.Unlock()

	if _, err := q.saveDirty(); err != nil {
		q.mu.Lock()
		delete(q.sending, j.id)
		q.holdLocked(now)
		q.mu.Unlock()
		q.cfg.Logf("queue: item %s not sent: saving %s: %v", j.id, fileName, err)
		return time.Time{}, false
	}

	// A started Prompt runs to its end even when Run is stopping (herdr's
	// client bounds each request), so its result is recorded.
	err := q.cfg.Prompt(context.WithoutCancel(ctx), j.pane, orig.Text)

	var herr *herdr.Error
	var dial *net.OpError
	answered := err == nil || errors.As(err, &herr)
	unsent := errors.As(err, &dial) && dial.Op == "dial" // a failed dial never reached herdr

	q.mu.Lock()
	delete(q.sending, j.id)
	now = q.cfg.Now()
	var stallAt time.Time
	out := orig
	if it := q.findLocked(j.id); it != nil {
		if it.State == Waiting {
			switch {
			case herr != nil && herr.Code == herdr.CodeAgentBlocked && q.retried[it.ID] == nil:
				// It waits for the agent's state to change before the retry.
				q.retried[it.ID] = p
				q.poke() // a workspace item may go to another free agent
				q.cfg.Logf("queue: item %s: %s is blocked; it waits again", it.ID, j.pane)
			case herr != nil:
				it.SentTo, it.Error = j.pane, herr.Code
				q.endLocked(it, Failed, now)
				q.poke() // the agent's next item is due
			case unsent:
				// herdr never got it: it stays waiting.
				q.cfg.Logf("queue: item %s: herdr unreachable; it waits: %v", it.ID, err)
			default:
				// Accepted, or an error after the request was written, which
				// herdr may have taken: it counts as sent either way.
				if err != nil {
					q.cfg.Logf("queue: item %s: %v; counted as sent", it.ID, err)
				}
				it.State, it.SentAt, it.SentTo, it.Error = Sent, &now, j.pane, ""
				if mark.active {
					q.pickedUpLocked(it, now)
				} else {
					stallAt = now.Add(q.cfg.Stall)
				}
			}
			q.dirty = true
		}
		out = *it
	}
	// No answer from herdr: nothing more goes until a backoff passes.
	if answered {
		q.backoff = 0
	} else {
		q.holdLocked(now)
	}
	q.mu.Unlock()

	if _, err := q.saveDirty(); err != nil {
		q.cfg.Logf("queue: saving %s: %v", fileName, err)
	}
	q.cfg.OnChange()
	out.SentTo = j.pane
	q.cfg.OnSent(out, err)
	return stallAt, answered
}

// holdLocked stops sends for a backoff that doubles while herdr doesn't
// answer (or the list can't be saved), and makes Run's next step wake when
// it ends. Callers hold mu.
func (q *Queue) holdLocked(now time.Time) {
	q.backoff = min(max(2*q.backoff, minBackoff), maxBackoff)
	q.holdUntil = now.Add(q.backoff)
	q.poke()
}

func (q *Queue) findLocked(id string) *Item {
	for _, it := range q.items {
		if it.ID == id {
			return it
		}
	}
	return nil
}

// trimLocked forgets the finished items of a closed pane (dropped ones, or
// any whose pane is gone) after 7 days, and keeps the last 20 finished items
// per agent and per workspace. Callers hold mu.
func (q *Queue) trimLocked(now time.Time) {
	finished := map[Target]int{}
	keep := make([]*Item, 0, len(q.items))
	for i := len(q.items) - 1; i >= 0; i-- {
		it := q.items[i]
		if it.State.Finished() {
			// Before the first Observe no pane is known, so none is gone.
			gone := it.State == Dropped || (q.observed && q.panes[cmp.Or(it.SentTo, it.Target.PaneID)] == nil)
			if gone && it.EndedAt != nil && now.Sub(*it.EndedAt) >= keepDropped {
				continue
			}
			if finished[it.Target]++; finished[it.Target] > keepFinished {
				continue
			}
		}
		keep = append(keep, it)
	}
	if len(keep) == len(q.items) {
		return
	}
	slices.Reverse(keep)
	q.items = keep
	q.dirty = true
}
