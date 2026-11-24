package tailnet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"tailscale.com/client/local"
	"tailscale.com/envknob"
	"tailscale.com/ipn"
	"tailscale.com/tsnet"
)

// Config is a Node's setup. StateDir must already be locked (see Lock).
type Config struct {
	StateDir string
	// Hostname is the name asked for; Tailscale may give another (Self.FQDN).
	Hostname string
	// HerdrProtocol and TranscriptDirs are only reported in status.json.
	HerdrProtocol  int
	TranscriptDirs []string
	// Out gets the sign-in link; nil is os.Stderr.
	Out io.Writer
	// TailscaleLogs uploads tsnet's own debug logs to log.tailscale.com, as
	// a normal Tailscale client does. Off by default: nothing leaves the
	// node for Tailscale's log service unless the owner asks.
	TailscaleLogs bool
}

// Node is herdr-pocket's own tailnet node: a tsnet.Server whose state lives
// in <state>/tsnet. herdr-pocket sets no auth key: the owner signs it in with
// the link Start prints, so the node is owned by that user and untagged.
// tsnet still reads TS_AUTHKEY (or TS_AUTH_KEY) from the environment when
// the node needs login, and signs in with that key instead; a tagged key's
// node is then refused at serve time unless --allow names someone.
type Node struct {
	cfg Config
	srv *tsnet.Server

	mu      sync.Mutex
	lc      *local.Client // set by Start or Logout
	status  StatusFile
	printed string // the last sign-in link printed
	started bool   // Start passed the Funnel check
}

// NewNode returns a node that has not started; nothing touches the network
// or the state dir until Start or Logout.
func NewNode(cfg Config) *Node {
	if cfg.Out == nil {
		cfg.Out = os.Stderr
	}
	if !cfg.TailscaleLogs {
		// Process-wide: logpolicy then gives logtail a no-op transport, so
		// no log upload happens (tailscale.com v1.104.7 logpolicy.go:895).
		envknob.SetNoLogsNoSupport()
	}
	return &Node{
		cfg: cfg,
		srv: &tsnet.Server{
			Hostname: cfg.Hostname,
			Dir:      TsnetDir(cfg.StateDir),
			// tsnet re-prints the link every few seconds with wording about
			// TS_AUTHKEY; Start prints it once instead. Backend logs (Logf)
			// stay discarded.
			UserLogf: func(string, ...any) {},
		},
		status: StatusFile{
			HerdrProtocol:  cfg.HerdrProtocol,
			TranscriptDirs: cfg.TranscriptDirs,
			PID:            os.Getpid(),
		},
	}
}

// client starts the tsnet server if needed and returns its LocalClient.
func (n *Node) client() (*local.Client, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.lc != nil {
		return n.lc, nil
	}
	if err := os.MkdirAll(TsnetDir(n.cfg.StateDir), 0o700); err != nil {
		return nil, err
	}
	lc, err := n.srv.LocalClient() // starts the server
	if err != nil {
		return nil, err
	}
	n.lc = lc
	return lc, nil
}

// Start brings the node up and returns once it is running on the tailnet.
// While it needs login it prints the sign-in link once (again only if the
// link changes) and records needs_login in status.json. It refuses to run
// when Funnel is on for the node. Cancel ctx to give up.
func (n *Node) Start(ctx context.Context) (Self, error) {
	n.writeStatus(func(s *StatusFile) { s.State = StateStarting })
	lc, err := n.client()
	if err != nil {
		return Self{}, err
	}
	if _, err := n.waitRunning(ctx, lc); err != nil {
		return Self{}, err
	}
	// Check before Up: on its first run Up clears the serve config, so a
	// check after it would always pass.
	sc, err := lc.GetServeConfig(ctx)
	if err != nil {
		return Self{}, err
	}
	if err := CheckFunnel(sc); err != nil {
		return Self{}, err
	}
	if _, err := n.srv.Up(ctx); err != nil {
		return Self{}, err
	}
	n.mu.Lock()
	n.started = true
	n.mu.Unlock()
	return n.Refresh(ctx)
}

// waitRunning follows the IPN bus until the node runs, printing the sign-in
// link while it needs login. signedIn reports whether it had to sign in.
func (n *Node) waitRunning(ctx context.Context, lc *local.Client) (signedIn bool, err error) {
	w, err := lc.WatchIPNBus(ctx, ipn.NotifyInitialState)
	if err != nil {
		return false, err
	}
	defer w.Close()
	for {
		note, err := w.Next()
		if err != nil {
			if ctx.Err() != nil {
				return signedIn, ctx.Err()
			}
			return signedIn, err
		}
		if note.ErrMessage != nil {
			return signedIn, fmt.Errorf("tailnet: %s", *note.ErrMessage)
		}
		if u := note.BrowseToURL; u != nil && *u != "" {
			signedIn = true
			n.signIn(*u, "Open this link to add herdr-pocket to your tailnet")
		}
		if st := note.State; st != nil {
			switch *st {
			case ipn.Running:
				return signedIn, nil
			case ipn.NeedsLogin:
				signedIn = true
				n.writeStatus(func(s *StatusFile) { s.State = StateNeedsLogin })
			}
		}
	}
}

