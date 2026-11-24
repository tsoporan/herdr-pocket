package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/tailnet"
)

// logoutNode opens the node in <state>/tsnet and logs it out of the
// tailnet through its LocalClient.
// Tests replace it, so they never start a node.
var logoutNode = func(ctx context.Context, stateDir string) error {
	node := tailnet.NewNode(tailnet.Config{StateDir: stateDir})
	return errors.Join(node.Logout(ctx), node.Close())
}

func runLogout(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	stateDir := fs.String("state-dir", tailnet.DefaultStateDir(), "directory for the tailnet node, status and audit log")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	nodeDir := tailnet.TsnetDir(*stateDir)
	if !signedIn(*stateDir) {
		fmt.Fprintf(stdout, "Not signed in (no %s).\n", nodeDir)
		return nil
	}
	unlock, err := tailnet.Lock(*stateDir)
	if errors.Is(err, tailnet.ErrLocked) {
		return errors.New("herdr-pocket is running; stop it first (systemctl --user stop herdr-pocket), then log out")
	}
	if err != nil {
		return err
	}
	defer unlock()
	if err := logoutNode(ctx, *stateDir); err != nil {
		return fmt.Errorf("log out of the tailnet: %w", err)
	}
	// The node's keys are moved aside, not deleted, so a mistaken logout can
	// be inspected; the user deletes the backup when sure.
	backup := nodeDir + ".bak-" + time.Now().Format("20060102-150405")
	if err := os.Rename(nodeDir, backup); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Logged out: herdr-pocket left the tailnet. Its old node state is in %s; delete it when you're sure.\n", backup)
	return nil
}
