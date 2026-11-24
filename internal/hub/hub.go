// Package hub keeps the inventory of herdr agents and fans it out to app
// clients: each subscriber gets a full Snapshot, then Patch messages.
//
// One goroutine owns the herdr side. It opens one events.subscribe stream for
// the global pane, tab and workspace types plus one stream per agent pane
// (herdr fixes a stream's subscription set for its life, and one unknown
// pane_id fails a whole subscribe). After every subscribe it re-reads
// agent.list and workspace.list, so a change in the gap isn't lost.
// pane.agent_status_changed is applied from its payload; every other event
// triggers a coalesced re-list. When herdr drops, the hub goes offline: it
// closes every subscriber and refuses new ones (ErrOffline) until it has
// reconnected and re-listed, so the app shows the outage instead of a stale
// list. The last inventory is kept, so unchanged agents keep their changed_at.
//
// Row lines come from each Claude agent's transcript (see Line), re-read on
// every list and status change and every enrichEvery. A subscriber may also
// watch panes (watch.go): the hub then polls each watched pane and sends a
// Detail when the agent view changes and an Output when its screen does.
//
// With a Pusher attached (push.go), the hub feeds the push engine the full
// agent set after every change, every herdr focus event, and the key expiry.
// With a Queuer attached (queue.go), it feeds the work queue the same agent
// set and focus events, and carries the queue's items in snapshots and
// patches.
package hub

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/herdr"
	"github.com/tsoporan/herdr-pocket/internal/queue"
	"github.com/tsoporan/herdr-pocket/internal/start"
	"github.com/tsoporan/herdr-pocket/internal/transcript"
)

// Agent is one agent row as the app sees it (WebSocket contract v1).
type Agent struct {
	PaneID string `json:"pane_id"`
	// Name is the name shown (see Name); HerdrName is herdr's own, "" when
	// it has none, which a new agent's name must not repeat.
	Name           string       `json:"name"`
	HerdrName      string       `json:"herdr_name,omitempty"`
	Agent          string       `json:"agent"`
	Status         herdr.Status `json:"status"`
	Line           string       `json:"line"`
	WorkspaceID    string       `json:"workspace_id"`
	WorkspaceLabel string       `json:"workspace_label"`
	TabID          string       `json:"tab_id"`
	ChangedAt      time.Time    `json:"changed_at"`
	// StateChangeSeq is herdr's recency counter. It orders agents whose
	// changed_at ties (all agents seen at server start share one). Status
	// events carry no seq, so it lags until the next re-list.
	StateChangeSeq uint64            `json:"state_change_seq"`
	Tokens         map[string]string `json:"tokens"`
	Focused        bool              `json:"focused"`
}

// equal compares records; ChangedAt is always UTC, so DeepEqual is exact.
func (a Agent) equal(b Agent) bool { return reflect.DeepEqual(a, b) }

// Workspace is one workspace as the app sees it.
type Workspace struct {
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Number      int    `json:"number"`
	// Path is the most common cwd among its agents, "" when it has none: a
	// new tab started from the app opens there (see WorkspacePath).
	Path       string `json:"path"`
	AgentCount int    `json:"agent_count"`
}

// Snapshot is the first message a subscriber gets.
type Snapshot struct {
	Type string `json:"type"` // "snapshot"
	Host string `json:"host"`
	// Role is the viewer's, "controller" or "reader". It is per connection,
	// so the hub leaves it empty and the server fills it in.
	Role string `json:"role"`
	// Expiry is null unless the sign-in ends within ExpiryWarnDays.
	Expiry *Expiry `json:"expiry"`
	// Push is what the app needs to subscribe, or null when push is off.
	// The server fills it in, like Role.
	Push *PushInfo `json:"push"`
	// Build identifies the app bundle the server embeds; the server fills
	// it in. An app that sees it change after a reconnect reloads itself.
	Build      string      `json:"build,omitempty"`
	Agents     []Agent     `json:"agents"`
	Workspaces []Workspace `json:"workspaces"`
	// Queue is the work lined up for agents: the unfinished items plus
	// those finished in the last 24 h. Empty when the queue is off.
	Queue []queue.Item `json:"queue"`
	// StartKinds are the agent kinds the app may start; Starts are the
	// start jobs, running and finished in the last hour. Both are empty
	// when starting is off.
	StartKinds []StartKind `json:"start_kinds"`
	Starts     []start.Job `json:"starts"`
}

