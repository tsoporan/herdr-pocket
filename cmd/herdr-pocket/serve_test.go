package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/tsoporan/herdr-pocket/internal/audit"
	"github.com/tsoporan/herdr-pocket/internal/fakeherdr"
	"github.com/tsoporan/herdr-pocket/internal/hub"
	"github.com/tsoporan/herdr-pocket/internal/tailnet"
)

// The serve path end to end: real flags, lock, fake-herdr, hub, server and
// audit log, with a fake node in place of tsnet. Its listener is plain HTTP
// on loopback; the origin the server accepts still comes from Self.FQDN.

// failingNode is TestMain's guard: a test that forgets to fake the node
// fails here instead of starting tsnet.
type failingNode struct{ err error }

func (f failingNode) Start(context.Context) (tailnet.Self, error)   { return tailnet.Self{}, f.err }
func (f failingNode) Refresh(context.Context) (tailnet.Self, error) { return tailnet.Self{}, f.err }
func (f failingNode) Identity() tailnet.Identity                    { return nil }
func (f failingNode) Listen() (net.Listener, error)                 { return nil, f.err }
func (f failingNode) Close() error                                  { return nil }

// The tailnet users and node; all invented.
const (
	testFQDN    = "pocket.example-tailnet.ts.net"
	testOrigin  = "https://" + testFQDN
	testOwnerID = 1001
)

var (
	owner    = tailnet.Peer{Login: "owner@example.com", UserID: testOwnerID, Node: "owner-phone"}
	friend   = tailnet.Peer{Login: "friend@example.com", UserID: 3003, Node: "friend-phone"}
	stranger = tailnet.Peer{Login: "stranger@example.com", UserID: 2002, Node: "their-laptop"}
	// A tagged node carrying the owner's user ID: tags are checked first.
	kiosk = tailnet.Peer{Login: "tagged-devices", UserID: testOwnerID, Node: "kiosk", Tags: []string{"tag:kiosk"}}
)

// fakeIdentity answers WhoIs with the peer the test set last.
type fakeIdentity struct {
	mu   sync.Mutex
	peer tailnet.Peer
}

func (f *fakeIdentity) set(p tailnet.Peer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.peer = p
}

func (f *fakeIdentity) WhoIs(context.Context, string) (tailnet.Peer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peer, nil
}

// fakeNode stands in for tailnet.Node.
type fakeNode struct {
	self     tailnet.Self
	startErr error
	who      *fakeIdentity
	cfg      tailnet.Config

	mu      sync.Mutex
	refresh *tailnet.Self // what Refresh returns once set; else self

	ln        net.Listener
	listening chan struct{} // closed by Listen
	closed    chan struct{} // closed by Close
}

func (f *fakeNode) Start(context.Context) (tailnet.Self, error) { return f.self, f.startErr }
func (f *fakeNode) Refresh(context.Context) (tailnet.Self, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refresh != nil {
		return *f.refresh, nil
	}
	return f.self, nil
}
func (f *fakeNode) Identity() tailnet.Identity { return f.who }
func (f *fakeNode) Close() error               { close(f.closed); return nil }
func (f *fakeNode) Listen() (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f.ln = ln
	close(f.listening)
	return ln, nil
}

// served is a running serve.
type served struct {
	node     *fakeNode
	url      string // http://127.0.0.1:port
	stateDir string
	stop     func() error // cancels and returns serve's error
}

// startServe runs dispatch with args plus a temp state dir and fake-herdr,
// against node, and waits until it listens. It fails the test if serve
// returns first.
func startServe(t *testing.T, n *fakeNode, args ...string) served {
	t.Helper()
	stateDir, errc, cancel := launch(t, n, args...)
	select {
	case <-n.listening:
	case err := <-errc:
		t.Fatalf("serve returned before listening: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve never listened")
	}
	return served{node: n, url: "http://" + n.ln.Addr().String(), stateDir: stateDir, stop: func() error {
		cancel()
		select {
		case err := <-errc:
			return err
		case <-time.After(10 * time.Second):
			return errors.New("serve did not stop")
		}
	}}
}

