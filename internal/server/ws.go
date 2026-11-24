package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/tsoporan/herdr-pocket/internal/answer"
	"github.com/tsoporan/herdr-pocket/internal/audit"
	"github.com/tsoporan/herdr-pocket/internal/herdr"
	"github.com/tsoporan/herdr-pocket/internal/hub"
	"github.com/tsoporan/herdr-pocket/internal/push"
	"github.com/tsoporan/herdr-pocket/internal/queue"
	"github.com/tsoporan/herdr-pocket/internal/start"
	"github.com/tsoporan/herdr-pocket/internal/tailnet"
)

const (
	writeTimeout = 10 * time.Second
	readLimit    = 64 << 10 // one app message; prompts are typed on a phone
	maxInFlight  = 16       // requests per socket still waiting on herdr
)

// denyWait is how long a "Don't allow" with a note waits for the agent to
// leave blocked before it gives up with still_blocked. New copies it into
// each handler; tests shorten it through export_test.go.
var denyWait = 5 * time.Second

// Ack error codes.
const (
	CodeAgentBlocked    = "agent_blocked"    // a prompt to an agent waiting on a question
	CodeQuestionChanged = "question_changed" // the answered question is no longer on screen
	CodeStillBlocked    = "still_blocked"    // the deny digit went, the note didn't
	CodeReadOnly        = "read_only"        // the server runs --read-only
	CodeForbidden       = "forbidden"        // the viewer is a reader
	CodeNotFound        = "not_found"        // no such agent
	CodeHerdrError      = "herdr_error"      // herdr refused or failed
	CodeInvalidRequest  = "invalid_request"  // a malformed or unsupported request
	CodeBadEndpoint     = "bad_endpoint"     // push.subscribe to a URL that isn't a known push service
	CodeBadSettings     = "bad_settings"     // push settings out of range (quiet hours, desk minutes)
	CodePushFailed      = "push_failed"      // the push engine or the push service failed
	CodeTooMany         = "too_many"         // push.subscribe past the per-login (or total) subscription cap
	CodeQueueFull       = "queue_full"       // queue.add past the waiting items a target may hold
	CodeNotWaiting      = "not_waiting"      // queue.remove of an item already sent
	CodeQueueFailed     = "queue_failed"     // the queue couldn't save the change
	CodeUnknownKind     = "unknown_kind"     // start with a kind the server doesn't offer
	CodeInvalidName     = "invalid_name"     // start with a name herdr would refuse
	CodeNameTaken       = "name_taken"       // start with a live agent's name, or a starting one's
	CodeTooLong         = "too_long"         // start with a task over 4000 bytes
	CodeStopped         = "stopped"          // an own answer stopped part way: the screen wasn't as the next step expected
)

// ownWait is how long each step of an own answer waits for the screen to
// show what the step did (ownAnswer), and ownPoll how often it reads it.
const (
	ownWait = 3 * time.Second
	ownPoll = 100 * time.Millisecond
)

// maxOwnAnswer bounds an own answer typed into a question, in bytes.
const maxOwnAnswer = 2000

// pushTestTimeout bounds push.test's send, which runs on past a closed
// socket (see read) and so must not hold up shutdown for long.
const pushTestTimeout = 15 * time.Second

// inbound is any app message. Only watch, unwatch and terminal have no id.
type inbound struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	PaneID     string   `json:"pane_id"`
	Text       string   `json:"text"`
	Keys       []string `json:"keys"`
	QuestionID string   `json:"question_id"`
	Option     string   `json:"option"`
	Note       string   `json:"note"`
	Name       string   `json:"name"`
	Shown      bool     `json:"shown"`
	// The push.* messages'.
	Subscription *push.Subscription `json:"subscription"`
	Settings     *push.Settings     `json:"settings"`
	Endpoint     string             `json:"endpoint"`
	// The queue.* messages': queue.add's target, queue.remove's item.
	Target *queue.Target `json:"target"`
	ItemID string        `json:"item_id"`
	// start's (with Name): the app-made job id, where, what and the task.
	JobID       string `json:"job_id"`
	WorkspaceID string `json:"workspace_id"`
	Kind        string `json:"kind"`
	Task        string `json:"task"`
}

