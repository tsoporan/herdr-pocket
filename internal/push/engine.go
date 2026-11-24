package push

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"sync"
	"time"
)

// The engine is stateless between steps apart from what it records: each
// step looks at every agent and subscription and decides to send now, hold
// until a time, or do nothing. A hold (cooldown, desk, quiet hours) is
// re-checked on every step, so a held alert goes out when its window ends if
// the agent is still in the same state with the same seq, and is dropped
// otherwise.
//
// The alerts still waiting (settling or held) are saved in pending.json, by
// subscription, pane and status: seqs are the hub's and restart with the
// process, so after a restart the baseline lets a waiting alert through if
// the agent is still in that status, and marks everything else as seen.

const (
	statusBlocked = "blocked"
	statusDone    = "done"
	idleWake      = time.Hour // the longest Run sleeps; expiry checks ride on it
	expiryLabel7d = "7d"
	expiryLabel1d = "1d"
)

// paneState is the last observed agent and when its (status, seq) began.
type paneState struct {
	agent Agent
	since time.Time
}

type slotKey struct{ sub, pane string }

// slot is the alert state for one subscription and one agent.
type slot struct {
	alerted       bool // an alert for (alertedStatus, alertedSeq) was decided
	alertedStatus string
	alertedSeq    uint64
	lastAlert     time.Time // for the cooldown
	shown         bool      // a blocked alert went out and is believed shown
	shownAgent    Agent     // the agent as it was alerted, for the clear
}

func (sl *slot) markAlerted(a Agent) {
	sl.alerted, sl.alertedStatus, sl.alertedSeq = true, a.Status, a.Seq
}

func (sl *slot) alertedFor(a Agent) bool {
	return sl.alerted && sl.alertedStatus == a.Status && sl.alertedSeq == a.Seq
}

// expiryMarks records, per subscription, the strongest expiry push sent for
// one expiry value.
type expiryMarks struct {
	Expiry time.Time         `json:"expiry"`
	Sent   map[string]string `json:"sent"` // subscription id -> "7d" or "1d"
}

// pendingAlert is one alert still waiting, as saved in pending.json.
type pendingAlert struct {
	Sub    string `json:"sub"`
	Pane   string `json:"pane"`
	Status string `json:"status"`
}

// job is one push to send.
type job struct {
	subID    string
	endpoint string
	keys     Keys
	msg      message
	agent    Agent // for blocked alerts, recorded as shown on success
	who      Who   // for blocked alerts, whether it may carry answer buttons
}

// Observe records the full current agent set. Call it after every hub change,
// and only once the agents have been listed: the first call is a baseline
// (states already there are not alerted), and a pane missing from a later
// set has gone, so its shown alert is cleared and its state forgotten. An
// empty set while herdr is unreachable would make every blocked agent alert
// again when it comes back.
func (s *Service) Observe(agents []Agent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now()
	seen := make(map[string]bool, len(agents))
	for _, a := range agents {
		seen[a.PaneID] = true
		p := s.panes[a.PaneID]
		if p == nil || p.agent.Status != a.Status || p.agent.Seq != a.Seq {
			p = &paneState{since: now}
			s.panes[a.PaneID] = p
		}
		p.agent = a
		if !s.observed {
			for id := range s.subs {
				if s.restored[slotKey{id, a.PaneID}] != a.Status {
					s.slot(id, a.PaneID).markAlerted(a)
				}
			}
		}
	}
	for id := range s.panes {
		if !seen[id] {
			delete(s.panes, id)
		}
	}
	s.observed = true
	s.restored = nil
	s.poke()
}

// FocusEvent records a herdr focus event, or an agent.focus from the app.
func (s *Service) FocusEvent(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if at.After(s.lastFocus) {
		s.lastFocus = at
	}
	s.poke()
}