// launch starts serve in the background and returns its state dir and
// result.
func launch(t *testing.T, n *fakeNode, args ...string) (string, <-chan error, context.CancelFunc) {
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

	n.listening, n.closed = make(chan struct{}), make(chan struct{})
	orig := newNode
	t.Cleanup(func() { newNode = orig })
	newNode = func(cfg tailnet.Config) node { n.cfg = cfg; return n }

	stateDir := filepath.Join(dir, "state")
	args = append([]string{"--state-dir", stateDir, "--herdr-socket", fake.Addr(), "--claude-config-dir", "../../internal/fakeherdr/testdata/claude"}, args...)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errc := make(chan error, 1)
	go func() { errc <- dispatch(ctx, args, io.Discard) }()
	return stateDir, errc, cancel
}

func ownerNode(expiry *time.Time) *fakeNode {
	return &fakeNode{
		self: tailnet.Self{FQDN: testFQDN, KeyExpiry: expiry, OwnerUserID: testOwnerID},
		who:  &fakeIdentity{peer: owner},
	}
}

func (s served) dial(t *testing.T, peer tailnet.Peer, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	s.node.who.set(peer)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, s.url+"/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {origin}}})
	if err == nil {
		t.Cleanup(func() { c.CloseNow() })
	}
	return c, resp, err
}

