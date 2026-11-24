package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/audit"
	"github.com/tsoporan/herdr-pocket/internal/herdr"
	"github.com/tsoporan/herdr-pocket/internal/hub"
	"github.com/tsoporan/herdr-pocket/internal/push"
	"github.com/tsoporan/herdr-pocket/internal/server"
	"github.com/tsoporan/herdr-pocket/internal/tailnet"
	"github.com/tsoporan/herdr-pocket/internal/transcript"
	"github.com/tsoporan/herdr-pocket/web"
)

// tailnetOptions are serve's flags for the tailnet.
type tailnetOptions struct {
	stateDir   string
	hostname   string // the node name asked for
	socket     string // --herdr-socket
	claudeDirs []string
	readOnly   bool
	// allow and readers are --allow and --reader: logins or tags.
	allow, readers []string
	// tailscaleLogs is --tailscale-logs.
	tailscaleLogs bool
	kinds         []hub.StartKind // --kinds
}

// node is the part of *tailnet.Node that serve uses.
type node interface {
	Start(ctx context.Context) (tailnet.Self, error)
	Refresh(ctx context.Context) (tailnet.Self, error)
	Identity() tailnet.Identity
	Listen() (net.Listener, error)
	Close() error
}

// newNode makes the tailnet node. Tests replace it, so they never start one.
var newNode = func(cfg tailnet.Config) node { return tailnet.NewNode(cfg) }

// keyRefreshEvery is how often serve rereads the node's key expiry, which
// changes when the owner signs in again or turns expiry off. Tests shorten it.
var keyRefreshEvery = time.Hour

// serveTailnet runs the server on herdr-pocket's own tailnet node, over
// HTTPS on :443 only, until ctx ends. One process owns the node: it takes
// the state dir's lock before anything else.
func serveTailnet(ctx context.Context, o tailnetOptions) error {
	unlock, err := tailnet.Lock(o.stateDir)
	if err != nil {
		return err // ErrLocked reads "herdr-pocket is already running"
	}
	defer unlock()
	err = runTailnet(ctx, o)
	if ctx.Err() != nil && errors.Is(err, context.Canceled) {
		return nil // stopped by a signal, possibly while waiting for sign-in
	}
	return err
}

func runTailnet(ctx context.Context, o tailnetOptions) error {
	// herdr first: a missing herdr fails before the node starts, and its
	// protocol goes into status.json.
	socket, err := herdr.ResolveSocket(o.socket)
	if err != nil {
		return err
	}
	client := herdr.NewClient(socket)
	pong, err := client.CheckProtocol(ctx)
	if err != nil {
		return fmt.Errorf("herdr at %s: %w", socket, err)
	}
	// The snapshot's host is this computer's name, not the node's.
	host, err := os.Hostname()
	if err != nil {
		return err
	}

	n := newNode(tailnet.Config{
		StateDir:       o.stateDir,
		Hostname:       o.hostname,
		TailscaleLogs:  o.tailscaleLogs,
		HerdrProtocol:  pong.Protocol,
		TranscriptDirs: o.claudeDirs,
		Out:            os.Stderr,
	})
	defer func() {
		if err := n.Close(); err != nil {
			log.Printf("tailnet: close: %v", err)
		}
	}()
	self, err := n.Start(ctx) // refuses when Funnel is on
	if err != nil {
		return err
	}
	if !self.HTTPS {
		log.Printf("tailnet: %s", tailnet.HTTPSHint)
	}
	auditLog, err := audit.Open(o.stateDir)
	if err != nil {
		return err
	}
	defer auditLog.Close()

	origin := appURL(self.FQDN)
	cfg := server.Config{
		Origins:    []string{origin},
		Input:      client,
		Identity:   n.Identity(),
		Owner:      self.OwnerUserID,
		SelfTagged: self.Tagged,
		Allow:      o.allow,
		Readers:    o.readers,
		Audit:      auditLog,
	}
	if o.readOnly {
		cfg.Input = nil
	}
	canAct, err := server.CanAct(cfg) // refuses a tagged node without --allow
	if err != nil {
		return err
	}
	h := hub.New(client, host, transcript.NewReader(o.claudeDirs...))
	pusher, err := push.Open(o.stateDir, push.Config{Origin: origin, Logf: log.Printf, Question: h.Question, CanAct: canAct})
	if err != nil {
		return err
	}
	cfg.Push = pusher

	h.SetKeyExpiry(self.KeyExpiry)
	h.SetPusher(pusher)
	q, err := openQueue(o.stateDir, h, client, auditLog, o.readOnly, canAct)
	if err != nil {
		return err
	}
	cfg.Queue = q
	if !o.readOnly {
		cfg.Starts = openStarts(h, client, q, auditLog, o.kinds)
	}
	if err := h.Start(ctx); err != nil {
		return fmt.Errorf("herdr at %s: %w", socket, err)
	}
	handler, err := server.New(h, web.Dist, cfg)
	if err != nil {
		return err
	}
	ln, err := n.Listen()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait() // after cancel, before the node closes
	defer cancel()
	wg.Go(func() { refreshKeyExpiry(ctx, n, h, self.FQDN) })
	wg.Go(func() { pusher.Run(ctx) })
	if !o.readOnly {
		wg.Go(func() { q.Run(ctx) })
		wg.Go(func() { cfg.Starts.Run(ctx) })
	}

	mode := ""
	if o.readOnly {
		mode = ", read-only"
	}
	log.Printf("herdr-pocket on %s (herdr %s, protocol %d%s; transcripts from %s; audit in %s)", origin, pong.Version, pong.Protocol, mode, strings.Join(o.claudeDirs, ", "), o.stateDir)
	return serveHTTP(ctx, ln, handler)
}

// refreshKeyExpiry rereads the node's status every keyRefreshEvery and
// hands the key expiry to the hub; the hub patches the app if it changed.
func refreshKeyExpiry(ctx context.Context, n node, h *hub.Hub, fqdn string) {
	t := time.NewTicker(keyRefreshEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		self, err := n.Refresh(ctx)
		if err != nil {
			log.Printf("tailnet: refresh: %v", err)
			continue
		}
		h.SetKeyExpiry(self.KeyExpiry)
		if self.FQDN != fqdn {
			log.Printf("tailnet: the node is now %s, but the app accepts only %s; restart herdr-pocket", self.FQDN, fqdn)
		}
	}
}

// serveHTTP serves handler on ln until ctx ends, then shuts down and waits
// (up to 5 s in all) for open HTTP requests and running app requests to
// finish, so every app request is audited before the caller closes the log.
func serveHTTP(ctx context.Context, ln net.Listener, handler *server.Server) error {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		// http.Server.Shutdown doesn't wait for WebSockets.
		if err := handler.Shutdown(shutdown); err != nil {
			log.Printf("server: shutdown: app requests still running: %v", err)
		}
	})
	err := srv.Serve(ln)
	if !stop() { // the shutdown started; let it finish
		<-done
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
