// Package push sends Web Push alerts (RFC 8291 payloads, VAPID) when an
// agent becomes blocked or done, and the key-expiry reminders.
//
// State lives in <state>/push (0700): vapid.json, subscriptions.json,
// expiry.json and pending.json, each 0600 and rewritten atomically. Observe, FocusEvent and
// SetExpiry only record state; Run decides and sends (see engine.go).
package push

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/tsoporan/herdr-pocket/internal/answer"
)

// The push dir and its files.
const (
	dirName     = "push"
	vapidFile   = "vapid.json"
	subsFile    = "subscriptions.json"
	expiryFile  = "expiry.json"
	pendingFile = "pending.json"
	// maxPerOwner and maxSubs bound subscriptions.json and each alert's
	// fan-out: readers may subscribe too.
	maxPerOwner    = 10
	maxSubs        = 100
	defaultSettle  = 3 * time.Second
	defaultCoolOff = 60 * time.Second
)

// Errors the server maps to ack codes.
var (
	ErrBadEndpoint = errors.New("bad_endpoint") // not an https push-service URL, or unusable keys
	ErrBadSettings = errors.New("bad_settings") // quiet hours or desk minutes out of range
	ErrNotFound    = errors.New("not_found")    // no such subscription (or the push service dropped it)
	ErrForbidden   = errors.New("forbidden")    // another login owns the subscription
	ErrPushFailed  = errors.New("push_failed")  // the push service refused or could not be reached
	ErrTooMany     = errors.New("too_many")     // the owner (or the computer) has as many subscriptions as allowed
)

// Quiet is a device's quiet hours: "HH:MM" start and end in an IANA zone.
// A window whose end is before its start crosses midnight.
type Quiet struct {
	Start string `json:"start"`
	End   string `json:"end"`
	TZ    string `json:"tz"`
}

// Settings are one device's push choices.
type Settings struct {
	Blocked     bool   `json:"blocked"`
	Done        bool   `json:"done"`
	Quiet       *Quiet `json:"quiet"`
	DeskMinutes int    `json:"desk_minutes"` // 0 turns desk suppression off
}

// DefaultSettings: blocked on, done off, no quiet hours, 5 desk minutes.
func DefaultSettings() Settings { return Settings{Blocked: true, DeskMinutes: 5} }

// UnmarshalJSON decodes onto DefaultSettings, so a missing field keeps its
// default (a missing desk_minutes is 5, not 0).
func (s *Settings) UnmarshalJSON(b []byte) error {
	type plain Settings
	p := plain(DefaultSettings())
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*s = Settings(p)
	return nil
}

// Keys are a subscription's p256dh and auth, base64url as the browser gives them.
type Keys struct {
	P256dh string `json:"p256dh"`
	Auth   string `json:"auth"`
}

// Subscription is the browser's PushSubscription.toJSON() shape.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     Keys   `json:"keys"`
}

// Agent is one agent as the hub sees it. Status is herdr's agent_status
// word (blocked, done, working, idle, ...); Seq is its state_change_seq.
type Agent struct {
	PaneID, Name, Workspace, Status string
	Seq                             uint64
	Focused                         bool
}

// Config configures a Service.
type Config struct {
	// Origin is the app's https://<fqdn>; it is the VAPID subscriber, so no
	// email address leaves the machine. It must start with https://.
	Origin string
	// HTTPClient sends the pushes. Nil means a client that refuses to dial
	// loopback, private, link-local and CGNAT/tailnet addresses.
	HTTPClient *http.Client
	Now        func() time.Time
	// Settle and Cooldown default to 3 s and 60 s when zero.
	Settle, Cooldown time.Duration
	Logf             func(string, ...any)
	// Question and CanAct give a blocked alert its answer buttons (herd
	// pass 1): Question re-reads the pane's question as the alert is sent
	// (hub.Question), and CanAct says whether a subscription's identity may
	// answer it now. Either nil, an error or a slow Question means no
	// buttons; the alert still goes.
	Question func(ctx context.Context, paneID string) (*answer.Question, error)
	CanAct   func(Who) bool
}

// record is one stored subscription. ID is a hash of the endpoint.
type record struct {
	ID       string    `json:"id"`
	Endpoint string    `json:"endpoint"`
	Keys     Keys      `json:"keys"`
	Login    string    `json:"login"` // the owner: a login, or a tagged node's own string
	Node     string    `json:"node"`
	Created  time.Time `json:"created"`
	Settings Settings  `json:"settings"`
	// UserID and Tags are the owner's tailnet identity as last seen
	// (Identify), for CanAct. Missing on a subscription made before herd
	// pass 1 until its device connects again.
	UserID int64    `json:"user_id,omitempty"`
	Tags   []string `json:"tags,omitempty"`

	quiet *window // parsed Settings.Quiet; nil when off
}