// AckError is a failed request's reason.
type AckError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *AckError) Error() string { return e.Code + ": " + e.Message }

// Ack answers one request.
type Ack struct {
	Type  string    `json:"type"` // "ack"
	ID    string    `json:"id"`
	OK    bool      `json:"ok"`
	Error *AckError `json:"error,omitempty"`
	// Settings are the stored push settings, on push.subscribe and
	// push.get acks that succeeded.
	Settings *push.Settings `json:"settings,omitempty"`
	// ItemID is the new item's, on a queue.add that succeeded, so the app
	// can follow it.
	ItemID string `json:"item_id,omitempty"`
}

func fail(code, format string, args ...any) *AckError {
	return &AckError{Code: code, Message: fmt.Sprintf(format, args...)}
}

type wsHandler struct {
	build    string // bundleID of the embedded app
	owner    int64  // the node owner's user ID; 0 when the node is tagged
	hub      *hub.Hub
	origins  []string
	input    *herdr.Client // nil: read-only
	audit    *audit.Log
	push     *push.Service // nil: push is off
	queue    *queue.Queue  // nil: the queue is off
	starts   *start.Runner // nil: starting agents is off
	denyWait time.Duration
	requests *requests
}

// requests counts the app requests running across all sockets, so shutdown
// can wait for their acks (and audit lines) before the audit log closes.
type requests struct {
	mu      sync.Mutex
	closing bool
	wg      sync.WaitGroup
}

func newRequests() *requests { return &requests{} }

// begin registers a request; false once close has been called.
func (r *requests) begin() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return false
	}
	r.wg.Add(1)
	return true
}

func (r *requests) end() { r.wg.Done() }

// close stops new requests and waits until the running ones end or ctx does.
func (r *requests) close(ctx context.Context) error {
	r.mu.Lock()
	r.closing = true
	r.mu.Unlock()
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *wsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	who, ok := viewerFrom(r.Context())
	if !ok { // authenticate runs first; this only guards against miswiring
		refuse(w)
		return
	}
	origin := r.Header.Get("Origin")
	if !slices.Contains(s.origins, origin) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	// The exact check above is the policy; the pattern only stops Accept's
	// own host check refusing localhost against a 127.0.0.1 Host.
	u, err := url.Parse(origin)
	if err != nil {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	// While herdr is down, refuse the upgrade so the app keeps showing that
	// it is reconnecting and retries with backoff.
	snap, sub, err := s.hub.Subscribe()
	if err != nil {
		http.Error(w, "herdr unavailable", http.StatusServiceUnavailable)
		return
	}
	defer sub.Close()
	snap.Role = string(who.role)
	snap.Build = s.build
	if s.push != nil {
		snap.Push = &hub.PushInfo{VAPIDPublicKey: s.push.VAPIDPublicKey()}
		s.identify(who)
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{u.Host}})
	if err != nil {
		return // Accept has written the error response
	}
	defer c.CloseNow()
	c.SetReadLimit(readLimit)
	if d := describeDrop(r.URL.Query().Get("drop")); d != "" {
		log.Printf("server: ws %s opened; the app says its last socket ended: %s", who.peer.Node, d)
	}

	// One line per socket when it ends, with why, so a phone's flaky
	// connection leaves a trace. Deferred before the waits below, so it runs
	// after the read loop has returned and readErr is set.
	started := time.Now()
	var why string
	var readErr error
	defer func() {
		if why == "" {
			why = fmt.Sprintf("read: %v", readErr)
		}
		log.Printf("server: ws %s closed after %s: %s", who.peer.Node, time.Since(started).Round(time.Second), why)
	}()

	ctx, cancel := context.WithCancel(r.Context())
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	wg.Go(func() {
		defer cancel()
		readErr = s.read(ctx, c, sub, &wg, who)
	})

	if err := write(ctx, c, snap); err != nil {
		why = fmt.Sprintf("write snapshot: %v", err)
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-sub.C:
			if !ok {
				why = "dropped by the hub (the app fell behind, or herdr went away)"
				c.Close(websocket.StatusTryAgainLater, "reconnect for a fresh snapshot")
				return
			}
			if err := write(ctx, c, msg); err != nil {
				why = fmt.Sprintf("write: %v", err)
				return
			}
		}
	}
}

