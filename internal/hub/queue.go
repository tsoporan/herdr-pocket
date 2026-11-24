package hub

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/queue"
)

// Queuer is what the hub feeds the work queue (*queue.Queue): the same agent
// set and desk-activity times as the push engine, called under the hub's
// lock, so each call must return quickly. List is the queue's items, for the
// snapshot and patches; it takes only the queue's short lock.
type Queuer interface {
	Observe(agents []queue.Agent)
	FocusEvent(at time.Time)
	List() []queue.Item
}

// SetQueue attaches the work queue. It hands over the agent set once the hub
// has listed (at once if it has), so it may be called before or after Start.
// The queue's Config.OnChange must call QueueChanged.
func (h *Hub) SetQueue(q Queuer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.queue = q
	h.queueItems = h.queueListLocked()
	h.observeQueueLocked()
}

// QueueChanged tells the hub the queue's items changed; it patches the app.
// It only signals, so the queue may call it from anywhere, Observe and its
// own lock included: the hub's lock is taken by the loop that reads List.
func (h *Hub) QueueChanged() {
	select {
	case h.queueKick <- struct{}{}:
	default:
	}
}

// queueLoop publishes the queue's items each time QueueChanged signals.
func (h *Hub) queueLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.queueKick:
		}
		h.mu.Lock()
		h.publishQueueLocked()
		h.mu.Unlock()
	}
}

// publishQueueLocked patches subscribers with the queue's items when they
// differ from the last published. Callers hold h.mu.
func (h *Hub) publishQueueLocked() {
	items := h.queueListLocked()
	if reflect.DeepEqual(items, h.queueItems) {
		return
	}
	h.queueItems = items
	p := Patch{Type: "patch", Queue: &items}
	for s := range h.subs {
		h.sendLocked(s, p)
	}
}

// queueListLocked is the app's view of the queue (Queuer.List: the
// unfinished items plus those finished in the last 24 h), never nil.
// Callers hold h.mu.
func (h *Hub) queueListLocked() []queue.Item {
	if h.queue == nil {
		return []queue.Item{}
	}
	items := h.queue.List()
	if items == nil {
		items = []queue.Item{}
	}
	return items
}

// observeQueueLocked sends the queue the full agent set, in pane order, once
// the hub has listed. Callers hold h.mu.
func (h *Hub) observeQueueLocked() {
	if h.queue == nil || !h.listed {
		return
	}
	h.queueFed = true
	agents := make([]queue.Agent, 0, len(h.records))
	for _, r := range h.records {
		agents = append(agents, queue.Agent{
			PaneID:      r.raw.PaneID,
			WorkspaceID: r.raw.WorkspaceID,
			Status:      string(r.raw.AgentStatus),
			Seq:         r.pushSeq,
			Focused:     r.raw.Focused,
		})
	}
	slices.SortFunc(agents, func(a, b queue.Agent) int { return strings.Compare(a.PaneID, b.PaneID) })
	h.queue.Observe(agents)
}
