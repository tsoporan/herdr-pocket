package transcript

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const fixtures = "../fakeherdr/testdata/claude"

func sid(n string) string { return "0f3c9a2e-7b41-4d6a-9e10-00000000000" + n }

func readFixture(t *testing.T, n string) Snapshot {
	t.Helper()
	snap, err := NewReader(fixtures).Read(sid(n))
	if err != nil {
		t.Fatalf("Read(%s): %v", n, err)
	}
	return snap
}

func stepsOf(snap Snapshot) []Step {
	out := make([]Step, len(snap.Steps))
	for i, s := range snap.Steps {
		s.At = time.Time{}
		out[i] = s
	}
	return out
}

func TestRateLimitMatchesMockup(t *testing.T) {
	snap := readFixture(t, "2")
	want := []Step{
		{KindYou, "Add a token-bucket rate limit to every /v2 route, 100 requests a minute per key", "You", time.Time{}},
		{KindOK, "Read index.ts", "src/routes/v2/index.ts", time.Time{}},
		{KindOK, "Created limit.ts", "src/middleware/limit.ts · 58 lines", time.Time{}},
		{KindOK, "Changed index.ts", "src/routes/v2/index.ts · 1 line changed", time.Time{}},
		{KindOK, "Ran the unit tests: 18 passed", "0.4 s", time.Time{}},
		{KindNow, "Running the integration tests", "", time.Time{}},
	}
	if got := stepsOf(snap); !slices.Equal(got, want) {
		t.Errorf("steps:\n got %+v\nwant %+v", got, want)
	}
	if want := "Running the integration tests for the v2 routes, to check the new limit doesn't break existing clients."; snap.Now != want {
		t.Errorf("Now = %q", snap.Now)
	}
	if snap.Pending == nil || snap.Pending.Command != "bun test tests/integration/v2" {
		t.Fatalf("Pending = %+v", snap.Pending)
	}
	if got, want := snap.TurnStart, time.Date(2026, 11, 21, 9, 10, 1, 0, time.UTC); !got.Equal(want) {
		t.Errorf("TurnStart = %v, want %v", got, want)
	}
	if !snap.Pending.At.Equal(snap.LastAssistant) {
		t.Errorf("pending at %v, last assistant %v", snap.Pending.At, snap.LastAssistant)
	}
}

func TestRefactorAuthPendingBash(t *testing.T) {
	snap := readFixture(t, "1")
	p := snap.Pending
	if p == nil {
		t.Fatal("no pending call")
	}
	led := "The auth tests pass (212 of 212). It wants to rebuild the bundle to check its size, which deletes the old build first."
	if p.Tool != "Bash" || p.Command != "rm -rf dist && bun run build" || p.Cwd != "/home/dev/src/api" ||
		p.Description != "Rebuild the bundle to check its size" || p.LedHere != led {
		t.Errorf("pending = %+v", p)
	}
	if ShortDir(p.Cwd) != "~/api" {
		t.Errorf("ShortDir = %q", ShortDir(p.Cwd))
	}
	if snap.Now != led {
		t.Errorf("Now = %q", snap.Now)
	}
	if p.At.IsZero() {
		t.Error("pending At is zero")
	}
	last := snap.Steps[len(snap.Steps)-2]
	if last.Text != "Ran the auth tests: 212 passed" {
		t.Errorf("test step = %+v", last)
	}
}

func TestPostgresPendingQuestion(t *testing.T) {
	p := readFixture(t, "4").Pending
	if p == nil || p.Tool != "AskUserQuestion" || p.Question != "staging or dev first?" {
		t.Fatalf("pending = %+v", p)
	}
	if labels := []string{p.Options[0].Label, p.Options[1].Label}; !slices.Equal(labels, []string{"Staging", "Dev"}) {
		t.Errorf("options = %+v", p.Options)
	}
}

func TestPendingEditPath(t *testing.T) {
	snap := readFixture(t, "8")
	if snap.Pending == nil || snap.Pending.Path != "/home/dev/src/docs/api/auth.md" {
		t.Fatalf("pending = %+v", snap.Pending)
	}
	if last := snap.Steps[len(snap.Steps)-1]; last.Kind != KindNow || last.Text != "Changing auth.md" {
		t.Errorf("now step = %+v", last)
	}
}

