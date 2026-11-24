package hub

import (
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tsoporan/herdr-pocket/internal/herdr"
	"github.com/tsoporan/herdr-pocket/internal/transcript"
)

// The detail carries the transcript's message and its asks, worked out
// before the bound (transcript.Asks).
func TestDetailLastMessageAndAsks(t *testing.T) {
	a := herdr.Agent{PaneID: "w9:p1", Agent: "claude", AgentStatus: herdr.StatusDone}
	d := buildDetail(a, time.Now(), &transcript.Snapshot{}, nil, "", "", time.Now())
	if d.LastMessage != nil || d.Asks {
		t.Errorf("no message: last_message %v asks %v", d.LastMessage, d.Asks)
	}
	snap := &transcript.Snapshot{LastMessage: "Want me to open a PR?", LastAsks: true}
	d = buildDetail(a, time.Now(), snap, nil, "", "", time.Now())
	if d.LastMessage == nil || *d.LastMessage != snap.LastMessage || !d.Asks {
		t.Errorf("last_message %v asks %v", d.LastMessage, d.Asks)
	}
}

func screenFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../fakeherdr/testdata/screens/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTerminalLastLines(t *testing.T) {
	cases := []struct {
		name, screen, want string
	}{
		{"done", screenFixture(t, "done.txt"),
			"⏺ Done. The flaky checkout test waited on a fixed timeout; it now waits for\n" +
				"the order-confirmed event instead. 20 runs in a row pass. PR #48 is ready."},
		{"idle", screenFixture(t, "idle.txt"), "│ ✻ Welcome to Claude Code!                                    │"},
		{"shell", "$ make test\nok  ./...\n\n\n\nPASS\n─────\n$ \n", "$ make test\nok  ./...\n\nPASS"}, // the bare "$" prompt goes
		{"empty", "\n\n────\n>\n────\n", ""},
		// A quote or prompt under a rule in the output, not at the bottom, stays.
		{"quote", "Notes\n────\n> keep the old API\none\ntwo\nthree\nfour\nfive\nsix\n\n\n",
			"Notes\n> keep the old API\none\ntwo\nthree\nfour\nfive\nsix"},
	}
	for _, c := range cases {
		if got := terminalLastLines(c.screen); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestTerminalLastLinesKeepsTheLastForty(t *testing.T) {
	var b strings.Builder
	for i := range 100 {
		b.WriteString("line " + string(rune('a'+i%26)) + "\n")
		if i == 59 {
			b.WriteString("\n") // a blank line just above the kept 40
		}
	}
	lines := strings.Split(terminalLastLines(b.String()), "\n")
	if len(lines) != lastLinesMax || lines[0] != "line i" || lines[len(lines)-1] != "line v" {
		t.Errorf("%d lines, %q … %q", len(lines), lines[0], lines[len(lines)-1])
	}
}

// Over MaxMessage characters, the newest lines are kept and the oldest go.
func TestTerminalLastLinesKeepsTheNewest(t *testing.T) {
	var b strings.Builder
	for i := range lastLinesMax {
		b.WriteString(strings.Repeat(string(rune('a'+i%26)), 300) + "\n")
	}
	got := terminalLastLines(b.String())
	lines := strings.Split(got, "\n")
	if n := utf8.RuneCountInString(got); n > transcript.MaxMessage || lines[0] != "…" ||
		lines[len(lines)-1] != strings.Repeat("n", 300) || len(lines) != 27 {
		t.Errorf("%d characters, %d lines, ends %q", n, len(lines), lines[len(lines)-1][:3])
	}

	one := "start " + strings.Repeat("x", 9000) + " end"
	got = terminalLastLines("older\n" + one)
	if n := utf8.RuneCountInString(got); n > transcript.MaxMessage || !strings.HasPrefix(got, "…x") || !strings.HasSuffix(got, " end") {
		t.Errorf("one long line: %d characters", n)
	}
}

// A terminal agent's last message comes from the recent read, else the
// screen, and never asks.
func TestTerminalDetailLastMessage(t *testing.T) {
	a := herdr.Agent{PaneID: "w9:p2", AgentStatus: herdr.StatusIdle}
	d := buildDetail(a, time.Now(), nil, nil, "visible line?", "recent line?", time.Now())
	if d.LastMessage == nil || *d.LastMessage != "recent line?" || d.Asks {
		t.Errorf("last_message %v asks %v", d.LastMessage, d.Asks)
	}
	d = buildDetail(a, time.Now(), nil, nil, "visible line", "", time.Now())
	if d.LastMessage == nil || *d.LastMessage != "visible line" {
		t.Errorf("fallback last_message %v", d.LastMessage)
	}
	if d = buildDetail(a, time.Now(), nil, nil, "", "", time.Now()); d.LastMessage != nil {
		t.Errorf("empty screen: last_message %q", *d.LastMessage)
	}
}
