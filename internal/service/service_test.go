package service_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tsoporan/herdr-pocket/internal/service"
)

const golden = `# Managed by herdr-pocket install; herdr-pocket uninstall removes it.
[Unit]
Description=herdr-pocket: herdr agents on your phone over the tailnet

[Service]
ExecStart=/opt/herdr-pocket/bin/herdr-pocket --hostname pocket-test --claude-config-dir "/home/dev/My Claude" --reader 50%%$$off
Environment=CLAUDE_CONFIG_DIR=/home/dev/.claude-work
Environment="HERDR_SOCKET_PATH=/run/user/1000/herdr sock"
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`

var testUnit = service.Unit{
	Binary: "/opt/herdr-pocket/bin/herdr-pocket",
	Args:   []string{"--hostname", "pocket-test", "--claude-config-dir", "/home/dev/My Claude", "--reader", "50%$off"},
	Env: []service.EnvVar{
		{Name: "CLAUDE_CONFIG_DIR", Value: "/home/dev/.claude-work"},
		{Name: "HERDR_SOCKET_PATH", Value: "/run/user/1000/herdr sock"},
	},
}

func TestRender(t *testing.T) {
	if got := testUnit.Render(); got != golden {
		t.Errorf("Render() =\n%s\nwant\n%s", got, golden)
	}
}

// fakeSystemctl returns a manager on a temp unit dir whose systemctl
// appends its args to a log, prints isActive for is-active, and exits with
// isActiveCode there.
func fakeSystemctl(t *testing.T, isActive string, isActiveCode int) (service.Manager, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "systemctl.log")
	script := filepath.Join(dir, "systemctl")
	body := "#!/bin/sh\necho \"$@\" >> '" + log + "'\n" +
		"if [ \"$2\" = is-active ]; then echo " + isActive + "; exit " + strconv.Itoa(isActiveCode) + "; fi\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return service.Manager{UnitDir: filepath.Join(dir, "units"), Systemctl: script}, log
}

func calls(t *testing.T, log string) string {
	t.Helper()
	b, err := os.ReadFile(log)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInstallUninstall(t *testing.T) {
	ctx := context.Background()
	m, log := fakeSystemctl(t, "active", 0)

	if err := m.Install(ctx, testUnit); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(m.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != golden {
		t.Errorf("unit file =\n%s", b)
	}
	if got, want := calls(t, log), "--user daemon-reload\n--user enable --now herdr-pocket.service\n"; got != want {
		t.Errorf("install calls = %q, want %q", got, want)
	}

	// Installing again replaces our own unit and restarts it.
	if err := m.Install(ctx, testUnit); err != nil {
		t.Fatal(err)
	}
	if got := calls(t, log); !strings.HasSuffix(got, "--user restart herdr-pocket.service\n") {
		t.Errorf("reinstall calls = %q, want a restart last", got)
	}

	if err := os.Truncate(log, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unit still there after uninstall: %v", err)
	}
	if got, want := calls(t, log), "--user disable --now herdr-pocket.service\n--user daemon-reload\n"; got != want {
		t.Errorf("uninstall calls = %q, want %q", got, want)
	}

	if err := m.Uninstall(ctx); !errors.Is(err, service.ErrNotInstalled) {
		t.Errorf("second Uninstall = %v, want ErrNotInstalled", err)
	}
}

func TestForeignUnitRefused(t *testing.T) {
	ctx := context.Background()
	m, log := fakeSystemctl(t, "active", 0)
	foreign := "[Service]\nExecStart=/usr/bin/something-else\n"
	if err := os.MkdirAll(m.UnitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.Path(), []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := m.Install(ctx, testUnit); !errors.Is(err, service.ErrForeignUnit) {
		t.Errorf("Install over a foreign unit = %v, want ErrForeignUnit", err)
	}
	if err := m.Uninstall(ctx); !errors.Is(err, service.ErrForeignUnit) {
		t.Errorf("Uninstall of a foreign unit = %v, want ErrForeignUnit", err)
	}
	if b, _ := os.ReadFile(m.Path()); string(b) != foreign {
		t.Errorf("foreign unit changed to %q", b)
	}
	if got := calls(t, log); got != "" {
		t.Errorf("systemctl called: %q", got)
	}
}

func TestIsActive(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		word string
		code int
	}{{"active", 0}, {"inactive", 3}, {"failed", 3}} {
		m, _ := fakeSystemctl(t, tc.word, tc.code)
		got, err := m.IsActive(ctx)
		if err != nil || got != tc.word {
			t.Errorf("IsActive() = %q, %v; want %q", got, err, tc.word)
		}
	}
	m := service.Manager{Systemctl: filepath.Join(t.TempDir(), "missing")}
	if _, err := m.IsActive(ctx); err == nil {
		t.Error("IsActive with no systemctl: want an error")
	}
}