// Seen records that owner opened the agent in the app on node (a tap on its
// notification opens it, and closes the notification): no clear is sent to
// that device's subscriptions for the alert it was shown. The app open there
// closes the notification itself once the agent leaves blocked.
func (s *Service) Seen(owner, node, paneID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, sl := range s.slots {
		if r := s.subs[k.sub]; k.pane == paneID && r != nil && r.Login == owner && r.Node == node {
			sl.shown = false
		}
	}
}

// SetExpiry sets the node key's expiry; nil means expiry is off.
func (s *Service) SetExpiry(at *time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if at == nil {
		s.expiry = nil
	} else {
		t := *at
		s.expiry = &t
	}
	s.poke()
}

// Test sends a test push to owner's subscription now, ignoring quiet hours
// and desk suppression.
func (s *Service) Test(ctx context.Context, owner, endpoint string) error {
	s.mu.Lock()
	r, err := s.ownedLocked(owner, endpoint)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	j := job{subID: r.ID, endpoint: r.Endpoint, keys: r.Keys, msg: testMessage()}
	s.mu.Unlock()
	return s.deliver(ctx, j)
}

// Run drives the timers (settle, held alerts, expiry) until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.wake:
		}
		wait := idleWake
		if next := s.step(ctx); !next.IsZero() {
			wait = min(max(next.Sub(s.cfg.Now()), 0), idleWake)
		}
		t.Reset(wait)
	}
}

// poke wakes Run without blocking. Callers may hold mu.
func (s *Service) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) slot(sub, pane string) *slot {
	k := slotKey{sub, pane}
	sl := s.slots[k]
	if sl == nil {
		sl = &slot{}
		s.slots[k] = sl
	}
	return sl
}

