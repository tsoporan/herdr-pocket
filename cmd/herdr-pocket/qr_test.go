package main

import (
	"os"
	"strings"
	"testing"

	"rsc.io/qr"

	"github.com/tsoporan/herdr-pocket/internal/tailnet"
)

// TestRenderQRMatchesCode reads the half blocks back into modules and checks
// every one against the encoder's bitmap, quiet zone included.
func TestRenderQRMatchesCode(t *testing.T) {
	const url = "https://herdr-pocket.example-tailnet.ts.net"
	out, err := renderQR(url)
	if err != nil {
		t.Fatal(err)
	}
	code, err := qr.Encode(url, qr.M)
	if err != nil {
		t.Fatal(err)
	}
	side := code.Size + 2*qrQuiet
	rows := strings.Split(out, "\n")
	if len(rows) != (side+1)/2 {
		t.Fatalf("got %d rows, want %d", len(rows), (side+1)/2)
	}
	for row, line := range rows {
		cells := []rune(line)
		if len(cells) != side {
			t.Fatalf("row %d has %d cells, want %d", row, len(cells), side)
		}
		for x, r := range cells {
			top := r == '█' || r == '▀'
			bottom := r == '█' || r == '▄'
			for i, gotLight := range []bool{top, bottom} {
				y := 2*row + i
				if y >= side {
					continue
				}
				mx, my := x-qrQuiet, y-qrQuiet
				wantLight := mx < 0 || my < 0 || mx >= code.Size || my >= code.Size || !code.Black(mx, my)
				if gotLight != wantLight {
					t.Fatalf("module (%d,%d): light=%v, want %v", x, y, gotLight, wantLight)
				}
			}
		}
	}
}

func TestQRCommand(t *testing.T) {
	stateDir := stateWith(t, tailnet.StatusFile{State: "running", FQDN: "herdr-pocket.example-tailnet.ts.net.", PID: os.Getpid()}, false)
	out, err := run(t, "qr", "--state-dir", stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"█", "https://herdr-pocket.example-tailnet.ts.net\n", "Install Tailscale"} {
		if !strings.Contains(out, want) {
			t.Errorf("qr output lacks %q:\n%s", want, out)
		}
	}
}

func TestQRCommandNotSignedIn(t *testing.T) {
	_, err := run(t, "qr", "--state-dir", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "herdr-pocket login") {
		t.Errorf("qr with no status = %v, want a hint to log in", err)
	}
}
