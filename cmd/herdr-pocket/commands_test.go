package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/service"
	"github.com/tsoporan/herdr-pocket/internal/tailnet"
)

// TestMain points every default path at temp dirs and puts a systemctl on
// PATH that only fails, so a test that forgets an override can't reach the
// real state dir, unit dir or systemd.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "herdr-pocket-cmd-test-")
	if err != nil {
		panic(err)
	}
	for name, dir := range map[string]string{"HOME": "home", "XDG_STATE_HOME": "state", "XDG_CONFIG_HOME": "config", "PATH": "bin"} {
		p := filepath.Join(root, dir)
		if err := os.MkdirAll(p, 0o700); err != nil {
			panic(err)
		}
		if name == "PATH" {
			p += string(os.PathListSeparator) + os.Getenv("PATH")
		}
		os.Setenv(name, p)
	}
	guard := "#!/bin/sh\necho 'test used the systemctl on PATH' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(root, "bin", "systemctl"), []byte(guard), 0o755); err != nil {
		panic(err)
	}
	os.Unsetenv("CLAUDE_CONFIG_DIR")
	os.Unsetenv("HERDR_SOCKET_PATH")
	// Nothing in a test may start a real tsnet node; tests that need these
	// replace them and restore the guard.
	errRealNode := errors.New("test reached the real node")
	startNodeForLogin = func(context.Context, string, string, io.Writer) error { return errRealNode }
	logoutNode = func(context.Context, string) error { return errRealNode }
	newNode = func(tailnet.Config) node { return failingNode{errRealNode} }
	code := m.Run()
	os.RemoveAll(root)
	os.Exit(code)
}

// fakeSystemctl writes a systemctl that logs its args and answers
// is-active with isActive.
func fakeSystemctl(t *testing.T, isActive string) (script, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "systemctl.log")
	script = filepath.Join(dir, "systemctl")
	body := "#!/bin/sh\necho \"$@\" >> '" + log + "'\n" +
		"if [ \"$2\" = is-active ]; then echo " + isActive + "; [ " + isActive + " = active ]; exit; fi\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, log
}

func readLog(t *testing.T, log string) string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := dispatch(context.Background(), args, &out)
	return out.String(), err
}

func TestDispatchUnknownCommand(t *testing.T) {
	if _, err := run(t, "instal"); !errors.Is(err, errUsage) {
		t.Errorf("unknown command = %v, want a usage error", err)
	}
	if _, err := run(t, "status", "extra"); !errors.Is(err, errUsage) {
		t.Errorf("positional arg = %v, want a usage error", err)
	}
}

func TestServeRoutesDev(t *testing.T) {
	// checkDevAddr refuses before any socket or listener is touched.
	for _, args := range [][]string{
		{"--dev", "--dev-addr", "0.0.0.0:1"},
		{"serve", "--dev", "--dev-addr", "0.0.0.0:1"},
	} {
		if _, err := run(t, args...); err == nil || !strings.Contains(err.Error(), "only loopback addresses") {
			t.Errorf("%v = %v, want the --dev loopback refusal", args, err)
		}
	}
}

func TestInstallCommand(t *testing.T) {
	systemctl, log := fakeSystemctl(t, "active")
	unitDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "/home/dev/.claude-alt")

	out, err := run(t, "install", "--unit-dir", unitDir, "--systemctl", systemctl, "--", "--hostname", "pocket-test", "--state-dir", "/var/lib/pocket test")
	if err != nil {
		t.Fatal(err)
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin, _ = filepath.EvalSymlinks(bin)
	want := service.Unit{
		Binary: bin,
		Args:   []string{"--hostname", "pocket-test", "--state-dir", "/var/lib/pocket test"},
		Env:    []service.EnvVar{{Name: "CLAUDE_CONFIG_DIR", Value: "/home/dev/.claude-alt"}},
	}.Render()
	got, err := os.ReadFile(filepath.Join(unitDir, service.Name))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("unit =\n%s\nwant\n%s", got, want)
	}
	if !strings.Contains(string(got), `--state-dir "/var/lib/pocket test"`) || strings.Contains(string(got), "HERDR_SOCKET_PATH") {
		t.Errorf("unit args or env wrong:\n%s", got)
	}
	// login must use the service's state dir and hostname.
	if !strings.Contains(out, "`herdr-pocket login --state-dir '/var/lib/pocket test' --hostname pocket-test`") {
		t.Errorf("install output %q doesn't point at the matching login", out)
	}
	if got, want := readLog(t, log), "--user daemon-reload\n--user enable --now herdr-pocket.service\n"; got != want {
		t.Errorf("systemctl calls = %q, want %q", got, want)
	}
}

