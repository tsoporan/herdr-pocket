package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/tsoporan/herdr-pocket/internal/audit"
	"github.com/tsoporan/herdr-pocket/internal/fakeherdr"
	"github.com/tsoporan/herdr-pocket/internal/herdr"
	"github.com/tsoporan/herdr-pocket/internal/hub"
	"github.com/tsoporan/herdr-pocket/internal/queue"
	"github.com/tsoporan/herdr-pocket/internal/tailnet"
)

// Both serve paths open the queue in the state dir and run it: an item
// already lined up for an idle agent is sent. With --read-only it is never
// sent: run-real is --dev --read-only and shares --dev's state dir, so a
// list left by a dev session must not reach the real herdr.

const seededItem = "invented-seeded-item"

// queueFixture is fake-herdr plus a state dir whose queue.json holds one
// waiting item for w1:p3, which is idle. With dev, that queue.json is in
// the dir --dev uses for this socket, and the serve path's queue.json holds
// another herdr's item, which must never be sent.
type queueFixture struct {
	fake     *fakeherdr.Server
	stateDir string
	queueDir string // the queue.json's dir
	seed     []byte // queue.json as written
	by       string // the seeded item's author's login
}

const otherHerdrText = "invented item for another herdr"

func newQueueFixture(t *testing.T, dev bool) queueFixture {
	t.Helper()
	fake, stateDir := queueHerdr(t)
	f := queueFixture{fake: fake, stateDir: stateDir, queueDir: filepath.Join(stateDir, "queue")}
	if dev {
		writeQueue(t, f.queueDir, "invented-other-item", otherHerdrText, owner)
		f.queueDir = filepath.Join(devQueueDir(stateDir, fake.Addr()), "queue")
	}
	// The item's author is the controller of the mode under test, since the
	// queue checks the author's role again before sending.
	by := owner
	if dev {
		by = devOwner
	}
	f.seed, f.by = writeQueue(t, f.queueDir, seededItem, "invented seeded task", by), by.Login
	return f
}

