package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/tsoporan/herdr-pocket/internal/audit"
	"github.com/tsoporan/herdr-pocket/internal/fakeherdr"
	"github.com/tsoporan/herdr-pocket/internal/herdr"
	"github.com/tsoporan/herdr-pocket/internal/hub"
	"github.com/tsoporan/herdr-pocket/internal/push"
	"github.com/tsoporan/herdr-pocket/internal/queue"
	"github.com/tsoporan/herdr-pocket/internal/server"
	agentstart "github.com/tsoporan/herdr-pocket/internal/start"
	"github.com/tsoporan/herdr-pocket/internal/tailnet"
	"github.com/tsoporan/herdr-pocket/internal/transcript"
)

// env is fake-herdr, a started hub and the server on a real loopback port.
type env struct {
	fake     *fakeherdr.Server
	hub      *hub.Hub
	url      string // http://127.0.0.1:port
	origin   string
	who      *fakeIdentity
	stateDir string // holds audit.jsonl
	srv      *server.Server
	push     *push.Service      // with options.push
	sink     *pushSink          // the httptest push service push sends to
	queue    *queue.Queue       // with options.queue
	starts   *agentstart.Runner // with options.starts
}

// The tailnet users the tests use; all invented.
const ownerID = 1001

var (
	owner    = tailnet.Peer{Login: "owner@example.com", UserID: ownerID, Node: "owner-phone"}
	stranger = tailnet.Peer{Login: "stranger@example.com", UserID: 2002, Node: "their-laptop"}
	friend   = tailnet.Peer{Login: "friend@example.com", UserID: 3003, Node: "friend-phone"}
	// A tagged node: WhoIs reports the tagged-devices pseudo-user.
	kiosk = tailnet.Peer{Login: "tagged-devices", UserID: 9009, Node: "kiosk", Tags: []string{"tag:kiosk"}}
)

// fakeIdentity answers WhoIs with whichever peer the test set last; every
// test request comes from 127.0.0.1, so the address can't tell them apart.
type fakeIdentity struct {
	mu   sync.Mutex
	peer tailnet.Peer
	err  error
}

func (f *fakeIdentity) set(p tailnet.Peer, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.peer, f.err = p, err
}

func (f *fakeIdentity) WhoIs(context.Context, string) (tailnet.Peer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peer, f.err
}

// claudeFixtures holds the fixture agents' transcripts. Tests never read
// ~/.claude.
const claudeFixtures = "../fakeherdr/testdata/claude"

// options vary the server under test.
type options struct {
	noTranscripts bool // every agent is terminal-source
	readOnly      bool
	// owner (or SelfTagged with no owner) and the access lists.
	selfTagged     bool
	allow, readers []string
	// push runs a push.Service whose sends go to an httptest push service,
	// on pushNow's clock (nil: the real one).
	push    bool
	pushNow func() time.Time
	// queue runs a work queue in the state dir, wired as main wires it,
	// with a short settle.
	queue bool
	// starts runs a start runner wired as main wires it, lining up into
	// the queue (it implies queue).
	starts bool
}

func start(t *testing.T) env { return startWith(t, options{}) }

