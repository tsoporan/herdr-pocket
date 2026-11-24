package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/herdr"
	"github.com/tsoporan/herdr-pocket/internal/hub"
	"github.com/tsoporan/herdr-pocket/internal/push"
	"github.com/tsoporan/herdr-pocket/internal/server"
)

// Herd pass 1: POST /api/answer, a notification's Allow once or Don't allow. w1:p1 is a Bash permission prompt with
// 1 Allow once, 2 Always allow and 3 Don't allow; w2:p1 an AskUserQuestion.

type answerReply struct {
	OK      bool   `json:"ok"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// postAnswer POSTs body to /api/answer as the service worker would; edit
// changes the request first.
func postAnswer(t *testing.T, e env, body string, edit func(*http.Request)) (int, answerReply) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.url+"/api/answer", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", e.origin)
	if edit != nil {
		edit(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var r answerReply
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, r
}

func answerBody(pane, questionID, option string) string {
	b, _ := json.Marshal(map[string]string{"pane_id": pane, "question_id": questionID, "option": option})
	return string(b)
}

// questionID is the pane's current question id, as the push carried it.
func questionID(t *testing.T, e env, pane string) string {
	t.Helper()
	c, d := watch(t, e, pane)
	c.CloseNow()
	if d.Question == nil {
		t.Fatalf("%s has no question", pane)
	}
	return d.Question.ID
}

func wantReply(t *testing.T, what string, status int, r answerReply, wantStatus int, wantCode string) {
	t.Helper()
	if status != wantStatus || r.OK != (wantCode == "") || r.Code != wantCode {
		t.Errorf("%s: %d %+v, want %d %q", what, status, r, wantStatus, wantCode)
	}
}

func TestAnswerAllowOnceFromNotification(t *testing.T) {
	e := start(t)
	qid := questionID(t, e, "w1:p1")
	status, r := postAnswer(t, e, answerBody("w1:p1", qid, "1"), nil)
	wantReply(t, "allow once", status, r, http.StatusOK, "")
	got := inputs(e.fake, "w1:p1")
	if len(got) != 1 || got[0].Method != "agent.send_keys" || !slices.Equal(got[0].Keys, []string{"1"}) {
		t.Fatalf("inputs %+v", got)
	}
	// A second tap (the other device's, say) is refused, not sent.
	status, r = postAnswer(t, e, answerBody("w1:p1", qid, "1"), nil)
	wantReply(t, "a repeat", status, r, http.StatusConflict, server.CodeQuestionChanged)
	if n := len(inputs(e.fake, "w1:p1")); n != 1 {
		t.Errorf("%d inputs after a repeat, want 1", n)
	}

	// Audited as an answer via the notification, with no content: the
	// option's size and hash only.
	lines := auditLines(t, e, qid, "rm -rf")
	if len(lines) != 2 {
		t.Fatalf("%d audit lines, want 2", len(lines))
	}
	l := lines[0]
	if l.Method != "answer" || l.Via != "notification" || l.PaneID != "w1:p1" || l.Result != "ok" ||
		l.Login != owner.Login || l.Node != owner.Node || l.Size != 2 {
		t.Errorf("audit %+v", l)
	}
	if lines[1].Result != server.CodeQuestionChanged || lines[1].Via != "notification" {
		t.Errorf("audit %+v", lines[1])
	}
}

func TestAnswerDenyFromNotification(t *testing.T) {
	e := start(t)
	qid := questionID(t, e, "w1:p1")
	status, r := postAnswer(t, e, answerBody("w1:p1", qid, "3"), nil)
	wantReply(t, "don't allow", status, r, http.StatusOK, "")
	// The digit only: a notification never sends a note.
	got := inputs(e.fake, "w1:p1")
	if len(got) != 1 || !slices.Equal(got[0].Keys, []string{"3"}) {
		t.Fatalf("inputs %+v", got)
	}
}

// Taps from several devices at once send the digit once; the rest are told
// the question changed.
func TestConcurrentAnswersFromNotification(t *testing.T) {
	e := start(t)
	qid := questionID(t, e, "w1:p1")
	body := answerBody("w1:p1", qid, "1")
	const taps = 8
	statuses := make(chan int, taps)
	var wg sync.WaitGroup
	for range taps {
		wg.Go(func() {
			req, err := http.NewRequest(http.MethodPost, e.url+"/api/answer", strings.NewReader(body))
			if err != nil {
				statuses <- 0
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			req.Header.Set("Origin", e.origin)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				statuses <- 0
				return
			}
			resp.Body.Close()
			statuses <- resp.StatusCode
		})
	}
	wg.Wait()
	close(statuses)
	count := map[int]int{}
	for s := range statuses {
		count[s]++
	}
	if count[http.StatusOK] != 1 || count[http.StatusConflict] != taps-1 {
		t.Errorf("statuses %v, want one 200 and %d 409", count, taps-1)
	}
	if n := len(inputs(e.fake, "w1:p1")); n != 1 {
		t.Errorf("%d inputs, want 1", n)
	}
}

// What a notification may not do is refused before anything is claimed, so
// the real answer still goes afterwards.
func TestAnswerFromNotificationRefusals(t *testing.T) {
	e := start(t)
	qid := questionID(t, e, "w1:p1")
	ask := questionID(t, e, "w2:p1")
	ok := answerBody("w1:p1", qid, "1")
	header := func(k, v string) func(*http.Request) {
		return func(r *http.Request) {
			if v == "" {
				r.Header.Del(k)
			} else {
				r.Header.Set(k, v)
			}
		}
	}
	cases := []struct {
		name   string
		body   string
		edit   func(*http.Request)
		status int
		code   string
	}{
		{"cross-site", ok, header("Sec-Fetch-Site", "cross-site"), http.StatusForbidden, server.CodeForbidden},
		{"user-initiated", ok, header("Sec-Fetch-Site", "none"), http.StatusForbidden, server.CodeForbidden},
		{"same-site", ok, header("Sec-Fetch-Site", "same-site"), http.StatusForbidden, server.CodeForbidden},
		{"no fetch metadata", ok, header("Sec-Fetch-Site", ""), http.StatusForbidden, server.CodeForbidden},
		{"another origin", ok, header("Origin", "https://evil.example.com"), http.StatusForbidden, server.CodeForbidden},
		{"no origin", ok, header("Origin", ""), http.StatusForbidden, server.CodeForbidden},
		{"a form", ok, header("Content-Type", "application/x-www-form-urlencoded"), http.StatusBadRequest, server.CodeInvalidRequest},
		{"text/plain", ok, header("Content-Type", "text/plain"), http.StatusBadRequest, server.CodeInvalidRequest},
		{"oversize", answerBody("w1:p1", qid+strings.Repeat("a", 1100), "1"), nil, http.StatusBadRequest, server.CodeInvalidRequest},
		{"a note", `{"pane_id":"w1:p1","question_id":"` + qid + `","option":"3","note":"do this instead"}`, nil, http.StatusBadRequest, server.CodeInvalidRequest},
		{"trailing data", ok + ok, nil, http.StatusBadRequest, server.CodeInvalidRequest},
		{"missing option", answerBody("w1:p1", qid, ""), nil, http.StatusBadRequest, server.CodeInvalidRequest},
		{"always allow", answerBody("w1:p1", qid, "2"), nil, http.StatusBadRequest, server.CodeInvalidRequest},
		{"no such option", answerBody("w1:p1", qid, "7"), nil, http.StatusBadRequest, server.CodeInvalidRequest},
		{"an AskUserQuestion", answerBody("w2:p1", ask, "1"), nil, http.StatusBadRequest, server.CodeInvalidRequest},
		{"a stale question", answerBody("w1:p1", "0123456789abcdef", "1"), nil, http.StatusConflict, server.CodeQuestionChanged},
		{"an unknown pane", answerBody("w9:p9", qid, "1"), nil, http.StatusNotFound, server.CodeNotFound},
	}
	for _, tc := range cases {
		status, r := postAnswer(t, e, tc.body, tc.edit)
		wantReply(t, tc.name, status, r, tc.status, tc.code)
	}
	if got := e.fake.InputLog(); len(got) != 0 {
		t.Fatalf("inputs after refusals %+v", got)
	}
	lines := auditLines(t, e, qid, "do this instead")
	if len(lines) != len(cases) {
		t.Errorf("%d audit lines, want %d", len(lines), len(cases))
	}
	for _, l := range lines {
		if l.Via != "notification" || l.Result == "ok" {
			t.Errorf("audit %+v", l)
		}
	}

	// GET is not the answer.
	resp, err := http.Get(e.url + "/api/answer?" + "pane_id=w1:p1&option=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("GET /api/answer: %d", resp.StatusCode)
	}

	// None of that claimed the question: the real tap still answers it.
	status, r := postAnswer(t, e, ok, nil)
	wantReply(t, "allow once after the refusals", status, r, http.StatusOK, "")
}

func TestAnswerFromNotificationNeedsController(t *testing.T) {
	e := startWith(t, options{readers: []string{friend.Login}})
	qid := questionID(t, e, "w1:p1")
	e.who.set(friend, nil)
	status, r := postAnswer(t, e, answerBody("w1:p1", qid, "1"), nil)
	wantReply(t, "a reader", status, r, http.StatusForbidden, server.CodeForbidden)
	e.who.set(stranger, nil)
	status, _ = postAnswer(t, e, answerBody("w1:p1", qid, "1"), nil)
	if status != http.StatusForbidden {
		t.Errorf("a stranger: %d", status)
	}

	ro := startWith(t, options{readOnly: true})
	status, r = postAnswer(t, ro, answerBody("w1:p1", questionID(t, ro, "w1:p1"), "1"), nil)
	wantReply(t, "read-only", status, r, http.StatusForbidden, server.CodeReadOnly)
	if got := slices.Concat(e.fake.InputLog(), ro.fake.InputLog()); len(got) != 0 {
		t.Fatalf("inputs %+v", got)
	}
}

// The WebSocket's answer is unchanged by the notification's limits: Always
// allow still goes from the app.
func TestWebSocketAnswerStillAllowsAlways(t *testing.T) {
	e := start(t)
	c, d := watch(t, e, "w1:p1")
	send(t, c, map[string]string{"id": "a1", "type": "answer", "pane_id": "w1:p1", "question_id": d.Question.ID, "option": "2"})
	if a := ack(t, c, "a1"); !a.OK {
		t.Fatalf("ack %+v", a)
	}
	if lines := auditLines(t, e); len(lines) != 1 || lines[0].Via != "" || lines[0].Method != "answer" {
		t.Errorf("audit %+v", lines)
	}
}

// answerPayload is a decrypted blocked push. Unknown fields fail the decode.
type answerPayload struct {
	Kind   string `json:"kind"`
	Tag    string `json:"tag"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	URL    string `json:"url"`
	Answer *struct {
		PaneID     string `json:"pane_id"`
		QuestionID string `json:"question_id"`
		Allow      string `json:"allow"`
		Deny       string `json:"deny"`
	} `json:"answer"`
}