func readJSON[T any](t *testing.T, c *websocket.Conn) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var v T
	if err := wsjson.Read(ctx, c, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestServeTailnet(t *testing.T) {
	expiry := time.Now().Add(9*24*time.Hour - time.Minute)
	n := ownerNode(&expiry)
	s := startServe(t, n, "--hostname", "pocket", "--reader", "friend@example.com")
	if n.cfg.StateDir != s.stateDir || n.cfg.Hostname != "pocket" || n.cfg.HerdrProtocol == 0 || len(n.cfg.TranscriptDirs) == 0 {
		t.Errorf("node config %+v", n.cfg)
	}
	// One process owns the node.
	if _, err := tailnet.Lock(s.stateDir); !errors.Is(err, tailnet.ErrLocked) {
		t.Errorf("lock while serving: %v, want ErrLocked", err)
	}

	t.Run("owner is controller with expiry", func(t *testing.T) {
		c, _, err := s.dial(t, owner, testOrigin)
		if err != nil {
			t.Fatal(err)
		}
		snap := readJSON[hub.Snapshot](t, c)
		if snap.Type != "snapshot" || snap.Role != "controller" || len(snap.Agents) == 0 {
			t.Fatalf("snapshot type %q role %q, %d agents", snap.Type, snap.Role, len(snap.Agents))
		}
		if snap.Expiry == nil || snap.Expiry.DaysLeft != 9 || !snap.Expiry.At.Equal(expiry.UTC().Truncate(time.Second)) {
			t.Errorf("expiry %+v, want 9 days at %v", snap.Expiry, expiry)
		}
		if host, _ := os.Hostname(); snap.Host != host {
			t.Errorf("host %q, want this computer's %q", snap.Host, host)
		}
		if snap.Push == nil || snap.Push.VAPIDPublicKey == "" {
			t.Errorf("snapshot push %+v, want the VAPID key", snap.Push)
		}
		// Push keeps its keys in the state dir, private.
		for name, want := range map[string]os.FileMode{"push": 0o700, "push/vapid.json": 0o600} {
			if info, err := os.Stat(filepath.Join(s.stateDir, name)); err != nil || info.Mode().Perm() != want {
				t.Errorf("%s: %v, want mode %o", name, err, want)
			}
		}
	})

	t.Run("reader forbidden and audited", func(t *testing.T) {
		c, _, err := s.dial(t, friend, testOrigin)
		if err != nil {
			t.Fatal(err)
		}
		if snap := readJSON[hub.Snapshot](t, c); snap.Role != "reader" {
			t.Fatalf("role %q", snap.Role)
		}
		secret := "invented secret prompt text"
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := wsjson.Write(ctx, c, map[string]string{"id": "r1", "type": "prompt", "pane_id": "w1:p2", "text": secret}); err != nil {
			t.Fatal(err)
		}
		for {
			var m struct {
				Type  string `json:"type"`
				ID    string `json:"id"`
				OK    bool   `json:"ok"`
				Error *struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := wsjson.Read(ctx, c, &m); err != nil {
				t.Fatal(err)
			}
			if m.Type != "ack" {
				continue
			}
			if m.ID != "r1" || m.OK || m.Error == nil || m.Error.Code != "forbidden" {
				t.Fatalf("ack %+v, want forbidden", m)
			}
			break
		}
		path := filepath.Join(s.stateDir, "audit.jsonl")
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "secret") {
			t.Errorf("audit log holds the payload: %s", b)
		}
		var lines []audit.Entry
		for sc := bufio.NewScanner(strings.NewReader(string(b))); sc.Scan(); {
			var e audit.Entry
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, e)
		}
		if len(lines) != 1 || lines[0].Login != friend.Login || lines[0].Result != "forbidden" || lines[0].Method != "prompt" || lines[0].Size != len(secret) {
			t.Errorf("audit %+v", lines)
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("audit mode %v, %v", info.Mode().Perm(), err)
		}
	})

	t.Run("stranger and tagged peer get 403", func(t *testing.T) {
		for _, p := range []tailnet.Peer{stranger, kiosk} {
			n.who.set(p)
			resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(s.url + "/")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "not allowed") {
				t.Errorf("%s: %d %q, want the 403 page", p.Login, resp.StatusCode, body)
			}
		}
	})

	t.Run("only the node's https origin", func(t *testing.T) {
		for _, origin := range []string{s.url, "https://pocket.evil.example", "https://pocket"} {
			if _, resp, err := s.dial(t, owner, origin); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
				t.Errorf("origin %q: %v, want 403", origin, err)
			}
		}
	})

	if err := s.stop(); err != nil {
		t.Fatalf("serve after cancel: %v, want nil", err)
	}
	select {
	case <-n.closed:
	default:
		t.Error("node not closed")
	}
	unlock, err := tailnet.Lock(s.stateDir)
	if err != nil {
		t.Fatalf("lock after stop: %v", err)
	}
	unlock()
}

