// Package server serves the embedded PWA and the app WebSocket.
//
// Every request is authenticated through an Identity (WhoIs on the tailnet,
// a fake one in --dev) and given a role, or refused (auth.go). The WebSocket
// also checks its Origin. The app's messages and their acks are in ws.go;
// every mutation is audited there.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/tsoporan/herdr-pocket/internal/audit"
	"github.com/tsoporan/herdr-pocket/internal/herdr"
	"github.com/tsoporan/herdr-pocket/internal/hub"
	"github.com/tsoporan/herdr-pocket/internal/push"
	"github.com/tsoporan/herdr-pocket/internal/queue"
	"github.com/tsoporan/herdr-pocket/internal/start"
	"github.com/tsoporan/herdr-pocket/internal/tailnet"
)

// CSP is the policy on every response; nothing third-party loads at runtime.
// frame-ancestors does not fall back to default-src: without it a page on
// the phone could frame the app, which WhoIs authenticates as the owner, and
// trick taps on it (clickjacking). X-Frame-Options says the same to older
// browsers.
const CSP = "default-src 'self'; frame-ancestors 'none'"

// Config is what the handler needs besides the hub and the web files.
type Config struct {
	// Origins are the exact Origin values the WebSocket and /api/ accept:
	// "https://<fqdn>" on the tailnet, the loopback listener's in --dev.
	Origins []string
	// Input sends app input to herdr. Nil is read-only: every mutation is
	// refused with read_only, and nothing can reach herdr's input methods.
	Input *herdr.Client
	// Identity says who sent each request. Required.
	Identity tailnet.Identity
	// Owner is the user ID that owns this node; it is a controller. Unused
	// when SelfTagged, since a tagged node has no owner.
	Owner      int64
	SelfTagged bool
	// Allow and Readers name the other controllers and readers: logins
	// ("someone@example.com") or tags ("tag:phone").
	Allow   []string
	Readers []string
	// Audit records every mutation. Required.
	Audit *audit.Log
	// Push serves the push.* messages and the snapshot's VAPID key. Nil
	// turns push off: the snapshot's push is null and push.* is refused.
	Push *push.Service
	// Queue serves queue.add and queue.remove. Nil turns the queue off:
	// they are refused. The hub carries its items (hub.SetQueue).
	Queue *queue.Queue
	// Starts serves start. Nil turns starting agents off: it is refused.
	// The hub carries its jobs (hub.SetStarts).
	Starts *start.Runner
}

// Server is the app's handler.
type Server struct {
	handler  http.Handler
	requests *requests
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// Shutdown waits, until ctx ends, for the app requests already running to
// finish and be audited, and drops any that arrive after it is called. Call
// it after http.Server.Shutdown, which doesn't wait for WebSockets, and
// before closing the audit log. Idle sockets don't delay it.
func (s *Server) Shutdown(ctx context.Context) error { return s.requests.close(ctx) }

// New returns the handler: GET /ws is the WebSocket, POST /api/answer is a
// notification's answer, the rest of /api/ is reserved for read-only JSON
// endpoints (none yet), and everything else is the PWA from
// static, with index.html for unknown extensionless paths. It fails when the
// access rules leave no one in charge: a tagged node without --allow.
func New(h *hub.Hub, static fs.FS, cfg Config) (*Server, error) {
	a, err := newAccess(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.Identity == nil || cfg.Audit == nil {
		return nil, errors.New("server: an Identity and an audit log are required")
	}
	reqs := newRequests()
	mux := http.NewServeMux()
	ws := &wsHandler{build: bundleID(static), owner: cfg.Owner, hub: h, origins: cfg.Origins, input: cfg.Input, audit: cfg.Audit, push: cfg.Push, queue: cfg.Queue, starts: cfg.Starts, denyWait: denyWait, requests: reqs}
	mux.Handle("GET /ws", ws)
	// The notification's answer checks its own, stricter origin (api.go).
	mux.HandleFunc("POST /api/answer", ws.answerHTTP)
	mux.Handle("GET /api/", sameSite(cfg.Origins, http.NotFoundHandler()))
	mux.Handle("GET /", spa(static))
	auth := authenticate(cfg.Identity, a, mux)
	return &Server{
		handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Security-Policy", CSP)
			w.Header().Set("X-Frame-Options", "DENY")
			auth.ServeHTTP(w, r)
		}),
		requests: reqs,
	}, nil
}

// bundleID is a short hash of the embedded index.html. Vite names every
// asset by its content hash, so a new bundle always has a new index.html.
func bundleID(static fs.FS) string {
	b, err := fs.ReadFile(static, "index.html")
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:6])
}

func spa(static fs.FS) http.Handler {
	files := http.FileServerFS(static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if info, err := fs.Stat(static, name); err == nil && !info.IsDir() {
			// Go's mime table may not know .webmanifest (it reads the system's).
			if path.Ext(name) == ".webmanifest" {
				w.Header().Set("Content-Type", "application/manifest+json")
			}
			files.ServeHTTP(w, r)
			return
		}
		if name != "" && name != "." && path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		if _, err := fs.Stat(static, "index.html"); err != nil {
			http.Error(w, "the web app is not built; run just build", http.StatusNotFound)
			return
		}
		http.ServeFileFS(w, r, static, "index.html")
	})
}