func TestInstallRejectsArgsBeforeDashDash(t *testing.T) {
	systemctl, log := fakeSystemctl(t, "active")
	unitDir := t.TempDir()
	if _, err := run(t, "install", "--unit-dir", unitDir, "--systemctl", systemctl, "--hostname", "x"); !errors.Is(err, errUsage) {
		t.Errorf("server flag before -- = %v, want a usage error", err)
	}
	if _, err := run(t, "install", "--unit-dir", unitDir, "--systemctl", systemctl, "extra"); !errors.Is(err, errUsage) {
		t.Errorf("positional before -- = %v, want a usage error", err)
	}
	if got := readLog(t, log); got != "" {
		t.Errorf("systemctl called: %q", got)
	}
}

func TestInstallRefusesForeignUnit(t *testing.T) {
	systemctl, log := fakeSystemctl(t, "active")
	unitDir := t.TempDir()
	path := filepath.Join(unitDir, service.Name)
	foreign := "[Service]\nExecStart=/usr/bin/someone-else\n"
	if err := os.WriteFile(path, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "install", "--unit-dir", unitDir, "--systemctl", systemctl); !errors.Is(err, service.ErrForeignUnit) {
		t.Errorf("install over a foreign unit = %v, want ErrForeignUnit", err)
	}
	if _, err := run(t, "uninstall", "--unit-dir", unitDir, "--systemctl", systemctl); !errors.Is(err, service.ErrForeignUnit) {
		t.Errorf("uninstall of a foreign unit = %v, want ErrForeignUnit", err)
	}
	if b, _ := os.ReadFile(path); string(b) != foreign {
		t.Errorf("foreign unit changed to %q", b)
	}
	if got := readLog(t, log); got != "" {
		t.Errorf("systemctl called: %q", got)
	}
}

func TestUninstallCommand(t *testing.T) {
	systemctl, log := fakeSystemctl(t, "active")
	unitDir := t.TempDir()
	if _, err := run(t, "install", "--unit-dir", unitDir, "--systemctl", systemctl); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(log, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "uninstall", "--unit-dir", unitDir, "--systemctl", systemctl); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(unitDir, service.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unit still there: %v", err)
	}
	if got, want := readLog(t, log), "--user disable --now herdr-pocket.service\n--user daemon-reload\n"; got != want {
		t.Errorf("systemctl calls = %q, want %q", got, want)
	}
	out, err := run(t, "uninstall", "--unit-dir", unitDir, "--systemctl", systemctl)
	if err != nil || !strings.Contains(out, "not installed") {
		t.Errorf("second uninstall = %q, %v", out, err)
	}
}

func TestStatusNoServer(t *testing.T) {
	systemctl, _ := fakeSystemctl(t, "inactive")
	stateDir := filepath.Join(t.TempDir(), "state")
	out, err := run(t, "status", "--state-dir", stateDir, "--systemctl", systemctl)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"inactive", "not running", "not signed in"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("status created the state dir: %v", err)
	}
}

