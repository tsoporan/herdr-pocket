package tailnet

import (
	"context"
	"errors"
	"strings"
	"time"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
)

// Peer is who sent a request, from WhoIs.
type Peer struct {
	// Login is the user's UserProfile.LoginName. A tagged node reports the
	// shared "tagged-devices" pseudo-user, so check Tags first.
	Login string
	// UserID is the node's owning user, comparable with Self.OwnerUserID.
	UserID int64
	// Node is the peer machine's name, for the audit log.
	Node string
	// Tags are the peer node's ACL tags ("tag:phone"); empty for a user's node.
	Tags []string
}

// Identity says who is behind a request's remote address.
type Identity interface {
	WhoIs(ctx context.Context, remoteAddr string) (Peer, error)
}

// whoIser is the part of local.Client that Identity needs.
type whoIser interface {
	WhoIs(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error)
}

type localIdentity struct{ lc whoIser }

func (l localIdentity) WhoIs(ctx context.Context, remoteAddr string) (Peer, error) {
	r, err := l.lc.WhoIs(ctx, remoteAddr)
	if err != nil {
		return Peer{}, err
	}
	return peerFrom(r)
}

// peerFrom maps a WhoIs response to a Peer. It fails closed on a response
// without a node or user.
func peerFrom(r *apitype.WhoIsResponse) (Peer, error) {
	if r == nil || r.Node == nil || r.UserProfile == nil {
		return Peer{}, errors.New("whois: incomplete response")
	}
	name := r.Node.ComputedName
	if name == "" {
		name, _, _ = strings.Cut(r.Node.Name, ".")
	}
	return Peer{
		Login:  r.UserProfile.LoginName,
		UserID: int64(r.Node.User),
		Node:   name,
		Tags:   append([]string(nil), r.Node.Tags...),
	}, nil
}

// Self is this node as the tailnet sees it.
type Self struct {
	// FQDN is Self.DNSName without its trailing dot. The app's origin is
	// "https://" + FQDN; Tailscale may rename the node on a collision, so
	// this, never --hostname, is the name to trust.
	FQDN string
	// KeyExpiry is nil when key expiry is off for the node.
	KeyExpiry *time.Time
	// OwnerUserID is the user who owns the node. A tagged node has none.
	OwnerUserID int64
	// Tagged nodes have no owner (Self.UserID is the tagged-devices user).
	Tagged bool
	// HTTPS is whether the tailnet issues HTTPS certificates (the admin
	// console's DNS page). Without them the phone can't open the app.
	HTTPS bool
}

// HTTPSHint is what to do when Self.HTTPS is false.
const HTTPSHint = "HTTPS certificates are off for this tailnet, so the phone can't open the app. Turn on HTTPS in the Tailscale admin console (DNS page): https://login.tailscale.com/admin/dns"

// selfFrom reads Self from a node status.
func selfFrom(st *ipnstate.Status) (Self, error) {
	if st == nil || st.Self == nil {
		return Self{}, errors.New("tailnet: status has no self node")
	}
	fqdn := TrimDNSName(st.Self.DNSName)
	if fqdn == "" {
		return Self{}, errors.New("tailnet: self node has no DNS name; is MagicDNS on?")
	}
	tagged := st.Self.Tags != nil && st.Self.Tags.Len() > 0
	s := Self{FQDN: fqdn, KeyExpiry: st.Self.KeyExpiry, Tagged: tagged, HTTPS: len(st.CertDomains) > 0}
	if !tagged {
		s.OwnerUserID = int64(st.Self.UserID)
	}
	return s, nil
}

// TrimDNSName drops the trailing dot from a MagicDNS name
// ("host.tailnet.ts.net." → "host.tailnet.ts.net").
func TrimDNSName(name string) string { return strings.TrimSuffix(name, ".") }

// ErrFunnel means Funnel is enabled for the node; herdr-pocket refuses to
// serve the public internet.
var ErrFunnel = errors.New("tailnet: Funnel is on for this node, and herdr-pocket serves the tailnet only; turn Funnel off for it and start again")

// CheckFunnel refuses a serve config that has any Funnel enabled.
func CheckFunnel(sc *ipn.ServeConfig) error {
	if sc.IsFunnelOn() {
		return ErrFunnel
	}
	return nil
}