// reblock makes pane blocked again with a new seq, so it alerts.
func reblock(t *testing.T, e env, pane string) {
	t.Helper()
	for _, s := range []herdr.Status{herdr.StatusWorking, herdr.StatusBlocked} {
		if err := e.fake.SetStatus(pane, s); err != nil {
			t.Fatal(err)
		}
	}
}

// nextBlocked waits for the next blocked push to the device and decrypts it.
func nextBlocked(t *testing.T, e env, dev interface{ Decrypt([]byte) ([]byte, error) }, token string, after int) (answerPayload, string) {
	t.Helper()
	e.sink.waitFor(t, "a blocked alert to "+token, func(*http.Request) bool {
		got, _ := e.sink.requests()
		for _, r := range got[after:] {
			if r.URL.Path == "/fcm/send/"+token && r.Header.Get("Urgency") == "high" {
				return true
			}
		}
		return false
	})
	got, bodies := e.sink.requests()
	for i := after; i < len(got); i++ {
		if got[i].URL.Path != "/fcm/send/"+token || got[i].Header.Get("Urgency") != "high" {
			continue
		}
		plain, err := dev.Decrypt(bodies[i])
		if err != nil {
			t.Fatal(err)
		}
		var p answerPayload
		dec := json.NewDecoder(bytes.NewReader(plain))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("payload %q: %v", plain, err)
		}
		return p, string(plain)
	}
	t.Fatal("unreachable")
	return answerPayload{}, ""
}

