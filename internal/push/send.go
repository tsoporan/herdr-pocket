package push

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	webpush "github.com/SherClockHolmes/webpush-go"
)

const (
	pushTTL = 3600 // seconds
	// sendTimeout bounds one send, well inside the 10 s a blocked alert
	// has to reach the phone; each device sends on its own (Service.send).
	sendTimeout = 8 * time.Second
	maxEndpoint = 2048
	// maxTitle and maxBody, in runes, keep a payload inside one 4096-byte
	// record: the name and workspace label can be anything herdr reports.
	maxTitle = 120
	maxBody  = 200
)

// The payload kinds.
const (
	kindBlocked = "blocked"
	kindDone    = "done"
	kindClear   = "clear"
	kindExpiry  = "expiry"
	kindTest    = "test"
)

// pushHosts are the push services an endpoint may point at: the host must
// be one of these or a subdomain of one.
var pushHosts = []string{
	"fcm.googleapis.com",
	"updates.push.services.mozilla.com",
	"push.apple.com",
	"notify.windows.com",
}

// message is the push payload. It never carries prompt or output text.
type message struct {
	Kind  string `json:"kind"`
	Tag   string `json:"tag"`
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	// Answer is set only on a blocked alert that may be answered from the
	// notification (Service.addAnswers).
	Answer *answerKeys `json:"answer,omitempty"`
}

// clip cuts v to at most n runes, ending a cut one with an ellipsis.
func clip(v string, n int) string {
	if utf8.RuneCountInString(v) <= n {
		return v
	}
	return string([]rune(v)[:n-1]) + "…"
}

func agentTag(paneID string) string { return "agent-" + paneID }

// agentURL is the app's deep link to an agent, escaped as the app's
// agentHash escapes it (URLSearchParams), so any pane id round-trips.
func agentURL(paneID string) string { return "/#agent=" + url.QueryEscape(paneID) }

func agentTitle(a Agent) string {
	if a.Name != "" {
		return a.Name
	}
	return "Agent"
}

func withWorkspace(words, workspace string) string {
	if workspace == "" {
		return words
	}
	return words + " · " + workspace
}

func alertMessage(a Agent) message {
	m := message{Kind: kindBlocked, Tag: agentTag(a.PaneID), Title: agentTitle(a), URL: agentURL(a.PaneID)}
	if a.Status == statusDone {
		m.Kind, m.Body = kindDone, withWorkspace("Finished", a.Workspace)
	} else {
		// With buttons, Service.addAnswers replaces this.
		m.Body = withWorkspace("Asks you a question", a.Workspace) + ". Tap to answer."
	}
	return m
}

func clearMessage(a Agent) message {
	return message{Kind: kindClear, Tag: agentTag(a.PaneID), Title: agentTitle(a), URL: agentURL(a.PaneID)}
}

func testMessage() message {
	return message{Kind: kindTest, Tag: "test", Title: "Test push", Body: "Notifications work on this phone.", URL: "/"}
}

// expiryMessage follows the app's banner wording (web/src/lib/words.ts).
func expiryMessage(days int) message {
	title := fmt.Sprintf("Tailscale sign-in ends in %d days", days)
	if days <= 1 {
		title = "Tailscale sign-in ends tomorrow"
	}
	return message{Kind: kindExpiry, Tag: "expiry", Title: title,
		Body: "After that this app can't reach your computer. Turn off key expiry for herdr-pocket, or sign it in again.",
		URL:  "/"}
}

func urgency(kind string) webpush.Urgency {
	switch kind {
	case kindBlocked:
		return webpush.UrgencyHigh
	case kindClear:
		return webpush.UrgencyLow
	}
	return webpush.UrgencyNormal
}

// topic is the Topic header for a tag: base64url of a SHA-256 prefix, 32
// characters, so the push service collapses queued alerts for one agent.
func topic(tag string) string {
	sum := sha256.Sum256([]byte(tag))
	return base64.RawURLEncoding.EncodeToString(sum[:24])
}

