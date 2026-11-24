package server_test

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/tsoporan/herdr-pocket/internal/answer"
	"github.com/tsoporan/herdr-pocket/internal/fakeherdr"
	"github.com/tsoporan/herdr-pocket/internal/herdr"
	"github.com/tsoporan/herdr-pocket/internal/hub"
	"github.com/tsoporan/herdr-pocket/internal/server"
)

// M3 integration tests: real fake-herdr, hub, server and a WebSocket client.

func send(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, c, v); err != nil {
		t.Fatal(err)
	}
}

// waitFor reads until a message of type typ that match accepts, skipping
// everything else (patches, other frames).
func waitFor[T any](t *testing.T, c *websocket.Conn, typ string, match func(T) bool) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		var raw json.RawMessage
		if err := wsjson.Read(ctx, c, &raw); err != nil {
			t.Fatalf("waiting for %s: %v", typ, err)
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			t.Fatal(err)
		}
		if head.Type != typ {
			continue
		}
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		if match == nil || match(v) {
			return v
		}
	}
}

func ack(t *testing.T, c *websocket.Conn, id string) server.Ack {
	t.Helper()
	return waitFor(t, c, "ack", func(a server.Ack) bool { return a.ID == id })
}

func wantCode(t *testing.T, a server.Ack, code string) {
	t.Helper()
	if a.OK || a.Error == nil || a.Error.Code != code {
		t.Fatalf("ack %+v, want error %s", a, code)
	}
}

// watch opens a socket, watches pane and returns its first detail.
func watch(t *testing.T, e env, pane string) (*websocket.Conn, hub.Detail) {
	t.Helper()
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	send(t, c, map[string]string{"type": "watch", "pane_id": pane})
	return c, waitFor[hub.Detail](t, c, "detail", nil)
}

func inputs(f *fakeherdr.Server, pane string) []fakeherdr.Input {
	var out []fakeherdr.Input
	for _, in := range f.InputLog() {
		if in.PaneID == pane {
			out = append(out, in)
		}
	}
	return out
}

// "Add rate limiting to /v2" shows the mockup's Now and steps.
func TestWatchSendsDetail(t *testing.T) {
	e := start(t)
	_, d := watch(t, e, "w1:p2")
	if d.PaneID != "w1:p2" || d.Source != hub.SourceTranscript || d.AgentLabel != "Claude Code" || d.Question != nil {
		t.Fatalf("detail %+v", d)
	}
	if want := "Running the integration tests for the v2 routes, to check the new limit doesn't break existing clients."; d.Now != want {
		t.Errorf("now %q", d.Now)
	}
	want := []hub.Step{
		{Kind: "you", Text: "Add a token-bucket rate limit to every /v2 route, 100 requests a minute per key", Meta: "You"},
		{Kind: "ok", Text: "Read index.ts", Meta: "src/routes/v2/index.ts"},
		{Kind: "ok", Text: "Created limit.ts", Meta: "src/middleware/limit.ts · 58 lines"},
		{Kind: "ok", Text: "Changed index.ts", Meta: "src/routes/v2/index.ts · 1 line changed"},
		{Kind: "ok", Text: "Ran the unit tests: 18 passed", Meta: "0.4 s"},
		{Kind: "now", Text: "Running the integration tests", Meta: ""},
	}
	// The prompt's meta carries its age: "You · 5 h 50 min ago".
	if len(d.Steps) > 0 && strings.HasPrefix(d.Steps[0].Meta, "You · ") && strings.HasSuffix(d.Steps[0].Meta, " ago") {
		d.Steps[0].Meta = "You"
	}
	if !slices.Equal(d.Steps, want) {
		t.Errorf("steps\n got %+v\nwant %+v", d.Steps, want)
	}
	if d.PR == nil || *d.PR != "#51 · checks running" {
		t.Errorf("pr %v", d.PR)
	}
	if d.ContextPct == nil || *d.ContextPct != 27 {
		t.Errorf("context_pct %v", d.ContextPct)
	}
	if d.StatusSince.IsZero() {
		t.Error("no status_since")
	}
	if d.LastMessage == nil || *d.LastMessage != "Running the integration tests for the v2 routes, to check the new limit doesn't break existing clients." || d.Asks {
		t.Errorf("last_message %v asks %v", d.LastMessage, d.Asks)
	}
}