// read handles the app's messages until the socket ends. Watch, unwatch and
// terminal apply at once; requests run on their own, since a deny with a
// note can wait seconds, and each gets an ack.
func (s *wsHandler) read(ctx context.Context, c *websocket.Conn, sub *hub.Subscription, wg *sync.WaitGroup, who viewer) error {
	inFlight := make(chan struct{}, maxInFlight)
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return err
		}
		var m inbound
		if err := json.Unmarshal(data, &m); err != nil {
			log.Printf("server: bad app message: %v", err)
			// A wrong field type still decodes the id; that request gets
			// its ack rather than leaving the app to time out.
			if m.ID != "" && s.requests.begin() {
				s.ack(ctx, c, who, m, nil, fail(CodeInvalidRequest, "malformed request"))
				s.requests.end()
			}
			continue
		}
		switch m.Type {
		case "watch":
			if s.hub.Has(m.PaneID) {
				sub.Watch(m.PaneID)
			}
			// Opening the agent (a tap on its notification does) counts as
			// seeing its alert, so no clear follows to a closed one.
			if s.push != nil {
				s.push.Seen(pushOwner(who.peer), who.peer.Node, m.PaneID)
			}
		case "unwatch":
			sub.Unwatch(m.PaneID)
		case "terminal":
			sub.ShowTerminal(m.PaneID, m.Shown)
		case "ping":
			// The app's liveness probe: acked at once, not audited or logged.
			if err := write(ctx, c, Ack{Type: "ack", ID: m.ID, OK: true}); err != nil {
				return err
			}
		default:
			if m.ID == "" {
				log.Printf("server: app request %q without an id", m.Type)
				continue
			}
			// After shutdown has begun the audit log may close at any
			// moment, so a new request is dropped unacked and unaudited;
			// it never reaches herdr.
			if !s.requests.begin() {
				log.Printf("server: stopping; dropped %s request %s", m.Type, m.ID)
				continue
			}
			select {
			case inFlight <- struct{}{}:
			default:
				s.ack(ctx, c, who, m, nil, fail(CodeInvalidRequest, "too many requests waiting"))
				s.requests.end()
				continue
			}
			wg.Go(func() {
				defer s.requests.end()
				defer func() { <-inFlight }()
				// A started request runs to its end even if the socket
				// closes (herdr calls and the deny wait are bounded), so
				// the audit line has its real result.
				ctx := context.WithoutCancel(ctx)
				if strings.HasPrefix(m.Type, "push.") {
					settings, e := s.doPush(ctx, who, m)
					s.ack(ctx, c, who, m, settings, e)
					return
				}
				if strings.HasPrefix(m.Type, "queue.") {
					id, e := s.doQueue(who, m)
					if m.Type == "queue.add" {
						m.ItemID = id // the new item's, for the audit; empty if refused
					}
					s.ack(ctx, c, who, m, nil, e)
					return
				}
				if m.Type == "start" {
					s.ack(ctx, c, who, m, nil, s.doStart(who, m))
					return
				}
				s.ack(ctx, c, who, m, nil, s.do(ctx, who, m))
			})
		}
	}
}