// writeQueue writes a queue.json in dir with one waiting item for w1:p3,
// lined up by by.
func writeQueue(t *testing.T, dir, id, text string, by tailnet.Peer) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"items": []queue.Item{{
		ID: id, Target: queue.Target{PaneID: "w1:p3"}, Text: text,
		By: by.Login, Added: time.Now().UTC(), State: queue.Waiting,
		Author: &queue.Author{Login: by.Login, Node: by.Node, UserID: by.UserID, Tags: by.Tags},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "queue.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return b
}

// queueHerdr starts fake-herdr on a short temp socket and shortens the
// queue's settle; it returns the fake and an unused state dir path.
func queueHerdr(t *testing.T) (*fakeherdr.Server, string) {
	t.Helper()
	orig := queueSettle
	queueSettle = 10 * time.Millisecond
	t.Cleanup(func() { queueSettle = orig })
	dir, err := os.MkdirTemp("", "hq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	fake, err := fakeherdr.Listen(filepath.Join(dir, "h.sock"), fakeherdr.Studio())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fake.Close() })
	return fake, filepath.Join(dir, "state")
}

func (f queueFixture) sent() bool {
	return slices.ContainsFunc(f.fake.InputLog(), func(in fakeherdr.Input) bool { return in.Method == "agent.prompt" })
}

// check waits for the seeded item to be sent, or (readOnly) checks it never
// is, once the server is up.
func (f queueFixture) check(t *testing.T, readOnly bool) {
	t.Helper()
	if readOnly {
		time.Sleep(50 * queueSettle)
		if f.sent() {
			t.Fatalf("a --read-only server sent a lined-up item: %+v", f.fake.InputLog())
		}
		return
	}
	// Wait for the send's audit line, not just the input, so the checks
	// after the stop see the send's result.
	deadline := time.Now().Add(5 * time.Second)
	for !f.sent() || !f.auditedSend() {
		if time.Now().After(deadline) {
			t.Fatal("the lined-up item was never sent")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := f.fake.InputLog(); got[0].PaneID != "w1:p3" || got[0].Text != "invented seeded task" ||
		slices.ContainsFunc(got, func(in fakeherdr.Input) bool { return in.Text == otherHerdrText }) {
		t.Fatalf("input %+v", got)
	}
}

// auditedSend reports whether the audit log has a queued send yet.
func (f queueFixture) auditedSend() bool {
	b, _ := os.ReadFile(filepath.Join(f.stateDir, audit.FileName))
	return bytes.Contains(b, []byte(`"via":"queue"`))
}

// checkAfterStop reads what the stopped server left: the queued send
// audited by who lined it up, or (readOnly) no send and queue.json as seeded.
func (f queueFixture) checkAfterStop(t *testing.T, readOnly bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.stateDir, audit.FileName))
	if err != nil {
		t.Fatal(err)
	}
	var queued []audit.Entry
	for line := range bytes.Lines(b) {
		var e audit.Entry
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatal(err)
		}
		if e.Via == "queue" {
			queued = append(queued, e)
		}
	}
	if readOnly {
		if len(queued) != 0 {
			t.Errorf("read-only audited queued sends %+v", queued)
		}
		if got, err := os.ReadFile(filepath.Join(f.queueDir, "queue.json")); err != nil || !bytes.Equal(got, f.seed) {
			t.Errorf("read-only changed queue.json: %s (%v)", got, err)
		}
		return
	}
	if len(queued) != 1 {
		t.Fatalf("queued sends audited %+v", queued)
	}
	if e := queued[0]; e.Login != f.by || e.Method != "prompt" || e.Result != "ok" || e.ItemID != seededItem || e.PaneID != "w1:p3" {
		t.Errorf("audit %+v", e)
	}
}

func TestServeQueue(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "sends", true: "read-only never sends"}[readOnly], func(t *testing.T) {
			f := newQueueFixture(t, false)
			// The later --state-dir and --herdr-socket win over launch's.
			args := []string{"--state-dir", f.stateDir, "--herdr-socket", f.fake.Addr()}
			if readOnly {
				args = append(args, "--read-only")
			}
			s := startServe(t, ownerNode(nil), args...)
			c, _, err := s.dial(t, owner, testOrigin)
			if err != nil {
				t.Fatal(err)
			}
			if snap := readJSON[hub.Snapshot](t, c); len(snap.Queue) != 1 || snap.Queue[0].ID != seededItem {
				t.Fatalf("snapshot queue %+v", snap.Queue)
			}
			f.check(t, readOnly)
			if readOnly {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := wsjson.Write(ctx, c, map[string]any{"type": "queue.add", "id": "q1", "target": map[string]string{"pane_id": "w3:p2"}, "text": "invented"}); err != nil {
					t.Fatal(err)
				}
				for {
					var m struct {
						Type  string `json:"type"`
						ID    string `json:"id"`
						Error *struct {
							Code string `json:"code"`
						} `json:"error"`
					}
					if err := wsjson.Read(ctx, c, &m); err != nil {
						t.Fatal(err)
					}
					if m.Type == "ack" {
						if m.ID != "q1" || m.Error == nil || m.Error.Code != "read_only" {
							t.Fatalf("ack %+v, want read_only", m)
						}
						break
					}
				}
			}
			if err := s.stop(); err != nil {
				t.Fatal(err)
			}
			f.checkAfterStop(t, readOnly)
		})
	}
}

// The --dev path, as run-real uses it. It is up once it logs its address.
// Its queue is the one for this herdr socket only.
func TestDevQueue(t *testing.T) {
	var logs syncBuffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	for _, readOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "sends", true: "read-only never sends"}[readOnly], func(t *testing.T) {
			f := newQueueFixture(t, true)
			args := []string{"--dev", "--dev-addr", "127.0.0.1:0", "--state-dir", f.stateDir, "--herdr-socket", f.fake.Addr(),
				"--claude-config-dir", "../../internal/fakeherdr/testdata/claude", "--hostname", "studio"}
			if readOnly {
				args = append(args, "--read-only")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			start := strings.Count(logs.String(), "herdr-pocket --dev on")
			errc := make(chan error, 1)
			go func() { errc <- dispatch(ctx, args, io.Discard) }()
			deadline := time.Now().Add(10 * time.Second)
			for strings.Count(logs.String(), "herdr-pocket --dev on") == start {
				select {
				case err := <-errc:
					t.Fatalf("--dev returned before serving: %v", err)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("--dev never served")
				}
				time.Sleep(10 * time.Millisecond)
			}
			f.check(t, readOnly)
			cancel()
			if err := <-errc; err != nil {
				t.Fatal(err)
			}
			f.checkAfterStop(t, readOnly)
		})
	}
}

