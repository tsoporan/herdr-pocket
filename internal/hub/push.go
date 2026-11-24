package hub

import (
	"slices"
	"strings"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/herdr"
	"github.com/tsoporan/herdr-pocket/internal/push"
)

// Pusher is what the hub feeds the push engine (*push.Service): the full
// agent set after every change, desk-activity times and the node key's
// expiry. Each call must return quickly; the hub holds its lock across it,
// so the engine sees changes in order.
type Pusher interface {
	Observe(agents []push.Agent)
	FocusEvent(at time.Time)
	SetExpiry(at *time.Time)
}

// PushInfo is the snapshot's push field: what the app needs to subscribe.
type PushInfo struct {
	VAPIDPublicKey string `json:"vapid_public_key"`
}

// SetPusher attaches the push engine. It hands over the key expiry at once,
// and the agent set once the hub has listed (at once if it has), so it may
// be called before or after Start and SetKeyExpiry.
func (h *Hub) SetPusher(p Pusher) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pusher = p
	p.SetExpiry(h.keyExpiry)
	h.observePushLocked()
}

// FocusEvent records desk activity the hub didn't see as a herdr event,
// such as the app's focus_on_desktop.
func (h *Hub) FocusEvent() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.focusEventLocked()
}

// focusEventLocked tells the push engine and the queue the desktop was
// just used. Callers hold h.mu.
func (h *Hub) focusEventLocked() {
	if h.pusher != nil {
		h.pusher.FocusEvent(h.now())
	}
	if h.queue != nil {
		h.queue.FocusEvent(h.now())
	}
}

// pushName is the push title: the herdr agent name, else the agent kind.
// Unlike Name it never falls back to the terminal title, which the program
// in the pane sets (Claude Code puts its current task there), and a push
// carries no screen text.
func pushName(a herdr.Agent) string {
	for _, s := range []string{a.Name, a.DisplayAgent, a.Agent} {
		if s != "" {
			return s
		}
	}
	return a.PaneID
}

// observeLocked sends the push engine and the queue the full agent set.
// Callers hold h.mu.
func (h *Hub) observeLocked() {
	h.observePushLocked()
	h.observeQueueLocked()
}

// observePushLocked sends the push engine the full agent set, in pane
// order, once the hub has listed. Callers hold h.mu.
func (h *Hub) observePushLocked() {
	if h.pusher == nil || !h.listed {
		return
	}
	h.pushFed = true
	agents := make([]push.Agent, 0, len(h.records))
	for _, r := range h.records {
		agents = append(agents, push.Agent{
			PaneID:    r.raw.PaneID,
			Name:      pushName(r.raw),
			Workspace: h.rawWS[r.raw.WorkspaceID].Label,
			Status:    string(r.raw.AgentStatus),
			Seq:       r.pushSeq,
			Focused:   r.raw.Focused,
		})
	}
	slices.SortFunc(agents, func(a, b push.Agent) int { return strings.Compare(a.PaneID, b.PaneID) })
	h.pusher.Observe(agents)
}