// ack answers a request and audits it. Every request (each message with an
// id) is a mutation, so this one place audits all of them, refused ones too.
// settings go only on a push.subscribe or push.get ack that succeeded.
func (s *wsHandler) ack(ctx context.Context, c *websocket.Conn, who viewer, m inbound, settings *push.Settings, e *AckError) {
	a := Ack{Type: "ack", ID: m.ID, OK: e == nil, Error: e, Settings: settings}
	if e == nil && m.Type == "queue.add" {
		a.ItemID = m.ItemID
	}
	result := "ok"
	if e != nil {
		result = e.Code
		// The code only: payloads are never logged.
		log.Printf("server: %s %s: %s", m.Type, m.PaneID, e.Code)
	}
	entry := audit.Entry{Login: who.peer.Login, Node: who.peer.Node, Method: m.Type, PaneID: m.PaneID, ItemID: m.ItemID, Result: result}
	if m.Target != nil && m.Type == "queue.add" {
		entry.PaneID, entry.WorkspaceID = m.Target.PaneID, m.Target.WorkspaceID
	}
	if m.Type == "start" {
		entry.WorkspaceID, entry.ItemID, entry.Kind, entry.Name = m.WorkspaceID, m.JobID, m.Kind, m.Name
	}
	if err := s.audit.Record(entry, payload(m)); err != nil {
		log.Printf("server: audit: %v", err)
	}
	if err := write(ctx, c, a); err != nil && ctx.Err() == nil {
		log.Printf("server: ws write: %v", err)
	}
}

// payload is what the audit log hashes for a request: the text it would
// send to herdr (now, or when lined up with queue.add), or for push.* the
// endpoint (plus the settings asked for). queue.remove has none; its item
// id is in the line.
// The endpoint is a bearer URL, so it is only ever hashed, never logged.
func payload(m inbound) []byte {
	switch m.Type {
	case "push.subscribe":
		var endpoint string
		if m.Subscription != nil {
			endpoint = m.Subscription.Endpoint
		}
		settings, _ := json.Marshal(m.Settings)
		return []byte(endpoint + "\n" + string(settings))
	case "push.get", "push.unsubscribe", "push.test":
		return []byte(m.Endpoint)
	case "prompt", "text":
		return []byte(m.Text)
	case "queue.add":
		// The queue keeps and later sends the trimmed text; hashing the
		// same bytes matches this line to the send's (AuditQueued).
		return []byte(strings.TrimSpace(m.Text))
	case "start":
		// Likewise the start runner, for its prompt (AuditStart).
		return []byte(strings.TrimSpace(m.Task))
	case "keys":
		return []byte(strings.Join(m.Keys, " "))
	case "answer":
		return []byte(m.Option + "\n" + m.Note)
	case "rename":
		return []byte(m.Name)
	}
	return nil
}

// do checks a request and carries it out.
func (s *wsHandler) do(ctx context.Context, who viewer, m inbound) *AckError {
	switch m.Type {
	case "prompt", "keys", "text", "answer", "rename", "focus_on_desktop":
	default:
		return fail(CodeInvalidRequest, "unknown request %q", m.Type)
	}
	if who.role != Controller {
		return fail(CodeForbidden, "view only")
	}
	if s.input == nil {
		return fail(CodeReadOnly, "this server is read-only")
	}
	// Input goes only to a pane the hub lists, never to an agent name.
	if !s.hub.Has(m.PaneID) {
		return fail(CodeNotFound, "no agent %q", m.PaneID)
	}
	switch m.Type {
	case "prompt":
		if strings.TrimSpace(m.Text) == "" {
			return fail(CodeInvalidRequest, "empty prompt")
		}
		return fromHerdr(s.input.Prompt(ctx, m.PaneID, m.Text))
	case "keys":
		if err := herdr.ValidateKeys(m.Keys); err != nil {
			return fail(CodeInvalidRequest, "%v", err)
		}
		return fromHerdr(s.input.SendKeys(ctx, m.PaneID, m.Keys))
	case "text":
		if m.Text == "" {
			return fail(CodeInvalidRequest, "empty text")
		}
		// A digit typed into a permission prompt picks an option, so text
		// to a blocked agent would answer it without the question check.
		// herdr refuses only agent.prompt there; this refuses text too.
		if s.hub.Blocked(m.PaneID) {
			return fail(CodeAgentBlocked, "the agent is waiting on a question")
		}
		return fromHerdr(s.input.SendText(ctx, m.PaneID, m.Text))
	case "answer":
		return s.answer(ctx, answerArgs{paneID: m.PaneID, questionID: m.QuestionID, option: m.Option, note: m.Note})
	case "rename":
		return fromHerdr(s.input.Rename(ctx, m.PaneID, m.Name))
	default: // focus_on_desktop
		if err := s.input.Focus(ctx, m.PaneID); err != nil {
			return fromHerdr(err)
		}
		// Asking for the agent on the desktop means someone is going there.
		s.hub.FocusEvent()
		return nil
	}
}