func startWith(t *testing.T, o options) env {
	t.Helper()
	// A short dir: unix socket paths are limited to about 108 bytes.
	dir, err := os.MkdirTemp("", "hp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	fake, err := fakeherdr.Listen(filepath.Join(dir, "h.sock"), fakeherdr.Studio())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fake.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var transcripts *transcript.Reader
	if !o.noTranscripts {
		transcripts = transcript.NewReader(claudeFixtures)
	}
	client := herdr.NewClient(fake.Addr())
	h := hub.New(client, "studio", transcripts)
	if err := h.Start(ctx); err != nil {
		t.Fatal(err)
	}

	static := fstest.MapFS{
		"index.html":           {Data: []byte("<!doctype html><title>herdr-pocket</title>")},
		"assets/index.js":      {Data: []byte("export {};")},
		"manifest.webmanifest": {Data: []byte(`{"name":"herdr-pocket"}`)},
	}
	ts := httptest.NewUnstartedServer(nil)
	origin := "http://" + ts.Listener.Addr().String()
	stateDir := t.TempDir()
	log, err := audit.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	who := &fakeIdentity{peer: owner}
	cfg := server.Config{
		Origins: []string{origin}, Input: client, Identity: who, Audit: log,
		Owner: ownerID, SelfTagged: o.selfTagged, Allow: o.allow, Readers: o.readers,
	}
	if o.selfTagged {
		cfg.Owner = 0
	}
	if o.readOnly {
		cfg.Input = nil
	}
	var svc *push.Service
	var sink *pushSink
	if o.push {
		// The answer-button hooks as main wires them.
		canAct, err := server.CanAct(cfg)
		if err != nil {
			t.Fatal(err)
		}
		svc, sink = startPush(t, stateDir, o.pushNow, push.Config{Question: h.Question, CanAct: canAct})
		h.SetPusher(svc)
		cfg.Push = svc
	}
	var q *queue.Queue
	if o.queue || o.starts {
		q = startQueue(t, ctx, stateDir, h, client, log, o.readOnly)
		cfg.Queue = q
	}
	var starts *agentstart.Runner
	if o.starts {
		starts = startRunner(t, ctx, h, client, q, log, o.readOnly)
		cfg.Starts = starts
	}
	handler, err := server.New(h, static, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts.Config.Handler = handler
	ts.Start()
	t.Cleanup(ts.Close)
	return env{fake: fake, hub: h, url: ts.URL, origin: origin, who: who, stateDir: stateDir, srv: handler, push: svc, sink: sink, queue: q, starts: starts}
}

func dial(t *testing.T, e env) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, e.url+"/ws", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {e.origin}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

func read[T any](t *testing.T, c *websocket.Conn) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var v T
	if err := wsjson.Read(ctx, c, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func byID(agents []hub.Agent) map[string]hub.Agent {
	m := map[string]hub.Agent{}
	for _, a := range agents {
		m[a.PaneID] = a
	}
	return m
}

func TestSnapshot(t *testing.T) {
	e := start(t)
	snap := read[hub.Snapshot](t, dial(t, e))
	if snap.Type != "snapshot" || snap.Host != "studio" {
		t.Fatalf("type %q host %q", snap.Type, snap.Host)
	}
	if len(snap.Agents) != 9 || len(snap.Workspaces) != 4 {
		t.Fatalf("%d agents, %d workspaces; want 9 and 4", len(snap.Agents), len(snap.Workspaces))
	}
	counts := map[herdr.Status]int{}
	for _, a := range snap.Agents {
		counts[a.Status]++
		if a.Name == "" || a.Line == "" || a.WorkspaceLabel == "" || a.ChangedAt.IsZero() || a.Tokens == nil {
			t.Errorf("incomplete agent %+v", a)
		}
	}
	want := map[herdr.Status]int{herdr.StatusBlocked: 2, herdr.StatusDone: 1, herdr.StatusWorking: 4, herdr.StatusIdle: 2}
	for s, n := range want {
		if counts[s] != n {
			t.Errorf("%d %s agents, want %d", counts[s], s, n)
		}
	}
	got := byID(snap.Agents)
	for _, want := range fakeherdr.Studio().Agents {
		a := got[want.PaneID]
		if a.Name != want.Name || a.Status != want.AgentStatus || a.StateChangeSeq != want.StateChangeSeq {
			t.Errorf("%s: name %q status %q line %q; fixture name %q status %q", want.PaneID, a.Name, a.Status, a.Line, want.Name, want.AgentStatus)
		}
	}
}

func TestStatusChangeArrivesAsPatch(t *testing.T) {
	e := start(t)
	c := dial(t, e)
	snap := read[hub.Snapshot](t, c)
	before, ok := byID(snap.Agents)["w1:p2"]
	if !ok || before.Status != herdr.StatusWorking {
		t.Fatalf("w1:p2 before: %+v", before)
	}

	// changed_at has millisecond resolution; make the change land in a later millisecond.
	time.Sleep(time.Until(before.ChangedAt.Add(time.Millisecond)))
	if err := e.fake.SetStatus("w1:p2", herdr.StatusBlocked); err != nil {
		t.Fatal(err)
	}
	p := read[hub.Patch](t, c)
	after := p.Agents["w1:p2"]
	if p.Type != "patch" || after == nil {
		t.Fatalf("patch %+v", p)
	}
	if after.Status != herdr.StatusBlocked || after.Line != "Wants to run a command" {
		t.Errorf("after: status %q line %q", after.Status, after.Line)
	}
	if !after.ChangedAt.After(before.ChangedAt) {
		t.Errorf("changed_at %v not after %v", after.ChangedAt, before.ChangedAt)
	}
	if len(p.Agents) != 1 {
		t.Errorf("patch touches %d agents, want 1", len(p.Agents))
	}
}

// Without a transcript, a working agent's line is its terminal title.
func TestTitleChangeArrivesAsPatch(t *testing.T) {
	e := startWith(t, options{noTranscripts: true})
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	if err := e.fake.SetTitle("w1:p2", "Reading the migration guide"); err != nil {
		t.Fatal(err)
	}
	p := read[hub.Patch](t, c)
	if a := p.Agents["w1:p2"]; a == nil || a.Line != "Reading the migration guide" {
		t.Fatalf("patch %+v", p)
	}
}

// When herdr goes away, the app's socket closes and new ones are refused
// until herdr is back, so the app shows that it is reconnecting.
func TestHerdrLossClosesSockets(t *testing.T) {
	e := start(t)
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	e.fake.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var v any
	if err := wsjson.Read(ctx, c, &v); websocket.CloseStatus(err) != websocket.StatusTryAgainLater {
		t.Fatalf("read after herdr went away: %v, want close %v", err, websocket.StatusTryAgainLater)
	}
	_, resp, err := websocket.Dial(ctx, e.url+"/ws", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {e.origin}},
	})
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("dial while herdr is down: %v, want 503", err)
	}
}

func TestOriginRefused(t *testing.T) {
	e := start(t)
	for _, origin := range []string{"", "http://evil.example", "https://" + strings.TrimPrefix(e.origin, "http://")} {
		req, _ := http.NewRequest("GET", e.url+"/ws", nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("origin %q: status %d, want 403", origin, resp.StatusCode)
		}
	}
}

func TestStaticAndCSP(t *testing.T) {
	e := start(t)
	check := func(path string, code int) {
		t.Helper()
		resp, err := http.Get(e.url + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != code {
			t.Errorf("GET %s: %d, want %d", path, resp.StatusCode, code)
		}
		// Literal, not server.CSP: the app must never be framed.
		if got := resp.Header.Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'self'") || !strings.Contains(got, "frame-ancestors 'none'") {
			t.Errorf("GET %s: CSP %q", path, got)
		}
		if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("GET %s: X-Frame-Options %q", path, got)
		}
	}
	for path, code := range map[string]int{"/": 200, "/agents": 200, "/assets/index.js": 200, "/missing.js": 404} {
		check(path, code)
	}
	resp, err := http.Get(e.url + "/manifest.webmanifest")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); got != "application/manifest+json" {
		t.Errorf("manifest Content-Type %q", got)
	}
	e.who.set(stranger, nil)
	check("/", http.StatusForbidden)
}
