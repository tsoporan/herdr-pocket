package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"

	"github.com/tsoporan/herdr-pocket/internal/tailnet"
)

// Role is what a viewer may do.
type Role string

const (
	Controller Role = "controller" // may send input
	Reader     Role = "reader"     // may only view
)

// access decides who may use the app.
type access struct {
	owner      int64 // 0 when this node is tagged and so has no owner
	controller []string
	reader     []string
}

func newAccess(cfg Config) (access, error) {
	for _, e := range slices.Concat(cfg.Allow, cfg.Readers) {
		if e == "" || e == "tag:" {
			return access{}, fmt.Errorf("an empty --allow or --reader entry")
		}
	}
	switch {
	case cfg.SelfTagged && len(cfg.Allow) == 0:
		return access{}, errors.New("this node is tagged, so it has no owner; name who may control it with --allow")
	case cfg.SelfTagged:
		return access{controller: cfg.Allow, reader: cfg.Readers}, nil
	case cfg.Owner == 0:
		return access{}, errors.New("no owner user ID for this node")
	}
	return access{owner: cfg.Owner, controller: cfg.Allow, reader: cfg.Readers}, nil
}

// role is p's role, or false when p is refused. The order is fixed:
//  1. A tagged peer is allowed only by one of its tags. WhoIs reports a
//     tagged node as the shared tagged-devices user, so it never reaches
//     the user checks.
//  2. The node's owner, by user ID, is a controller.
//  3. Otherwise the login decides: --allow for controllers, --reader for
//     readers.
func (a access) role(p tailnet.Peer) (Role, bool) {
	if len(p.Tags) > 0 {
		return a.listed(p.Tags)
	}
	if a.owner != 0 && p.UserID == a.owner {
		return Controller, true
	}
	if p.Login == "" || isTag(p.Login) {
		return "", false
	}
	return a.listed([]string{p.Login})
}

// listed finds any of names (a peer's tags, or its login) in the entries,
// controllers first. Tags compare exactly, logins ignoring ASCII case only
// (Unicode folding would let "K" (Kelvin) or "ſ" match "k" or "s"); a login
// never starts with "tag:", so it can't match a tag entry.
func (a access) listed(names []string) (Role, bool) {
	match := func(entries []string) bool {
		return slices.ContainsFunc(entries, func(e string) bool {
			return slices.ContainsFunc(names, func(n string) bool {
				return n == e || (!isTag(e) && asciiEqualFold(n, e))
			})
		})
	}
	switch {
	case match(a.controller):
		return Controller, true
	case match(a.reader):
		return Reader, true
	}
	return "", false
}

func isTag(s string) bool { return strings.HasPrefix(s, "tag:") }

// viewer is the authenticated peer behind a request.
type viewer struct {
	peer tailnet.Peer
	role Role
}

type viewerKey struct{}

func viewerFrom(ctx context.Context) (viewer, bool) {
	v, ok := ctx.Value(viewerKey{}).(viewer)
	return v, ok
}

// forbiddenPage is all a stranger gets.
const forbiddenPage = "herdr-pocket: this account or device is not allowed here.\n"

// authenticate runs WhoIs on every request, the app shell and assets
// included, and refuses anyone the access rules don't name. A failed WhoIs
// is refused too.
func authenticate(id tailnet.Identity, a access, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := id.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil {
			log.Printf("server: whois %s: %v", r.RemoteAddr, err)
			refuse(w)
			return
		}
		role, ok := a.role(p)
		if !ok {
			log.Printf("server: refused %q on %q", p.Login, p.Node)
			refuse(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), viewerKey{}, viewer{peer: p, role: role})))
	})
}

func refuse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(forbiddenPage))
}

// sameSite guards the JSON data endpoints under /api/: a request must be
// same-origin or user-initiated by its Sec-Fetch-Site, or carry one of the
// app's origins. The app shell is exempt, since opening it from a link is a
// cross-site navigation.
func sameSite(origins []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Sec-Fetch-Site") {
		case "same-origin", "none":
		default:
			if o := r.Header.Get("Origin"); o == "" || !slices.Contains(origins, o) {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// asciiEqualFold reports whether a and b are equal ignoring the case of
// ASCII letters; any other byte must match exactly.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
