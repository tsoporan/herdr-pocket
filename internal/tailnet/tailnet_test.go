package tailnet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/types/views"
)

// Nothing here starts a node: Start, Logout and LocalClient are never called.

func TestDefaultStateDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/xdg/state")
	if got := DefaultStateDir(); got != "/xdg/state/herdr-pocket" {
		t.Errorf("with XDG_STATE_HOME: %q", got)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/someone")
	if got := DefaultStateDir(); got != "/home/someone/.local/state/herdr-pocket" {
		t.Errorf("without XDG_STATE_HOME: %q", got)
	}
}

func TestLockIsExclusive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("state dir mode %o, want 700", mode)
	}
	if _, err := Lock(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Lock: %v, want ErrLocked", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	unlock2, err := Lock(dir)
	if err != nil {
		t.Fatalf("Lock after unlock: %v", err)
	}
	unlock2()
}

func TestStatusRoundTripAndMode(t *testing.T) {
	dir := t.TempDir()
	exp := time.Date(2027, 5, 1, 12, 0, 0, 0, time.UTC)
	want := StatusFile{State: StateRunning, FQDN: "pocket.example-tailnet.ts.net", KeyExpiry: &exp, HerdrProtocol: 3, TranscriptDirs: []string{"/fixtures/claude"}, PID: 42}
	if err := WriteStatus(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt not stamped")
	}
	got.UpdatedAt = time.Time{}
	if got.State != want.State || got.FQDN != want.FQDN || !got.KeyExpiry.Equal(exp) || got.PID != 42 || !slices.Equal(got.TranscriptDirs, want.TranscriptDirs) {
		t.Errorf("round trip: %+v", got)
	}
	info, err := os.Stat(filepath.Join(dir, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("status.json mode %o, want 600", mode)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp files left: %v", entries)
	}
}

func TestReadStatusMissing(t *testing.T) {
	if _, err := ReadStatus(t.TempDir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want ErrNotExist", err)
	}
}

// A reader never sees a partial file while writers race.
func TestStatusAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := WriteStatus(dir, StatusFile{State: StateStarting, PID: 1}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			dirs := slices.Repeat([]string{"/a/long/transcript/dir"}, 200*(i+1))
			for ctx.Err() == nil {
				if err := WriteStatus(dir, StatusFile{State: StateRunning, TranscriptDirs: dirs, PID: i}); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	for ctx.Err() == nil {
		if _, err := ReadStatus(dir); err != nil {
			t.Fatalf("read during writes: %v", err)
		}
	}
	wg.Wait()
}

func TestTrimDNSName(t *testing.T) {
	for in, want := range map[string]string{
		"herdr-pocket.example-tailnet.ts.net.":   "herdr-pocket.example-tailnet.ts.net",
		"herdr-pocket-1.example-tailnet.ts.net.": "herdr-pocket-1.example-tailnet.ts.net",
		"already.trimmed.ts.net":                 "already.trimmed.ts.net",
		"":                                       "",
	} {
		if got := TrimDNSName(in); got != want {
			t.Errorf("TrimDNSName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSelfFrom(t *testing.T) {
	exp := time.Date(2027, 5, 1, 0, 0, 0, 0, time.UTC)
	tags := views.SliceOf([]string{"tag:server"})
	for name, tc := range map[string]struct {
		self *ipnstate.PeerStatus
		want Self
		err  bool
	}{
		"owned": {
			self: &ipnstate.PeerStatus{DNSName: "herdr-pocket-1.example-tailnet.ts.net.", UserID: 7, KeyExpiry: &exp},
			want: Self{FQDN: "herdr-pocket-1.example-tailnet.ts.net", OwnerUserID: 7, KeyExpiry: &exp},
		},
		"expiry off": {
			self: &ipnstate.PeerStatus{DNSName: "herdr-pocket.example-tailnet.ts.net.", UserID: 7},
			want: Self{FQDN: "herdr-pocket.example-tailnet.ts.net", OwnerUserID: 7},
		},
		"tagged has no owner": {
			self: &ipnstate.PeerStatus{DNSName: "herdr-pocket.example-tailnet.ts.net.", UserID: 99, Tags: &tags},
			want: Self{FQDN: "herdr-pocket.example-tailnet.ts.net", Tagged: true},
		},
		"no dns name": {self: &ipnstate.PeerStatus{UserID: 7}, err: true},
		"no self":     {err: true},
	} {
		got, err := selfFrom(&ipnstate.Status{Self: tc.self})
		if (err != nil) != tc.err {
			t.Errorf("%s: err %v", name, err)
			continue
		}
		if got.FQDN != tc.want.FQDN || got.OwnerUserID != tc.want.OwnerUserID || got.Tagged != tc.want.Tagged || (got.KeyExpiry == nil) != (tc.want.KeyExpiry == nil) {
			t.Errorf("%s: %+v, want %+v", name, got, tc.want)
		}
	}
}

func TestCheckFunnel(t *testing.T) {
	if err := CheckFunnel(nil); err != nil {
		t.Errorf("nil config: %v", err)
	}
	if err := CheckFunnel(&ipn.ServeConfig{}); err != nil {
		t.Errorf("empty config: %v", err)
	}
	off := &ipn.ServeConfig{AllowFunnel: map[ipn.HostPort]bool{"herdr-pocket.example-tailnet.ts.net:443": false}}
	if err := CheckFunnel(off); err != nil {
		t.Errorf("funnel listed but off: %v", err)
	}
	on := &ipn.ServeConfig{AllowFunnel: map[ipn.HostPort]bool{"herdr-pocket.example-tailnet.ts.net:443": true}}
	if err := CheckFunnel(on); !errors.Is(err, ErrFunnel) {
		t.Errorf("funnel on: %v, want ErrFunnel", err)
	}
	fg := &ipn.ServeConfig{Foreground: map[string]*ipn.ServeConfig{"session": on}}
	if err := CheckFunnel(fg); !errors.Is(err, ErrFunnel) {
		t.Errorf("foreground funnel on: %v, want ErrFunnel", err)
	}
}

type fakeWhoIs struct {
	resp *apitype.WhoIsResponse
	err  error
}

func (f fakeWhoIs) WhoIs(context.Context, string) (*apitype.WhoIsResponse, error) {
	return f.resp, f.err
}

func TestIdentity(t *testing.T) {
	user := &apitype.WhoIsResponse{
		Node:        &tailcfg.Node{Name: "phone.example-tailnet.ts.net.", ComputedName: "phone", User: 7},
		UserProfile: &tailcfg.UserProfile{ID: 7, LoginName: "owner@example.com"},
	}
	got, err := localIdentity{fakeWhoIs{resp: user}}.WhoIs(context.Background(), "100.64.0.2:4321")
	if err != nil {
		t.Fatal(err)
	}
	if got.Login != "owner@example.com" || got.UserID != 7 || got.Node != "phone" || len(got.Tags) != 0 {
		t.Errorf("user peer: %+v", got)
	}

	tagged := &apitype.WhoIsResponse{
		Node:        &tailcfg.Node{Name: "kiosk.example-tailnet.ts.net.", User: 123, Tags: []string{"tag:kiosk"}},
		UserProfile: &tailcfg.UserProfile{ID: 123, LoginName: "tagged-devices"},
	}
	got, err = localIdentity{fakeWhoIs{resp: tagged}}.WhoIs(context.Background(), "100.64.0.3:4321")
	if err != nil {
		t.Fatal(err)
	}
	if got.Node != "kiosk" || !slices.Equal(got.Tags, []string{"tag:kiosk"}) || got.Login != "tagged-devices" {
		t.Errorf("tagged peer: %+v", got)
	}

	for name, f := range map[string]fakeWhoIs{
		"error":      {err: errors.New("no such peer")},
		"no node":    {resp: &apitype.WhoIsResponse{UserProfile: user.UserProfile}},
		"no profile": {resp: &apitype.WhoIsResponse{Node: user.Node}},
		"nil":        {},
	} {
		if _, err := (localIdentity{f}).WhoIs(context.Background(), "100.64.0.4:1"); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// A lapsed key shows in status.json as needs_login, not as running.
func TestBackendState(t *testing.T) {
	const link = "https://login.tailscale.com/a/example"
	for backend, want := range map[string][2]string{
		ipn.Running.String():          {StateRunning, ""},
		ipn.NeedsLogin.String():       {StateNeedsLogin, link},
		ipn.Starting.String():         {StateStarting, ""},
		ipn.NoState.String():          {StateStarting, ""},
		ipn.NeedsMachineAuth.String(): {StateStarting, ""},
	} {
		state, url := backendState(backend, link)
		if state != want[0] || url != want[1] {
			t.Errorf("%s: %q %q, want %q %q", backend, state, url, want[0], want[1])
		}
	}
}

func TestSelfFromHTTPS(t *testing.T) {
	self := &ipnstate.PeerStatus{DNSName: "herdr-pocket.example-tailnet.ts.net.", UserID: 7}
	on, _ := selfFrom(&ipnstate.Status{Self: self, CertDomains: []string{"herdr-pocket.example-tailnet.ts.net"}})
	off, _ := selfFrom(&ipnstate.Status{Self: self})
	if !on.HTTPS || off.HTTPS {
		t.Errorf("HTTPS with cert domains %v, without %v", on.HTTPS, off.HTTPS)
	}
}
