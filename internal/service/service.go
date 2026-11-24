// Package service writes and removes herdr-pocket's systemd user unit.
//
// Every systemctl call goes through Manager.Systemctl, so tests point it at
// a fake script and a temp unit dir and never reach the real systemd.
package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Name is the unit's name.
const Name = "herdr-pocket.service"

// Marker is the first line of every unit this package writes. A unit
// without it belongs to someone else and is never overwritten or removed.
const Marker = "# Managed by herdr-pocket install; herdr-pocket uninstall removes it."

// ErrForeignUnit means the unit file exists but herdr-pocket didn't write it.
var ErrForeignUnit = errors.New("unit was not written by herdr-pocket install")

// ErrNotInstalled means there is no unit file.
var ErrNotInstalled = errors.New("herdr-pocket is not installed")

// Unit is what goes into the unit file.
type Unit struct {
	// Binary is the absolute path of the herdr-pocket binary.
	Binary string
	// Args follow Binary in ExecStart.
	Args []string
	// Env becomes Environment= lines, in order.
	Env []EnvVar
}

// EnvVar is one Environment= assignment.
type EnvVar struct{ Name, Value string }

// Render returns the unit file's content.
func (u Unit) Render() string {
	words := []string{quote(u.Binary, true)}
	for _, a := range u.Args {
		words = append(words, quote(a, true))
	}
	var b strings.Builder
	b.WriteString(Marker + "\n")
	b.WriteString("[Unit]\n")
	b.WriteString("Description=herdr-pocket: herdr agents on your phone over the tailnet\n")
	b.WriteString("\n[Service]\n")
	b.WriteString("ExecStart=" + strings.Join(words, " ") + "\n")
	for _, e := range u.Env {
		b.WriteString("Environment=" + quote(e.Name+"="+e.Value, false) + "\n")
	}
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=5\n")
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

// quote escapes s for a systemd unit line: % starts a specifier, and in
// ExecStart $ starts a variable. Words with spaces, quotes or backslashes
// are double-quoted.
func quote(s string, execLine bool) string {
	s = strings.ReplaceAll(s, "%", "%%")
	if execLine {
		s = strings.ReplaceAll(s, "$", "$$")
	}
	if s != "" && !strings.ContainsAny(s, " \t\"'\\;") {
		return s
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// DefaultUnitDir is $XDG_CONFIG_HOME/systemd/user, else ~/.config/systemd/user.
func DefaultUnitDir() (string, error) {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "systemd", "user"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

// Manager installs the unit into UnitDir and drives it with Systemctl.
type Manager struct {
	UnitDir   string
	Systemctl string // the systemctl binary; "--user" is always passed
}

// Path is the unit file's path.
func (m Manager) Path() string { return filepath.Join(m.UnitDir, Name) }

// Install writes the unit, reloads systemd and enables and starts it. A
// unit this package wrote before is replaced and restarted, so new flags
// take effect; any other unit is refused with ErrForeignUnit.
func (m Manager) Install(ctx context.Context, u Unit) error {
	replaced, err := m.owned()
	if err != nil && !errors.Is(err, ErrNotInstalled) {
		return err
	}
	if err := os.MkdirAll(m.UnitDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(m.Path(), []byte(u.Render()), 0o644); err != nil {
		return err
	}
	if err := m.run(ctx, "daemon-reload"); err != nil {
		return err
	}
	if err := m.run(ctx, "enable", "--now", Name); err != nil {
		return err
	}
	if replaced {
		return m.run(ctx, "restart", Name)
	}
	return nil
}

// Uninstall stops and disables the unit, removes it and reloads systemd.
// It returns ErrNotInstalled when there is no unit and ErrForeignUnit for a
// unit this package didn't write.
func (m Manager) Uninstall(ctx context.Context) error {
	if _, err := m.owned(); err != nil {
		return err
	}
	if err := m.run(ctx, "disable", "--now", Name); err != nil {
		return err
	}
	if err := os.Remove(m.Path()); err != nil {
		return err
	}
	return m.run(ctx, "daemon-reload")
}

// IsActive returns systemctl's is-active word for the unit, such as
// "active", "inactive" or "failed". is-active exits non-zero for every
// state but active, so only a failure to run systemctl is an error.
func (m Manager) IsActive(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, m.Systemctl, "--user", "is-active", Name).Output()
	state := strings.TrimSpace(string(out))
	var exitErr *exec.ExitError
	if err != nil && !(errors.As(err, &exitErr) && state != "") {
		return "", fmt.Errorf("systemctl --user is-active: %w", err)
	}
	return state, nil
}

// owned reports whether the unit file exists and carries the marker:
// true for ours, ErrNotInstalled when absent, ErrForeignUnit otherwise.
func (m Manager) owned() (bool, error) {
	b, err := os.ReadFile(m.Path())
	if errors.Is(err, os.ErrNotExist) {
		return false, ErrNotInstalled
	}
	if err != nil {
		return false, err
	}
	if !bytes.HasPrefix(b, []byte(Marker+"\n")) {
		return false, fmt.Errorf("%s: %w", m.Path(), ErrForeignUnit)
	}
	return true, nil
}

// Stop stops the unit and waits until it has stopped.
func (m Manager) Stop(ctx context.Context) error { return m.run(ctx, "stop", Name) }

// Start starts the unit.
func (m Manager) Start(ctx context.Context) error { return m.run(ctx, "start", Name) }

func (m Manager) run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, m.Systemctl, append([]string{"--user"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl --user %s: %w\n%s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}