// queueMsg is any server message, as far as these tests read it.
type queueMsg struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error *struct {
		Code string `json:"code"`
	} `json:"error"`
	Queue *[]queue.Item `json:"queue"`
}

// queueConn reads one socket and remembers the last list it carried: a
// patch may arrive before the ack of the request that caused it.
type queueConn struct {
	t    *testing.T
	c    *websocket.Conn
	last *[]queue.Item
}

// next reads until match accepts a message.
func (r *queueConn) next(what string, match func(queueMsg) bool) queueMsg {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var m queueMsg
		if err := wsjson.Read(ctx, r.c, &m); err != nil {
			r.t.Fatalf("waiting for %s: %v", what, err)
		}
		if m.Queue != nil {
			r.last = m.Queue
		}
		if match(m) {
			return m
		}
	}
}

// queue returns the last list seen once match accepts it. Patches replace
// the whole list and may skip states, so match on the outcome.
func (r *queueConn) queue(what string, match func([]queue.Item) bool) []queue.Item {
	r.t.Helper()
	if r.last == nil || !match(*r.last) {
		r.next(what, func(m queueMsg) bool { return m.Queue != nil && match(*m.Queue) })
	}
	return *r.last
}

func queuePrompts(fake *fakeherdr.Server, pane string) []string {
	var out []string
	for _, in := range fake.InputLog() {
		if in.Method == "agent.prompt" && in.PaneID == pane {
			out = append(out, in.Text)
		}
	}
	return out
}