// A finished agent whose whole last message ends in a question asks it.
func TestFinishedAgentAsks(t *testing.T) {
	e := start(t)
	_, d := watch(t, e, "w3:p1")
	m := d.LastMessage
	if m == nil || !strings.HasPrefix(*m, "PR #48 is ready: ") || !strings.Contains(*m, "\n| The spec, 20 times | 20 passed |") ||
		!strings.HasSuffix(*m, "on this branch?") || !d.Asks {
		t.Errorf("last_message %v asks %v", m, d.Asks)
	}
	if !strings.HasPrefix(d.Now, "PR #48 is ready") || strings.Contains(d.Now, "\n") {
		t.Errorf("now %q", d.Now)
	}
}

func TestBlockedAgentQuestion(t *testing.T) {
	e := start(t)
	_, d := watch(t, e, "w1:p1")
	q := d.Question
	if q == nil {
		t.Fatal("no question")
	}
	if q.Kind != "permission" || q.Title != "Allow this command?" || q.Where != "~/api" {
		t.Errorf("question %+v", q)
	}
	if q.Command == nil || *q.Command != "rm -rf dist && bun run build" {
		t.Errorf("command %v", q.Command)
	}
	var keys, labels []string
	for _, o := range q.Options {
		keys, labels = append(keys, o.Key), append(labels, o.Label)
	}
	if !slices.Equal(keys, []string{"1", "2", "3"}) || !slices.Equal(labels, []string{"Allow once", "Always allow", "Don’t allow"}) {
		t.Errorf("options %q %q", keys, labels)
	}
	if !q.Options[0].Primary || q.Options[0].Sub != "Runs this command now" || !q.Options[2].NeedsNote {
		t.Errorf("options %+v", q.Options)
	}
}

func TestAnswerSendsDigit(t *testing.T) {
	e := start(t)
	c, d := watch(t, e, "w1:p1")
	answer := map[string]string{"id": "a1", "type": "answer", "pane_id": "w1:p1", "question_id": d.Question.ID, "option": "1"}
	send(t, c, answer)
	if a := ack(t, c, "a1"); !a.OK {
		t.Fatalf("ack %+v", a)
	}
	got := inputs(e.fake, "w1:p1")
	if len(got) != 1 || got[0].Method != "agent.send_keys" || !slices.Equal(got[0].Keys, []string{"1"}) {
		t.Fatalf("inputs %+v", got)
	}
	waitFor(t, c, "patch", func(p hub.Patch) bool {
		a := p.Agents["w1:p1"]
		return a != nil && a.Status == herdr.StatusWorking
	})
	// A second tap on the same question is refused, not sent.
	answer["id"] = "a2"
	send(t, c, answer)
	wantCode(t, ack(t, c, "a2"), server.CodeQuestionChanged)
	if n := len(inputs(e.fake, "w1:p1")); n != 1 {
		t.Errorf("%d inputs after a repeat answer, want 1", n)
	}
}

func TestPromptToBlockedAgent(t *testing.T) {
	e := start(t)
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	send(t, c, map[string]string{"id": "p1", "type": "prompt", "pane_id": "w1:p1", "text": "carry on"})
	wantCode(t, ack(t, c, "p1"), server.CodeAgentBlocked)
	if got := inputs(e.fake, "w1:p1"); len(got) != 0 {
		t.Errorf("inputs %+v", got)
	}
}

func TestStopSendsEsc(t *testing.T) {
	e := start(t)
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	send(t, c, map[string]any{"id": "k1", "type": "keys", "pane_id": "w1:p2", "keys": []string{"esc"}})
	if a := ack(t, c, "k1"); !a.OK {
		t.Fatalf("ack %+v", a)
	}
	got := inputs(e.fake, "w1:p2")
	if len(got) != 1 || !slices.Equal(got[0].Keys, []string{"esc"}) {
		t.Fatalf("inputs %+v", got)
	}
	send(t, c, map[string]any{"id": "k2", "type": "keys", "pane_id": "w1:p2", "keys": []string{"rm -rf"}})
	wantCode(t, ack(t, c, "k2"), server.CodeInvalidRequest)
}