// answerArgs is one answer, from the app's answer sheet or a notification.
type answerArgs struct {
	paneID, questionID, option, note string
	// notification limits the answer to a notification's two buttons: the
	// question must be a Bash permission (push.NotifyKeys) and the option
	// its "Allow once" or "Don't allow". It never carries a note.
	notification bool
}

// answer sends an option's digit, but only if the question the app showed is
// still the one on screen: it re-reads and re-parses the screen and compares
// ids. With a note (for a "tell it what to do instead" option) it then waits
// for the agent to leave blocked and sends the note as a prompt; herdr
// refuses a prompt while it is still blocked, so the note can't land in the
// wrong UI. The caller has checked the role, read-only and the pane.
func (s *wsHandler) answer(ctx context.Context, a answerArgs) *AckError {
	q, err := s.hub.Question(ctx, a.paneID)
	if errors.Is(err, hub.ErrNotFound) {
		return fail(CodeNotFound, "no agent %q", a.paneID)
	}
	if err != nil {
		return fromHerdr(err)
	}
	if q == nil || q.ID != a.questionID {
		return fail(CodeQuestionChanged, "the question changed")
	}
	i := slices.IndexFunc(q.Options, func(o answer.Option) bool { return o.Key == a.option })
	if i < 0 {
		return fail(CodeInvalidRequest, "no option %q", a.option)
	}
	opt := q.Options[i]
	note := strings.TrimSpace(a.note)
	if note != "" && (!opt.NeedsNote || a.notification) {
		return fail(CodeInvalidRequest, "option %q takes no note", a.option)
	}
	if opt.Own {
		// The box takes one line: Enter there answers. So line breaks
		// become spaces, and the text is typed, never pasted with Enter.
		note = strings.Join(strings.Fields(note), " ")
		switch {
		case note == "":
			return fail(CodeInvalidRequest, "your own answer is empty")
		case len(note) > maxOwnAnswer:
			return fail(CodeInvalidRequest, "your own answer is over %d bytes", maxOwnAnswer)
		}
	}
	// Checked before the claim, so a refused tap can't block the real answer.
	if a.notification {
		allow, deny, ok := push.NotifyKeys(q)
		if !ok {
			return fail(CodeInvalidRequest, "this question is answered in the app")
		}
		if opt.Key != allow && opt.Key != deny {
			return fail(CodeInvalidRequest, "option %q is answered in the app", a.option)
		}
	}
	if !s.hub.ClaimAnswer(a.paneID, q.ID) {
		return fail(CodeQuestionChanged, "the question was already answered")
	}
	if opt.Own {
		return s.ownAnswer(ctx, a.paneID, q.ID, opt.Key, note)
	}
	if err := s.input.SendKeys(ctx, a.paneID, []string{opt.Key}); err != nil {
		var he *herdr.Error
		if errors.As(err, &he) {
			s.hub.ReleaseAnswer(a.paneID, q.ID) // herdr refused it, so nothing was sent
		}
		return fromHerdr(err)
	}
	if note == "" {
		return nil
	}
	wait, cancel := context.WithTimeout(ctx, s.denyWait)
	defer cancel()
	if _, err := s.hub.WaitStatus(wait, a.paneID, func(st herdr.Status) bool { return st != herdr.StatusBlocked }); err != nil {
		return fail(CodeStillBlocked, "the agent is still waiting, so the note wasn't sent")
	}
	if e := fromHerdr(s.input.Prompt(ctx, a.paneID, note)); e != nil {
		if e.Code == CodeAgentBlocked {
			return fail(CodeStillBlocked, "the agent is waiting again, so the note wasn't sent")
		}
		return e
	}
	return nil
}

