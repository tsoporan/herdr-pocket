package hub_test

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/herdr"
	"github.com/tsoporan/herdr-pocket/internal/hub"
	"github.com/tsoporan/herdr-pocket/internal/queue"
)

// queueRecorder is a hub.Queuer that keeps what the hub fed it and serves
// items the test sets. Its Observe calls back into the hub, as a queue's
// OnChange might: that must not deadlock on the hub's lock.
type queueRecorder struct {
	h       *hub.Hub
	mu      sync.Mutex
	sets    [][]queue.Agent
	focuses int
	items   []queue.Item
	changed chan struct{}
}

func (q *queueRecorder) note() {
	select {
	case q.changed <- struct{}{}:
	default:
	}
}

func (q *queueRecorder) Observe(agents []queue.Agent) {
	q.mu.Lock()
	q.sets = append(q.sets, agents)
	q.mu.Unlock()
	q.h.QueueChanged()
	q.note()
}

func (q *queueRecorder) FocusEvent(time.Time) {
	q.mu.Lock()
	q.focuses++
	q.mu.Unlock()
	q.note()
}

func (q *queueRecorder) List() []queue.Item {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]queue.Item{}, q.items...)
}

func (q *queueRecorder) set(items ...queue.Item) {
	q.mu.Lock()
	q.items = items
	q.mu.Unlock()
	q.h.QueueChanged()
}

func (q *queueRecorder) wait(t *testing.T, what string, ok func(q *queueRecorder) bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		q.mu.Lock()
		done := ok(q)
		q.mu.Unlock()
		if done {
			return
		}
		select {
		case <-q.changed:
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// nextQueuePatch reads the subscription until a patch that carries the queue.
func nextQueuePatch(t *testing.T, sub *hub.Subscription) hub.Patch {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case m, ok := <-sub.C:
			if !ok {
				t.Fatal("subscription closed")
			}
			if p, isPatch := m.(hub.Patch); isPatch && p.Queue != nil {
				return p
			}
		case <-deadline:
			t.Fatal("timed out waiting for a queue patch")
		}
	}
}

func TestHubFeedsQueue(t *testing.T) {
	srv := fake(t)
	h := newHub(srv)
	h.SetPusher(newRecorder()) // push fed first, so the queue's first set isn't the pusher's
	if err := h.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	q := &queueRecorder{h: h, changed: make(chan struct{}, 64)}
	h.SetQueue(q) // after Start: it gets the agent set at once
	q.wait(t, "the first agent set", func(q *queueRecorder) bool { return len(q.sets) > 0 && len(q.sets[0]) == 9 })
	q.mu.Lock()
	var p3 queue.Agent
	for _, a := range q.sets[0] {
		if a.PaneID == "w1:p3" {
			p3 = a
		}
	}
	q.mu.Unlock()
	if p3.WorkspaceID != "w1" || p3.Status != string(herdr.StatusIdle) || p3.Seq == 0 || p3.Focused {
		t.Fatalf("w1:p3 %+v", p3)
	}

	snap, sub, err := h.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if b, _ := json.Marshal(snap); !strings.Contains(string(b), `"queue":[]`) {
		t.Errorf("snapshot without an empty queue list: %s", b)
	}

	// A status change feeds the queue the new set; the agents' patch
	// carries no queue.
	if err := srv.SetStatus("w1:p3", herdr.StatusWorking); err != nil {
		t.Fatal(err)
	}
	q.wait(t, "w1:p3 working", func(q *queueRecorder) bool {
		for _, a := range q.sets[len(q.sets)-1] {
			if a.PaneID == "w1:p3" {
				return a.Status == string(herdr.StatusWorking) && a.Seq != p3.Seq
			}
		}
		return false
	})

	added := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	item := queue.Item{ID: "invented-item-1", Target: queue.Target{PaneID: "w1:p3"}, Text: "invented follow-up", By: "owner@example.com", Added: added, State: queue.Waiting}
	q.set(item)
	p := nextQueuePatch(t, sub)
	if len(*p.Queue) != 1 || (*p.Queue)[0].ID != item.ID || len(p.Agents) != 0 {
		t.Fatalf("patch %+v", p)
	}
	if snap, sub2, err := h.Subscribe(); err != nil || len(snap.Queue) != 1 {
		t.Fatalf("snapshot queue %+v, %v", snap.Queue, err)
	} else {
		sub2.Close()
	}

	// Emptied, the patch says so with an empty list, not by leaving it out.
	q.set()
	p = nextQueuePatch(t, sub)
	if b, _ := json.Marshal(p); !strings.Contains(string(b), `"queue":[]`) {
		t.Errorf("emptied queue patch %s", b)
	}

	h.FocusEvent()
	q.wait(t, "the focus event", func(q *queueRecorder) bool { return q.focuses == 1 })
}