// Patch carries changed records keyed by ID; nil means removed.
type Patch struct {
	Type       string                `json:"type"` // "patch"
	Agents     map[string]*Agent     `json:"agents,omitempty"`
	Workspaces map[string]*Workspace `json:"workspaces,omitempty"`
	// Expiry is absent when unchanged; see ExpiryPatch.
	Expiry *ExpiryPatch `json:"expiry,omitempty"`
	// Queue is absent when unchanged, else the whole list as in Snapshot.
	Queue *[]queue.Item `json:"queue,omitempty"`
	// Starts is absent when unchanged, else the whole list as in Snapshot.
	Starts *[]start.Job `json:"starts,omitempty"`
}

// Name is the herdr agent name, else the terminal title, else the agent kind.
func Name(a herdr.Agent) string {
	for _, s := range []string{a.Name, a.TerminalTitleStripped, a.Title, a.DisplayAgent, a.Agent} {
		if s != "" {
			return s
		}
	}
	return a.PaneID
}

type record struct {
	raw       herdr.Agent
	snap      *transcript.Snapshot // nil: no usable transcript
	line      string
	changedAt time.Time
	// seenChange is set once the hub saw this agent's status change. Until
	// then changedAt is when the hub first saw it, and the transcript's
	// times are better (see since).
	seenChange bool
	// epoch changes with every status change the hub sees, and whenever a
	// re-list shows herdr's state_change_seq moved. Status events carry no
	// seq, so it stands in for that in question ids.
	epoch uint64
	// pushSeq is the push engine's state_change_seq stand-in: it moves once
	// per status the agent enters, or when a re-list shows herdr saw a
	// change the hub missed. Unlike epoch, the re-list that catches up on a
	// status event (catchUp) adopts herdr's seq without moving it, so an
	// alert the engine holds isn't dropped as stale.
	pushSeq uint64
	catchUp bool
}

// since is when the agent entered its current status. Before the hub has
// seen a real change, the transcript wins: the turn's start for working,
// the pending call for blocked, the last assistant record for done and
// idle. It is never later than when the hub first saw the agent.
func (r *record) since() time.Time {
	if r.seenChange || r.snap == nil {
		return r.changedAt
	}
	var t time.Time
	switch r.raw.AgentStatus {
	case herdr.StatusWorking:
		t = r.snap.TurnStart
	case herdr.StatusBlocked:
		if r.snap.Pending != nil {
			t = r.snap.Pending.At
		}
	case herdr.StatusDone, herdr.StatusIdle:
		t = r.snap.LastAssistant
	}
	if t.IsZero() || t.After(r.changedAt) {
		return r.changedAt
	}
	return t
}

const subscriberBuffer = 64

// Hub is the shared inventory.
type Hub struct {
	client      *herdr.Client
	transcripts *transcript.Reader // nil: no transcripts, every agent is terminal-source
	host        string
	now         func() time.Time

	mu         sync.Mutex
	ctx        context.Context // Start's; pollers run under it
	records    map[string]*record
	rawWS      map[string]herdr.Workspace
	agents     map[string]Agent // last published
	workspaces map[string]Workspace
	subs       map[*Subscription]struct{}
	watched    map[string]*paneWatch
	answered   map[string]claim // pane → the last question answered
	online     bool
	keyExpiry  *time.Time   // the node key's; nil when expiry is off
	expiry     *Expiry      // last published
	pusher     Pusher       // nil: push is off
	queue      Queuer       // nil: the queue is off
	queueItems []queue.Item // last published
	starter    Starter      // nil: starting agents is off
	startKinds []StartKind
	starts     []start.Job // last published
	// listed is set by the first agent.list; pushFed and queueFed once the
	// pusher and the queue have had an agent set. The push engine takes its
	// first set as the baseline (no alerts for states already there), and
	// the queue drops the items of panes missing from it, so neither may
	// get an empty set from before the hub has listed.
	listed, pushFed, queueFed bool
	// epoch numbers status changes for question ids. It starts at a random
	// value so ids from before a restart don't come back.
	epoch   uint64
	changed chan struct{} // closed and replaced on every published change
	kick    chan struct{} // asks the enrich loop to re-read transcripts now
	// queueKick asks the queue loop to publish the queue's items (see
	// QueueChanged).
	queueKick chan struct{}
	// startsKick asks the starts loop to publish the jobs (StartsChanged).
	startsKick chan struct{}
	// relistKick asks the session to re-list (Relist).
	relistKick chan struct{}
}