// A Bash permission alert to a controller carries the two option keys and
// no labels or command; an AskUserQuestion alert carries none.
func TestPushCarriesAnswerKeys(t *testing.T) {
	e := startWith(t, options{push: true})
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	sub, dev := browserDevice(t, "invented-device-h1")
	if a := pushSubscribe(t, c, "s1", sub, map[string]any{"blocked": true, "desk_minutes": 0}); !a.OK {
		t.Fatalf("subscribe %+v", a)
	}

	reblock(t, e, "w1:p1")
	p, plain := nextBlocked(t, e, dev, "invented-device-h1", 0)
	if p.Answer == nil || p.Answer.PaneID != "w1:p1" || p.Answer.Allow != "1" || p.Answer.Deny != "3" || p.Answer.QuestionID == "" {
		t.Fatalf("answer %+v in %q", p.Answer, plain)
	}
	if p.Body != "Wants to run a command · api. Tap to see it first." || p.Title != "Refactor auth middleware" {
		t.Errorf("payload %+v", p)
	}
	for _, leak := range []string{"rm -rf", "bun run build", "Allow", "allow once", "Always", "~/api"} {
		if strings.Contains(plain, leak) {
			t.Errorf("payload %q carries %q", plain, leak)
		}
	}
	// The keys answer it: the push's question id is the one on screen. (A
	// pause first, so the engine has recorded the alert as shown.)
	time.Sleep(100 * time.Millisecond)
	// A refused tap first: it must not cost this device its clear, which
	// closes the refusal notice once the agent is answered.
	status, r := postAnswer(t, e, answerBody("w1:p1", p.Answer.QuestionID, "2"), nil)
	wantReply(t, "always allow from the push", status, r, http.StatusBadRequest, server.CodeInvalidRequest)
	status, r = postAnswer(t, e, answerBody("w1:p1", p.Answer.QuestionID, p.Answer.Allow), nil)
	wantReply(t, "allow once from the push", status, r, http.StatusOK, "")
	// The agent leaves blocked and this device gets the clear too; the
	// service worker keeps the sent outcome through it (closesOnClear).
	waitFor(t, c, "patch", func(p hub.Patch) bool { a := p.Agents["w1:p1"]; return a != nil && a.Status == herdr.StatusWorking })
	e.sink.waitFor(t, "the clear", func(r *http.Request) bool { return r.Header.Get("Urgency") == "low" })
	n, _ := e.sink.requests()

	reblock(t, e, "w2:p1")
	p, plain = nextBlocked(t, e, dev, "invented-device-h1", len(n))
	if p.Answer != nil || p.Body != "Asks you a question · infra. Tap to answer." {
		t.Errorf("AskUserQuestion payload %q", plain)
	}
}