// step decides and sends everything due now, and returns the earliest time
// something held or settling becomes due (zero when nothing is waiting).
func (s *Service) step(ctx context.Context) time.Time {
	s.stepMu.Lock()
	defer s.stepMu.Unlock()

	s.mu.Lock()
	now := s.cfg.Now()
	var next time.Time
	soonest := func(t time.Time) {
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	var jobs []job

	// Clears first: an agent that left blocked (or vanished) gets one silent
	// clear per shown alert. One blocked again with a new seq keeps its
	// notification up until the new alert replaces it (same tag).
	for k, sl := range s.slots {
		r := s.subs[k.sub]
		p := s.panes[k.pane]
		if sl.shown && (p == nil || p.agent.Status != statusBlocked) {
			sl.shown = false
			if r != nil {
				jobs = append(jobs, job{subID: r.ID, endpoint: r.Endpoint, keys: r.Keys, msg: clearMessage(sl.shownAgent)})
			}
		}
		if r == nil || (p == nil && !sl.shown) {
			delete(s.slots, k)
		}
	}

	// Alerts.
	pending := map[slotKey]string{}
	for _, p := range s.panes {
		a := p.agent
		if a.Status != statusBlocked && a.Status != statusDone {
			continue
		}
		settled := p.since.Add(s.cfg.Settle)
		settling := now.Before(settled)
		if settling {
			soonest(settled)
		}
		for _, r := range s.subs {
			sl := s.slot(r.ID, a.PaneID)
			if sl.alertedFor(a) {
				continue
			}
			if settling {
				if wants(r.Settings, a.Status) {
					pending[slotKey{r.ID, a.PaneID}] = a.Status
				}
				continue
			}
			if !wants(r.Settings, a.Status) {
				// Decided: turning the trigger on later must not alert
				// for a state that is already old.
				sl.markAlerted(a)
				continue
			}
			if until := s.holdLocked(r, sl, a, now); !until.IsZero() {
				pending[slotKey{r.ID, a.PaneID}] = a.Status
				soonest(until)
				continue
			}
			// Marked before the send, so a dead endpoint is not retried
			// every step.
			sl.markAlerted(a)
			sl.lastAlert = now
			jobs = append(jobs, job{subID: r.ID, endpoint: r.Endpoint, keys: r.Keys, msg: alertMessage(a), agent: a, who: r.who()})
		}
	}

	expiryJobs, marksChanged, expiryNext := s.expiryLocked(now)
	jobs = append(jobs, expiryJobs...)
	if !expiryNext.IsZero() {
		soonest(expiryNext)
	}
	var marks expiryMarks
	if marksChanged {
		marks = expiryMarks{Expiry: s.marks.Expiry, Sent: maps.Clone(s.marks.Sent)}
	}
	var pendingList []pendingAlert
	// Before the baseline nothing is known yet; keep what Open read.
	pendingChanged := s.observed && !maps.Equal(pending, s.savedPending)
	if pendingChanged {
		s.savedPending = pending
		pendingList = make([]pendingAlert, 0, len(pending))
		for k, status := range pending {
			pendingList = append(pendingList, pendingAlert{Sub: k.sub, Pane: k.pane, Status: status})
		}
		slices.SortFunc(pendingList, func(a, b pendingAlert) int {
			return cmp.Or(cmp.Compare(a.Sub, b.Sub), cmp.Compare(a.Pane, b.Pane))
		})
	}
	s.mu.Unlock()

	if marksChanged {
		if err := writeJSON(s.dir, expiryFile, marks); err != nil {
			s.cfg.Logf("push: saving %s: %v", expiryFile, err)
		}
	}
	if pendingChanged {
		if err := writeJSON(s.dir, pendingFile, pendingList); err != nil {
			s.cfg.Logf("push: saving %s: %v", pendingFile, err)
		}
	}
	s.addAnswers(ctx, jobs)
	s.send(ctx, jobs)
	return next
}

// send delivers jobs with one sender per subscription, so a slow push
// service holds up only its own device; each device's jobs keep their order
// (a clear before a new alert). It returns when all are done.
func (s *Service) send(ctx context.Context, jobs []job) {
	bySub := map[string][]job{}
	for _, j := range jobs {
		bySub[j.subID] = append(bySub[j.subID], j)
	}
	var wg sync.WaitGroup
	for _, js := range bySub {
		wg.Go(func() {
			for _, j := range js {
				if ctx.Err() != nil {
					return
				}
				err := s.deliver(ctx, j)
				if errors.Is(err, ErrNotFound) {
					return // the subscription is gone
				}
				if err == nil && j.msg.Kind == kindBlocked {
					s.mu.Lock()
					if _, ok := s.subs[j.subID]; ok {
						sl := s.slot(j.subID, j.agent.PaneID)
						sl.shown, sl.shownAgent = true, j.agent
					}
					s.mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
}

func wants(set Settings, status string) bool {
	switch status {
	case statusBlocked:
		return set.Blocked
	case statusDone:
		return set.Done
	}
	return false
}

// holdLocked returns when the alert may go out, or zero if it may go now.
// Every window that applies is checked; the latest end wins, and the next
// step checks them all again.
func (s *Service) holdLocked(r *record, sl *slot, a Agent, now time.Time) time.Time {
	var until time.Time
	later := func(t time.Time) {
		if now.Before(t) && t.After(until) {
			until = t
		}
	}
	if !sl.lastAlert.IsZero() {
		later(sl.lastAlert.Add(s.cfg.Cooldown))
	}
	if d := r.Settings.DeskMinutes; d > 0 && a.Focused && !s.lastFocus.IsZero() {
		later(s.lastFocus.Add(time.Duration(d) * time.Minute))
	}
	if r.quiet != nil {
		if end, inside := r.quiet.endIfInside(now); inside {
			later(end)
		}
	}
	return until
}

// expiryLocked returns the expiry pushes due now, whether the marks changed,
// and when the next threshold is crossed. Quiet hours hold them per device.
func (s *Service) expiryLocked(now time.Time) ([]job, bool, time.Time) {
	if s.expiry == nil || !s.expiry.After(now) {
		return nil, false, time.Time{}
	}
	at := *s.expiry
	changed := false
	if !s.marks.Expiry.Equal(at) {
		s.marks = expiryMarks{Expiry: at, Sent: map[string]string{}}
		changed = true
	}
	if s.marks.Sent == nil {
		s.marks.Sent = map[string]string{}
	}
	var next time.Time
	soonest := func(t time.Time) {
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	days := daysLeft(at, now)
	label := ""
	switch {
	case days <= 1:
		label = expiryLabel1d
	case days <= 7:
		label = expiryLabel7d
		soonest(at.Add(-24 * time.Hour))
	default:
		soonest(at.Add(-7 * 24 * time.Hour))
	}
	var jobs []job
	if label != "" {
		for _, r := range s.subs {
			prev := s.marks.Sent[r.ID]
			if prev == label || prev == expiryLabel1d {
				continue
			}
			if r.quiet != nil {
				if end, inside := r.quiet.endIfInside(now); inside {
					soonest(end)
					continue
				}
			}
			s.marks.Sent[r.ID] = label
			changed = true
			jobs = append(jobs, job{subID: r.ID, endpoint: r.Endpoint, keys: r.Keys, msg: expiryMessage(days)})
		}
	}
	for id := range s.marks.Sent {
		if _, ok := s.subs[id]; !ok {
			delete(s.marks.Sent, id)
			changed = true
		}
	}
	return jobs, changed, next
}

// daysLeft is whole days until t, rounded up, as the app counts them
// (hub.DaysLeft; copied to keep push free of the hub).
func daysLeft(t, now time.Time) int { return int(math.Ceil(t.Sub(now).Hours() / 24)) }

// window is a parsed quiet-hours window, in minutes after local midnight.
type window struct {
	start, end int
	loc        *time.Location
}

func parseWindow(q Quiet) (*window, error) {
	start, err := parseClock(q.Start)
	if err != nil {
		return nil, fmt.Errorf("quiet start: %w", err)
	}
	end, err := parseClock(q.End)
	if err != nil {
		return nil, fmt.Errorf("quiet end: %w", err)
	}
	if q.TZ == "" || q.TZ == "Local" {
		return nil, errors.New("quiet tz must be an IANA zone")
	}
	loc, err := time.LoadLocation(q.TZ)
	if err != nil {
		return nil, fmt.Errorf("quiet tz: %w", err)
	}
	return &window{start: start, end: end, loc: loc}, nil
}

// parseClock parses "HH:MM" (24-hour) into minutes after midnight.
func parseClock(v string) (int, error) {
	if len(v) != 5 || v[2] != ':' {
		return 0, fmt.Errorf("%q is not HH:MM", v)
	}
	h, err1 := strconv.Atoi(v[:2])
	m, err2 := strconv.Atoi(v[3:])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("%q is not HH:MM", v)
	}
	return h*60 + m, nil
}

// endIfInside reports whether now is inside the window and, if so, when the
// window ends. A start equal to its end is an empty window.
func (w *window) endIfInside(now time.Time) (time.Time, bool) {
	t := now.In(w.loc)
	m := t.Hour()*60 + t.Minute()
	var inside bool
	switch {
	case w.start < w.end:
		inside = m >= w.start && m < w.end
	case w.start > w.end: // crosses midnight
		inside = m >= w.start || m < w.end
	}
	if !inside {
		return time.Time{}, false
	}
	// Count the minutes left on now's own wall clock, so the second pass
	// through a repeated hour (DST fall-back) ends this time round, not a
	// day later. If a DST jump lies between, that lands off the end time;
	// then the next instant the zone calls w.end is it.
	left := (w.end - m + 24*60) % (24 * 60)
	end := now.Add(time.Duration(left)*time.Minute - time.Duration(t.Second())*time.Second - time.Duration(t.Nanosecond()))
	if e := end.In(w.loc); e.Hour()*60+e.Minute() == w.end {
		return end, true
	}
	y, mo, d := t.Date()
	end = time.Date(y, mo, d, w.end/60, w.end%60, 0, 0, w.loc)
	if !end.After(now) {
		end = time.Date(y, mo, d+1, w.end/60, w.end%60, 0, 0, w.loc)
	}
	return end, true
}