// Service owns the subscriptions and the alert state.
type Service struct {
	cfg    Config
	dir    string
	pub    string // VAPID public key, base64url
	priv   string // VAPID private key, base64url
	client *http.Client
	wake   chan struct{}

	stepMu sync.Mutex // serializes step, so a clear always sees the alert before it

	mu       sync.Mutex
	subs     map[string]*record
	panes    map[string]*paneState
	slots    map[slotKey]*slot
	observed bool // the first Observe is a baseline: no alerts for states already there
	// restored is pending.json as Open found it, for the baseline; nil
	// after it. savedPending is what pending.json holds now.
	restored     map[slotKey]string
	savedPending map[slotKey]string
	lastFocus    time.Time
	expiry       *time.Time
	marks        expiryMarks
}

// Open loads or creates <stateDir>/push. A vapid.json that exists but does
// not parse is an error: regenerating it would orphan every subscription.
func Open(stateDir string, cfg Config) (*Service, error) {
	if !strings.HasPrefix(cfg.Origin, "https://") {
		return nil, fmt.Errorf("push: origin %q must start with https://", cfg.Origin)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Settle == 0 {
		cfg.Settle = defaultSettle
	}
	if cfg.Cooldown == 0 {
		cfg.Cooldown = defaultCoolOff
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
	s := &Service{
		cfg:    cfg,
		dir:    dir,
		client: cfg.HTTPClient,
		wake:   make(chan struct{}, 1),
		subs:   map[string]*record{},
		panes:  map[string]*paneState{},
		slots:  map[slotKey]*slot{},
	}
	if s.client == nil {
		s.client = safeClient()
	}
	if err := s.loadVAPID(); err != nil {
		return nil, err
	}
	if err := s.loadSubs(); err != nil {
		return nil, err
	}
	if err := readJSON(filepath.Join(dir, expiryFile), &s.marks); err != nil {
		return nil, err
	}
	// Losing pending.json loses only the alerts waiting at the last stop,
	// so a bad one doesn't stop the start.
	var pending []pendingAlert
	if err := readJSON(filepath.Join(dir, pendingFile), &pending); err != nil {
		cfg.Logf("push: %v; alerts held before the restart are dropped", err)
		pending = nil
	}
	s.restored = make(map[slotKey]string, len(pending))
	for _, p := range pending {
		if _, ok := s.subs[p.Sub]; ok {
			s.restored[slotKey{p.Sub, p.Pane}] = p.Status
		}
	}
	s.savedPending = maps.Clone(s.restored)
	return s, nil
}

// VAPIDPublicKey is the applicationServerKey for PushManager.subscribe.
func (s *Service) VAPIDPublicKey() string { return s.pub }

// Subscribe stores sub for owner (an upsert) and returns the stored settings.
// A new subscription is not alerted for states already on screen. owner is
// the login, or for a tagged node one string per node (see the server).
func (s *Service) Subscribe(owner, node string, sub Subscription, set Settings) (Settings, error) {
	return s.subscribe(owner, node, sub, set, false)
}

// SubscribeNodeOwner is Subscribe for the node's owner. The owner's
// subscriptions are outside the overall cap (still within maxPerOwner), so
// other principals filling it can't lock the owner out of push.
func (s *Service) SubscribeNodeOwner(owner, node string, sub Subscription, set Settings) (Settings, error) {
	return s.subscribe(owner, node, sub, set, true)
}

func (s *Service) subscribe(owner, node string, sub Subscription, set Settings, nodeOwner bool) (Settings, error) {
	if err := checkEndpoint(sub.Endpoint); err != nil {
		return Settings{}, err
	}
	if err := checkKeys(sub.Keys); err != nil {
		return Settings{}, err
	}
	q, err := parseSettings(set)
	if err != nil {
		return Settings{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := subID(sub.Endpoint)
	old := s.subs[id]
	if old != nil && old.Login != owner {
		return Settings{}, ErrForbidden
	}
	if old == nil {
		if (!nodeOwner && len(s.subs) >= maxSubs) || s.countLocked(owner) >= maxPerOwner {
			return Settings{}, ErrTooMany
		}
	}
	r := &record{ID: id, Endpoint: sub.Endpoint, Keys: sub.Keys, Login: owner, Node: node,
		Created: s.cfg.Now().UTC(), Settings: set, quiet: q}
	if old != nil {
		r.Created = old.Created
	}
	s.subs[id] = r
	if err := s.saveSubsLocked(); err != nil {
		if old != nil {
			s.subs[id] = old
		} else {
			delete(s.subs, id)
		}
		return Settings{}, err
	}
	if old == nil && s.observed {
		for _, p := range s.panes {
			s.slot(id, p.agent.PaneID).markAlerted(p.agent)
		}
	}
	s.poke()
	return set, nil
}

// Identify records the tailnet identity behind owner's subscriptions on
// node, which CanAct decides answer buttons from. The server calls it on
// every connection and subscribe, so an identity is never older than the
// device's last visit.
func (s *Service) Identify(owner, node string, userID int64, tags []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for _, r := range s.subs {
		if r.Login == owner && r.Node == node && (r.UserID != userID || !slices.Equal(r.Tags, tags)) {
			r.UserID, r.Tags = userID, slices.Clone(tags)
			changed = true
		}
	}
	if !changed {
		return
	}
	if err := s.saveSubsLocked(); err != nil {
		s.cfg.Logf("push: saving %s: %v", subsFile, err)
	}
}

func (r *record) who() Who {
	return Who{Login: r.Login, Node: r.Node, UserID: r.UserID, Tags: r.Tags}
}

// countLocked is how many subscriptions owner has.
func (s *Service) countLocked(owner string) int {
	n := 0
	for _, r := range s.subs {
		if r.Login == owner {
			n++
		}
	}
	return n
}

// Get returns owner's settings for endpoint.
func (s *Service) Get(owner, endpoint string) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.ownedLocked(owner, endpoint)
	if err != nil {
		return Settings{}, err
	}
	return r.Settings, nil
}

// Unsubscribe removes owner's subscription for endpoint.
func (s *Service) Unsubscribe(owner, endpoint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.ownedLocked(owner, endpoint)
	if err != nil {
		return err
	}
	return s.removeLocked(r.ID)
}

func (s *Service) ownedLocked(owner, endpoint string) (*record, error) {
	r := s.subs[subID(endpoint)]
	if r == nil {
		return nil, ErrNotFound
	}
	if r.Login != owner {
		return nil, ErrForbidden
	}
	return r, nil
}

// removeLocked drops a subscription and its alert state, and saves.
func (s *Service) removeLocked(id string) error {
	if _, ok := s.subs[id]; !ok {
		return nil
	}
	delete(s.subs, id)
	for k := range s.slots {
		if k.sub == id {
			delete(s.slots, k)
		}
	}
	return s.saveSubsLocked()
}

func subID(endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(sum[:16])
}

// checkKeys refuses keys a send would fail on: p256dh must be a P-256 point
// and auth 16 bytes.
func checkKeys(k Keys) error {
	p, err := decodeKey(k.P256dh)
	if err != nil {
		return fmt.Errorf("%w: p256dh: %v", ErrBadEndpoint, err)
	}
	if _, err := ecdh.P256().NewPublicKey(p); err != nil {
		return fmt.Errorf("%w: p256dh: %v", ErrBadEndpoint, err)
	}
	a, err := decodeKey(k.Auth)
	if err != nil || len(a) != 16 {
		return fmt.Errorf("%w: auth must be 16 bytes", ErrBadEndpoint)
	}
	return nil
}

// decodeKey accepts base64url with or without padding (browsers send it
// without).
func decodeKey(v string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(v, "="))
}

// parseSettings validates set and parses its quiet window.
func parseSettings(set Settings) (*window, error) {
	if set.DeskMinutes < 0 || set.DeskMinutes > 24*60 {
		return nil, fmt.Errorf("%w: desk_minutes out of range", ErrBadSettings)
	}
	if set.Quiet == nil {
		return nil, nil
	}
	w, err := parseWindow(*set.Quiet)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSettings, err)
	}
	return w, nil
}