func TestQuestionChangedRefused(t *testing.T) {
	e := start(t)
	c, d := watch(t, e, "w1:p1")
	// The desktop answers and a different question comes up.
	other, err := os.ReadFile("../fakeherdr/testdata/screens/w2-p1.blocked.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fake.SetScreen("w1:p1", string(other)); err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]string{"id": "a1", "type": "answer", "pane_id": "w1:p1", "question_id": d.Question.ID, "option": "1"})
	wantCode(t, ack(t, c, "a1"), server.CodeQuestionChanged)
	if got := inputs(e.fake, "w1:p1"); len(got) != 0 {
		t.Errorf("inputs %+v", got)
	}
	// The watcher gets the new question.
	waitFor(t, c, "detail", func(d2 hub.Detail) bool { return d2.Question != nil && d2.Question.ID != d.Question.ID })
}

func TestReadOnlyRefusesInput(t *testing.T) {
	e := startWith(t, options{readOnly: true})
	c, d := watch(t, e, "w1:p1")
	reqs := []map[string]any{
		{"id": "r1", "type": "prompt", "pane_id": "w1:p2", "text": "hi"},
		{"id": "r2", "type": "keys", "pane_id": "w1:p2", "keys": []string{"esc"}},
		{"id": "r3", "type": "text", "pane_id": "w1:p2", "text": "hi"},
		{"id": "r4", "type": "answer", "pane_id": "w1:p1", "question_id": d.Question.ID, "option": "1"},
		{"id": "r5", "type": "rename", "pane_id": "w1:p2", "name": "x"},
		{"id": "r6", "type": "focus_on_desktop", "pane_id": "w1:p2"},
	}
	for _, r := range reqs {
		send(t, c, r)
		wantCode(t, ack(t, c, r["id"].(string)), server.CodeReadOnly)
	}
	if got := e.fake.InputLog(); len(got) != 0 {
		t.Errorf("inputs %+v", got)
	}
}

func TestDenyWithNote(t *testing.T) {
	e := start(t)
	c, d := watch(t, e, "w1:p1")
	note := "Use bun run clean instead"
	send(t, c, map[string]string{"id": "a1", "type": "answer", "pane_id": "w1:p1", "question_id": d.Question.ID, "option": "3", "note": note})
	if a := ack(t, c, "a1"); !a.OK {
		t.Fatalf("ack %+v", a)
	}
	got := inputs(e.fake, "w1:p1")
	if len(got) != 2 || !slices.Equal(got[0].Keys, []string{"3"}) || got[1].Method != "agent.prompt" || got[1].Text != note {
		t.Fatalf("inputs %+v", got)
	}
}

func TestTerminalShownSendsOutput(t *testing.T) {
	e := start(t)
	c, _ := watch(t, e, "w1:p2")
	want, err := os.ReadFile("../fakeherdr/testdata/screens/w1-p2.working.txt")
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"type": "terminal", "pane_id": "w1:p2", "shown": true})
	o := waitFor[hub.Output](t, c, "output", nil)
	if o.PaneID != "w1:p2" || o.Text != string(want) {
		t.Errorf("output %+v", o)
	}
}

// A terminal-source agent gets its Now line from the screen.
func TestTerminalSourceDetail(t *testing.T) {
	e := startWith(t, options{noTranscripts: true})
	c, d := watch(t, e, "w1:p2")
	if d.Now == "" {
		d = waitFor(t, c, "detail", func(d hub.Detail) bool { return d.Now != "" })
	}
	if d.Source != hub.SourceTerminal || len(d.Steps) != 0 || d.AgentLabel != "Claude Code" {
		t.Errorf("detail %+v", d)
	}
	// Its last message is the screen's last lines, without the input box.
	if d.LastMessage == nil {
		d = waitFor(t, c, "detail", func(d hub.Detail) bool { return d.LastMessage != nil })
	}
	m := *d.LastMessage
	if !strings.HasPrefix(m, "⏺ Added a token bucket per API key") || !strings.HasSuffix(m, "esc to interrupt)") ||
		strings.Contains(m, "accept edits") || d.Asks {
		t.Errorf("last_message %q asks %v", m, d.Asks)
	}
}

