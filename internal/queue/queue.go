// Package queue lines up messages for agents and sends each one when its
// agent is free.
//
// An item targets one pane, or a workspace ("the next free agent there").
// The server keeps the list in <state>/queue/queue.json (dir 0700, file
// 0600, rewritten atomically); it holds message text, which the audit log
// never does. Observe and FocusEvent only record and signal; Run decides and
// sends (see engine.go), so no herdr I/O happens under the hub's lock.
package queue

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	dirName  = "queue"
	fileName = "queue.json"

	defaultSettle = 5 * time.Second
	defaultStall  = 10 * time.Second
	defaultDesk   = 5 * time.Minute

	maxText      = 4000 // bytes, after trimming
	maxWaiting   = 20   // waiting items per target
	keepFinished = 20   // finished items kept per agent and per workspace
	keepDropped  = 7 * 24 * time.Hour
	listWindow   = 24 * time.Hour // finished items List still returns
)

var (
	ErrInvalid    = errors.New("queue: invalid item")
	ErrNotFound   = errors.New("queue: not found")
	ErrFull       = errors.New("queue: too many items waiting")
	ErrNotWaiting = errors.New("queue: item is no longer waiting")
)

// Target is where an item goes: one pane, or the next free agent in a
// workspace. Exactly one field is set.
type Target struct {
	PaneID      string `json:"pane_id,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
}

// State is an item's place in its life: waiting, then sent, then one of the
// finished states.
type State string

const (
	Waiting  State = "waiting"
	Sent     State = "sent"      // herdr accepted agent.prompt
	PickedUp State = "picked_up" // the agent was seen working or blocked after it
	Stalled  State = "stalled"   // not seen working within the stall time
	Failed   State = "failed"    // herdr refused it
	Dropped  State = "dropped"   // its pane closed
)

// Finished reports whether the item will not change again.
func (s State) Finished() bool { return s != Waiting && s != Sent }

// Item is one lined-up message.
type Item struct {
	ID     string `json:"id"`
	Target Target `json:"target"`
	Text   string `json:"text"`
	By     string `json:"by"` // the login that added it
	// Author is who added it, kept so Config.CanSend can check them again
	// against the current access rules right before it is sent.
	Author     *Author    `json:"author,omitempty"`
	Added      time.Time  `json:"added"`
	State      State      `json:"state"`
	SentAt     *time.Time `json:"sent_at,omitempty"`
	PickedUpAt *time.Time `json:"picked_up_at,omitempty"`
	SentTo     string     `json:"sent_to,omitempty"` // the pane it went to
	Error      string     `json:"error,omitempty"`   // herdr's error code when it refused it
	// EndedAt is when it reached a finished state, for the 24 h window and
	// the 7-day rule.
	EndedAt *time.Time `json:"ended_at,omitempty"`
}

// ErrorForbidden is Item.Error for an item whose author may no longer
// send input when its turn came (Config.CanSend).
const ErrorForbidden = "forbidden"

// Author is the identity that lined an item up: enough to decide their
// role again later (an owner by user ID, a tagged node by its tags).
type Author struct {
	Login  string   `json:"login"`
	Node   string   `json:"node,omitempty"`
	UserID int64    `json:"user_id,omitempty"`
	Tags   []string `json:"tags,omitempty"`
}

// Agent is one agent as the hub sees it. Seq changes whenever the hub sees
// a change, so the same (Status, Seq) means nothing happened in between.
type Agent struct {
	PaneID      string
	WorkspaceID string
	Status      string
	Seq         uint64
	Focused     bool
}

// Config configures a Queue. Prompt is required.
type Config struct {
	Now    func() time.Time
	Settle time.Duration // how long an agent stays idle or done before a send; 0 means 5 s
	Stall  time.Duration // how long a sent item may go unstarted; 0 means 10 s
	Desk   time.Duration // a focus event this recent puts the focused agent at the desk; 0 means 5 min
	// Prompt sends text to the pane (herdr agent.prompt). A *herdr.Error is
	// a refusal (agent_blocked puts the item back once); a *net.OpError
	// from the dial means herdr never got it; any other error may have
	// reached herdr, so the item counts as sent. Run does not cancel a
	// started Prompt when it stops, so Prompt must bound itself.
	Prompt func(ctx context.Context, paneID, text string) error
	// OnChange is called after the list changes, without the queue's lock,
	// from Add, Remove and Run; never from Observe or FocusEvent.
	OnChange func()
	// OnSent is called from Run after every Prompt, for the audit. The item
	// has SentTo set, whatever the outcome.
	OnSent func(it Item, err error)
	// CanSend reports whether an item's author may still send input, under
	// the access rules in force now. It is checked right before each send;
	// an item it refuses (or one with no Author) fails with "forbidden"
	// instead. nil allows every item. It must not block.
	CanSend func(Author) bool
	// Clear reports whether the pane's screen is free to type into, read
	// right before each send. herdr can call an agent idle while a dialog is
	// still open (Claude Code's question, with its own-answer box focused),
	// and a prompt's text and Enter would land in it, even submitting
	// answers. false (also for a failed read) holds the item and checks
	// again after ClearRecheck. nil means always clear. It is called
	// without the queue's lock and must bound itself.
	Clear        func(ctx context.Context, paneID string) bool
	ClearRecheck time.Duration // 0 means 5 s
	Logf         func(string, ...any)
}

// paneState is the last observed agent and when its (status, seq) began.
type paneState struct {
	agent Agent
	since time.Time
}

// sendMark is an item whose Prompt is in flight. The item stays waiting in
// memory until the Prompt returns, but is saved as sent to pane at at.
type sendMark struct {
	pane   string
	at     time.Time
	active bool // the pane was seen working or blocked during the Prompt
}

// Queue holds the items and decides when to send them.
type Queue struct {
	cfg  Config
	dir  string
	wake chan struct{}

	stepMu sync.Mutex // serializes step
	saveMu sync.Mutex // serializes saves, so an older snapshot never wins the rename

	mu        sync.Mutex
	items     []*Item // in the order added
	panes     map[string]*paneState
	observed  bool      // panes holds a real agent set; before that no pane is known
	lastFocus time.Time // the last desk activity; Open counts as one
	holdUntil time.Time // no sends before this: herdr didn't answer
	backoff   time.Duration
	sending   map[string]*sendMark  // item id -> its Prompt in flight
	retried   map[string]*paneState // items put back once after agent_blocked, with the pane's state then; lost on restart
	heldFor   map[string]string     // pane → the item Clear last held there, so the hold is logged once
	dirty     bool                  // changed since the last save and OnChange
}

type fileData struct {
	Items []*Item `json:"items"`
}

// Open loads or creates <stateDir>/queue. A queue.json that does not parse
// is an error: starting empty would lose lined-up messages.
func Open(stateDir string, cfg Config) (*Queue, error) {
	if cfg.Prompt == nil {
		return nil, errors.New("queue: Config.Prompt is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Settle == 0 {
		cfg.Settle = defaultSettle
	}
	if cfg.Stall == 0 {
		cfg.Stall = defaultStall
	}
	if cfg.ClearRecheck == 0 {
		cfg.ClearRecheck = 5 * time.Second
	}
	if cfg.Desk == 0 {
		cfg.Desk = defaultDesk
	}
	if cfg.OnChange == nil {
		cfg.OnChange = func() {}
	}
	if cfg.OnSent == nil {
		cfg.OnSent = func(Item, error) {}
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	dir := filepath.Join(stateDir, dirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	var data fileData
	if err := readJSON(filepath.Join(dir, fileName), &data); err != nil {
		return nil, err
	}
	return &Queue{
		cfg:  cfg,
		dir:  dir,
		wake: make(chan struct{}, 1),
		// Until herdr reports focus, the focused pane may be in use: a
		// restart (a deploy from the desk) holds it like a focus event.
		lastFocus: cfg.Now(),
		items:     slices.DeleteFunc(data.Items, func(it *Item) bool { return it == nil }),
		panes:     map[string]*paneState{},
		sending:   map[string]*sendMark{},
		retried:   map[string]*paneState{},
		heldFor:   map[string]string{},
	}, nil
}

// Add lines up text for target. The text is trimmed and must be 1–4000
// bytes of UTF-8; a pane target must be a current agent, and a target may
// hold at most 20 waiting items.
func (q *Queue) Add(by Author, t Target, text string) (Item, error) {
	text = strings.TrimSpace(text)
	switch {
	case by.Login == "":
		return Item{}, fmt.Errorf("%w: no login", ErrInvalid)
	case (t.PaneID == "") == (t.WorkspaceID == ""):
		return Item{}, fmt.Errorf("%w: set exactly one of pane_id and workspace_id", ErrInvalid)
	case text == "" || len(text) > maxText:
		return Item{}, fmt.Errorf("%w: text must be 1 to %d bytes", ErrInvalid, maxText)
	case !utf8.ValidString(text):
		return Item{}, fmt.Errorf("%w: text is not UTF-8", ErrInvalid)
	}

	out, err := q.add(by, t, text)
	if err != nil {
		return Item{}, err
	}
	q.poke()
	q.cfg.OnChange()
	return out, nil
}

// add appends and saves the item, under saveMu.
func (q *Queue) add(by Author, t Target, text string) (Item, error) {
	q.saveMu.Lock()
	defer q.saveMu.Unlock()
	q.mu.Lock()
	if t.PaneID != "" && q.panes[t.PaneID] == nil {
		q.mu.Unlock()
		return Item{}, fmt.Errorf("%w: no agent in pane %s", ErrNotFound, t.PaneID)
	}
	waiting := 0
	for _, it := range q.items {
		if it.State == Waiting && it.Target == t {
			waiting++
		}
	}
	if waiting >= maxWaiting {
		q.mu.Unlock()
		return Item{}, ErrFull
	}
	by.Tags = slices.Clone(by.Tags)
	it := &Item{ID: rand.Text(), Target: t, Text: text, By: by.Login, Author: &by, Added: q.cfg.Now(), State: Waiting}
	q.items = append(q.items, it)
	out := *it
	snap := q.snapshotLocked()
	q.dirty = false
	q.mu.Unlock()

	if err := q.write(snap); err != nil {
		q.mu.Lock()
		defer q.mu.Unlock()
		q.dirty = true
		if q.sending[it.ID] != nil {
			// A step is about to send it; that step saves it first, or
			// doesn't send. Taking it out now would ack a failure for a
			// message that may still reach the agent.
			q.cfg.Logf("queue: saving %s: %v; item %s kept, it is being sent", fileName, err, it.ID)
			return out, nil
		}
		q.items = slices.DeleteFunc(q.items, func(x *Item) bool { return x == it })
		return Item{}, fmt.Errorf("queue: saving: %w", err)
	}
	return out, nil
}

// Remove deletes a waiting item. Any controller may remove any item; login
// is for the log.
func (q *Queue) Remove(login, id string) error {
	if err := q.remove(id); err != nil {
		return err
	}
	q.cfg.Logf("queue: %s removed item %s", login, id)
	q.poke() // another item may now be due for that agent
	q.cfg.OnChange()
	return nil
}

// remove deletes and saves, under saveMu.
func (q *Queue) remove(id string) error {
	q.saveMu.Lock()
	defer q.saveMu.Unlock()
	q.mu.Lock()
	i := slices.IndexFunc(q.items, func(it *Item) bool { return it.ID == id })
	if i < 0 {
		q.mu.Unlock()
		return ErrNotFound
	}
	it := q.items[i]
	if it.State != Waiting || q.sending[id] != nil {
		q.mu.Unlock()
		return ErrNotWaiting
	}
	q.items = slices.Delete(q.items, i, i+1)
	delete(q.retried, id)
	snap := q.snapshotLocked()
	q.dirty = false
	q.mu.Unlock()

	if err := q.write(snap); err != nil {
		q.mu.Lock()
		q.items = slices.Insert(q.items, min(i, len(q.items)), it)
		q.dirty = true
		q.mu.Unlock()
		return fmt.Errorf("queue: saving: %w", err)
	}
	return nil
}

// List returns the items not yet finished and those finished in the last
// 24 hours, in the order added. It takes only the queue's short lock, so the
// hub may call it under its own.
func (q *Queue) List() []Item {
	q.mu.Lock()
	defer q.mu.Unlock()
	cutoff := q.cfg.Now().Add(-listWindow)
	out := []Item{}
	for _, it := range q.items {
		if !it.State.Finished() || it.EndedAt == nil || it.EndedAt.After(cutoff) {
			out = append(out, *it)
		}
	}
	return out
}

// snapshotLocked copies the items for a save, an item with its Prompt in
// flight as sent. Callers hold mu.
func (q *Queue) snapshotLocked() []*Item {
	out := make([]*Item, len(q.items))
	for i, it := range q.items {
		c := *it
		if m := q.sending[it.ID]; m != nil && c.State == Waiting {
			c.State, c.SentAt, c.SentTo = Sent, &m.at, m.pane
		}
		out[i] = &c
	}
	return out
}

func (q *Queue) write(items []*Item) error {
	return writeJSON(q.dir, fileName, fileData{Items: items})
}

// saveDirty saves the list if it changed since the last save, and reports
// whether it changed. A failed save is retried on the next step.
func (q *Queue) saveDirty() (bool, error) {
	q.saveMu.Lock()
	defer q.saveMu.Unlock()
	q.mu.Lock()
	if !q.dirty {
		q.mu.Unlock()
		return false, nil
	}
	q.dirty = false
	snap := q.snapshotLocked()
	q.mu.Unlock()
	if err := q.write(snap); err != nil {
		q.mu.Lock()
		q.dirty = true
		q.mu.Unlock()
		return true, err
	}
	return true, nil
}

// readJSON decodes path into v; a missing file leaves v alone. An existing
// file is set to 0600.
func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("queue: %s: %w", path, err)
	}
	return os.Chmod(path, 0o600)
}

// writeJSON replaces dir/name atomically with mode 0600, synced so a power
// loss leaves the old file or the new one, never an empty one.
func writeJSON(dir, name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, name+".*") // CreateTemp uses 0600
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op after the rename
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