// Key expiry off, or more than 14 days away, is an explicit null.
func TestServeTailnetExpiryNull(t *testing.T) {
	far := time.Now().Add(30 * 24 * time.Hour)
	for name, expiry := range map[string]*time.Time{"off": nil, "30 days": &far} {
		t.Run(name, func(t *testing.T) {
			s := startServe(t, ownerNode(expiry))
			c, _, err := s.dial(t, owner, testOrigin)
			if err != nil {
				t.Fatal(err)
			}
			raw := readJSON[map[string]json.RawMessage](t, c)
			if v, ok := raw["expiry"]; !ok || string(v) != "null" {
				t.Errorf("expiry %s (present %v), want null", v, ok)
			}
			if err := s.stop(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// serveRefused runs serve against n and returns its error; serve must stop
// on its own without listening, close the node and release the lock.
func serveRefused(t *testing.T, n *fakeNode, args ...string) error {
	t.Helper()
	stateDir, errc, _ := launch(t, n, args...)
	var err error
	select {
	case err = <-errc:
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not refuse")
	}
	select {
	case <-n.listening:
		t.Error("listened anyway")
	default:
	}
	select {
	case <-n.closed:
	default:
		t.Error("node not closed")
	}
	unlock, lerr := tailnet.Lock(stateDir)
	if lerr != nil {
		t.Fatalf("lock after refusal: %v", lerr)
	}
	unlock()
	return err
}

func TestServeTailnetRefusesFunnel(t *testing.T) {
	n := ownerNode(nil)
	n.startErr = tailnet.ErrFunnel
	if err := serveRefused(t, n); !errors.Is(err, tailnet.ErrFunnel) {
		t.Fatalf("err %v, want ErrFunnel", err)
	}
}

func TestServeTailnetTaggedSelf(t *testing.T) {
	tagged := func() *fakeNode {
		return &fakeNode{self: tailnet.Self{FQDN: testFQDN, Tagged: true}, who: &fakeIdentity{peer: friend}}
	}
	if err := serveRefused(t, tagged()); err == nil || !strings.Contains(err.Error(), "--allow") {
		t.Fatalf("tagged self without --allow: %v", err)
	}
	if err := serveRefused(t, tagged(), "--reader", "friend@example.com"); err == nil || !strings.Contains(err.Error(), "--allow") {
		t.Fatalf("tagged self with readers only: %v", err)
	}
	// With --allow it serves, and the owner-less node lets in only the list.
	s := startServe(t, tagged(), "--allow", "friend@example.com")
	c, _, err := s.dial(t, friend, testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if snap := readJSON[hub.Snapshot](t, c); snap.Role != "controller" {
		t.Errorf("role %q", snap.Role)
	}
	if _, resp, err := s.dial(t, owner, testOrigin); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("unlisted user on a tagged node: %v, want 403", err)
	}
	if err := s.stop(); err != nil {
		t.Fatal(err)
	}
}

// A second instance stops at the lock and never touches the node, so it
// can't overwrite the running server's status.json.
func TestServeTailnetAlreadyRunning(t *testing.T) {
	stateDir := t.TempDir()
	unlock, err := tailnet.Lock(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	orig := newNode
	t.Cleanup(func() { newNode = orig })
	newNode = func(tailnet.Config) node { t.Error("node made while locked"); return failingNode{errors.New("no")} }
	if _, err := run(t, "--state-dir", stateDir); err == nil || err.Error() != "herdr-pocket is already running" {
		t.Fatalf("err %v", err)
	}
}

func TestServeModeFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--dev-role", "reader"},
		{"--dev-expiry", "9d"},
		{"--dev-addr", "127.0.0.1:8391"},
		{"--dev", "--allow", "friend@example.com"},
		{"--dev", "--reader", "friend@example.com"},
	} {
		if _, err := run(t, args...); !errors.Is(err, errUsage) {
			t.Errorf("%v = %v, want a usage error", args, err)
		}
	}
}

// The hourly refresh hands a changed key expiry to the hub, which patches
// open sockets.
func TestServeTailnetRefreshesExpiry(t *testing.T) {
	orig := keyRefreshEvery
	t.Cleanup(func() { keyRefreshEvery = orig })
	keyRefreshEvery = 20 * time.Millisecond

	n := ownerNode(nil)
	s := startServe(t, n)
	c, _, err := s.dial(t, owner, testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if snap := readJSON[hub.Snapshot](t, c); snap.Expiry != nil {
		t.Fatalf("expiry %+v, want null", snap.Expiry)
	}
	soon := time.Now().Add(2*24*time.Hour - time.Minute)
	n.mu.Lock()
	n.refresh = &tailnet.Self{FQDN: testFQDN, KeyExpiry: &soon, OwnerUserID: testOwnerID}
	n.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var p hub.Patch
		if err := wsjson.Read(ctx, c, &p); err != nil {
			t.Fatal(err)
		}
		if p.Expiry != nil {
			if e := p.Expiry.Value; e == nil || e.DaysLeft != 2 {
				t.Fatalf("expiry patch %+v, want 2 days", e)
			}
			break
		}
	}
	if err := s.stop(); err != nil {
		t.Fatal(err)
	}
}