// The studio's Codex agent has no transcript even with transcripts on: its
// last message is its screen's last lines, and it never asks.
func TestCodexAgentLastLines(t *testing.T) {
	e := start(t)
	c, d := watch(t, e, "w4:p2")
	if d.LastMessage == nil {
		d = waitFor(t, c, "detail", func(d hub.Detail) bool { return d.LastMessage != nil })
	}
	m := *d.LastMessage
	if d.Source != hub.SourceTerminal || d.AgentLabel != "Codex" || d.Asks ||
		!strings.HasPrefix(m, "› Write an onboarding guide") || !strings.HasSuffix(m, "already covers it.") {
		t.Errorf("source %q label %q asks %v last_message %q", d.Source, d.AgentLabel, d.Asks, m)
	}
}

// Every edit permission has the same options: one for another file must not
// pass the answer check.
func TestQuestionChangedSameOptions(t *testing.T) {
	e := start(t)
	edit, err := os.ReadFile("../fakeherdr/testdata/screens/blocked.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fake.SetScreen("w1:p1", string(edit)); err != nil {
		t.Fatal(err)
	}
	c, d := watch(t, e, "w1:p1")
	if d.Question == nil || d.Question.Where != "CHANGELOG.md" {
		d = waitFor(t, c, "detail", func(d hub.Detail) bool { return d.Question != nil && d.Question.Where == "CHANGELOG.md" })
	}
	if err := e.fake.SetScreen("w1:p1", strings.ReplaceAll(string(edit), "CHANGELOG.md", "go.mod")); err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]string{"id": "a1", "type": "answer", "pane_id": "w1:p1", "question_id": d.Question.ID, "option": "1"})
	wantCode(t, ack(t, c, "a1"), server.CodeQuestionChanged)
	if got := inputs(e.fake, "w1:p1"); len(got) != 0 {
		t.Errorf("inputs %+v", got)
	}
}

// When the agent stays blocked after the deny digit, the note is not typed
// into whatever is on screen.
func TestDenyStillBlocked(t *testing.T) {
	t.Cleanup(server.SetDenyWait(300 * time.Millisecond))
	e := start(t)
	if err := e.fake.KeepBlocked("w1:p1"); err != nil {
		t.Fatal(err)
	}
	c, d := watch(t, e, "w1:p1")
	send(t, c, map[string]string{"id": "a1", "type": "answer", "pane_id": "w1:p1", "question_id": d.Question.ID, "option": "3", "note": "Use bun run clean instead"})
	wantCode(t, ack(t, c, "a1"), server.CodeStillBlocked)
	got := inputs(e.fake, "w1:p1")
	if len(got) != 1 || got[0].Method != "agent.send_keys" || !slices.Equal(got[0].Keys, []string{"3"}) {
		t.Fatalf("inputs %+v", got)
	}
}

// Raw text could pick an option, so it is refused while the agent is blocked.
func TestTextToBlockedAgent(t *testing.T) {
	e := start(t)
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	send(t, c, map[string]string{"id": "t1", "type": "text", "pane_id": "w1:p1", "text": "1"})
	wantCode(t, ack(t, c, "t1"), server.CodeAgentBlocked)
	if got := inputs(e.fake, "w1:p1"); len(got) != 0 {
		t.Errorf("inputs %+v", got)
	}
}

// A request with an id that fails to decode still gets an ack.
func TestMalformedRequestAcked(t *testing.T) {
	e := start(t)
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	send(t, c, map[string]any{"id": "k1", "type": "keys", "pane_id": "w1:p2", "keys": "esc"})
	wantCode(t, ack(t, c, "k1"), server.CodeInvalidRequest)
	if got := e.fake.InputLog(); len(got) != 0 {
		t.Errorf("inputs %+v", got)
	}
}