func TestFinishedWithPR(t *testing.T) {
	snap := readFixture(t, "6")
	if snap.Pending != nil {
		t.Errorf("pending = %+v", snap.Pending)
	}
	if snap.PRNumber != 48 {
		t.Errorf("PRNumber = %d", snap.PRNumber)
	}
	if !strings.HasPrefix(snap.Now, "PR #48 is ready") || strings.Contains(snap.Now, "race") {
		t.Errorf("Now = %q", snap.Now)
	}
	// The last message is the whole text, markdown and all.
	m := snap.LastMessage
	if !strings.HasPrefix(m, "PR #48 is ready: ") || !strings.Contains(m, "explains the race.\n\n## What changed\n\n- `tests/checkout.spec.ts`") ||
		!strings.HasSuffix(m, "on this branch?") {
		t.Errorf("LastMessage = %q", m)
	}
	for _, s := range snap.Steps {
		if s.Kind == KindNow {
			t.Errorf("step %+v still running", s)
		}
	}
}

func TestToolErrorIsFail(t *testing.T) {
	snap := readFixture(t, "5")
	var fail *Step
	for i := range snap.Steps {
		if snap.Steps[i].Kind == KindFail {
			fail = &snap.Steps[i]
		}
	}
	if fail == nil || fail.Text != "Ran the worker with the inspector" || fail.Meta != "Exit code 1" {
		t.Errorf("fail step = %+v", fail)
	}
	if snap.Steps[1].Text != "Searched for setInterval" || snap.Steps[1].Meta != "worker" {
		t.Errorf("grep step = %+v", snap.Steps[1])
	}
}

func TestHelperAgentAndSidechain(t *testing.T) {
	snap := readFixture(t, "7")
	if last := snap.Steps[len(snap.Steps)-1]; last.Text != "Asking a helper agent to find hard-coded colours" {
		t.Errorf("now step = %+v", last)
	}
	idle := readFixture(t, "9")
	if strings.Contains(idle.Now, "Sidechain") || !strings.HasPrefix(idle.Now, "The onboarding guide") {
		t.Errorf("Now = %q", idle.Now)
	}
	if idle.Steps[0].Kind != KindYou || len(idle.Steps) != 2 {
		t.Errorf("steps = %+v (meta prompt must be skipped)", idle.Steps)
	}
}

func TestAllFixturesRead(t *testing.T) {
	r := NewReader(fixtures)
	for i := 1; i <= 9; i++ {
		snap, err := r.Read(sid(string(rune('0' + i))))
		if err != nil || snap.Now == "" || len(snap.Steps) == 0 || snap.TurnStart.IsZero() {
			t.Errorf("fixture %d: err=%v snap=%+v", i, err, snap)
		}
	}
}

// writeSession puts lines in a temp config dir and returns a reader and the
// file path.
func writeSession(t *testing.T, id, content string) (*Reader, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "projects", "-home-dev-src-x", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return NewReader(filepath.Join(dir, "missing"), dir), p
}

const (
	lPrompt = `{"type":"user","timestamp":"2026-11-21T10:00:00.000Z","cwd":"/home/dev/src/x","message":{"role":"user","content":"Do the thing"}}` + "\n"
	lBash   = `{"type":"assistant","timestamp":"2026-11-21T10:00:05.000Z","cwd":"/home/dev/src/x","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"make","description":"Build it"}}],"stop_reason":"tool_use"}}` + "\n"
	lResult = `{"type":"user","timestamp":"2026-11-21T10:00:17.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"done","is_error":false}]},"toolUseResult":{"stdout":"done"}}` + "\n"
	lText   = `{"type":"assistant","timestamp":"2026-11-21T10:00:20.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Built it. Nothing broke. Also, more."}],"stop_reason":"end_turn"}}` + "\n"
)

