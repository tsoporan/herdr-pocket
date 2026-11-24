package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/tsoporan/herdr-pocket/internal/service"
)

// runUninstall stops and removes the unit. The node's state stays, so a
// reinstall is still signed in; logout removes the node.
func runUninstall(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	manager := serviceFlags(fs, "herdr-pocket uninstall")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	m, err := manager()
	if err != nil {
		return err
	}
	switch err := m.Uninstall(ctx); {
	case errors.Is(err, service.ErrNotInstalled):
		fmt.Fprintf(stdout, "herdr-pocket is not installed (no %s).\n", m.Path())
		return nil
	case errors.Is(err, service.ErrForeignUnit):
		return fmt.Errorf("%w; leaving it alone", err)
	case err != nil:
		return err
	}
	fmt.Fprintf(stdout, "Stopped %s and removed %s.\n", service.Name, m.Path())
	fmt.Fprintln(stdout, "The node is still in your tailnet; `herdr-pocket logout` removes it.")
	return nil
}
