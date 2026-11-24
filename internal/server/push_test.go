package server_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/tsoporan/herdr-pocket/internal/fakeherdr"
	"github.com/tsoporan/herdr-pocket/internal/herdr"
	"github.com/tsoporan/herdr-pocket/internal/hub"
	"github.com/tsoporan/herdr-pocket/internal/push"
	"github.com/tsoporan/herdr-pocket/internal/push/pushtest"
	"github.com/tsoporan/herdr-pocket/internal/server"
)

// M5 integration tests for the push.* messages: real fake-herdr, hub,
// server, push.Service and a WebSocket client. Pushes go to an httptest
// push service, never a real one.

// pushSink is an httptest TLS push service. Its client dials it for every
// address, so endpoints keep a real push-service host (which push.Subscribe
// requires) and still never leave the machine.
type pushSink struct {
	ts   *httptest.Server
	mu   sync.Mutex
	got  []*http.Request // bodies are drained; headers and URL kept
	body [][]byte
	more chan struct{}
}

func newPushSink(t *testing.T) *pushSink {
	t.Helper()
	p := &pushSink{more: make(chan struct{}, 64)}
	p.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.got = append(p.got, r)
		p.body = append(p.body, b)
		p.mu.Unlock()
		select {
		case p.more <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(p.ts.Close)
	return p
}

func (p *pushSink) client() *http.Client {
	tr := p.ts.Client().Transport.(*http.Transport).Clone()
	// httptest's certificate names example.com; the endpoint's host is a
	// push service's.
	tr.TLSClientConfig.ServerName = "example.com"
	addr := p.ts.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	return &http.Client{Transport: tr, Timeout: 5 * time.Second}
}

// requests returns the pushes received so far.
func (p *pushSink) requests() ([]*http.Request, [][]byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*http.Request(nil), p.got...), append([][]byte(nil), p.body...)
}

// waitFor waits until a received push satisfies match.
func (p *pushSink) waitFor(t *testing.T, what string, match func(*http.Request) bool) *http.Request {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		got, _ := p.requests()
		for _, r := range got {
			if match(r) {
				return r
			}
		}
		select {
		case <-p.more:
		case <-deadline:
			t.Fatalf("no push for %s; got %d", what, len(got))
		}
	}
}