func TestIncrementalAndTruncatedLastLine(t *testing.T) {
	partial := lResult[:40]
	r, p := writeSession(t, "s1", lPrompt+lBash+partial)
	snap, err := r.Read("s1")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Pending == nil || snap.Pending.Command != "make" || snap.Steps[1].Kind != KindNow {
		t.Fatalf("before: %+v", snap)
	}
	// The writer finishes the line and appends more.
	fh, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString(lResult[40:] + lText); err != nil {
		t.Fatal(err)
	}
	fh.Close()
	snap, err = r.Read("s1")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Pending != nil {
		t.Errorf("pending = %+v", snap.Pending)
	}
	if got := snap.Steps[1]; got.Kind != KindOK || got.Text != "Ran build it" || got.Meta != "12 s" {
		t.Errorf("step = %+v", got)
	}
	if snap.Now != "Built it. Nothing broke." {
		t.Errorf("Now = %q", snap.Now)
	}
	if !snap.LastAssistant.Equal(time.Date(2026, 11, 21, 10, 0, 20, 0, time.UTC)) {
		t.Errorf("LastAssistant = %v", snap.LastAssistant)
	}
}

func TestRewrittenFileStartsOver(t *testing.T) {
	r, p := writeSession(t, "s2", lPrompt+lBash+lResult+lText)
	if _, err := r.Read("s2"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(lPrompt), 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := r.Read("s2")
	if err != nil || len(snap.Steps) != 1 || snap.Now != "" {
		t.Errorf("after rewrite: err=%v %+v", err, snap)
	}
}

func TestUnknownAndMalformedLinesSkipped(t *testing.T) {
	junk := `{"type":"brand-new-type","whatever":[1,2]}` + "\n" +
		"not json at all\n" +
		`{"type":"assistant","message":{"content":42}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"nope"}]}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"mystery_block"}]}}` + "\n"
	r, _ := writeSession(t, "s3", junk+lPrompt+junk+lBash+lResult+lText)
	snap, err := r.Read("s3")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Steps) != 2 || snap.Steps[1].Kind != KindOK {
		t.Errorf("steps = %+v", snap.Steps)
	}
}

func TestOversizedLineSkipped(t *testing.T) {
	huge := `{"type":"user","message":{"content":"` + strings.Repeat("x", maxLine) + `"}}` + "\n"
	r, p := writeSession(t, "s9", lPrompt+huge[:maxLine+10])
	if snap, err := r.Read("s9"); err != nil || len(snap.Steps) != 1 {
		t.Fatalf("partial oversized line: err=%v %+v", err, snap)
	}
	fh, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString(huge[maxLine+10:] + lBash + lResult + lText); err != nil {
		t.Fatal(err)
	}
	fh.Close()
	snap, err := r.Read("s9")
	if err != nil || len(snap.Steps) != 2 || snap.Now != "Built it. Nothing broke." {
		t.Errorf("after oversized line: err=%v %+v", err, snap)
	}
}

func TestInterruptedCallFailsOnNextPrompt(t *testing.T) {
	r, _ := writeSession(t, "s4", lPrompt+lBash+strings.Replace(lPrompt, "Do the thing", "Stop, do this instead", 1))
	snap, err := r.Read("s4")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Pending != nil || snap.Steps[1].Kind != KindFail || snap.Steps[2].Text != "Stop, do this instead" {
		t.Errorf("snap = %+v", snap)
	}
}

func TestCompactSummaryIsNotAPrompt(t *testing.T) {
	summary := `{"type":"user","isCompactSummary":true,"isVisibleInTranscriptOnly":true,"timestamp":"2026-11-21T10:01:00.000Z","message":{"role":"user","content":"This session is being continued."}}` + "\n"
	queued := `{"type":"user","promptSource":"queued","timestamp":"2026-11-21T10:02:00.000Z","message":{"role":"user","content":"Then tidy up"}}` + "\n"
	r, _ := writeSession(t, "s7", lPrompt+summary+queued)
	snap, err := r.Read("s7")
	if err != nil || len(snap.Steps) != 2 || snap.Steps[1].Text != "Then tidy up" {
		t.Errorf("err=%v steps=%+v", err, snap.Steps)
	}
}

