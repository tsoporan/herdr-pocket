package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/hub"
	"github.com/tsoporan/herdr-pocket/internal/service"
	"github.com/tsoporan/herdr-pocket/internal/tailnet"
)

// serverInfo is what the CLI can learn about a running server without
// opening the node: whether the lock is held and, if status.json belongs to
// a live process, its contents.
type serverInfo struct {
	running bool
	status  *tailnet.StatusFile
}

// probeServer never creates the state dir: with no dir there is no server.
// The lock is the truth for "running", since status.json can outlive its
// process. Probing takes the lock for an instant, so a server starting in
// that instant exits "already running"; the unit's Restart=on-failure
// starts it again. A shared lock wouldn't help: it blocks the server's
// exclusive one just the same.
func probeServer(stateDir string) (serverInfo, error) {
	if _, err := os.Stat(stateDir); errors.Is(err, os.ErrNotExist) {
		return serverInfo{}, nil
	}
	unlock, err := tailnet.Lock(stateDir)
	switch {
	case err == nil:
		return serverInfo{}, unlock()
	case !errors.Is(err, tailnet.ErrLocked):
		return serverInfo{}, err
	}
	info := serverInfo{running: true}
	if st, err := tailnet.ReadStatus(stateDir); err == nil && pidAlive(st.PID) {
		info.status = &st
	}
	return info, nil
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// signedIn reports whether the node has state on disk.
func signedIn(stateDir string) bool {
	_, err := os.Stat(tailnet.TsnetDir(stateDir))
	return err == nil
}

// appURL is the app's https origin for a node FQDN.
func appURL(fqdn string) string {
	if strings.Contains(fqdn, "://") {
		return fqdn
	}
	return "https://" + strings.TrimSuffix(fqdn, ".")
}

func runStatus(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	stateDir := fs.String("state-dir", tailnet.DefaultStateDir(), "directory for the tailnet node, status and audit log")
	systemctl := fs.String("systemctl", "systemctl", "") // hidden, for tests
	fs.Usage = func() { fmt.Fprintln(fs.Output(), "usage: herdr-pocket status [--state-dir DIR]") }
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	unit, err := service.Manager{Systemctl: *systemctl}.IsActive(ctx)
	if err != nil {
		unit = "unknown (" + err.Error() + ")"
	}
	info, err := probeServer(*stateDir)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "service\t%s\n", unit)
	switch {
	case !info.running:
		fmt.Fprintln(w, "server\tnot running")
		if signedIn(*stateDir) {
			fmt.Fprintln(w, "tailnet\tsigned in")
		} else {
			fmt.Fprintln(w, "tailnet\tnot signed in")
		}
	case info.status == nil:
		fmt.Fprintln(w, "server\trunning, no status yet")
	default:
		st := info.status
		fmt.Fprintf(w, "server\trunning (pid %d)\n", st.PID)
		fmt.Fprintf(w, "state\t%s\n", st.State)
		if st.State == tailnet.StateNeedsLogin && st.AuthURL != "" {
			fmt.Fprintf(w, "sign in\t%s\n", st.AuthURL)
		}
		if st.FQDN != "" {
			fmt.Fprintf(w, "address\t%s\n", appURL(st.FQDN))
		}
		if st.HTTPSOff {
			fmt.Fprintf(w, "https\toff: %s\n", tailnet.HTTPSHint)
		}
		switch {
		case st.KeyExpiry != nil:
			days := hub.DaysLeft(*st.KeyExpiry, time.Now()) // as the app counts them
			fmt.Fprintf(w, "key expiry\t%s (%d days)\n", st.KeyExpiry.Local().Format(time.DateOnly), days)
		case st.State == tailnet.StateRunning:
			fmt.Fprintln(w, "key expiry\toff")
		}
		if st.HerdrProtocol != 0 {
			fmt.Fprintf(w, "herdr\tprotocol %d\n", st.HerdrProtocol)
		}
		if len(st.TranscriptDirs) > 0 {
			fmt.Fprintf(w, "transcripts\t%s\n", strings.Join(st.TranscriptDirs, ", "))
		}
	}
	return w.Flush()
}