// stateWith returns a state dir holding a signed-in node and st, and holds
// the lock as a running server would until the test ends.
func stateWith(t *testing.T, st tailnet.StatusFile, locked bool) string {
	t.Helper()
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(filepath.Join(stateDir, "tsnet"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := tailnet.WriteStatus(stateDir, st); err != nil {
		t.Fatal(err)
	}
	if locked {
		unlock, err := tailnet.Lock(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { unlock() })
	}
	return stateDir
}

func TestStatusRunningServer(t *testing.T) {
	systemctl, _ := fakeSystemctl(t, "active")
	expiry := time.Date(2027, 5, 20, 12, 0, 0, 0, time.Local)
	stateDir := stateWith(t, tailnet.StatusFile{
		State:          "running",
		FQDN:           "herdr-pocket.example-tailnet.ts.net",
		KeyExpiry:      &expiry,
		HerdrProtocol:  3,
		TranscriptDirs: []string{"/home/dev/.claude", "/home/dev/.claude-alt"},
		PID:            os.Getpid(),
		UpdatedAt:      time.Now(),
	}, true)
	out, err := run(t, "status", "--state-dir", stateDir, "--systemctl", systemctl)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"service      active",
		"running (pid ",
		"https://herdr-pocket.example-tailnet.ts.net",
		"2027-05-20",
		"protocol 3",
		"/home/dev/.claude, /home/dev/.claude-alt",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}
}

func TestStatusStaleFileIsNotRunning(t *testing.T) {
	systemctl, _ := fakeSystemctl(t, "inactive")
	stateDir := stateWith(t, tailnet.StatusFile{State: "running", FQDN: "herdr-pocket.example-tailnet.ts.net", PID: os.Getpid()}, false)
	out, err := run(t, "status", "--state-dir", stateDir, "--systemctl", systemctl)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "not running") || !strings.Contains(out, "signed in") || strings.Contains(out, "not signed in") {
		t.Errorf("status with an unlocked, stale status.json:\n%s", out)
	}
}