// deliver sends one push. A 404 or 410 deletes the subscription and returns
// ErrNotFound. Logs name the endpoint's host only: the path is a capability.
func (s *Service) deliver(ctx context.Context, j job) error {
	host := endpointHost(j.endpoint)
	msg := j.msg
	msg.Title, msg.Body = clip(msg.Title, maxTitle), clip(msg.Body, maxBody)
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	resp, err := webpush.SendNotificationWithContext(ctx, body,
		&webpush.Subscription{Endpoint: j.endpoint, Keys: webpush.Keys{P256dh: j.keys.P256dh, Auth: j.keys.Auth}},
		&webpush.Options{
			HTTPClient:      s.client,
			Subscriber:      s.cfg.Origin,
			Topic:           topic(j.msg.Tag),
			TTL:             pushTTL,
			Urgency:         urgency(j.msg.Kind),
			VAPIDPublicKey:  s.pub,
			VAPIDPrivateKey: s.priv,
		})
	if err != nil {
		// A *url.Error's text holds the full endpoint; keep only the cause.
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		s.cfg.Logf("push: %s to %s failed: %v", j.msg.Kind, host, err)
		return fmt.Errorf("%w: %v", ErrPushFailed, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		// One line per push, with the kind and tag but never the title or body.
		s.cfg.Logf("push: sent %s %s to %s", j.msg.Kind, j.msg.Tag, host)
		return nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		s.cfg.Logf("push: %s says the subscription is gone (%d); removing it", host, resp.StatusCode)
		s.mu.Lock()
		err := s.removeLocked(j.subID)
		s.mu.Unlock()
		if err != nil {
			s.cfg.Logf("push: saving %s: %v", subsFile, err)
		}
		return fmt.Errorf("%w: the push service no longer has this subscription", ErrNotFound)
	default:
		s.cfg.Logf("push: %s to %s refused: %d", j.msg.Kind, host, resp.StatusCode)
		return fmt.Errorf("%w: status %d", ErrPushFailed, resp.StatusCode)
	}
}

// endpointHost is the endpoint's host, the only part of it ever logged.
func endpointHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "(bad endpoint)"
	}
	return u.Host
}

// checkEndpoint allows only https on port 443 to a known push-service host.
func checkEndpoint(raw string) error {
	bad := func(why string) error { return fmt.Errorf("%w: %s", ErrBadEndpoint, why) }
	if raw == "" || len(raw) > maxEndpoint {
		return bad("missing or too long")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return bad("not a URL")
	}
	if u.Scheme != "https" {
		return bad("not https")
	}
	if u.User != nil || u.Opaque != "" || u.Host == "" {
		return bad("not a plain https URL")
	}
	if p := u.Port(); p != "" && p != "443" {
		return bad("not port 443")
	}
	host := strings.ToLower(u.Hostname())
	if _, err := netip.ParseAddr(host); err == nil {
		return bad("an IP address, not a push service")
	}
	for _, h := range pushHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return nil
		}
	}
	return bad("not a known push service")
}

// notPublic are global-unicast ranges that still aren't the public
// internet: CGNAT (Tailscale's tailnet addresses), special-use and
// documentation blocks, and IPv6 ranges that embed an IPv4 address.
var notPublic = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fec0::/10"),
}

// dialable reports whether a resolved address is public unicast.
func dialable(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}
	for _, p := range notPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// refuseLocal is a net.Dialer Control hook: it sees the resolved address,
// so a push-service name that resolves somewhere local is still refused.
func refuseLocal(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("push: refusing to dial %q: %v", address, err)
	}
	if !dialable(ap.Addr()) {
		return fmt.Errorf("push: refusing to dial non-public address %s", ap.Addr())
	}
	return nil
}

// safeClient is the default sender: no proxy, no redirects, public
// addresses only, and bounded time.
func safeClient() *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second, Control: refuseLocal}
	return &http.Client{
		Timeout: sendTimeout,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         d.DialContext,
			ForceAttemptHTTP2:   true,
			TLSHandshakeTimeout: 5 * time.Second,
			IdleConnTimeout:     90 * time.Second,
			MaxIdleConns:        8,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