// ownAnswer types an own answer into AskUserQuestion's "Type something."
// row, one step at a time, reading the screen after each, so no key lands
// blind (verified against Claude Code on 2026-11-23):
//
//  1. the row's digit must focus its box, still empty;
//  2. the typed text must show in the box;
//  3. only then does Enter answer the tab with it.
//
// When a step's screen isn't as expected it stops with CodeStopped and
// sends nothing more; the box may be left open with the text in it, which
// the message says. herdr reports the agent idle or done while the box is
// focused, which is why the steps read the screen rather than the hub.
func (s *wsHandler) ownAnswer(ctx context.Context, paneID, questionID, key, text string) *AckError {
	if err := s.input.SendKeys(ctx, paneID, []string{key}); err != nil {
		var he *herdr.Error
		if errors.As(err, &he) {
			s.hub.ReleaseAnswer(paneID, questionID) // refused, so nothing was sent
		}
		return fromHerdr(err)
	}
	focused := func(screen string) bool { got, ok := answer.OwnBox(screen, key); return ok && got == "" }
	if e := s.waitScreen(ctx, paneID, focused); e != nil {
		return e
	}
	if err := s.input.SendText(ctx, paneID, text); err != nil {
		return fromHerdr(err)
	}
	// The box shows the text's first screen line; a long answer wraps.
	typed := func(screen string) bool {
		got, ok := answer.OwnBox(screen, key)
		return ok && got != "" && strings.HasPrefix(text, got)
	}
	if e := s.waitScreen(ctx, paneID, typed); e != nil {
		return fail(CodeStopped, "your answer is typed in the box but wasn't sent; check the terminal")
	}
	return fromHerdr(s.input.SendKeys(ctx, paneID, []string{"enter"}))
}

// waitScreen reads the pane's screen until want accepts it, for up to
// ownWait. It fails with CodeStopped when it never does.
func (s *wsHandler) waitScreen(ctx context.Context, paneID string, want func(string) bool) *AckError {
	ctx, cancel := context.WithTimeout(ctx, ownWait)
	defer cancel()
	for {
		res, err := s.input.Read(ctx, paneID, herdr.SourceVisible, 0)
		if err == nil && want(res.Text) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fail(CodeStopped, "the agent's screen changed, so nothing more was sent")
		case <-time.After(ownPoll):
		}
	}
}

// fromHerdr maps a herdr result to an ack error, or nil for success.
func fromHerdr(err error) *AckError {
	if err == nil {
		return nil
	}
	var he *herdr.Error
	if errors.As(err, &he) {
		switch he.Code {
		case herdr.CodeAgentBlocked:
			return fail(CodeAgentBlocked, "%s", he.Message)
		case herdr.CodeAgentNotFound, herdr.CodePaneNotFound:
			return fail(CodeNotFound, "%s", he.Message)
		}
		return fail(CodeHerdrError, "%s: %s", he.Code, he.Message)
	}
	return fail(CodeHerdrError, "%v", err)
}

func write(ctx context.Context, c *websocket.Conn, v any) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return wsjson.Write(ctx, c, v)
}

// doPush carries out a push.* request. Push is reading, not acting, so
// readers and a --read-only server may use it; a
// subscription belongs to the login that made it. It returns the stored
// settings for push.subscribe and push.get.
func (s *wsHandler) doPush(ctx context.Context, who viewer, m inbound) (*push.Settings, *AckError) {
	if s.push == nil {
		return nil, fail(CodeInvalidRequest, "push is off on this server")
	}
	login, node := pushOwner(who.peer), who.peer.Node
	var (
		settings push.Settings
		err      error
	)
	switch m.Type {
	case "push.subscribe":
		if m.Subscription == nil {
			return nil, fail(CodeInvalidRequest, "no subscription")
		}
		set := push.DefaultSettings()
		if m.Settings != nil {
			set = *m.Settings
		}
		subscribe := s.push.Subscribe
		if s.owner != 0 && who.peer.UserID == s.owner && len(who.peer.Tags) == 0 {
			subscribe = s.push.SubscribeNodeOwner
		}
		settings, err = subscribe(login, node, *m.Subscription, set)
		if err == nil {
			s.identify(who)
		}
	case "push.get":
		settings, err = s.push.Get(login, m.Endpoint)
	case "push.unsubscribe":
		return nil, fromPush(s.push.Unsubscribe(login, m.Endpoint))
	case "push.test":
		ctx, cancel := context.WithTimeout(ctx, pushTestTimeout)
		defer cancel()
		return nil, fromPush(s.push.Test(ctx, login, m.Endpoint))
	default:
		return nil, fail(CodeInvalidRequest, "unknown request %q", m.Type)
	}
	if e := fromPush(err); e != nil {
		return nil, e
	}
	return &settings, nil
}

