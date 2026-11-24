package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/tsoporan/herdr-pocket/internal/herdr"
	"github.com/tsoporan/herdr-pocket/internal/hub"
	"github.com/tsoporan/herdr-pocket/internal/push"
	"github.com/tsoporan/herdr-pocket/internal/push/pushtest"
	"github.com/tsoporan/herdr-pocket/internal/server"
)

// manualClock is the push engine's clock in the end-to-end test: frozen
// until the test moves it. It starts at the real time, because the hub
// stamps focus events with the real clock.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// payload is a decrypted push. Unknown fields fail the decode, so a payload
// that grew prompt or screen text would too.
type payload struct {
	Kind  string `json:"kind"`
	Tag   string `json:"tag"`
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	// Answer is a blocked alert's buttons (herd pass 1); nil on the rest.
	Answer *answerKeys `json:"answer"`
}

// answerKeys is a push's answer object: option digits, never labels.
type answerKeys struct {
	PaneID     string `json:"pane_id"`
	QuestionID string `json:"question_id"`
	Allow      string `json:"allow"`
	Deny       string `json:"deny"`
}

type gotPush struct {
	urgency string
	msg     payload
}

// e2e drives the whole path: fake-herdr, the hub, the server's WebSocket,
// push.Service and an httptest push service, with the engine on clk.
type e2e struct {
	t    *testing.T
	e    env
	c    *websocket.Conn
	clk  *manualClock
	dev  *pushtest.Device
	sub  push.Subscription
	seen int // pushes already checked
}

// advance moves the engine's clock. Run sleeps on a real timer worked out
// from the clock at its last step, so it is then woken with a call that
// changes nothing (the test server has no key expiry).
func (x *e2e) advance(d time.Duration) {
	x.clk.add(d)
	x.e.push.SetExpiry(nil)
}

// status sets a fake-herdr agent's status and waits for the app to see it,
// which is after the hub has handed the new agent set to the engine.
func (x *e2e) status(pane string, s herdr.Status) {
	x.t.Helper()
	if err := x.e.fake.SetStatus(pane, s); err != nil {
		x.t.Fatal(err)
	}
	waitFor(x.t, x.c, "patch", func(p hub.Patch) bool { a := p.Agents[pane]; return a != nil && a.Status == s })
}

// next waits for the next push and decrypts it with the device's keys.
func (x *e2e) next(what string) gotPush {
	x.t.Helper()
	n := x.seen
	x.e.sink.waitFor(x.t, what, func(*http.Request) bool {
		got, _ := x.e.sink.requests()
		return len(got) > n
	})
	got, bodies := x.e.sink.requests()
	x.seen++
	r := got[n]
	if r.URL.Path != "/fcm/send/invented-device-e2e" || r.Header.Get("Content-Encoding") != "aes128gcm" || r.Header.Get("TTL") != "3600" {
		x.t.Fatalf("%s: push %s %v", what, r.URL, r.Header)
	}
	plain, err := x.dev.Decrypt(bodies[n])
	if err != nil {
		x.t.Fatalf("%s: %v", what, err)
	}
	var m payload
	dec := json.NewDecoder(bytes.NewReader(plain))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		x.t.Fatalf("%s: payload %q: %v", what, plain, err)
	}
	return gotPush{urgency: r.Header.Get("Urgency"), msg: m}
}

// none checks that nothing more was pushed, after giving a woken Run time
// to step.
func (x *e2e) none(what string) {
	x.t.Helper()
	time.Sleep(200 * time.Millisecond)
	if got, _ := x.e.sink.requests(); len(got) != x.seen {
		x.t.Fatalf("%s: %d pushes, want %d", what, len(got), x.seen)
	}
}