// End to end through serve's own wiring: two messages lined up over the
// WebSocket for a working agent go one at a time as it comes free, and a
// workspace task goes to the workspace's idle agent, never its done one.
// fake-herdr holds turns, so each item sits sent until the test starts the
// agent working.
func TestQueueEndToEnd(t *testing.T) {
	fake, stateDir := queueHerdr(t)
	fake.HoldTurns()
	s := startServe(t, ownerNode(nil), "--state-dir", stateDir, "--herdr-socket", fake.Addr())
	c, _, err := s.dial(t, owner, testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if snap := readJSON[hub.Snapshot](t, c); snap.Queue == nil || len(snap.Queue) != 0 {
		t.Fatalf("snapshot queue %+v, want []", snap.Queue)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := &queueConn{t: t, c: c}
	add := func(id string, target queue.Target, text string) {
		t.Helper()
		if err := wsjson.Write(ctx, c, map[string]any{"type": "queue.add", "id": id, "target": target, "text": text}); err != nil {
			t.Fatal(err)
		}
		if a := r.next("ack "+id, func(m queueMsg) bool { return m.Type == "ack" && m.ID == id }); !a.OK {
			t.Fatalf("ack %s: %+v", id, a.Error)
		}
	}
	setStatus := func(pane string, st herdr.Status) {
		t.Helper()
		if err := fake.SetStatus(pane, st); err != nil {
			t.Fatal(err)
		}
	}
	wantPrompts := func(pane string, want ...string) {
		t.Helper()
		if got := queuePrompts(fake, pane); !slices.Equal(got, want) {
			t.Fatalf("prompts to %s %q, want %q", pane, got, want)
		}
	}
	settle := func() { time.Sleep(20 * queueSettle) }
	const agent = "w3:p2" // working
	first, second, task := "invented first follow-up", "invented second follow-up", "invented workspace task"

	add("q1", queue.Target{PaneID: agent}, first)
	add("q2", queue.Target{PaneID: agent}, second)
	r.queue("two waiting", func(q []queue.Item) bool {
		return len(q) == 2 && q[0].State == queue.Waiting && q[1].State == queue.Waiting
	})
	settle()
	wantPrompts(agent)

	// Idle: after the settle the first goes, and stays sent until the agent
	// starts; the second waits.
	setStatus(agent, herdr.StatusIdle)
	items := r.queue("the first sent", func(q []queue.Item) bool { return len(q) == 2 && q[0].State == queue.Sent })
	if it := items[0]; it.Text != first || it.SentTo != agent || it.SentAt == nil || it.PickedUpAt != nil || items[1].State != queue.Waiting {
		t.Fatalf("items %+v", items)
	}
	wantPrompts(agent, first)
	c2, _, err := s.dial(t, owner, testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if snap := readJSON[hub.Snapshot](t, c2); len(snap.Queue) != 2 || snap.Queue[0].State != queue.Sent || snap.Queue[1].State != queue.Waiting {
		t.Fatalf("a new socket's snapshot queue %+v", snap.Queue)
	}
	c2.CloseNow()
	settle()
	wantPrompts(agent, first) // one in flight at a time

	setStatus(agent, herdr.StatusWorking)
	items = r.queue("the first picked up", func(q []queue.Item) bool { return len(q) == 2 && q[0].State == queue.PickedUp })
	if items[0].PickedUpAt == nil || items[1].State != queue.Waiting {
		t.Fatalf("items %+v", items)
	}
	settle()
	wantPrompts(agent, first)

	setStatus(agent, herdr.StatusIdle)
	r.queue("the second sent", func(q []queue.Item) bool { return len(q) == 2 && q[1].State == queue.Sent })
	wantPrompts(agent, first, second)
	setStatus(agent, herdr.StatusWorking)
	r.queue("the second picked up", func(q []queue.Item) bool { return len(q) == 2 && q[1].State == queue.PickedUp })

	// "The next free agent in w3": w3:p1 is done, so the task waits for
	// w3:p2 to be idle.
	add("q3", queue.Target{WorkspaceID: "w3"}, task)
	settle()
	wantPrompts("w3:p1")
	wantPrompts(agent, first, second)
	setStatus(agent, herdr.StatusIdle)
	items = r.queue("the task sent", func(q []queue.Item) bool { return len(q) == 3 && q[2].State == queue.Sent })
	if it := items[2]; it.Target != (queue.Target{WorkspaceID: "w3"}) || it.SentTo != agent {
		t.Fatalf("task %+v", it)
	}
	wantPrompts(agent, first, second, task)
	wantPrompts("w3:p1")

	// Wait for the last send's audit line before stopping (see check).
	auditPath := filepath.Join(stateDir, audit.FileName)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if b, _ := os.ReadFile(auditPath); bytes.Count(b, []byte(`"via":"queue"`)) == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the sends were never audited")
		}
	}
	if err := s.stop(); err != nil {
		t.Fatal(err)
	}

	// The audit: three adds and three queued sends, by who lined them up,
	// with no text.
	b, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{first, second, task} {
		if bytes.Contains(b, []byte(text)) {
			t.Errorf("the audit log holds message text %q", text)
		}
	}
	type row struct{ method, pane, workspace, item, via, result string }
	var got []row
	for line := range bytes.Lines(b) {
		var e audit.Entry
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(e.Method, "queue.") || e.Via == "queue" {
			if e.Login != owner.Login {
				t.Errorf("audit login %+v", e)
			}
			got = append(got, row{e.Method, e.PaneID, e.WorkspaceID, e.ItemID, e.Via, e.Result})
		}
	}
	want := []row{
		{"queue.add", agent, "", items[0].ID, "", "ok"},
		{"queue.add", agent, "", items[1].ID, "", "ok"},
		{"prompt", agent, "", items[0].ID, "queue", "ok"},
		{"prompt", agent, "", items[1].ID, "queue", "ok"},
		{"queue.add", "", "w3", items[2].ID, "", "ok"},
		{"prompt", agent, "w3", items[2].ID, "queue", "ok"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("audit\n got %+v\nwant %+v", got, want)
	}
	if info, err := os.Stat(filepath.Join(stateDir, "queue", "queue.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("queue.json: %v, want mode 600", err)
	}
}