// Shutdown waits for a request still running (a deny waiting on the agent)
// and its audit line, but not for idle sockets.
func TestShutdownWaitsForRequests(t *testing.T) {
	t.Cleanup(server.SetDenyWait(300 * time.Millisecond))
	e := start(t)
	idle := dial(t, e)
	read[hub.Snapshot](t, idle)
	if err := e.fake.KeepBlocked("w1:p1"); err != nil {
		t.Fatal(err)
	}
	c, d := watch(t, e, "w1:p1")
	send(t, c, map[string]string{"id": "a1", "type": "answer", "pane_id": "w1:p1", "question_id": d.Question.ID, "option": "3", "note": "Use bun run clean instead"})
	for deadline := time.Now().Add(5 * time.Second); len(inputs(e.fake, "w1:p1")) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the deny digit never reached herdr")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := e.srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("shutdown took %v", took)
	}
	lines := auditLines(t, e)
	if len(lines) != 1 || lines[0].Method != "answer" || lines[0].Result != server.CodeStillBlocked {
		t.Fatalf("audit %+v", lines)
	}
}

// Once shutdown has begun, a new request is dropped before it reaches herdr.
func TestShutdownDropsNewRequests(t *testing.T) {
	e := start(t)
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	if err := e.srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"id": "k1", "type": "keys", "pane_id": "w1:p2", "keys": []string{"esc"}})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	for {
		var m map[string]any
		if err := wsjson.Read(ctx, c, &m); err != nil {
			break // no ack before the timeout
		}
		if m["type"] == "ack" {
			t.Fatalf("ack after shutdown: %v", m)
		}
	}
	if got := e.fake.InputLog(); len(got) != 0 {
		t.Errorf("inputs %+v", got)
	}
}

