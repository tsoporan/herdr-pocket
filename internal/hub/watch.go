package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/answer"
	"github.com/tsoporan/herdr-pocket/internal/herdr"
)

// Watched panes. herdr has no output-changed event, so each watched pane has
// one poller: it rebuilds the Detail every detailEvery (and at once after a
// change to the agent's record), and reads the visible screen every
// outputEvery while some watcher shows the terminal or the agent is
// terminal-source. Each is sent only when it differs from the last one, and
// the last of each is sent at once to a new watcher. Output goes only to
// watchers showing the terminal.
const (
	detailEvery = time.Second
	outputEvery = 250 * time.Millisecond
)

type paneWatch struct {
	cancel context.CancelFunc
	wake   chan struct{} // rebuild the detail now
	subs   map[*Subscription]struct{}

	detail     *Detail // last sent
	detailJSON []byte
	output     *Output     // last sent
	screen     *screenRead // last successful screen read
	readErr    string      // last screen read error, logged once
	recent     string      // last recent-unwrapped read, for a terminal-source agent
	recentErr  string      // last recent-unwrapped read error, logged once
}

// screenRead is one read of the visible screen.
type screenRead struct {
	text  string
	at    time.Time
	epoch uint64 // the agent's epoch when it was read
}

func (pw *paneWatch) wakeDetail() {
	select {
	case pw.wake <- struct{}{}:
	default:
	}
}

// Watch starts sending s the pane's Detail and Output frames.
func (s *Subscription) Watch(paneID string) {
	h := s.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[s]; !ok || s.watching[paneID] || h.records[paneID] == nil {
		return
	}
	s.watching[paneID] = true
	pw := h.watched[paneID]
	if pw == nil {
		ctx, cancel := context.WithCancel(h.ctx)
		pw = &paneWatch{cancel: cancel, wake: make(chan struct{}, 1), subs: map[*Subscription]struct{}{}}
		h.watched[paneID] = pw
		go h.poll(ctx, paneID, pw)
	}
	pw.subs[s] = struct{}{}
	if pw.detail != nil {
		h.sendLocked(s, *pw.detail)
	}
	if pw.output != nil && s.terminal[paneID] {
		h.sendLocked(s, *pw.output)
	}
}

// Unwatch stops the pane's frames to s.
func (s *Subscription) Unwatch(paneID string) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	s.h.unwatchLocked(s, paneID)
}

// ShowTerminal says whether the app shows the pane's terminal, so the hub
// polls its screen. It may come before or after Watch.
func (s *Subscription) ShowTerminal(paneID string, shown bool) {
	h := s.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[s]; !ok {
		return
	}
	if !shown {
		delete(s.terminal, paneID)
		return
	}
	if s.terminal[paneID] {
		return
	}
	s.terminal[paneID] = true
	// The screen may not change for a while; send the last one now.
	if pw := h.watched[paneID]; pw != nil && pw.output != nil && s.watching[paneID] {
		h.sendLocked(s, *pw.output)
	}
}

// unwatchLocked removes s from the pane's watchers, stopping the poller with
// the last one. Callers hold h.mu.
func (h *Hub) unwatchLocked(s *Subscription, paneID string) {
	if !s.watching[paneID] {
		return
	}
	delete(s.watching, paneID)
	delete(s.terminal, paneID)
	pw := h.watched[paneID]
	if pw == nil {
		return
	}
	delete(pw.subs, s)
	if len(pw.subs) == 0 {
		pw.cancel()
		delete(h.watched, paneID)
	}
}

func (h *Hub) poll(ctx context.Context, paneID string, pw *paneWatch) {
	details := time.NewTicker(detailEvery)
	defer details.Stop()
	outputs := time.NewTicker(outputEvery)
	defer outputs.Stop()
	h.pollOutput(ctx, paneID, pw)
	h.pollDetail(ctx, paneID, pw)
	for {
		select {
		case <-ctx.Done():
			return
		case <-pw.wake:
			h.pollDetail(ctx, paneID, pw)
		case <-details.C:
			h.pollDetail(ctx, paneID, pw)
		case <-outputs.C:
			h.pollOutput(ctx, paneID, pw)
		}
	}
}

// needsOutputLocked reports whether anyone needs the pane's screen. Callers
// hold h.mu.
func (h *Hub) needsOutputLocked(paneID string, pw *paneWatch) bool {
	if h.records[paneID] == nil {
		return false
	}
	if pw.detail == nil || pw.detail.Source == SourceTerminal {
		return true // terminal-source, or not known yet
	}
	for s := range pw.subs {
		if s.terminal[paneID] {
			return true
		}
	}
	return false
}