// TestPushEndToEnd follows one phone through the triggers: a blocked
// agent, its clear, a block that flips back inside the settle delay, an
// alert held while the agent is at the desk, and a test push.
func TestPushEndToEnd(t *testing.T) {
	clk := &manualClock{t: time.Now()}
	e := startWith(t, options{push: true, pushNow: clk.Now})
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	sub, dev := browserDevice(t, "invented-device-e2e")
	x := &e2e{t: t, e: e, c: c, clk: clk, dev: dev, sub: sub}
	if a := pushSubscribe(t, c, "s1", sub, map[string]any{"blocked": true, "desk_minutes": 5}); !a.OK {
		t.Fatalf("subscribe %+v", a)
	}

	// Blocked: nothing until the settle delay, then exactly one alert.
	x.status("w1:p3", herdr.StatusBlocked)
	x.none("before the settle delay")
	x.advance(time.Second)
	p := x.next("the blocked alert")
	want := payload{Kind: "blocked", Tag: "agent-w1:p3", Title: "Bump deps and changelog", Body: "Asks you a question · api. Tap to answer.", URL: "/#agent=w1%3Ap3"}
	if p.msg != want || p.urgency != "high" {
		t.Fatalf("blocked alert %+v urgency %q, want %+v high", p.msg, p.urgency, want)
	}
	x.advance(2 * time.Minute) // past the cooldown: still one alert per block
	x.none("a repeat of the blocked alert")

	// Unblocked: a silent clear for the same tag.
	x.status("w1:p3", herdr.StatusWorking)
	x.advance(time.Second)
	p = x.next("the clear")
	if p.msg.Kind != "clear" || p.msg.Tag != "agent-w1:p3" || p.urgency != "low" {
		t.Fatalf("clear %+v urgency %q", p.msg, p.urgency)
	}

	// A block that flips back inside the settle delay is never sent.
	x.status("w4:p1", herdr.StatusBlocked)
	x.status("w4:p1", herdr.StatusWorking)
	x.advance(time.Second)
	x.none("a block that flipped back")

	// At the desk: the focused agent's alert is held for the desk window,
	// then sent because it is still blocked.
	if err := e.fake.SetFocus("w4:p2"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, c, "patch", func(p hub.Patch) bool { a := p.Agents["w4:p2"]; return a != nil && a.Focused })
	x.status("w4:p2", herdr.StatusBlocked)
	x.advance(time.Second)
	x.none("the agent at the desk")
	x.advance(10 * time.Minute)
	p = x.next("the held alert")
	if p.msg.Kind != "blocked" || p.msg.URL != "/#agent=w4%3Ap2" || p.msg.Body != "Asks you a question · docs. Tap to answer." {
		t.Fatalf("held alert %+v", p.msg)
	}

	// A test push through the WebSocket goes at once.
	if a := pushCall(t, c, "t1", "push.test", sub.Endpoint); !a.OK {
		t.Fatalf("test ack %+v", a)
	}
	if p = x.next("the test push"); p.msg.Kind != "test" || p.msg.URL != "/" || p.urgency != "normal" {
		t.Fatalf("test push %+v urgency %q", p.msg, p.urgency)
	}
	x.none("anything after the test push")
}

// TestNotificationAnswerEndToEnd follows herd pass 1 through the whole
// path: a Bash permission prompt, after the settle delay one alert with the
// answer keys, a tap on Allow once (POST /api/answer with the push's own
// question id) that reaches fake-herdr as the digit, and a second tap with
// the same id that is refused as question_changed.
func TestNotificationAnswerEndToEnd(t *testing.T) {
	clk := &manualClock{t: time.Now()}
	e := startWith(t, options{push: true, pushNow: clk.Now})
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	sub, dev := browserDevice(t, "invented-device-e2e")
	x := &e2e{t: t, e: e, c: c, clk: clk, dev: dev, sub: sub}
	if a := pushSubscribe(t, c, "s1", sub, map[string]any{"blocked": true, "desk_minutes": 0}); !a.OK {
		t.Fatalf("subscribe %+v", a)
	}

	// w1:p1's screen is a Bash permission prompt: 1 Allow once, 2 Always
	// allow, 3 Don't allow.
	x.status("w1:p1", herdr.StatusWorking)
	x.status("w1:p1", herdr.StatusBlocked)
	x.none("before the settle delay")
	x.advance(time.Second)
	p := x.next("the blocked alert")
	a := p.msg.Answer
	if p.msg.Kind != "blocked" || p.urgency != "high" || a == nil ||
		a.PaneID != "w1:p1" || a.QuestionID == "" || a.Allow != "1" || a.Deny != "3" {
		t.Fatalf("blocked alert %+v answer %+v urgency %q", p.msg, a, p.urgency)
	}
	if p.msg.Body != "Wants to run a command · api. Tap to see it first." {
		t.Errorf("body %q", p.msg.Body)
	}
	x.advance(2 * time.Minute)
	x.none("a second alert for the same block")

	status, r := postAnswer(t, e, answerBody(a.PaneID, a.QuestionID, a.Allow), nil)
	wantReply(t, "allow once from the push", status, r, http.StatusOK, "")
	got := inputs(e.fake, "w1:p1")
	if len(got) != 1 || got[0].Method != "agent.send_keys" || !slices.Equal(got[0].Keys, []string{"1"}) {
		t.Fatalf("inputs %+v", got)
	}

	status, r = postAnswer(t, e, answerBody(a.PaneID, a.QuestionID, a.Allow), nil)
	wantReply(t, "the same tap again", status, r, http.StatusConflict, server.CodeQuestionChanged)
	if n := len(inputs(e.fake, "w1:p1")); n != 1 {
		t.Errorf("%d inputs after the second tap, want 1", n)
	}
}