// A terminal-source agent's screen is read for its detail, but the frames go
// only to an app showing the terminal: a phone on a slow link shouldn't carry
// four screens a second it never shows.
func TestOutputOnlyWhenTerminalShown(t *testing.T) {
	e := startWith(t, options{noTranscripts: true})
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	send(t, c, map[string]string{"type": "watch", "pane_id": "w1:p2"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pinged := false
	for {
		var f struct {
			Type        string  `json:"type"`
			ID          string  `json:"id"`
			LastMessage *string `json:"last_message"`
		}
		if err := wsjson.Read(ctx, c, &f); err != nil {
			t.Fatal(err)
		}
		switch {
		case f.Type == "output":
			t.Fatal("output sent while the terminal isn't shown")
		case f.Type == "detail" && f.LastMessage != nil && !pinged:
			// The screen has been read; anything queued before this ping's ack
			// would show up above.
			send(t, c, map[string]string{"type": "ping", "id": "p1"})
			pinged = true
		}
		if f.Type == "ack" && f.ID == "p1" {
			break
		}
	}
	send(t, c, map[string]any{"type": "terminal", "pane_id": "w1:p2", "shown": true})
	if o := waitFor[hub.Output](t, c, "output", nil); o.PaneID != "w1:p2" || o.Text == "" {
		t.Errorf("output %+v", o)
	}
}

// The app's liveness probe is acked at once and leaves no audit line.
func TestPingAckedNotAudited(t *testing.T) {
	e := start(t)
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	send(t, c, map[string]string{"type": "ping", "id": "p1"})
	if a := ack(t, c, "p1"); !a.OK {
		t.Fatalf("ack %+v", a)
	}
	send(t, c, map[string]any{"type": "keys", "id": "k1", "pane_id": "w1:p2", "keys": []string{"esc"}})
	ack(t, c, "k1")
	for _, l := range auditLines(t, e) {
		if l.Method == "ping" {
			t.Errorf("audit line for a ping: %+v", l)
		}
	}
}

// TestAnswerSeveralQuestions walks fake-herdr's two-question ask on w2:p1:
// a digit answers the first tab, an own answer (digit, text, Enter, each
// checked on screen) the second, and the review's 1 sends them.
func TestAnswerSeveralQuestions(t *testing.T) {
	e := start(t)
	c, d := watch(t, e, "w2:p1")
	q := d.Question
	if q == nil || q.Kind != "ask" || len(q.Steps) != 2 || q.Steps[0].Done || q.Title != "Which database should I migrate to Postgres 18 first?" {
		t.Fatalf("first question %+v", q)
	}
	if own := q.Options[len(q.Options)-1]; !own.Own || own.Key != "3" || own.Label != "Your own answer" {
		t.Fatalf("own option %+v", own)
	}
	send(t, c, map[string]string{"id": "a1", "type": "answer", "pane_id": "w2:p1", "question_id": q.ID, "option": "1"})
	if a := ack(t, c, "a1"); !a.OK {
		t.Fatalf("ack %+v", a)
	}
	d = waitFor(t, c, "detail", func(d hub.Detail) bool { return d.Question != nil && d.Question.ID != q.ID })
	q = d.Question
	if q.Title != "When should the migration run?" || !q.Steps[0].Done || q.Steps[1].Done {
		t.Fatalf("second question %+v", q)
	}

	// An empty own answer is refused before any key.
	send(t, c, map[string]string{"id": "a2", "type": "answer", "pane_id": "w2:p1", "question_id": q.ID, "option": "3", "note": "  "})
	wantCode(t, ack(t, c, "a2"), server.CodeInvalidRequest)
	send(t, c, map[string]string{"id": "a3", "type": "answer", "pane_id": "w2:p1", "question_id": q.ID, "option": "3", "note": "After the\nnightly backup"})
	if a := ack(t, c, "a3"); !a.OK {
		t.Fatalf("ack %+v", a)
	}
	d = waitFor(t, c, "detail", func(d hub.Detail) bool { return d.Question != nil && d.Question.Review != nil })
	q = d.Question
	want := []answer.ReviewItem{
		{Question: "Which database should I migrate to Postgres 18 first?", Answer: "staging"},
		{Question: "When should the migration run?", Answer: "After the nightly backup"},
	}
	if !slices.Equal(q.Review, want) || q.Options[0].Label != "Send answers" {
		t.Fatalf("review %+v", q)
	}
	send(t, c, map[string]string{"id": "a4", "type": "answer", "pane_id": "w2:p1", "question_id": q.ID, "option": "1"})
	if a := ack(t, c, "a4"); !a.OK {
		t.Fatalf("ack %+v", a)
	}
	waitFor(t, c, "patch", func(p hub.Patch) bool { a := p.Agents["w2:p1"]; return a != nil && a.Status == herdr.StatusWorking })

	var got []string
	for _, in := range inputs(e.fake, "w2:p1") {
		got = append(got, in.Method+" "+strings.Join(in.Keys, ",")+in.Text)
	}
	wantIn := []string{"agent.send_keys 1", "agent.send_keys 3", "pane.send_text After the nightly backup", "agent.send_keys enter", "agent.send_keys 1"}
	if !slices.Equal(got, wantIn) {
		t.Errorf("inputs\n got %q\nwant %q", got, wantIn)
	}
}

// TestOwnAnswerStopsWhenTheBoxDoesNotOpen: if the digit doesn't focus the
// box (here the screen is pinned to the first question), no text is typed.
func TestOwnAnswerStopsWhenTheBoxDoesNotOpen(t *testing.T) {
	e := start(t)
	c, d := watch(t, e, "w2:p1")
	screen := readFixture(t, "../answer/testdata/ask-first.txt")
	if err := e.fake.SetScreen("w2:p1", screen); err != nil {
		t.Fatal(err)
	}
	d = waitFor(t, c, "detail", func(d2 hub.Detail) bool { return d2.Question != nil && d2.Question.ID != d.Question.ID })
	send(t, c, map[string]string{"id": "a1", "type": "answer", "pane_id": "w2:p1", "question_id": d.Question.ID, "option": "3", "note": "Kiwi"})
	wantCode(t, ack(t, c, "a1"), server.CodeStopped)
	got := inputs(e.fake, "w2:p1")
	if len(got) != 1 || !slices.Equal(got[0].Keys, []string{"3"}) {
		t.Errorf("inputs %+v, want only the digit", got)
	}
}

func readFixture(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