type vapidKeys struct {
	PublicKey  string `json:"public_key"`
	PrivateKey string `json:"private_key"`
}

func (s *Service) loadVAPID() error {
	path := filepath.Join(s.dir, vapidFile)
	var k vapidKeys
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if k.PrivateKey, k.PublicKey, err = webpush.GenerateVAPIDKeys(); err != nil {
			return err
		}
		if err := writeJSON(s.dir, vapidFile, k); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(b, &k); err != nil {
			return fmt.Errorf("push: %s: %w", path, err)
		}
		if err := checkVAPID(k); err != nil {
			return fmt.Errorf("push: %s: %w", path, err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
	}
	s.pub, s.priv = k.PublicKey, k.PrivateKey
	return nil
}

// checkVAPID makes sure the private key derives the stored public key.
func checkVAPID(k vapidKeys) error {
	raw, err := decodeKey(k.PrivateKey)
	if err != nil {
		return err
	}
	priv, err := ecdh.P256().NewPrivateKey(raw)
	if err != nil {
		return err
	}
	pub, err := decodeKey(k.PublicKey)
	if err != nil {
		return err
	}
	if !bytes.Equal(priv.PublicKey().Bytes(), pub) {
		return errors.New("public key does not match private key")
	}
	return nil
}

func (s *Service) loadSubs() error {
	var list []*record
	if err := readJSON(filepath.Join(s.dir, subsFile), &list); err != nil {
		return err
	}
	for _, r := range list {
		q, err := parseSettings(r.Settings)
		if err != nil {
			// Keep the device; it gets alerts without quiet hours until it
			// saves its settings again.
			s.cfg.Logf("push: subscription on %s: %v; quiet hours ignored", endpointHost(r.Endpoint), err)
		}
		r.quiet = q
		s.subs[r.ID] = r
	}
	return nil
}

func (s *Service) saveSubsLocked() error {
	list := make([]*record, 0, len(s.subs))
	for _, r := range s.subs {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return writeJSON(s.dir, subsFile, list)
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
		return fmt.Errorf("push: %s: %w", path, err)
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