// identify hands the push engine who is behind this login's subscriptions
// on this device, for its answer buttons (CanAct).
func (s *wsHandler) identify(who viewer) {
	s.push.Identify(pushOwner(who.peer), who.peer.Node, who.peer.UserID, who.peer.Tags)
}

// pushOwner is who owns a peer's subscriptions: its login, except that every
// tagged node shares the "tagged-devices" login, so a tagged node owns its
// own, by node name.
func pushOwner(p tailnet.Peer) string {
	if len(p.Tags) > 0 {
		return p.Login + "/" + p.Node
	}
	return p.Login
}

// fromPush maps a push engine result to an ack error, or nil for success.
func fromPush(err error) *AckError {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, push.ErrBadEndpoint):
		return fail(CodeBadEndpoint, "not a push service address")
	case errors.Is(err, push.ErrNotFound):
		return fail(CodeNotFound, "no such subscription")
	case errors.Is(err, push.ErrForbidden):
		return fail(CodeForbidden, "another login owns this subscription")
	case errors.Is(err, push.ErrBadSettings):
		return fail(CodeBadSettings, "settings out of range")
	case errors.Is(err, push.ErrTooMany):
		return fail(CodeTooMany, "too many subscriptions")
	}
	// The engine's errors may name the endpoint, a bearer URL: the app is
	// told only that it failed.
	return fail(CodePushFailed, "the push failed")
}

// doQueue carries out a queue.* request; queue.add returns the new item's
// id. Lining up work is acting, so it is for controllers on a server that
// takes input; any controller may remove any waiting item.
func (s *wsHandler) doQueue(who viewer, m inbound) (string, *AckError) {
	switch m.Type {
	case "queue.add", "queue.remove":
	default:
		return "", fail(CodeInvalidRequest, "unknown request %q", m.Type)
	}
	if who.role != Controller {
		return "", fail(CodeForbidden, "view only")
	}
	if s.input == nil {
		return "", fail(CodeReadOnly, "this server is read-only")
	}
	if s.queue == nil {
		return "", fail(CodeInvalidRequest, "the queue is off on this server")
	}
	if m.Type == "queue.remove" {
		if m.ItemID == "" {
			return "", fail(CodeInvalidRequest, "no item_id")
		}
		return "", fromQueue(s.queue.Remove(who.peer.Login, m.ItemID))
	}
	t := m.Target
	if t == nil || (t.PaneID == "") == (t.WorkspaceID == "") {
		return "", fail(CodeInvalidRequest, "target needs exactly one of pane_id and workspace_id")
	}
	// Work is lined up only for a pane or workspace the hub lists.
	if t.PaneID != "" && !s.hub.Has(t.PaneID) {
		return "", fail(CodeNotFound, "no agent %q", t.PaneID)
	}
	if t.WorkspaceID != "" && !s.hub.HasWorkspace(t.WorkspaceID) {
		return "", fail(CodeNotFound, "no workspace %q", t.WorkspaceID)
	}
	it, err := s.queue.Add(queue.Author{Login: who.peer.Login, Node: who.peer.Node, UserID: who.peer.UserID, Tags: who.peer.Tags}, *t, m.Text)
	if err != nil {
		return "", fromQueue(err)
	}
	return it.ID, nil
}