// ErrOffline is returned by Subscribe while the hub has no herdr connection.
var ErrOffline = errors.New("hub: herdr is not connected")

// ErrNotFound is returned for a pane the hub has no agent for.
var ErrNotFound = errors.New("hub: no such agent")

// New returns a hub that reads herdr through client and reports host in
// snapshots. transcripts reads Claude Code sessions; nil leaves every agent
// terminal-source.
func New(client *herdr.Client, host string, transcripts *transcript.Reader) *Hub {
	return &Hub{
		client:      client,
		transcripts: transcripts,
		host:        host,
		now:         time.Now,
		records:     map[string]*record{},
		rawWS:       map[string]herdr.Workspace{},
		agents:      map[string]Agent{},
		workspaces:  map[string]Workspace{},
		subs:        map[*Subscription]struct{}{},
		watched:     map[string]*paneWatch{},
		answered:    map[string]claim{},
		epoch:       rand.Uint64(),
		changed:     make(chan struct{}),
		kick:        make(chan struct{}, 1),
		queueKick:   make(chan struct{}, 1),
		queueItems:  []queue.Item{},
		startsKick:  make(chan struct{}, 1),
		relistKick:  make(chan struct{}, 1),
		startKinds:  []StartKind{},
		starts:      []start.Job{},
	}
}

// Subscription is one app's view: a snapshot, then Patch, Detail and Output
// messages on C. C is closed if the app falls behind or herdr drops (either
// way it should reconnect for a fresh snapshot), or on Close.
type Subscription struct {
	C <-chan any

	h        *Hub
	ch       chan any
	terminal map[string]bool // panes whose terminal the app shows; guarded by h.mu
	watching map[string]bool // guarded by h.mu
}

// Subscribe returns the current snapshot and a subscription for what
// follows, or ErrOffline while herdr is not connected.
func (h *Hub) Subscribe() (Snapshot, *Subscription, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.online {
		return Snapshot{}, nil, ErrOffline
	}
	// Finished items age out of List without a change; catch up first.
	h.publishQueueLocked()
	h.publishStartsLocked()
	snap := Snapshot{
		Type:       "snapshot",
		Host:       h.host,
		Expiry:     h.expiry,
		Agents:     slices.SortedFunc(maps.Values(h.agents), func(a, b Agent) int { return strings.Compare(a.PaneID, b.PaneID) }),
		Workspaces: slices.SortedFunc(maps.Values(h.workspaces), func(a, b Workspace) int { return a.Number - b.Number }),
		Queue:      h.queueItems,
		StartKinds: h.startKinds,
		Starts:     h.starts,
	}
	if snap.Agents == nil {
		snap.Agents = []Agent{}
	}
	if snap.Workspaces == nil {
		snap.Workspaces = []Workspace{}
	}
	ch := make(chan any, subscriberBuffer)
	sub := &Subscription{C: ch, h: h, ch: ch, terminal: map[string]bool{}, watching: map[string]bool{}}
	h.subs[sub] = struct{}{}
	return snap, sub, nil
}

// Close ends the subscription and its watches.
func (s *Subscription) Close() {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	s.h.dropLocked(s)
}

// dropLocked removes a subscriber and closes its channel. Callers hold h.mu.
func (h *Hub) dropLocked(s *Subscription) {
	if _, ok := h.subs[s]; !ok {
		return
	}
	delete(h.subs, s)
	for pane := range s.watching {
		h.unwatchLocked(s, pane)
	}
	close(s.ch)
}

// sendLocked queues msg for s, dropping s if it has fallen behind: it
// reconnects to a fresh snapshot. Callers hold h.mu.
func (h *Hub) sendLocked(s *Subscription, msg any) {
	select {
	case s.ch <- msg:
	default:
		h.dropLocked(s)
	}
}

// setOnline records whether herdr is connected. Going offline closes every
// subscriber.
func (h *Hub) setOnline(online bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.online = online
	if online {
		return
	}
	for s := range h.subs {
		h.dropLocked(s)
	}
}