// startPush opens a push.Service in stateDir that sends to a new sink, with
// a short settle so trigger tests are quick, and runs it for the test.
// now nil is the real clock. hooks' Question and CanAct are the engine's
// answer-button hooks (nil: no buttons).
func startPush(t *testing.T, stateDir string, now func() time.Time, hooks push.Config) (*push.Service, *pushSink) {
	t.Helper()
	sink := newPushSink(t)
	svc, err := push.Open(stateDir, push.Config{
		Origin:     "https://pocket.example.com",
		HTTPClient: sink.client(),
		Now:        now,
		Settle:     50 * time.Millisecond,
		Question:   hooks.Question,
		CanAct:     hooks.CanAct,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return svc, sink
}

// browserSub is what PushSubscription.toJSON() gives, with real P-256 keys
// so the engine can encrypt to it. The endpoint is FCM's shape with an
// invented token.
func browserSub(t *testing.T, token string) push.Subscription {
	t.Helper()
	sub, _ := browserDevice(t, token)
	return sub
}

// browserDevice is browserSub plus the device's private keys, to decrypt
// what the push service receives.
func browserDevice(t *testing.T, token string) (push.Subscription, *pushtest.Device) {
	t.Helper()
	d, err := pushtest.NewDevice()
	if err != nil {
		t.Fatal(err)
	}
	return push.Subscription{
		Endpoint: "https://fcm.googleapis.com/fcm/send/" + token,
		Keys:     push.Keys{P256dh: d.P256dh(), Auth: d.AuthKey()},
	}, d
}

func pushSubscribe(t *testing.T, c *websocket.Conn, id string, sub push.Subscription, settings any) server.Ack {
	t.Helper()
	send(t, c, map[string]any{"id": id, "type": "push.subscribe", "subscription": sub, "settings": settings})
	return ack(t, c, id)
}

func pushCall(t *testing.T, c *websocket.Conn, id, typ, endpoint string) server.Ack {
	t.Helper()
	send(t, c, map[string]any{"id": id, "type": typ, "endpoint": endpoint})
	return ack(t, c, id)
}

func TestPushRoundTrip(t *testing.T) {
	e := startWith(t, options{push: true})
	c := dial(t, e)
	snap := read[hub.Snapshot](t, c)
	if snap.Push == nil || snap.Push.VAPIDPublicKey == "" || snap.Push.VAPIDPublicKey != e.push.VAPIDPublicKey() {
		t.Fatalf("snapshot push %+v, want the VAPID key %q", snap.Push, e.push.VAPIDPublicKey())
	}

	sub := browserSub(t, "invented-device-1")
	a := pushSubscribe(t, c, "s1", sub, map[string]any{
		"blocked": true, "done": true, "desk_minutes": 10,
		"quiet": map[string]string{"start": "22:00", "end": "07:00", "tz": "Europe/Berlin"},
	})
	want := push.Settings{Blocked: true, Done: true, DeskMinutes: 10, Quiet: &push.Quiet{Start: "22:00", End: "07:00", TZ: "Europe/Berlin"}}
	if !a.OK || a.Settings == nil || *a.Settings.Quiet != *want.Quiet || a.Settings.Blocked != want.Blocked || a.Settings.Done != want.Done || a.Settings.DeskMinutes != want.DeskMinutes {
		t.Fatalf("subscribe ack %+v settings %+v", a, a.Settings)
	}
	a = pushCall(t, c, "g1", "push.get", sub.Endpoint)
	if !a.OK || a.Settings == nil || a.Settings.DeskMinutes != 10 || !a.Settings.Done {
		t.Fatalf("get ack %+v settings %+v", a, a.Settings)
	}

	// A test push ignores quiet hours and the desk: it goes now.
	if a := pushCall(t, c, "t1", "push.test", sub.Endpoint); !a.OK {
		t.Fatalf("test ack %+v", a)
	}
	got, bodies := e.sink.requests()
	if len(got) != 1 {
		t.Fatalf("%d pushes after push.test, want 1", len(got))
	}
	r := got[0]
	if r.Method != http.MethodPost || r.URL.Path != "/fcm/send/invented-device-1" || r.Header.Get("TTL") == "" ||
		!strings.HasPrefix(r.Header.Get("Authorization"), "vapid ") || r.Header.Get("Content-Encoding") != "aes128gcm" {
		t.Fatalf("push %s %s %v", r.Method, r.URL, r.Header)
	}
	if strings.Contains(string(bodies[0]), "test") {
		t.Errorf("push body is not encrypted: %q", bodies[0])
	}

	if a := pushCall(t, c, "u1", "push.unsubscribe", sub.Endpoint); !a.OK {
		t.Fatalf("unsubscribe ack %+v", a)
	}
	wantCode(t, pushCall(t, c, "g2", "push.get", sub.Endpoint), server.CodeNotFound)
	wantCode(t, pushCall(t, c, "t2", "push.test", sub.Endpoint), server.CodeNotFound)
	wantCode(t, pushCall(t, c, "u2", "push.unsubscribe", sub.Endpoint), server.CodeNotFound)

	// Bad endpoints and settings are refused before anything is stored.
	bad := browserSub(t, "x")
	bad.Endpoint = "http://fcm.googleapis.com/fcm/send/x"
	wantCode(t, pushSubscribe(t, c, "b1", bad, nil), server.CodeBadEndpoint)
	bad.Endpoint = "https://127.0.0.1/fcm/send/x"
	wantCode(t, pushSubscribe(t, c, "b2", bad, nil), server.CodeBadEndpoint)
	wantCode(t, pushSubscribe(t, c, "b3", browserSub(t, "y"), map[string]any{"desk_minutes": -1}), server.CodeBadSettings)
	send(t, c, map[string]any{"id": "b4", "type": "push.subscribe"})
	wantCode(t, ack(t, c, "b4"), server.CodeInvalidRequest)

	// Missing settings are the defaults.
	a = pushSubscribe(t, c, "s2", browserSub(t, "invented-device-2"), nil)
	if !a.OK || a.Settings == nil || *a.Settings != push.DefaultSettings() {
		t.Fatalf("subscribe without settings: %+v %+v", a, a.Settings)
	}
}

// A blocked agent reaches the push service through the hub's feed, and its
// clear follows when it leaves blocked.
func TestPushBlockedTrigger(t *testing.T) {
	e := startWith(t, options{push: true})
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	sub := browserSub(t, "invented-device-3")
	// desk_minutes 0: no desk suppression, whatever is focused.
	if a := pushSubscribe(t, c, "s1", sub, map[string]any{"blocked": true, "desk_minutes": 0}); !a.OK {
		t.Fatalf("subscribe %+v", a)
	}
	if err := e.fake.SetStatus("w1:p3", herdr.StatusBlocked); err != nil {
		t.Fatal(err)
	}
	e.sink.waitFor(t, "the blocked alert", func(r *http.Request) bool { return r.Header.Get("Urgency") == "high" })
	if err := e.fake.SetStatus("w1:p3", herdr.StatusWorking); err != nil {
		t.Fatal(err)
	}
	e.sink.waitFor(t, "the clear", func(r *http.Request) bool { return r.Header.Get("Urgency") == "low" })
}

// The agent the desktop is at gets no push while the desk window lasts: a
// herdr focus event marks the desk active.
func TestPushSuppressedAtDesk(t *testing.T) {
	e := startWith(t, options{push: true})
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	if a := pushSubscribe(t, c, "s1", browserSub(t, "invented-device-4"), map[string]any{"blocked": true, "desk_minutes": 5}); !a.OK {
		t.Fatalf("subscribe %+v", a)
	}
	if err := e.fake.SetFocus("w1:p3"); err != nil {
		t.Fatal(err)
	}
	// The focus re-list reaches the app before the block, so the engine
	// sees the focus first.
	waitFor(t, c, "patch", func(p hub.Patch) bool { a := p.Agents["w1:p3"]; return a != nil && a.Focused })
	if err := e.fake.SetStatus("w1:p3", herdr.StatusBlocked); err != nil {
		t.Fatal(err)
	}
	waitFor(t, c, "patch", func(p hub.Patch) bool {
		a := p.Agents["w1:p3"]
		return a != nil && a.Status == herdr.StatusBlocked
	})
	time.Sleep(300 * time.Millisecond) // six settle delays
	if got, _ := e.sink.requests(); len(got) != 0 {
		t.Fatalf("%d pushes for the agent at the desk, want 0", len(got))
	}
}

// Push is reading, not acting: a reader may subscribe, on a read-only
// server too. A subscription belongs to the login that made it.
func TestPushReaderAndOwnership(t *testing.T) {
	e := startWith(t, options{push: true, readOnly: true, readers: []string{friend.Login}})
	e.who.set(friend, nil)
	fc := dial(t, e)
	if snap := read[hub.Snapshot](t, fc); snap.Role != "reader" || snap.Push == nil {
		t.Fatalf("reader snapshot role %q push %+v", snap.Role, snap.Push)
	}
	sub := browserSub(t, "invented-device-5")
	if a := pushSubscribe(t, fc, "s1", sub, nil); !a.OK {
		t.Fatalf("reader subscribe %+v", a)
	}
	if a := pushCall(t, fc, "t1", "push.test", sub.Endpoint); !a.OK {
		t.Fatalf("reader test %+v", a)
	}

	e.who.set(owner, nil)
	oc := dial(t, e)
	read[hub.Snapshot](t, oc)
	wantCode(t, pushSubscribe(t, oc, "s2", sub, nil), server.CodeForbidden)
	wantCode(t, pushCall(t, oc, "g2", "push.get", sub.Endpoint), server.CodeForbidden)
	wantCode(t, pushCall(t, oc, "t2", "push.test", sub.Endpoint), server.CodeForbidden)
	wantCode(t, pushCall(t, oc, "u2", "push.unsubscribe", sub.Endpoint), server.CodeForbidden)
	// The friend's subscription is untouched.
	if a := pushCall(t, fc, "g3", "push.get", sub.Endpoint); !a.OK {
		t.Fatalf("friend get after refusals %+v", a)
	}

	// Every push.* request is audited with its result; the endpoint (a
	// bearer URL) and the keys never appear.
	lines := auditLines(t, e, sub.Endpoint, "invented-device-5", sub.Keys.P256dh, sub.Keys.Auth)
	type row struct{ login, method, result string }
	var got []row
	for _, l := range lines {
		if !strings.HasPrefix(l.Method, "push.") {
			continue
		}
		if l.Size == 0 || len(l.SHA256) != 64 || l.PaneID != "" {
			t.Errorf("audit line %+v", l)
		}
		got = append(got, row{l.Login, l.Method, l.Result})
	}
	want := []row{
		{friend.Login, "push.subscribe", "ok"},
		{friend.Login, "push.test", "ok"},
		{owner.Login, "push.subscribe", "forbidden"},
		{owner.Login, "push.get", "forbidden"},
		{owner.Login, "push.test", "forbidden"},
		{owner.Login, "push.unsubscribe", "forbidden"},
		{friend.Login, "push.get", "ok"},
	}
	if len(got) != len(want) {
		t.Fatalf("audit %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("audit line %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestPushOff(t *testing.T) {
	e := start(t)
	c := dial(t, e)
	if snap := read[hub.Snapshot](t, c); snap.Push != nil {
		t.Fatalf("snapshot push %+v, want null", snap.Push)
	}
	wantCode(t, pushCall(t, c, "g1", "push.get", "https://fcm.googleapis.com/fcm/send/x"), server.CodeInvalidRequest)
}

// focusCounter is a hub.Pusher that counts focus events.
type focusCounter struct {
	mu sync.Mutex
	n  int
}

func (f *focusCounter) Observe([]push.Agent) {}
func (f *focusCounter) SetExpiry(*time.Time) {}
func (f *focusCounter) FocusEvent(time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
}

func (f *focusCounter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// focus_on_desktop counts as desk activity even when herdr emits no focus
// event (the pane was focused already).
func TestFocusOnDesktopIsDeskActivity(t *testing.T) {
	e := start(t)
	f := &focusCounter{}
	e.hub.SetPusher(f)
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	send(t, c, map[string]string{"id": "f1", "type": "focus_on_desktop", "pane_id": "w1:p2"}) // focused in Studio
	if a := ack(t, c, "f1"); !a.OK {
		t.Fatalf("focus ack %+v", a)
	}
	if n := f.count(); n != 1 {
		t.Fatalf("%d focus events, want 1", n)
	}
}

// As serve and --dev wire it: a subscription stored from an earlier run, and
// the pusher attached before the hub starts. Agents already blocked at start
// (two in Studio) are the baseline, not new alerts; a new block is.
func TestPushRestartBaseline(t *testing.T) {
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
	svc, sink := startPush(t, t.TempDir(), nil, push.Config{})
	if _, err := svc.Subscribe(owner.Login, owner.Node, browserSub(t, "invented-device-6"), push.Settings{Blocked: true}); err != nil {
		t.Fatal(err)
	}

	h := hub.New(herdr.NewClient(fake.Addr()), "studio", nil)
	h.SetPusher(svc)
	if err := h.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond) // eight settle delays
	if got, _ := sink.requests(); len(got) != 0 {
		t.Fatalf("%d pushes for agents blocked before the start, want 0", len(got))
	}
	if err := fake.SetStatus("w1:p3", herdr.StatusBlocked); err != nil {
		t.Fatal(err)
	}
	sink.waitFor(t, "the new block", func(r *http.Request) bool { return r.Header.Get("Urgency") == "high" })
}

// Opening the agent in the app (a tap on its notification does: it opens
// /#agent=<pane>, which watches it) counts as seeing the alert, so no clear
// follows to a notification the tap already closed.
func TestPushWatchSkipsClear(t *testing.T) {
	e := startWith(t, options{push: true})
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	if a := pushSubscribe(t, c, "s1", browserSub(t, "invented-device-7"), map[string]any{"blocked": true, "desk_minutes": 0}); !a.OK {
		t.Fatalf("subscribe %+v", a)
	}
	if err := e.fake.SetStatus("w1:p3", herdr.StatusBlocked); err != nil {
		t.Fatal(err)
	}
	e.sink.waitFor(t, "the blocked alert", func(r *http.Request) bool { return r.Header.Get("Urgency") == "high" })
	send(t, c, map[string]any{"type": "watch", "pane_id": "w1:p3"})
	// The watch is read before the next request, whose ack proves it.
	if a := pushCall(t, c, "g1", "push.get", "https://fcm.googleapis.com/fcm/send/invented-device-7"); !a.OK {
		t.Fatalf("get %+v", a)
	}
	if err := e.fake.SetStatus("w1:p3", herdr.StatusWorking); err != nil {
		t.Fatal(err)
	}
	waitFor(t, c, "patch", func(p hub.Patch) bool {
		a := p.Agents["w1:p3"]
		return a != nil && a.Status == herdr.StatusWorking
	})
	time.Sleep(300 * time.Millisecond) // six settle delays
	got, _ := e.sink.requests()
	for _, r := range got {
		if r.Header.Get("Urgency") == "low" {
			t.Fatal("a clear went after the agent was opened in the app")
		}
	}
}

// Every tagged node reports the shared tagged-devices login, so each owns
// its own subscriptions by node; and one owner's subscriptions are capped.
func TestPushTaggedOwnersAndCap(t *testing.T) {
	e := startWith(t, options{push: true, readers: []string{"tag:kiosk"}})
	e.who.set(kiosk, nil)
	kc := dial(t, e)
	read[hub.Snapshot](t, kc)
	sub := browserSub(t, "invented-device-8")
	if a := pushSubscribe(t, kc, "s1", sub, nil); !a.OK {
		t.Fatalf("kiosk subscribe %+v", a)
	}
	other := kiosk
	other.Node = "kiosk-2"
	e.who.set(other, nil)
	oc := dial(t, e)
	read[hub.Snapshot](t, oc)
	wantCode(t, pushCall(t, oc, "g1", "push.get", sub.Endpoint), server.CodeForbidden)
	wantCode(t, pushCall(t, oc, "u1", "push.unsubscribe", sub.Endpoint), server.CodeForbidden)

	for i := range 9 { // the kiosk has one already; ten is the cap
		if a := pushSubscribe(t, kc, "c"+strconv.Itoa(i), browserSub(t, "invented-cap-"+strconv.Itoa(i)), nil); !a.OK {
			t.Fatalf("subscribe %d %+v", i, a)
		}
	}
	wantCode(t, pushSubscribe(t, kc, "c-over", browserSub(t, "invented-cap-over"), nil), server.CodeTooMany)
	if a := pushSubscribe(t, oc, "o1", browserSub(t, "invented-device-9"), nil); !a.OK {
		t.Fatalf("another tagged node's own subscribe %+v", a)
	}
}
