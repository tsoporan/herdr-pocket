package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tsoporan/herdr-pocket/internal/service"
)

// unitEnvVars are copied into the unit when set, so the service finds the
// same herdr socket and transcripts as the shell that ran install.
var unitEnvVars = []string{"CLAUDE_CONFIG_DIR", "HERDR_SOCKET_PATH"}

// serviceFlags adds the hidden --unit-dir and --systemctl overrides, which
// keep tests away from the real systemd, and returns the manager they fill.
func serviceFlags(fs *flag.FlagSet, usage string) func() (service.Manager, error) {
	unitDir := fs.String("unit-dir", "", "")
	systemctl := fs.String("systemctl", "systemctl", "")
	fs.Usage = func() { fmt.Fprintln(fs.Output(), "usage: "+usage) }
	return func() (service.Manager, error) {
		m := service.Manager{UnitDir: *unitDir, Systemctl: *systemctl}
		if m.UnitDir == "" {
			d, err := service.DefaultUnitDir()
			if err != nil {
				return m, err
			}
			m.UnitDir = d
		}
		return m, nil
	}
}

func runInstall(ctx context.Context, args []string, stdout io.Writer) error {
	// Flags after "--" are the server's; split there ourselves, because
	// fs.Args() can't tell "install x" from "install -- x".
	own, serveArgs := args, []string(nil)
	if i := slices.Index(args, "--"); i >= 0 {
		own, serveArgs = args[:i], args[i+1:]
	}
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	manager := serviceFlags(fs, "herdr-pocket install [-- server flags...]")
	if err := parseFlags(fs, own); err != nil {
		return err
	}
	m, err := manager()
	if err != nil {
		return err
	}
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	if bin, err = filepath.EvalSymlinks(bin); err != nil {
		return err
	}
	unit := service.Unit{Binary: bin, Args: serveArgs}
	for _, name := range unitEnvVars {
		if v := os.Getenv(name); v != "" {
			unit.Env = append(unit.Env, service.EnvVar{Name: name, Value: v})
		}
	}
	if err := m.Install(ctx, unit); err != nil {
		if errors.Is(err, service.ErrForeignUnit) {
			return fmt.Errorf("%w; move it aside to install herdr-pocket", err)
		}
		return err
	}
	fmt.Fprintf(stdout, "Installed %s and started %s.\n", m.Path(), service.Name)
	// login must look in the service's state dir, or it would sign in a
	// second node of its own.
	login := loginCommand(serveFlag(serveArgs, "state-dir"), serveFlag(serveArgs, "hostname"))
	fmt.Fprintf(stdout, "Run `%s` for the Tailscale sign-in link, or watch `journalctl --user -u herdr-pocket`.\n", login)
	fmt.Fprintln(stdout, "Once it's signed in, `herdr-pocket qr` shows its address as a QR code to scan with your phone.")
	return nil
}

// serveFlag is the last value of a string flag in the server's args, in any
// of Go's spellings (-name v, --name v, -name=v, --name=v), or "".
func serveFlag(args []string, name string) string {
	v := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		flagName, value, hasValue := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-"), "=")
		if !strings.HasPrefix(a, "-") || flagName != name {
			continue
		}
		if hasValue {
			v = value
		} else if i+1 < len(args) {
			i++
			v = args[i]
		}
	}
	return v
}