// agentLocked is the outward record. Callers hold h.mu.
func (h *Hub) agentLocked(r *record) Agent {
	tokens := r.raw.Tokens
	if tokens == nil {
		tokens = map[string]string{}
	}
	return Agent{
		PaneID:         r.raw.PaneID,
		Name:           Name(r.raw),
		HerdrName:      r.raw.Name,
		Agent:          r.raw.Agent,
		Status:         r.raw.AgentStatus,
		Line:           r.line,
		WorkspaceID:    r.raw.WorkspaceID,
		WorkspaceLabel: h.rawWS[r.raw.WorkspaceID].Label,
		TabID:          r.raw.TabID,
		ChangedAt:      r.since().UTC().Truncate(time.Millisecond),
		StateChangeSeq: r.raw.StateChangeSeq,
		Tokens:         tokens,
		Focused:        r.raw.Focused,
	}
}

// publish recomputes the outward records, and sends a patch of whatever
// differs from the last published state. Callers hold h.mu.
func (h *Hub) publish() {
	agents := make(map[string]Agent, len(h.records))
	for id, r := range h.records {
		agents[id] = h.agentLocked(r)
	}
	cwds := map[string]map[string]int{} // workspace → agent cwd → agents there
	for _, r := range h.records {
		ws := r.raw.WorkspaceID
		if cwds[ws] == nil {
			cwds[ws] = map[string]int{}
		}
		cwds[ws][r.raw.Cwd]++
	}
	workspaces := make(map[string]Workspace, len(h.rawWS))
	for id, w := range h.rawWS {
		path, count := commonPath(cwds[id])
		workspaces[id] = Workspace{WorkspaceID: w.WorkspaceID, Label: w.Label, Number: w.Number, Path: path, AgentCount: count}
	}

	p := Patch{Type: "patch", Agents: map[string]*Agent{}, Workspaces: map[string]*Workspace{}}
	for id, a := range agents {
		if old, ok := h.agents[id]; !ok || !old.equal(a) {
			p.Agents[id] = &a
		}
	}
	for id := range h.agents {
		if _, ok := agents[id]; !ok {
			p.Agents[id] = nil
		}
	}
	for id, w := range workspaces {
		if old, ok := h.workspaces[id]; !ok || old != w {
			p.Workspaces[id] = &w
		}
	}
	for id := range h.workspaces {
		if _, ok := workspaces[id]; !ok {
			p.Workspaces[id] = nil
		}
	}
	h.agents, h.workspaces = agents, workspaces
	if len(p.Agents) > 0 || (h.pusher != nil && !h.pushFed) || (h.queue != nil && !h.queueFed) {
		h.observeLocked()
	}
	if len(p.Agents) == 0 && len(p.Workspaces) == 0 {
		return
	}
	close(h.changed)
	h.changed = make(chan struct{})
	for s := range h.subs {
		h.sendLocked(s, p)
	}
	for id := range p.Agents {
		if pw := h.watched[id]; pw != nil {
			pw.wakeDetail()
		}
	}
}

// statusChangedLocked records a status change the hub saw. Callers hold h.mu.
func (h *Hub) statusChangedLocked(r *record) {
	r.changedAt = h.now()
	r.seenChange = true
	h.epoch++
	r.epoch = h.epoch
	r.pushSeq = h.epoch
}

// applyLists replaces the inventory with fresh list results, keeping
// changed_at for agents whose status is unchanged. snaps holds the
// transcripts read for them (see readTranscripts).
func (h *Hub) applyLists(agents []herdr.Agent, workspaces []herdr.Workspace, snaps map[string]*transcript.Snapshot) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	records := make(map[string]*record, len(agents))
	for _, a := range agents {
		r := h.records[a.PaneID]
		switch {
		case r == nil:
			h.epoch++
			r = &record{changedAt: now, epoch: h.epoch, pushSeq: h.epoch}
		case r.raw.AgentStatus != a.AgentStatus:
			h.statusChangedLocked(r)
		case r.raw.StateChangeSeq != a.StateChangeSeq:
			// The same status, but herdr saw a change the hub missed (say,
			// blocked on another prompt while disconnected). The first
			// re-list after a status event also lands here, since events
			// carry no seq; that only costs an open sheet a question_changed.
			h.epoch++
			r.epoch = h.epoch
			if !r.catchUp {
				r.pushSeq = h.epoch
			}
		}
		r.catchUp = false
		r.raw = a
		r.snap = snaps[a.PaneID]
		r.line = Line(r.raw, r.snap, now)
		records[a.PaneID] = r
	}
	h.records = records
	h.listed = true
	h.rawWS = make(map[string]herdr.Workspace, len(workspaces))
	for _, w := range workspaces {
		h.rawWS[w.WorkspaceID] = w
	}
	h.publish()
}