// Login is `herdr-pocket login` in the foreground; it never serves. A node
// that needs login prints the link and returns once it runs. A node already
// signed in is signed in again, like `tailscale up --force-reauth`: it
// prints a fresh link and returns once the owner has approved it and the
// node has its new key, whose expiry starts over. The machine stays the
// same, so the node keeps its name.
func (n *Node) Login(ctx context.Context) (Self, error) {
	n.writeStatus(func(s *StatusFile) { s.State = StateStarting })
	lc, err := n.client()
	if err != nil {
		return Self{}, err
	}
	signedIn, err := n.waitRunning(ctx, lc)
	if err != nil {
		return Self{}, err
	}
	if !signedIn {
		if err := n.reauth(ctx, lc); err != nil {
			return Self{}, err
		}
	}
	return n.Refresh(ctx)
}

// reauth forces a new interactive login on a running node and waits for
// both a running state and a new node key, as `tailscale up --force-reauth`
// does (cmd/tailscale/cli/up.go): the two arrive in separate notifications.
func (n *Node) reauth(ctx context.Context, lc *local.Client) error {
	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		return err
	}
	if st.Self == nil {
		return errors.New("tailnet: status has no self node")
	}
	oldKey := st.Self.PublicKey
	// Watch before asking, or the link's notification can be missed.
	w, err := lc.WatchIPNBus(ctx, ipn.NotifyInitialState)
	if err != nil {
		return err
	}
	defer w.Close()
	if err := lc.StartLoginInteractive(ctx); err != nil {
		return err
	}
	running, newKey := true, false
	for {
		note, err := w.Next()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if note.ErrMessage != nil {
			return fmt.Errorf("tailnet: %s", *note.ErrMessage)
		}
		if u := note.BrowseToURL; u != nil && *u != "" {
			n.signIn(*u, "Open this link to sign herdr-pocket in again")
		}
		if s := note.State; s != nil {
			running = *s == ipn.Running
		}
		if self := note.SelfChange; self != nil && self.Key != oldKey {
			newKey = true
		}
		if running && newKey {
			return nil
		}
	}
}

func (n *Node) signIn(url, prompt string) {
	n.mu.Lock()
	again := url == n.printed
	n.printed = url
	n.mu.Unlock()
	n.writeStatus(func(s *StatusFile) { s.State, s.AuthURL = StateNeedsLogin, url })
	if !again {
		fmt.Fprintf(n.cfg.Out, "%s: %s\n", prompt, url)
	}
}

// Listen is the node's HTTPS listener on :443, the only one it opens. It
// fails until Start has succeeded: ListenTLS would bring the node up itself
// and clear the serve config, skipping the Funnel check.
func (n *Node) Listen() (net.Listener, error) {
	n.mu.Lock()
	started := n.started
	n.mu.Unlock()
	if !started {
		return nil, errors.New("tailnet: Listen before Start")
	}
	return n.srv.ListenTLS("tcp", ":443")
}

// Refresh reads the node's status, updates status.json (its state, FQDN and
// key expiry) and returns Self. Call it after Start; the key expiry can
// change when the owner signs in again or turns expiry off, and a node whose
// key has lapsed needs login again.
func (n *Node) Refresh(ctx context.Context) (Self, error) {
	lc, err := n.client()
	if err != nil {
		return Self{}, err
	}
	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		return Self{}, err
	}
	state, authURL := backendState(st.BackendState, st.AuthURL)
	self, err := selfFrom(st)
	n.writeStatus(func(s *StatusFile) {
		s.State, s.AuthURL = state, authURL
		if err == nil {
			s.FQDN, s.KeyExpiry = self.FQDN, self.KeyExpiry
			s.HTTPSOff = state == StateRunning && !self.HTTPS
		}
	})
	return self, err
}

// backendState maps ipnstate.Status.BackendState to a status.json state.
// The sign-in link is kept only while the node needs login.
func backendState(backend, authURL string) (state, url string) {
	switch backend {
	case ipn.Running.String():
		return StateRunning, ""
	case ipn.NeedsLogin.String():
		return StateNeedsLogin, authURL
	}
	return StateStarting, "" // Starting, NoState, and the rarer ones
}

// Identity answers WhoIs for requests that reached the node. Call it after
// Start.
func (n *Node) Identity() Identity {
	n.mu.Lock()
	defer n.mu.Unlock()
	return localIdentity{lc: n.lc}
}

// Logout signs the node out of the tailnet. The caller removes TsnetDir
// afterwards.
func (n *Node) Logout(ctx context.Context) error {
	lc, err := n.client()
	if err != nil {
		return err
	}
	return lc.Logout(ctx)
}

// Close stops the node and records stopped in status.json.
func (n *Node) Close() error {
	n.mu.Lock()
	started := n.lc != nil
	n.mu.Unlock()
	n.writeStatus(func(s *StatusFile) { s.State, s.AuthURL = StateStopped, "" })
	if !started {
		return nil
	}
	return n.srv.Close()
}

// writeStatus applies change to the node's status and rewrites status.json.
// A failed write is reported, not fatal: the server keeps serving.
func (n *Node) writeStatus(change func(*StatusFile)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	change(&n.status)
	if err := WriteStatus(n.cfg.StateDir, n.status); err != nil { // UpdatedAt stays zero here, so each write stamps now
		fmt.Fprintf(n.cfg.Out, "herdr-pocket: writing status: %v\n", err)
	}
}
