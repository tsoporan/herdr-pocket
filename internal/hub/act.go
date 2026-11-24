package hub

import (
	"context"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/answer"
	"github.com/tsoporan/herdr-pocket/internal/herdr"
)

// What the server needs to act on an agent safely. The hub never sends
// input itself.

// Has reports whether paneID is a known agent pane. The server sends input
// only to these, never to a herdr agent name.
func (h *Hub) Has(paneID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.records[paneID] != nil
}

// HasWorkspace reports whether the hub lists the workspace.
func (h *Hub) HasWorkspace(workspaceID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.rawWS[workspaceID]
	return ok
}

// Blocked reports whether the hub sees the pane's agent as blocked.
func (h *Hub) Blocked(paneID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.records[paneID]
	return r != nil && r.raw.AgentStatus == herdr.StatusBlocked
}

// Question re-reads a blocked agent's screen now and returns its question,
// built exactly as the detail builds it. It returns nil when the agent is
// not blocked, and ErrNotFound for an unknown pane.
func (h *Hub) Question(ctx context.Context, paneID string) (*answer.Question, error) {
	h.mu.Lock()
	r := h.records[paneID]
	if r == nil {
		h.mu.Unlock()
		return nil, ErrNotFound
	}
	a, epoch := r.raw, r.epoch
	h.mu.Unlock()
	if a.AgentStatus != herdr.StatusBlocked {
		return nil, nil
	}
	snap := h.readTranscript(a)
	res, err := h.client.Read(ctx, paneID, herdr.SourceVisible, 0)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	changed := h.records[paneID] != r || r.epoch != epoch
	h.mu.Unlock()
	if changed {
		return nil, nil
	}
	return question(a, epoch, res.Text, snap), nil
}

// claimFor is how long an answered question stays claimed. Right after the
// digit is sent the hub may still see the agent blocked on the same screen;
// once the screen has had time to change, the same id on screen is a new,
// identical prompt the hub saw no status change for.
const claimFor = 5 * time.Second

type claim struct {
	questionID string
	at         time.Time
}

// ClaimAnswer records that question id of the pane is being answered. It
// reports false if it already was within claimFor, so a second tap must not
// answer whatever comes next.
func (h *Hub) ClaimAnswer(paneID, questionID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if c, ok := h.answered[paneID]; ok && c.questionID == questionID && now.Sub(c.at) < claimFor {
		return false
	}
	h.answered[paneID] = claim{questionID: questionID, at: now}
	return true
}

// ReleaseAnswer undoes ClaimAnswer after herdr refused the digit, so the
// question can be answered again.
func (h *Hub) ReleaseAnswer(paneID, questionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.answered[paneID].questionID == questionID {
		delete(h.answered, paneID)
	}
}

// WaitStatus waits until done accepts the pane's status, or ctx ends. It
// returns the last status seen.
func (h *Hub) WaitStatus(ctx context.Context, paneID string, done func(herdr.Status) bool) (herdr.Status, error) {
	for {
		h.mu.Lock()
		r := h.records[paneID]
		if r == nil {
			h.mu.Unlock()
			return "", ErrNotFound
		}
		status, changed := r.raw.AgentStatus, h.changed
		h.mu.Unlock()
		if done(status) {
			return status, nil
		}
		select {
		case <-ctx.Done():
			return status, ctx.Err()
		case <-changed:
		}
	}
}