// applyStatus applies one status change. It reports false for an unknown pane.
func (h *Hub) applyStatus(ev herdr.AgentStatusChanged) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.records[ev.PaneID]
	if !ok {
		return false
	}
	if r.raw.AgentStatus != ev.AgentStatus {
		r.raw.AgentStatus = ev.AgentStatus
		h.statusChangedLocked(r)
		r.catchUp = true // the event carries no seq; the next re-list does
		r.line = Line(r.raw, r.snap, h.now())
		h.publish()
		// The transcript was likely written just before the change.
		select {
		case h.kick <- struct{}{}:
		default:
		}
	}
	return true
}

// applyTranscripts stores freshly read transcripts and publishes the lines
// that changed.
func (h *Hub) applyTranscripts(snaps map[string]*transcript.Snapshot) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, snap := range snaps {
		h.applyTranscriptLocked(id, snap)
	}
	h.publish()
}

// applyTranscriptLocked stores a pane's transcript and re-derives its line
// if the transcript changed, or if the agent is working, whose line counts
// how long the current step has run. It reports whether the line or the
// transcript changed. Callers hold h.mu.
func (h *Hub) applyTranscriptLocked(paneID string, snap *transcript.Snapshot) bool {
	r := h.records[paneID]
	if r == nil {
		return false
	}
	same := reflect.DeepEqual(r.snap, snap)
	if same && r.raw.AgentStatus != herdr.StatusWorking {
		return false
	}
	r.snap = snap
	line := Line(r.raw, snap, h.now())
	changed := !same || line != r.line
	r.line = line
	return changed
}

// readTranscript reads a Claude agent's session, or returns nil for any
// other agent or an unusable transcript (the agent is then terminal-source).
func (h *Hub) readTranscript(a herdr.Agent) *transcript.Snapshot {
	s := a.AgentSession
	if h.transcripts == nil || a.Agent != "claude" || s == nil || s.Kind != "id" || s.Value == "" {
		return nil
	}
	snap, err := h.transcripts.Read(s.Value)
	if err != nil {
		if !errors.Is(err, transcript.ErrNotFound) && !errors.Is(err, transcript.ErrUnusable) {
			log.Printf("hub: transcript for %s: %v", a.PaneID, err)
		}
		return nil
	}
	return &snap
}

// readTranscripts reads every agent's transcript, keyed by pane.
func (h *Hub) readTranscripts(agents []herdr.Agent) map[string]*transcript.Snapshot {
	snaps := make(map[string]*transcript.Snapshot, len(agents))
	for _, a := range agents {
		snaps[a.PaneID] = h.readTranscript(a)
	}
	return snaps
}

func (h *Hub) rawAgents() []herdr.Agent {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]herdr.Agent, 0, len(h.records))
	for _, r := range h.records {
		out = append(out, r.raw)
	}
	return out
}

func (h *Hub) paneIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Collect(maps.Keys(h.records))
}

// enrichEvery is how often the row lines are re-read from the transcripts.
// A read is a stat when the file is unchanged, so this is cheap.
const enrichEvery = 5 * time.Second

// enrich keeps the row lines current with the transcripts.
func (h *Hub) enrich(ctx context.Context) {
	t := time.NewTicker(enrichEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-h.kick:
		}
		h.applyTranscripts(h.readTranscripts(h.rawAgents()))
	}
}

// Start checks herdr's protocol and builds the first inventory, then keeps
// it current in the background until ctx is done. It fails with
// herdr.ErrProtocolTooOld (or a connection error) if the first sync fails.
func (h *Hub) Start(ctx context.Context) error {
	h.mu.Lock()
	h.ctx = ctx
	h.mu.Unlock()
	s, err := h.connect(ctx)
	if err != nil {
		return err
	}
	go h.run(ctx, s)
	go h.enrich(ctx)
	go h.expiryLoop(ctx)
	go h.queueLoop(ctx)
	go h.startsLoop(ctx)
	return nil
}

const (
	minBackoff = time.Second
	maxBackoff = 5 * time.Second // herdr is a local socket; retrying is cheap
	relistWait = 150 * time.Millisecond
)

