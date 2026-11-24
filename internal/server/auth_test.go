package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/tsoporan/herdr-pocket/internal/audit"
	"github.com/tsoporan/herdr-pocket/internal/hub"
	"github.com/tsoporan/herdr-pocket/internal/server"
	"github.com/tsoporan/herdr-pocket/internal/tailnet"
)

// M4 auth, roles and audit, through a fake Identity.

// get fetches path as peer and returns the status and body.
func get(t *testing.T, e env, peer tailnet.Peer, path string, header http.Header) (int, string) {
	t.Helper()
	e.who.set(peer, nil)
	req, _ := http.NewRequest("GET", e.url+path, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// dialAs opens the WebSocket as peer and returns its snapshot.
func dialAs(t *testing.T, e env, peer tailnet.Peer) (*websocket.Conn, hub.Snapshot) {
	t.Helper()
	e.who.set(peer, nil)
	c := dial(t, e)
	return c, read[hub.Snapshot](t, c)
}

// refusedEverywhere checks that peer gets the plain 403 for the app shell,
// an asset and the WebSocket.
func refusedEverywhere(t *testing.T, e env, peer tailnet.Peer) {
	t.Helper()
	for _, path := range []string{"/", "/assets/app.js", "/ws"} {
		// No upgrade headers on /ws: an admitted peer would get a
		// non-403 from the handshake rather than a hijacked connection.
		code, body := get(t, e, peer, path, http.Header{"Origin": {e.origin}})
		if code != http.StatusForbidden || !strings.Contains(body, "not allowed") {
			t.Errorf("%s as %q: %d %q, want the 403 page", path, peer.Login, code, body)
		}
	}
}

func TestOwnerIsController(t *testing.T) {
	e := start(t)
	if code, _ := get(t, e, owner, "/", nil); code != http.StatusOK {
		t.Fatalf("GET / as owner: %d", code)
	}
	_, snap := dialAs(t, e, owner)
	if snap.Role != "controller" {
		t.Errorf("role %q, want controller", snap.Role)
	}
	// The app reloads when this changes, so it must identify the bundle.
	if len(snap.Build) != 12 {
		t.Errorf("build %q, want a 12-hex bundle id", snap.Build)
	}
}

func TestStrangerRefused(t *testing.T) {
	e := start(t)
	refusedEverywhere(t, e, stranger)
}

func TestWhoIsFailureRefused(t *testing.T) {
	e := start(t)
	e.who.set(tailnet.Peer{}, errors.New("no such peer"))
	resp, err := http.Get(e.url + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status %d, want 403", resp.StatusCode)
	}
}

func TestTaggedPeers(t *testing.T) {
	refusedEverywhere(t, start(t), kiosk)
	// Listing the login the tagged node reports doesn't let it in: only tags do.
	refusedEverywhere(t, startWith(t, options{allow: []string{"tagged-devices"}}), kiosk)

	_, snap := dialAs(t, startWith(t, options{allow: []string{"tag:kiosk"}}), kiosk)
	if snap.Role != "controller" {
		t.Errorf("--allow tag:kiosk: role %q", snap.Role)
	}
	_, snap = dialAs(t, startWith(t, options{readers: []string{"tag:kiosk"}}), kiosk)
	if snap.Role != "reader" {
		t.Errorf("--reader tag:kiosk: role %q", snap.Role)
	}
}

// Tags are checked first: a tagged peer whose pseudo-user ID happens to
// equal the owner's is still refused.
func TestTaggedPeerWithOwnerIDRefused(t *testing.T) {
	e := start(t)
	sneaky := kiosk
	sneaky.UserID = ownerID
	sneaky.Login = owner.Login
	refusedEverywhere(t, e, sneaky)
}

func TestAllowAndReaderLogins(t *testing.T) {
	_, snap := dialAs(t, startWith(t, options{allow: []string{"Friend@Example.com"}}), friend)
	if snap.Role != "controller" {
		t.Errorf("--allow login: role %q", snap.Role)
	}
	e := startWith(t, options{readers: []string{"friend@example.com"}})
	_, snap = dialAs(t, e, friend)
	if snap.Role != "reader" {
		t.Errorf("--reader login: role %q", snap.Role)
	}
	refusedEverywhere(t, e, stranger)
	// Logins ignore ASCII case only: a Unicode look-alike that folds to a
	// listed login (here the Kelvin sign for "k") is not that login.
	lookalike := tailnet.Peer{Login: "\u212Aate@example.com", UserID: 4004, Node: "lookalike-phone"}
	refusedEverywhere(t, startWith(t, options{allow: []string{"kate@example.com"}}), lookalike)
	// The owner is matched by user ID, not login.
	renamed := owner
	renamed.Login = "owner-renamed@example.com"
	if _, snap := dialAs(t, e, renamed); snap.Role != "controller" {
		t.Errorf("owner by user ID: role %q", snap.Role)
	}
}

func TestTaggedSelf(t *testing.T) {
	static := fstest.MapFS{}
	log, err := audit.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	base := server.Config{Identity: &fakeIdentity{}, Audit: log, SelfTagged: true}
	for name, cfg := range map[string]server.Config{
		"no lists":    base,
		"reader only": {Identity: base.Identity, Audit: log, SelfTagged: true, Readers: []string{"friend@example.com"}},
	} {
		if _, err := server.New(nil, static, cfg); err == nil {
			t.Errorf("%s: a tagged self started", name)
		}
	}
	ok := base
	ok.Allow = []string{"friend@example.com"}
	if _, err := server.New(nil, static, ok); err != nil {
		t.Errorf("tagged self with --allow: %v", err)
	}

	// With no owner, the tagged-devices user ID can't stand in for one.
	e := startWith(t, options{selfTagged: true, allow: []string{"friend@example.com"}})
	refusedEverywhere(t, e, owner)
	if _, snap := dialAs(t, e, friend); snap.Role != "controller" {
		t.Errorf("friend: role %q", snap.Role)
	}
}

func TestNewRequiresIdentityAndAudit(t *testing.T) {
	log, err := audit.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	for name, cfg := range map[string]server.Config{
		"no identity": {Audit: log, Owner: ownerID},
		"no audit":    {Identity: &fakeIdentity{}, Owner: ownerID},
		"no owner":    {Identity: &fakeIdentity{}, Audit: log},
	} {
		if _, err := server.New(nil, fstest.MapFS{}, cfg); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// The app shell opens from a link (a cross-site navigation), so it needs
// only WhoIs; /api/ also needs Sec-Fetch-Site or a matching Origin.
func TestAPISameSite(t *testing.T) {
	e := start(t)
	for name, tc := range map[string]struct {
		header http.Header
		code   int
	}{
		"missing":            {nil, http.StatusForbidden},
		"cross-site":         {http.Header{"Sec-Fetch-Site": {"cross-site"}}, http.StatusForbidden},
		"same-site":          {http.Header{"Sec-Fetch-Site": {"same-site"}}, http.StatusForbidden},
		"foreign origin":     {http.Header{"Sec-Fetch-Site": {"cross-site"}, "Origin": {"https://evil.example"}}, http.StatusForbidden},
		"same-origin":        {http.Header{"Sec-Fetch-Site": {"same-origin"}}, http.StatusNotFound},
		"none":               {http.Header{"Sec-Fetch-Site": {"none"}}, http.StatusNotFound},
		"app origin, no sfs": {http.Header{"Origin": {e.origin}}, http.StatusNotFound},
	} {
		if code, _ := get(t, e, owner, "/api/agents", tc.header); code != tc.code {
			t.Errorf("%s: %d, want %d", name, code, tc.code)
		}
	}
	if code, _ := get(t, e, owner, "/", http.Header{"Sec-Fetch-Site": {"cross-site"}}); code != http.StatusOK {
		t.Errorf("cross-site navigation to /: %d, want 200", code)
	}
}

// auditLines reads the audit log, checks its mode, and that no secret text
// leaked into it.
func auditLines(t *testing.T, e env, secrets ...string) []audit.Entry {
	t.Helper()
	path := filepath.Join(e.stateDir, audit.FileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("audit mode %o, want 600", mode)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets {
		if strings.Contains(string(data), s) {
			t.Errorf("audit log holds payload text %q", s)
		}
	}
	var out []audit.Entry
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		var a audit.Entry
		if err := json.Unmarshal(sc.Bytes(), &a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

func TestReaderForbiddenAndAudited(t *testing.T) {
	e := startWith(t, options{readers: []string{"friend@example.com"}})
	c, snap := dialAs(t, e, friend)
	if snap.Role != "reader" {
		t.Fatalf("role %q", snap.Role)
	}
	// Viewing works: watch gets a detail.
	send(t, c, map[string]string{"type": "watch", "pane_id": "w1:p1"})
	d := waitFor[hub.Detail](t, c, "detail", nil)
	secret := "please rotate the staging credentials"
	reqs := []map[string]any{
		{"id": "r1", "type": "prompt", "pane_id": "w1:p2", "text": secret},
		{"id": "r2", "type": "keys", "pane_id": "w1:p2", "keys": []string{"esc"}},
		{"id": "r3", "type": "text", "pane_id": "w1:p2", "text": secret},
		{"id": "r4", "type": "answer", "pane_id": "w1:p1", "question_id": d.Question.ID, "option": "3", "note": secret},
		{"id": "r5", "type": "rename", "pane_id": "w1:p2", "name": secret},
		{"id": "r6", "type": "focus_on_desktop", "pane_id": "w1:p2"},
	}
	for _, r := range reqs {
		send(t, c, r)
		wantCode(t, ack(t, c, r["id"].(string)), server.CodeForbidden)
	}
	if got := e.fake.InputLog(); len(got) != 0 {
		t.Errorf("a reader reached herdr: %+v", got)
	}
	lines := auditLines(t, e, secret, "rotate")
	if len(lines) != len(reqs) {
		t.Fatalf("%d audit lines, want %d", len(lines), len(reqs))
	}
	for i, l := range lines {
		if l.Login != friend.Login || l.Node != friend.Node || l.Result != server.CodeForbidden || l.Method != reqs[i]["type"] || l.PaneID != reqs[i]["pane_id"] || l.Time.IsZero() || len(l.SHA256) != 64 {
			t.Errorf("line %d: %+v", i, l)
		}
	}
	if lines[0].Size != len(secret) {
		t.Errorf("prompt size %d, want %d", lines[0].Size, len(secret))
	}
}

func TestControllerMutationAudited(t *testing.T) {
	e := start(t)
	c, _ := dialAs(t, e, owner)
	secret := "summarise the open incidents"
	send(t, c, map[string]string{"id": "p1", "type": "prompt", "pane_id": "w1:p2", "text": secret})
	if a := ack(t, c, "p1"); !a.OK {
		t.Fatalf("ack %+v", a)
	}
	send(t, c, map[string]string{"id": "p2", "type": "prompt", "pane_id": "nope", "text": secret})
	wantCode(t, ack(t, c, "p2"), server.CodeNotFound)
	lines := auditLines(t, e, secret, "incidents")
	if len(lines) != 2 || lines[0].Result != "ok" || lines[1].Result != server.CodeNotFound || lines[0].Login != owner.Login {
		t.Fatalf("audit %+v", lines)
	}
}

func TestBadOriginForTailnetHost(t *testing.T) {
	e := start(t)
	e.who.set(owner, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, e.url+"/ws", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {"https://herdr-pocket.evil.example"}},
	})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("dial with a foreign origin: %v, want 403", err)
	}
}

// The snapshot carries the expiry; a change is a patch, and clearing it
// sends an explicit null.
func TestExpiry(t *testing.T) {
	e := start(t)
	at := time.Now().Add(9 * 24 * time.Hour)
	e.hub.SetKeyExpiry(&at)
	c, snap := dialAs(t, e, owner)
	if snap.Expiry == nil || snap.Expiry.DaysLeft != 9 {
		t.Fatalf("snapshot expiry %+v, want 9 days", snap.Expiry)
	}

	e.hub.SetKeyExpiry(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var raw map[string]json.RawMessage
		if err := wsjson.Read(ctx, c, &raw); err != nil {
			t.Fatal(err)
		}
		if v, ok := raw["expiry"]; ok {
			if string(v) != "null" {
				t.Fatalf("expiry patch %s, want null", v)
			}
			break
		}
	}

	far := time.Now().Add(30 * 24 * time.Hour)
	e.hub.SetKeyExpiry(&far)
	if _, snap := dialAs(t, e, owner); snap.Expiry != nil {
		t.Errorf("30 days out: expiry %+v, want null", snap.Expiry)
	}
}
