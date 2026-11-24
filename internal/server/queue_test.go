package server_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/tsoporan/herdr-pocket/internal/audit"
	"github.com/tsoporan/herdr-pocket/internal/herdr"
	"github.com/tsoporan/herdr-pocket/internal/hub"
	"github.com/tsoporan/herdr-pocket/internal/queue"
	"github.com/tsoporan/herdr-pocket/internal/server"
)

// Herd pass 2 integration tests for queue.add and queue.remove: fake-herdr,
// the hub, the server's WebSocket and the real queue engine on a short
// settle, sending through fake-herdr.

// testSettle is the queue's settle in these tests.
const testSettle = 30 * time.Millisecond

// startQueue opens and runs a queue in stateDir as main does, and stops it
// before the audit log closes (cleanups run last-registered first).
func startQueue(t *testing.T, ctx context.Context, stateDir string, h *hub.Hub, client *herdr.Client, log *audit.Log, readOnly bool) *queue.Queue {
	t.Helper()
	q, err := queue.Open(stateDir, queue.Config{
		Settle:   testSettle,
		Prompt:   client.Prompt,
		OnChange: h.QueueChanged,
		OnSent:   server.AuditQueued(log),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.SetQueue(q)
	if readOnly {
		return q // main never runs it read-only
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { q.Run(ctx) })
	t.Cleanup(func() { cancel(); wg.Wait() })
	return q
}

// lineUp sends queue.add and returns its ack.
func lineUp(t *testing.T, c *websocket.Conn, id string, target queue.Target, text string) server.Ack {
	t.Helper()
	send(t, c, map[string]any{"type": "queue.add", "id": id, "target": target, "text": text})
	return ack(t, c, id)
}

func unqueue(t *testing.T, c *websocket.Conn, id, itemID string) server.Ack {
	t.Helper()
	send(t, c, map[string]any{"type": "queue.remove", "id": id, "item_id": itemID})
	return ack(t, c, id)
}

// waitQueue reads patches until one whose queue match accepts.
func waitQueue(t *testing.T, c *websocket.Conn, what string, match func([]queue.Item) bool) []queue.Item {
	t.Helper()
	p := waitFor(t, c, "patch", func(p hub.Patch) bool { return p.Queue != nil && match(*p.Queue) })
	return *p.Queue
}

func states(items []queue.Item) []queue.State {
	out := make([]queue.State, len(items))
	for i, it := range items {
		out[i] = it.State
	}
	return out
}

func prompts(e env, pane string) []string {
	var out []string
	for _, in := range inputs(e.fake, pane) {
		if in.Method == "agent.prompt" {
			out = append(out, in.Text)
		}
	}
	return out
}

func TestQueueSendsWhenDone(t *testing.T) {
	e := startWith(t, options{queue: true})
	c := dial(t, e)
	snap := read[json.RawMessage](t, c)
	if !strings.Contains(string(snap), `"queue":[]`) {
		t.Fatalf("snapshot without an empty queue: %s", snap)
	}

	// w3:p2 is working, so both wait.
	first, second := "invented first follow-up", "invented second follow-up"
	a1 := lineUp(t, c, "q1", queue.Target{PaneID: "w3:p2"}, first)
	if !a1.OK {
		t.Fatalf("ack %+v", a1)
	}
	a2 := lineUp(t, c, "q2", queue.Target{PaneID: "w3:p2"}, "  "+second+"\n")
	if !a2.OK {
		t.Fatalf("ack %+v", a2)
	}
	// The list's patch may come before the acks, so read the queue itself.
	items := e.queue.List()
	// Each ack names its item, so the app can follow it.
	if a1.ItemID != items[0].ID || a2.ItemID != items[1].ID || a1.ItemID == "" {
		t.Fatalf("ack item ids %q %q, items %q %q", a1.ItemID, a2.ItemID, items[0].ID, items[1].ID)
	}
	if items[0].Text != first || items[1].Text != second || items[0].By != owner.Login || items[0].Target.PaneID != "w3:p2" ||
		!slices.Equal(states(items), []queue.State{queue.Waiting, queue.Waiting}) {
		t.Fatalf("items %+v", items)
	}
	time.Sleep(5 * testSettle)
	if got := prompts(e, "w3:p2"); len(got) != 0 {
		t.Fatalf("sent to a working agent: %q", got)
	}

	// Done: the first goes, and fake-herdr's prompt starts a turn, so it is
	// picked up and the second waits for that turn to end.
	if err := e.fake.SetStatus("w3:p2", herdr.StatusIdle); err != nil {
		t.Fatal(err)
	}
	items = waitQueue(t, c, "the first picked up", func(q []queue.Item) bool {
		return len(q) == 2 && q[0].State == queue.PickedUp
	})
	if items[1].State != queue.Waiting || items[0].SentTo != "w3:p2" || items[0].SentAt == nil || items[0].PickedUpAt == nil {
		t.Fatalf("items %+v", items)
	}
	if got := prompts(e, "w3:p2"); !slices.Equal(got, []string{first}) {
		t.Fatalf("prompts %q", got)
	}

	if err := e.fake.SetStatus("w3:p2", herdr.StatusDone); err != nil {
		t.Fatal(err)
	}
	waitQueue(t, c, "both picked up", func(q []queue.Item) bool {
		return slices.Equal(states(q), []queue.State{queue.PickedUp, queue.PickedUp})
	})
	if got := prompts(e, "w3:p2"); !slices.Equal(got, []string{first, second}) {
		t.Fatalf("prompts %q", got)
	}

	// Audit: each add and each send, sizes and hashes only.
	type row struct{ login, method, pane, item, via, result string }
	var got []row
	sums := map[string][]string{} // item id -> its lines' hashes
	for _, l := range auditLines(t, e, first, second) {
		if l.Size == 0 || len(l.SHA256) != 64 {
			t.Errorf("audit line %+v", l)
		}
		got = append(got, row{l.Login, l.Method, l.PaneID, l.ItemID, l.Via, l.Result})
		sums[l.ItemID] = append(sums[l.ItemID], l.SHA256)
	}
	// The add hashes the trimmed text it lines up, so it matches the send.
	if s := sums[items[1].ID]; len(s) != 2 || s[0] != s[1] {
		t.Errorf("the second item's add and send hashes %q differ", s)
	}
	want := []row{
		{owner.Login, "queue.add", "w3:p2", items[0].ID, "", "ok"},
		{owner.Login, "queue.add", "w3:p2", items[1].ID, "", "ok"},
		{owner.Login, "prompt", "w3:p2", items[0].ID, "queue", "ok"},
		{owner.Login, "prompt", "w3:p2", items[1].ID, "queue", "ok"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("audit\n got %+v\nwant %+v", got, want)
	}

	// The list is kept in the state dir, private, and a new socket's
	// snapshot carries it.
	for name, mode := range map[string]os.FileMode{"queue": 0o700, "queue/queue.json": 0o600} {
		if info, err := os.Stat(filepath.Join(e.stateDir, name)); err != nil || info.Mode().Perm() != mode {
			t.Errorf("%s: %v, want mode %o", name, err, mode)
		}
	}
	if s := read[hub.Snapshot](t, dial(t, e)); len(s.Queue) != 2 {
		t.Errorf("snapshot queue %+v", s.Queue)
	}
}

// "The next free agent in api": w1:p3 is api's only idle agent.
func TestQueueWorkspaceTarget(t *testing.T) {
	e := startWith(t, options{queue: true})
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	text := "invented workspace task"
	if a := lineUp(t, c, "q1", queue.Target{WorkspaceID: "w1"}, text); !a.OK {
		t.Fatalf("ack %+v", a)
	}
	items := waitQueue(t, c, "sent to w1:p3", func(q []queue.Item) bool { return len(q) == 1 && q[0].SentTo == "w1:p3" })
	if items[0].Target != (queue.Target{WorkspaceID: "w1"}) {
		t.Fatalf("item %+v", items[0])
	}
	if got := prompts(e, "w1:p3"); !slices.Equal(got, []string{text}) {
		t.Fatalf("prompts %q", got)
	}
	var lines []audit.Entry
	for _, l := range auditLines(t, e, text) {
		if l.WorkspaceID != "w1" || l.ItemID != items[0].ID {
			t.Errorf("audit line %+v", l)
		}
		lines = append(lines, l)
	}
	if len(lines) != 2 || lines[0].Method != "queue.add" || lines[0].PaneID != "" ||
		lines[1].Method != "prompt" || lines[1].Via != "queue" || lines[1].PaneID != "w1:p3" {
		t.Errorf("audit %+v", lines)
	}
}

func TestQueueRemove(t *testing.T) {
	e := startWith(t, options{queue: true, allow: []string{friend.Login}})
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	lineUp(t, c, "q1", queue.Target{PaneID: "w3:p2"}, "invented task to remove")
	items := e.queue.List() // its patch may have come before the ack
	if len(items) != 1 {
		t.Fatalf("items %+v", items)
	}
	removed := items[0].ID

	// Any controller may remove it.
	e.who.set(friend, nil)
	c2 := dial(t, e)
	read[hub.Snapshot](t, c2)
	if a := unqueue(t, c2, "r1", removed); !a.OK {
		t.Fatalf("ack %+v", a)
	}
	waitQueue(t, c, "emptied", func(q []queue.Item) bool { return len(q) == 0 })
	wantCode(t, unqueue(t, c2, "r2", removed), server.CodeNotFound)
	wantCode(t, unqueue(t, c2, "r3", ""), server.CodeInvalidRequest)

	// A sent item can't be removed.
	lineUp(t, c2, "q2", queue.Target{PaneID: "w1:p3"}, "invented task already sent")
	items = waitQueue(t, c2, "sent", func(q []queue.Item) bool { return len(q) == 1 && q[0].State != queue.Waiting })
	wantCode(t, unqueue(t, c2, "r4", items[0].ID), server.CodeNotWaiting)

	lines := auditLines(t, e)
	if l := lines[1]; l.Method != "queue.remove" || l.Login != friend.Login || l.ItemID != removed || l.Result != "ok" {
		t.Errorf("audit %+v", l)
	}
}

func TestQueueRefused(t *testing.T) {
	pane := queue.Target{PaneID: "w3:p2"}
	t.Run("checks", func(t *testing.T) {
		e := startWith(t, options{queue: true})
		c := dial(t, e)
		read[hub.Snapshot](t, c)
		for i, tc := range []struct {
			target queue.Target
			text   string
			code   string
		}{
			{queue.Target{}, "invented", server.CodeInvalidRequest},
			{queue.Target{PaneID: "w3:p2", WorkspaceID: "w3"}, "invented", server.CodeInvalidRequest},
			{queue.Target{PaneID: "w9:p9"}, "invented", server.CodeNotFound},
			{queue.Target{WorkspaceID: "w9"}, "invented", server.CodeNotFound},
			{pane, " \n ", server.CodeInvalidRequest},
			{pane, strings.Repeat("x", 4001), server.CodeInvalidRequest},
		} {
			wantCode(t, lineUp(t, c, "bad"+string(rune('a'+i)), tc.target, tc.text), tc.code)
		}
		send(t, c, map[string]any{"type": "queue.add", "id": "nt", "text": "invented"})
		wantCode(t, ack(t, c, "nt"), server.CodeInvalidRequest)
		send(t, c, map[string]any{"type": "queue.clear", "id": "uk"})
		wantCode(t, ack(t, c, "uk"), server.CodeInvalidRequest)

		for i := range 20 {
			if a := lineUp(t, c, "ok"+string(rune('a'+i)), pane, "invented item"); !a.OK {
				t.Fatalf("item %d: %+v", i, a)
			}
		}
		wantCode(t, lineUp(t, c, "full", pane, "invented one too many"), server.CodeQueueFull)
	})
	t.Run("reader", func(t *testing.T) {
		e := startWith(t, options{queue: true, readers: []string{friend.Login}})
		e.who.set(friend, nil)
		c := dial(t, e)
		read[hub.Snapshot](t, c)
		wantCode(t, lineUp(t, c, "q1", pane, "invented reader text"), server.CodeForbidden)
		wantCode(t, unqueue(t, c, "r1", "invented-id"), server.CodeForbidden)
		lines := auditLines(t, e, "invented reader text")
		if len(lines) != 2 || lines[0].Result != server.CodeForbidden || lines[0].PaneID != "w3:p2" || lines[0].Size != len("invented reader text") {
			t.Errorf("audit %+v", lines)
		}
	})
	t.Run("read-only", func(t *testing.T) {
		e := startWith(t, options{queue: true, readOnly: true})
		c := dial(t, e)
		read[hub.Snapshot](t, c)
		wantCode(t, lineUp(t, c, "q1", pane, "invented"), server.CodeReadOnly)
		wantCode(t, unqueue(t, c, "r1", "invented-id"), server.CodeReadOnly)
		if len(e.queue.List()) != 0 {
			t.Errorf("queue %+v", e.queue.List())
		}
	})
	t.Run("queue off", func(t *testing.T) {
		e := start(t)
		c := dial(t, e)
		if s := read[hub.Snapshot](t, c); s.Queue == nil || len(s.Queue) != 0 {
			t.Errorf("snapshot queue %+v, want []", s.Queue)
		}
		wantCode(t, lineUp(t, c, "q1", pane, "invented"), server.CodeInvalidRequest)
	})
}
