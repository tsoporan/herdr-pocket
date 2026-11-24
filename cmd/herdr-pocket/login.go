package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/service"
	"github.com/tsoporan/herdr-pocket/internal/tailnet"
)

// startNodeForLogin signs the node in from the foreground while login holds
// the lock: it prints the sign-in link (a fresh one when the node is already
// signed in, which restarts its key expiry), waits until the node is
// running, then shuts the node down, leaving its state signed in for the
// service. Tests replace it, so they never start a node.
var startNodeForLogin = func(ctx context.Context, stateDir, hostname string, stdout io.Writer) error {
	node := tailnet.NewNode(tailnet.Config{StateDir: stateDir, Hostname: hostname, Out: stdout})
	self, err := node.Login(ctx)
	if err != nil {
		return errors.Join(err, node.Close())
	}
	fmt.Fprintf(stdout, "Signed in as %s\n", appURL(self.FQDN))
	if self.KeyExpiry != nil {
		fmt.Fprintf(stdout, "The sign-in ends on %s.\n", self.KeyExpiry.Local().Format(time.DateOnly))
	}
	if !self.HTTPS {
		fmt.Fprintln(stdout, tailnet.HTTPSHint)
	}
	return errors.Join(printPhoneSetup(stdout, appURL(self.FQDN)), node.Close())
}

// loginPoll is how often login rereads a running server's status.json.
var loginPoll = 500 * time.Millisecond

// linkWait is how long login waits for a server that needs login to show a
// link before it says what to do if none comes.
var linkWait = 10 * time.Second

func runLogin(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	stateDir := fs.String("state-dir", tailnet.DefaultStateDir(), "directory for the tailnet node, status and audit log")
	hostname := fs.String("hostname", "herdr-pocket", "tailnet node name, used when the node is new")
	systemctl := fs.String("systemctl", "systemctl", "") // hidden, for tests
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	unlock, err := tailnet.Lock(*stateDir)
	if errors.Is(err, tailnet.ErrLocked) {
		m := service.Manager{Systemctl: *systemctl}
		if signedInService(ctx, *stateDir, m) {
			return reloginService(ctx, *stateDir, *hostname, m, stdout)
		}
		again := loginCommand(flagIfSet(fs, "state-dir"), flagIfSet(fs, "hostname"))
		return waitForSignIn(ctx, *stateDir, again, stdout)
	}
	if err != nil {
		return err
	}
	defer unlock()
	return startNodeForLogin(ctx, *stateDir, *hostname, stdout)
}

// signedInService reports whether the server holding the lock is the
// systemd service and is already signed in. Only the server can use the
// node while it runs, so signing in again means stopping it first.
func signedInService(ctx context.Context, stateDir string, m service.Manager) bool {
	info, err := probeServer(stateDir)
	if err != nil || info.status == nil || info.status.State != tailnet.StateRunning {
		return false
	}
	state, err := m.IsActive(ctx)
	return err == nil && state == "active"
}

// reloginService stops the service, signs the node in again from the
// foreground (a fresh link, so the key expiry starts over), and starts the
// service again whether or not the sign-in worked.
func reloginService(ctx context.Context, stateDir, hostname string, m service.Manager, stdout io.Writer) (err error) {
	fmt.Fprintf(stdout, "Stopping %s to sign it in again…\n", service.Name)
	if err := m.Stop(ctx); err != nil {
		return err
	}
	defer func() {
		// The caller's context may be cancelled (Ctrl-C), but the service
		// still has to come back.
		startErr := m.Start(context.WithoutCancel(ctx))
		if startErr == nil {
			fmt.Fprintf(stdout, "Started %s again.\n", service.Name)
		}
		err = errors.Join(err, startErr)
	}()
	unlock, err := tailnet.Lock(stateDir)
	if err != nil {
		return fmt.Errorf("after stopping the service: %w", err)
	}
	defer unlock()
	return startNodeForLogin(ctx, stateDir, hostname, stdout)
}

// waitForSignIn follows the running server's status.json: it prints the
// sign-in link once (again if it changes) and returns when the server
// reports running. The server can't be signed in again while it runs, so
// for an already signed-in server it says how: stop the service and run
// again, the login command to show.
func waitForSignIn(ctx context.Context, stateDir, again string, stdout io.Writer) error {
	tick := time.NewTicker(loginPoll)
	defer tick.Stop()
	shown, first, waiting := "", true, false
	since := time.Now()
	for {
		info, err := probeServer(stateDir)
		if err != nil {
			return err
		}
		if !info.running {
			return errors.New("herdr-pocket stopped before it was signed in")
		}
		if st := info.status; st != nil {
			switch st.State {
			case tailnet.StateRunning:
				if first {
					fmt.Fprintf(stdout, "herdr-pocket is already signed in as %s\n", appURL(st.FQDN))
					fmt.Fprintf(stdout, "To sign in again, stop it with `systemctl --user stop %s`, run `%s`, then start it again.\n", service.Name, again)
					return nil
				}
				fmt.Fprintf(stdout, "Signed in as %s\n", appURL(st.FQDN))
				return printPhoneSetup(stdout, appURL(st.FQDN))
			case tailnet.StateNeedsLogin:
				if st.AuthURL != "" && st.AuthURL != shown {
					fmt.Fprintf(stdout, "Open this link to add herdr-pocket to your tailnet: %s\n", st.AuthURL)
					shown = st.AuthURL
				} else if st.AuthURL == "" && !waiting && time.Since(since) > linkWait {
					// A starting node gets its link in a moment; a running
					// one whose key lapsed may never get one.
					fmt.Fprintf(stdout, "herdr-pocket needs signing in; waiting for its link. If none appears, stop it with `systemctl --user stop %s` and run `%s`.\n", service.Name, again)
					waiting = true
				}
			}
		}
		first = false
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// loginCommand is the login command line for a state dir and hostname, each
// left out when empty (the default).
func loginCommand(stateDir, hostname string) string {
	cmd := "herdr-pocket login"
	if stateDir != "" {
		cmd += " --state-dir " + shellQuote(stateDir)
	}
	if hostname != "" {
		cmd += " --hostname " + shellQuote(hostname)
	}
	return cmd
}

// flagIfSet is the value of a flag given on the command line, or "".
func flagIfSet(fs *flag.FlagSet, name string) string {
	v := ""
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			v = f.Value.String()
		}
	})
	return v
}

// shellQuote quotes s for a POSIX shell when it needs it.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:@%+=,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