func (h *Hub) run(ctx context.Context, s *session) {
	backoff := minBackoff
	for {
		if s != nil {
			backoff = minBackoff
			err := s.loop()
			s.close()
			h.setOnline(false)
			if ctx.Err() != nil {
				return
			}
			log.Printf("hub: herdr connection lost: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
		var err error
		if s, err = h.connect(ctx); err != nil {
			if errors.Is(err, herdr.ErrProtocolTooOld) {
				log.Printf("hub: %v", err)
			} else {
				log.Printf("hub: reconnect to herdr: %v", err)
			}
			s = nil
		}
	}
}

// session is one connection's worth of herdr streams.
type session struct {
	h      *Hub
	ctx    context.Context
	cancel context.CancelFunc
	events chan herdr.Event
	ended  chan string // a per-pane stream ended
	global chan error  // the global stream ended
	panes  map[string]context.CancelFunc
}

func (h *Hub) connect(ctx context.Context) (*session, error) {
	if _, err := h.client.CheckProtocol(ctx); err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(ctx)
	s := &session{
		h: h, ctx: sctx, cancel: cancel,
		events: make(chan herdr.Event, 64),
		ended:  make(chan string, 8),
		global: make(chan error, 1),
		panes:  map[string]context.CancelFunc{},
	}
	stream, err := h.client.Subscribe(sctx, herdr.GlobalSubscriptions())
	if err != nil {
		cancel()
		return nil, err
	}
	go func() {
		s.global <- s.read(stream)
	}()
	if err := s.refresh(); err != nil {
		s.close()
		return nil, err
	}
	h.setOnline(true)
	return s, nil
}

func (s *session) close() { s.cancel() }

// read forwards a stream's events until it ends.
func (s *session) read(stream *herdr.Stream) error {
	defer stream.Close()
	for {
		ev, err := stream.Next()
		if err != nil {
			return err
		}
		select {
		case s.events <- ev:
		case <-s.ctx.Done():
			return s.ctx.Err()
		}
	}
}

// refresh re-lists agents and workspaces, then opens a status stream for each
// new agent pane and closes those of panes that are gone. Opening a stream
// leaves a gap since the list, so it lists once more until no stream opens.
func (s *session) refresh() error {
	for {
		agents, err := s.h.client.Agents(s.ctx)
		if err != nil {
			return err
		}
		workspaces, err := s.h.client.Workspaces(s.ctx)
		if err != nil {
			return err
		}
		s.h.applyLists(agents, workspaces, s.h.readTranscripts(agents))

		opened := false
		live := map[string]bool{}
		for _, a := range agents {
			live[a.PaneID] = true
			if _, ok := s.panes[a.PaneID]; ok {
				continue
			}
			pctx, cancel := context.WithCancel(s.ctx)
			stream, err := s.h.client.Subscribe(pctx, []herdr.Subscription{herdr.AgentStatusSubscription(a.PaneID)})
			var herr *herdr.Error
			if errors.As(err, &herr) && herr.Code == herdr.CodePaneNotFound {
				cancel() // closed since the list; the next re-list drops it
				continue
			}
			if err != nil {
				cancel()
				return fmt.Errorf("subscribe to pane %s: %w", a.PaneID, err)
			}
			s.panes[a.PaneID] = cancel
			opened = true
			go func() {
				_ = s.read(stream)
				if pctx.Err() != nil {
					return // closed by refresh or the session
				}
				select {
				case s.ended <- a.PaneID:
				case <-s.ctx.Done():
				}
			}()
		}
		for id, cancel := range s.panes {
			if !live[id] {
				cancel()
				delete(s.panes, id)
			}
		}
		if !opened {
			return nil
		}
	}
}

// loop applies events until the global stream or ctx ends.
func (s *session) loop() error {
	var relist <-chan time.Time
	schedule := func() {
		if relist == nil {
			relist = time.After(relistWait)
		}
	}
	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case err := <-s.global:
			return err
		case id := <-s.ended:
			// A pane stream ended (the pane likely closed); re-list, which
			// reopens it if the pane is still an agent.
			if cancel, ok := s.panes[id]; ok {
				cancel()
				delete(s.panes, id)
			}
			schedule()
		case ev := <-s.events:
			switch ev.Kind {
			case herdr.EventPaneFocused, herdr.EventTabFocused, herdr.EventWorkspaceFocused:
				// Someone is at the desk; the re-list below picks up which
				// agent is focused.
				s.h.FocusEvent()
			}
			if ev.Kind == herdr.EventPaneAgentStatusChanged {
				var p herdr.AgentStatusChanged
				if err := ev.Decode(&p); err == nil && s.h.applyStatus(p) {
					continue
				}
			}
			schedule()
		case <-s.h.relistKick:
			schedule()
		case <-relist:
			relist = nil
			if err := s.refresh(); err != nil {
				return err
			}
		}
	}
}