func TestErrors(t *testing.T) {
	r, _ := writeSession(t, "s5", "garbage\n{}\n")
	if _, err := r.Read("s5"); !errors.Is(err, ErrUnusable) {
		t.Errorf("unusable: %v", err)
	}
	if _, err := r.Read("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	for _, bad := range []string{"", "../x", "a/b", "*", ".hidden"} {
		if _, err := r.Read(bad); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Read(%q) = %v, want a bad id error", bad, err)
		}
	}
}

func TestStepsBounded(t *testing.T) {
	var b strings.Builder
	for range MaxSteps + 20 {
		b.WriteString(lPrompt)
	}
	r, _ := writeSession(t, "s6", b.String())
	snap, err := r.Read("s6")
	if err != nil || len(snap.Steps) != MaxSteps {
		t.Errorf("err=%v steps=%d", err, len(snap.Steps))
	}
}

func TestSummarize(t *testing.T) {
	for in, want := range map[string]string{
		"The auth tests pass (212 of 212). It wants more. A third.": "The auth tests pass (212 of 212). It wants more.",
		"Took 0.4 s on /v2, e.g. fine. Next one. Third.":            "Took 0.4 s on /v2, e.g. fine. Next one.",
		"**Done.** See `x.go`.\n\nMore detail here.":                "Done. See x.go.",
		"## Summary\nAll good":                                      "Summary All good",
		"":                                                          "",
	} {
		if got := summarize(in); got != want {
			t.Errorf("summarize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTestCounts(t *testing.T) {
	for in, want := range map[string]string{
		" 18 pass\n 0 fail":           "18 passed",
		"  20 passed (41.2s)":         "20 passed",
		"Tests  16 passed | 2 failed": "16 passed, 2 failed",
		"installed 6 packages":        "",
	} {
		if got := testCounts(in); got != want {
			t.Errorf("testCounts(%q) = %q, want %q", in, got, want)
		}
	}
}

func FuzzDecodeLine(f *testing.F) {
	for _, s := range []string{lPrompt, lBash, lResult, lText, `{"type":"pr-link","prNumber":3}`, "{", `{"type":"user","message":{"content":[{"type":"text"}]}}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		rec, ok := decodeLine(line)
		if !ok {
			return
		}
		s := newState()
		s.apply(rec)
		s.apply(rec)
		_ = s.snapshot()
	})
}

func TestDefaultConfigDirsDedupesSymlinkedProjects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	main := filepath.Join(home, ".claude")
	work := filepath.Join(home, ".claude-work")
	extra := filepath.Join(home, "other")
	for _, d := range []string{filepath.Join(main, "projects"), work, filepath.Join(extra, "projects")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(main, "projects"), filepath.Join(work, "projects")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", work)

	got := DefaultConfigDirs(extra, main)
	want := []string{main, extra}
	if !slices.Equal(got, want) {
		t.Fatalf("DefaultConfigDirs = %v, want %v", got, want)
	}
}

// assistantText is an assistant record with one text block.
func assistantText(text string) string {
	b, err := json.Marshal(text)
	if err != nil {
		panic(err)
	}
	return `{"type":"assistant","timestamp":"2026-11-21T10:00:30.000Z","message":{"role":"assistant","content":[{"type":"text","text":` +
		string(b) + `}],"stop_reason":"end_turn"}}` + "\n"
}

func TestLastMessageUnclipped(t *testing.T) {
	text := "Built it.\n\n- one\n- two\n\nThe second paragraph is long: " + strings.Repeat("word ", 200) + "end."
	r, _ := writeSession(t, "s10", lPrompt+lBash+lResult+lText+assistantText("\n  "+text+"\n\n"))
	snap, err := r.Read("s10")
	if err != nil {
		t.Fatal(err)
	}
	if snap.LastMessage != text {
		t.Errorf("LastMessage = %q, want the trimmed text", snap.LastMessage)
	}
	if snap.Now != "Built it." {
		t.Errorf("Now = %q", snap.Now)
	}
}

func TestLastMessageClearedByNewPrompt(t *testing.T) {
	next := strings.Replace(lPrompt, "Do the thing", "Now the other thing", 1)
	r, _ := writeSession(t, "s11", lPrompt+lText+next)
	snap, err := r.Read("s11")
	if err != nil {
		t.Fatal(err)
	}
	if snap.LastMessage != "" {
		t.Errorf("LastMessage = %q after a new prompt", snap.LastMessage)
	}
}

func TestBoundMessage(t *testing.T) {
	line := strings.Repeat("é", 99) + "\n" // 100 characters, 199 bytes
	long := strings.Repeat(line, 100)      // 10,000 characters
	got := BoundMessage(long)
	if n := utf8.RuneCountInString(got); n > MaxMessage {
		t.Errorf("%d characters, want at most %d", n, MaxMessage)
	}
	// Whole lines from the start, then the last line: 7,999 runes are left
	// after the tail and the "…" line, so 78 whole lines fit.
	if want := strings.Repeat(line, 78) + "…\n\n" + strings.TrimSuffix(line, "\n"); got != want {
		t.Errorf("cut at %d characters, want %d", utf8.RuneCountInString(got), utf8.RuneCountInString(want))
	}

	oneLine := strings.Repeat("ab", 5000)
	got = BoundMessage(oneLine)
	head, tail, ok := strings.Cut(got, "\n…\n\n")
	if n := utf8.RuneCountInString(got); n > MaxMessage || !ok || !strings.HasPrefix(oneLine, head) || !strings.HasSuffix(oneLine, tail) || len(tail) != maxTail {
		t.Errorf("one line: %d characters, %d kept from the end", n, len(tail))
	}

	// The final paragraph, with its question, survives the cut.
	asking := strings.Repeat("A line of the long message.\n", 400) + "\nShall I go on?\n"
	if got := BoundMessage(asking); utf8.RuneCountInString(got) > MaxMessage || !strings.HasSuffix(got, "message.\n…\n\nShall I go on?") {
		t.Errorf("asking: ends %q", got[len(got)-40:])
	}

	r, _ := writeSession(t, "s12", lPrompt+assistantText(long))
	if snap, err := r.Read("s12"); err != nil || snap.LastMessage != BoundMessage(strings.TrimSpace(long)) {
		t.Errorf("read: err=%v, %d characters", err, utf8.RuneCountInString(snap.LastMessage))
	}

	exact := strings.Repeat("x", MaxMessage)
	if BoundMessage(exact) != exact {
		t.Error("a message of exactly MaxMessage characters was cut")
	}
}

func TestAsks(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"", false},
		{"Done. PR #7 is open.", false},
		{"The tests pass.\n\nWant me to open a PR?", true},
		{"Want me to open a PR?  \n", true},
		{"Is it ready? I think so.", false},
		{"Which one?\n\n- `a`\n- `b`", false},
		{"**Want me to push?**", true},
		{"_Push it?_", true},
		{"Fix the other two (on this branch?)", true},
		{"He said \"ship it?\"", true},
		{"Shall I merge it? 🙂", true},
		{"Ship it? 👍🏽", true},
		{"Run `make check?`", false},
		{"Like this:\n```\nok?\n```", false},
	}
	for _, c := range cases {
		if got := Asks(c.text); got != c.want {
			t.Errorf("Asks(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

// A message too long to keep whole still asks when it ends in a question.
func TestLastAsksBeforeTheBound(t *testing.T) {
	long := strings.Repeat("A line of the long message.\n", 400) + "\nShall I go on?"
	r, _ := writeSession(t, "s13", lPrompt+assistantText(long))
	snap, err := r.Read("s13")
	if err != nil {
		t.Fatal(err)
	}
	if !snap.LastAsks || !strings.HasSuffix(snap.LastMessage, "Shall I go on?") {
		t.Errorf("LastAsks %v, ends %q", snap.LastAsks, snap.LastMessage[len(snap.LastMessage)-20:])
	}
	next := strings.Replace(lPrompt, "Do the thing", "Now the other thing", 1)
	r, _ = writeSession(t, "s14", lPrompt+assistantText("Go on?")+next)
	if snap, err := r.Read("s14"); err != nil || snap.LastAsks {
		t.Errorf("LastAsks %v after a new prompt (err %v)", snap.LastAsks, err)
	}
}
