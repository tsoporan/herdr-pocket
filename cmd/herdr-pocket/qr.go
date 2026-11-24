package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"rsc.io/qr"

	"github.com/tsoporan/herdr-pocket/internal/tailnet"
)

// qrQuiet is the light border around the code, in modules. The standard
// asks for 4; 2 scans fine on a terminal and keeps the code small.
const qrQuiet = 2

func runQR(_ context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("qr", flag.ContinueOnError)
	stateDir := fs.String("state-dir", tailnet.DefaultStateDir(), "directory for the tailnet node, status and audit log")
	fs.Usage = func() { fmt.Fprintln(fs.Output(), "usage: herdr-pocket qr [--state-dir DIR]") }
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	// status.json keeps the node's name after the server stops, and the name
	// only changes if the node is renamed, so a stale file still has it.
	st, err := tailnet.ReadStatus(*stateDir)
	if err != nil || st.FQDN == "" {
		return errors.New("no address yet: sign in first with `herdr-pocket login`")
	}
	if st.HTTPSOff {
		fmt.Fprintln(stdout, tailnet.HTTPSHint)
	}
	return printPhoneSetup(stdout, appURL(st.FQDN))
}

// printPhoneSetup prints the app's address as a QR code with the steps to
// open it on a phone. The code holds only the address, which is no secret:
// a phone off the tailnet can't reach it.
func printPhoneSetup(w io.Writer, url string) error {
	code, err := renderQR(url)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "\n%s\n%s\n\n", code, url)
	fmt.Fprintln(w, "On your phone:")
	fmt.Fprintln(w, "  1. Install Tailscale and sign in as yourself (once per phone).")
	fmt.Fprintln(w, "  2. Scan this code with the camera.")
	fmt.Fprintln(w, "  3. Add the page to the home screen.")
	return nil
}

// renderQR draws text as a QR code in half-block characters, two modules
// per character cell. Light modules are drawn and dark ones left blank, so
// the code reads right on a dark terminal, as `tailscale up --qr` does.
func renderQR(text string) (string, error) {
	c, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	light := func(x, y int) bool {
		x, y = x-qrQuiet, y-qrQuiet
		return x < 0 || y < 0 || x >= c.Size || y >= c.Size || !c.Black(x, y)
	}
	side := c.Size + 2*qrQuiet
	var b strings.Builder
	for y := 0; y < side; y += 2 {
		for x := 0; x < side; x++ {
			top, bottom := light(x, y), y+1 < side && light(x, y+1)
			switch {
			case top && bottom:
				b.WriteRune('█')
			case top:
				b.WriteRune('▀')
			case bottom:
				b.WriteRune('▄')
			default:
				b.WriteRune(' ')
			}
		}
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}