// readScreen reads the pane's visible screen and keeps it in pw.screen. A
// failure is logged once until a read succeeds.
func (h *Hub) readScreen(ctx context.Context, paneID string, pw *paneWatch, epoch uint64) (string, error) {
	res, err := h.client.Read(ctx, paneID, herdr.SourceVisible, 0)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		if ctx.Err() == nil && err.Error() != pw.readErr {
			log.Printf("hub: read screen of %s: %v", paneID, err)
		}
		pw.readErr = err.Error()
		return "", err
	}
	pw.readErr = ""
	pw.screen = &screenRead{text: res.Text, at: time.Now(), epoch: epoch}
	return res.Text, nil
}

func (h *Hub) pollOutput(ctx context.Context, paneID string, pw *paneWatch) {
	h.mu.Lock()
	need := h.needsOutputLocked(paneID, pw)
	var epoch uint64
	if r := h.records[paneID]; r != nil {
		epoch = r.epoch
	}
	h.mu.Unlock()
	if !need {
		return
	}
	text, err := h.readScreen(ctx, paneID, pw, epoch)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.watched[paneID] != pw {
		return // unwatched meanwhile
	}
	if pw.output != nil && pw.output.Text == text {
		return
	}
	o := Output{Type: "output", PaneID: paneID, Text: text}
	pw.output = &o
	// Only to apps showing the terminal: a terminal-source agent's screen is
	// read for its detail anyway, and four whole screens a second to a phone
	// that doesn't show them can back up a slow link until the hub drops it.
	for s := range pw.subs {
		if s.terminal[paneID] {
			h.sendLocked(s, o)
		}
	}
}

func (h *Hub) pollDetail(ctx context.Context, paneID string, pw *paneWatch) {
	h.mu.Lock()
	r := h.records[paneID]
	if r == nil {
		h.mu.Unlock()
		return
	}
	a, epoch := r.raw, r.epoch
	// The output poll may have just read the screen; reuse a read from this
	// epoch that is newer than one output tick instead of reading again.
	var cached *screenRead
	if sr := pw.screen; sr != nil && sr.epoch == epoch && time.Since(sr.at) < outputEvery {
		cached = sr
	}
	keep := pw.detail != nil && pw.detail.Question != nil
	h.mu.Unlock()

	snap := h.readTranscript(a)
	// A terminal-source agent's last message comes from recent scrollback,
	// reflowed. A failed read keeps the last one.
	var recent *string
	if snap == nil {
		res, err := h.client.Read(ctx, paneID, herdr.SourceRecentUnwrapped, recentReadLines)
		h.mu.Lock()
		switch {
		case err == nil:
			recent, pw.recentErr = &res.Text, ""
		case ctx.Err() == nil && err.Error() != pw.recentErr:
			log.Printf("hub: read recent lines of %s: %v", paneID, err)
			pw.recentErr = err.Error()
		}
		h.mu.Unlock()
	}
	var q *answer.Question
	if a.AgentStatus == herdr.StatusBlocked {
		var text string
		var err error
		if cached != nil {
			text = cached.text
		} else {
			text, err = h.readScreen(ctx, paneID, pw, epoch)
		}
		if err != nil && (keep || ctx.Err() != nil) {
			return // keep the last question; readScreen logged the error
		}
		// On a failed first read the question is unknown and empty, so the
		// app still gets a detail (and the quick keys).
		q = question(a, epoch, text, snap)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.watched[paneID] != pw || h.records[paneID] != r {
		return
	}
	if r.epoch != epoch {
		pw.wakeDetail() // the status changed while reading: start over
		return
	}
	if h.applyTranscriptLocked(paneID, snap) {
		h.publish()
	}
	screen := ""
	if pw.output != nil {
		screen = pw.output.Text
	}
	if recent != nil {
		pw.recent = *recent
	}
	d := buildDetail(r.raw, r.since(), snap, q, screen, pw.recent, h.now())
	b, err := json.Marshal(d)
	if err != nil {
		log.Printf("hub: detail of %s: %v", paneID, err)
		return
	}
	if bytes.Equal(b, pw.detailJSON) {
		return
	}
	pw.detail, pw.detailJSON = &d, b
	for s := range pw.subs {
		h.sendLocked(s, d)
	}
}