// fromQueue maps a queue result to an ack error, or nil for success. The
// queue's messages name panes and limits, never the text.
func fromQueue(err error) *AckError {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, queue.ErrInvalid):
		return fail(CodeInvalidRequest, "%v", err)
	case errors.Is(err, queue.ErrNotFound):
		return fail(CodeNotFound, "%v", err)
	case errors.Is(err, queue.ErrFull):
		return fail(CodeQueueFull, "too many messages lined up")
	case errors.Is(err, queue.ErrNotWaiting):
		return fail(CodeNotWaiting, "it was already sent")
	}
	log.Printf("server: queue: %v", err)
	return fail(CodeQueueFailed, "the queue couldn't save it")
}

// AuditQueued returns the queue's Config.OnSent: it audits each lined-up
// message the queue sends as method prompt, via queue, by the login that
// lined it up, with the text's size and hash only.
func AuditQueued(l *audit.Log) func(queue.Item, error) {
	return func(it queue.Item, err error) {
		result := "ok"
		if e := fromHerdr(err); e != nil {
			result = e.Code
		}
		pane := it.SentTo
		if pane == "" {
			pane = it.Target.PaneID
		}
		entry := audit.Entry{Login: it.By, Method: "prompt", PaneID: pane, WorkspaceID: it.Target.WorkspaceID, ItemID: it.ID, Result: result, Via: "queue"}
		if err := l.Record(entry, []byte(it.Text)); err != nil {
			log.Printf("server: audit: %v", err)
		}
	}
}

// doStart begins a start job: a new tab in the workspace, an agent of the
// kind and name in it, and the task. Starting an agent is acting, so it is
// for controllers on a server that takes input. Every check is made here
// and in Begin before anything reaches herdr; the steps then run on the
// server's context and arrive as starts patches, so the ack doesn't wait
// for them.
func (s *wsHandler) doStart(who viewer, m inbound) *AckError {
	if who.role != Controller {
		return fail(CodeForbidden, "view only")
	}
	if s.input == nil {
		return fail(CodeReadOnly, "this server is read-only")
	}
	if s.starts == nil {
		return fail(CodeInvalidRequest, "starting agents is off on this server")
	}
	// The phone never sends a folder: the runner takes the workspace's.
	_, err := s.starts.Begin(
		queue.Author{Login: who.peer.Login, Node: who.peer.Node, UserID: who.peer.UserID, Tags: who.peer.Tags},
		start.Request{ID: m.JobID, WorkspaceID: m.WorkspaceID, Kind: m.Kind, Name: m.Name, Task: m.Task},
	)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, start.ErrUnknownWorkspace):
		return fail(CodeNotFound, "no workspace %q", m.WorkspaceID)
	case errors.Is(err, start.ErrUnknownKind):
		return fail(CodeUnknownKind, "%v", err)
	case errors.Is(err, start.ErrInvalidName):
		return fail(CodeInvalidName, "%v", err)
	case errors.Is(err, start.ErrNameTaken):
		return fail(CodeNameTaken, "%v", err)
	case errors.Is(err, start.ErrTooLong):
		return fail(CodeTooLong, "%v", err)
	}
	// ErrInvalid; its message names the field, never the task.
	return fail(CodeInvalidRequest, "%v", err)
}

// AuditStart returns the start runner's Config.OnStep: it audits each herdr
// call a start job makes (tab.create, agent.start, and the task as prompt),
// via start, by the login that asked, with the task's size and hash only.
func AuditStart(l *audit.Log) func(start.Job, string, []byte, error) {
	return func(j start.Job, method string, payload []byte, err error) {
		// herdr's own code (agent_pane_busy; agent_not_ready, which the
		// client makes from agent.wait) says what happened better than an
		// ack code would.
		result := "ok"
		var he *herdr.Error
		switch {
		case errors.As(err, &he):
			result = he.Code
		case err != nil:
			result = CodeHerdrError
		}
		entry := audit.Entry{Login: j.By, Method: method, PaneID: j.PaneID, WorkspaceID: j.WorkspaceID, ItemID: j.ID, Kind: j.Kind, Name: j.Name, Result: result, Via: "start"}
		if err := l.Record(entry, payload); err != nil {
			log.Printf("server: audit: %v", err)
		}
	}
}