// A tagged controller's push carries the keys: its role comes from its
// tags, though it owns its subscriptions as login/node.
func TestPushAnswerKeysForTaggedController(t *testing.T) {
	e := startWith(t, options{push: true, allow: []string{"tag:kiosk"}})
	e.who.set(kiosk, nil)
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	sub, dev := browserDevice(t, "invented-device-kiosk")
	if a := pushSubscribe(t, c, "s1", sub, map[string]any{"blocked": true, "desk_minutes": 0}); !a.OK {
		t.Fatalf("subscribe %+v", a)
	}
	reblock(t, e, "w1:p1")
	if p, plain := nextBlocked(t, e, dev, "invented-device-kiosk", 0); p.Answer == nil {
		t.Errorf("tagged controller payload %q", plain)
	}
}

// A reader's push, and a read-only server's, carry no keys.
func TestPushAnswerKeysOnlyForControllers(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    options
	}{
		{"reader", options{push: true, readers: []string{friend.Login}}},
		{"read-only", options{push: true, readOnly: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := startWith(t, tc.o)
			if tc.name == "reader" {
				e.who.set(friend, nil)
			}
			c := dial(t, e)
			read[hub.Snapshot](t, c)
			sub, dev := browserDevice(t, "invented-device-ro")
			if a := pushSubscribe(t, c, "s1", sub, map[string]any{"blocked": true, "desk_minutes": 0}); !a.OK {
				t.Fatalf("subscribe %+v", a)
			}
			reblock(t, e, "w1:p1")
			p, plain := nextBlocked(t, e, dev, "invented-device-ro", 0)
			if p.Answer != nil || p.Body != "Asks you a question · api. Tap to answer." {
				t.Errorf("payload %q", plain)
			}
		})
	}
}

// A subscription the server hasn't identified (one stored before herd pass
// 1) gets no keys until its device connects again.
func TestPushAnswerKeysNeedIdentity(t *testing.T) {
	e := startWith(t, options{push: true})
	sub, dev := browserDevice(t, "invented-device-old")
	if _, err := e.push.Subscribe(owner.Login, owner.Node, sub, push.Settings{Blocked: true}); err != nil {
		t.Fatal(err)
	}
	reblock(t, e, "w1:p1")
	p, plain := nextBlocked(t, e, dev, "invented-device-old", 0)
	if p.Answer != nil {
		t.Errorf("unidentified payload %q", plain)
	}

	// The device opens the app: now it is known to be the owner (the
	// snapshot is written after Identify), and stays known after a restart.
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	data, err := os.ReadFile(filepath.Join(e.stateDir, "push", "subscriptions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"user_id": 1001`) {
		t.Errorf("subscriptions.json after connecting: %s", data)
	}
}