// syncBuffer is a bytes.Buffer safe to read while a command writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestLoginFollowsRunningServer(t *testing.T) {
	loginPoll = 5 * time.Millisecond
	t.Cleanup(func() { loginPoll = 500 * time.Millisecond })
	const authURL = "https://login.tailscale.com/a/0123456789ab"
	stateDir := stateWith(t, tailnet.StatusFile{State: "needs_login", AuthURL: authURL, PID: os.Getpid()}, true)

	var out syncBuffer
	done := make(chan error, 1)
	go func() { done <- dispatch(context.Background(), []string{"login", "--state-dir", stateDir}, &out) }()

	deadline := time.After(5 * time.Second)
	for !strings.Contains(out.String(), authURL) {
		select {
		case err := <-done:
			t.Fatalf("login returned early: %v, output %q", err, out.String())
		case <-deadline:
			t.Fatalf("login never printed the link; output %q", out.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := tailnet.WriteStatus(stateDir, tailnet.StatusFile{State: "running", FQDN: "herdr-pocket.example-tailnet.ts.net", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("login didn't return after the server reported running")
	}
	got := out.String()
	if strings.Count(got, authURL) != 1 || !strings.Contains(got, "Signed in as https://herdr-pocket.example-tailnet.ts.net") {
		t.Errorf("login output:\n%s", got)
	}
}

func TestLoginAlreadySignedIn(t *testing.T) {
	stateDir := stateWith(t, tailnet.StatusFile{State: "running", FQDN: "herdr-pocket.example-tailnet.ts.net.", PID: os.Getpid()}, true)
	out, err := run(t, "login", "--state-dir", stateDir)
	if err != nil || !strings.Contains(out, "already signed in as https://herdr-pocket.example-tailnet.ts.net\n") {
		t.Errorf("login = %q, %v", out, err)
	}
	// The running server can't sign in again, so login says how.
	if !strings.Contains(out, "systemctl --user stop herdr-pocket.service") || !strings.Contains(out, "`herdr-pocket login --state-dir "+shellQuote(stateDir)+"`") {
		t.Errorf("login doesn't say how to sign in again: %q", out)
	}
}

func TestInstallLoginHint(t *testing.T) {
	for args, want := range map[string]string{
		"":                                 "herdr-pocket login",
		"--allow tag:phone":                "herdr-pocket login",
		"-state-dir=/srv/pocket":           "herdr-pocket login --state-dir /srv/pocket",
		"--hostname=pocket --state-dir /a": "herdr-pocket login --state-dir /a --hostname pocket",
		"--state-dir /a --state-dir /b":    "herdr-pocket login --state-dir /b",
		"--reader x@example.com -- --state-dir /a": "herdr-pocket login",
	} {
		serveArgs := strings.Fields(args)
		if got := loginCommand(serveFlag(serveArgs, "state-dir"), serveFlag(serveArgs, "hostname")); got != want {
			t.Errorf("%q: %q, want %q", args, got, want)
		}
	}
}

func TestLoginForegroundHoldsLock(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	orig := startNodeForLogin
	t.Cleanup(func() { startNodeForLogin = orig })
	var called bool
	startNodeForLogin = func(_ context.Context, dir, hostname string, _ io.Writer) error {
		called = true
		if dir != stateDir || hostname != "pocket-test" {
			t.Errorf("startNodeForLogin(%q, %q)", dir, hostname)
		}
		if _, err := tailnet.Lock(dir); !errors.Is(err, tailnet.ErrLocked) {
			t.Errorf("lock not held during the foreground login: %v", err)
		}
		return nil
	}
	if _, err := run(t, "login", "--state-dir", stateDir, "--hostname", "pocket-test"); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("startNodeForLogin not called")
	}
	unlock, err := tailnet.Lock(stateDir)
	if err != nil {
		t.Fatalf("lock still held after login: %v", err)
	}
	unlock()
}

func TestLogout(t *testing.T) {
	orig := logoutNode
	t.Cleanup(func() { logoutNode = orig })
	var calls int
	logoutNode = func(context.Context, string) error { calls++; return nil }

	running := stateWith(t, tailnet.StatusFile{State: "running", PID: os.Getpid()}, true)
	if _, err := run(t, "logout", "--state-dir", running); err == nil || !strings.Contains(err.Error(), "stop it first") {
		t.Errorf("logout while running = %v, want a refusal", err)
	}
	if calls != 0 || !signedIn(running) {
		t.Error("logout touched the node while the server ran")
	}

	stopped := stateWith(t, tailnet.StatusFile{State: "stopped", PID: os.Getpid()}, false)
	if _, err := run(t, "logout", "--state-dir", stopped); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || signedIn(stopped) {
		t.Errorf("logout: %d calls, signed in %v", calls, signedIn(stopped))
	}
	if out, err := run(t, "logout", "--state-dir", stopped); err != nil || !strings.Contains(out, "Not signed in") {
		t.Errorf("second logout = %q, %v", out, err)
	}
}

// status counts the days left as the app's banner does: rounded up.
func TestStatusDaysLikeTheApp(t *testing.T) {
	systemctl, _ := fakeSystemctl(t, "active")
	expiry := time.Now().Add(8*24*time.Hour + 23*time.Hour)
	stateDir := stateWith(t, tailnet.StatusFile{State: "running", FQDN: "herdr-pocket.example-tailnet.ts.net", KeyExpiry: &expiry, PID: os.Getpid()}, true)
	out, err := run(t, "status", "--state-dir", stateDir, "--systemctl", systemctl)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "(9 days)") {
		t.Errorf("status output:\n%s", out)
	}
}

// A server that needs login but shows no link (a lapsed key) gets a hint.
func TestLoginNoLinkHint(t *testing.T) {
	loginPoll, linkWait = 5*time.Millisecond, 0
	t.Cleanup(func() { loginPoll, linkWait = 500*time.Millisecond, 10*time.Second })
	stateDir := stateWith(t, tailnet.StatusFile{State: "needs_login", PID: os.Getpid()}, true)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	if err := dispatch(ctx, []string{"login", "--state-dir", stateDir}, &out); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("login = %v", err)
	}
	if got := out.String(); strings.Count(got, "If none appears") != 1 || !strings.Contains(got, "systemctl --user stop herdr-pocket.service") {
		t.Errorf("login output %q", got)
	}
}

// login on a signed-in service stops it, signs in from the foreground with
// the lock held, and starts it again, so "run herdr-pocket login" is the
// whole fix the app's How to fix sheet gives.
func TestLoginRestartsSignedInService(t *testing.T) {
	flockBin, err := exec.LookPath("flock")
	if err != nil {
		t.Skip("needs flock(1) to stand in for the running service")
	}
	stateDir := stateWith(t, tailnet.StatusFile{State: "running", FQDN: "herdr-pocket.example-tailnet.ts.net", PID: os.Getpid()}, false)
	// The "service": another process holding the node lock until stopped.
	svc := exec.Command(flockBin, "-o", filepath.Join(stateDir, "tsnet.lock"), "sleep", "60")
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	go svc.Wait() // reap, so the fake stop sees it gone
	t.Cleanup(func() { svc.Process.Kill() })
	for i := 0; ; i++ {
		unlock, err := tailnet.Lock(stateDir)
		if errors.Is(err, tailnet.ErrLocked) {
			break
		}
		if err == nil {
			unlock()
		}
		if i == 200 {
			t.Fatal("the stand-in service never took the lock")
		}
		time.Sleep(5 * time.Millisecond)
	}

	dir := t.TempDir()
	log := filepath.Join(dir, "systemctl.log")
	script := filepath.Join(dir, "systemctl")
	body := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> '%s'\ncase \"$2\" in\n is-active) echo active ;;\n stop) kill %d; while kill -0 %d 2>/dev/null; do sleep 0.01; done ;;\nesac\n", log, svc.Process.Pid, svc.Process.Pid)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	orig := startNodeForLogin
	t.Cleanup(func() { startNodeForLogin = orig })
	var called bool
	startNodeForLogin = func(_ context.Context, dir, _ string, _ io.Writer) error {
		called = true
		if _, err := tailnet.Lock(dir); !errors.Is(err, tailnet.ErrLocked) {
			t.Errorf("lock not held during the sign-in: %v", err)
		}
		return nil
	}
	out, err := run(t, "login", "--state-dir", stateDir, "--systemctl", script)
	if err != nil {
		t.Fatalf("login: %v\n%s", err, out)
	}
	if !called {
		t.Error("startNodeForLogin not called")
	}
	want := "--user is-active herdr-pocket.service\n--user stop herdr-pocket.service\n--user start herdr-pocket.service\n"
	if got := readLog(t, log); got != want {
		t.Errorf("systemctl calls = %q, want %q", got, want)
	}
	if !strings.Contains(out, "Started herdr-pocket.service again.") {
		t.Errorf("login output:\n%s", out)
	}
}

func TestLogoutMovesStateAside(t *testing.T) {
	orig := logoutNode
	t.Cleanup(func() { logoutNode = orig })
	logoutNode = func(context.Context, string) error { return nil }
	stateDir := stateWith(t, tailnet.StatusFile{State: "stopped", PID: os.Getpid()}, false)
	marker := filepath.Join(stateDir, "tsnet", "node.state")
	if err := os.WriteFile(marker, []byte("keys"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "logout", "--state-dir", stateDir)
	if err != nil {
		t.Fatal(err)
	}
	backups, _ := filepath.Glob(filepath.Join(stateDir, "tsnet.bak-*", "node.state"))
	if len(backups) != 1 || !strings.Contains(out, filepath.Dir(backups[0])) {
		t.Errorf("backups %v, output %q", backups, out)
	}
}
