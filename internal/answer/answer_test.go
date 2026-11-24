package answer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fixture reads a fake-herdr screen, or one of this package's testdata
// files when name starts with "testdata/".
func fixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "fakeherdr", "testdata", "screens", name)
	if strings.HasPrefix(name, "testdata/") {
		path = name
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func ptr(s string) *string { return &s }

var (
	allowOnce  = Option{Key: "1", Label: "Allow once", Sub: "Runs this command now", Primary: true}
	dontAllow  = Option{Key: "3", Label: "Don’t allow", Sub: "Then tell it what to do instead", NeedsNote: true}
	dontAllow2 = Option{Key: "2", Label: "Don’t allow", Sub: "Then tell it what to do instead", NeedsNote: true}
	alwaysRmRf = Option{Key: "2", Label: "Always allow", Sub: "Won’t ask again for rm -rf dist in this project"}
	ownAnswer3 = Option{Key: "3", Label: "Your own answer", NeedsNote: true, Own: true}
)

func TestBuildFixtures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		screen  string
		pending Pending
		want    Question // ID is checked separately
	}{
		{
			name:   "bash permission, screen only",
			screen: "w1-p1.blocked.txt",
			want: Question{Kind: KindPermission, Title: "Allow this command?", Command: ptr("rm -rf dist && bun run build"),
				Options: []Option{allowOnce, alwaysRmRf, dontAllow}},
		},
		{
			name:   "older boxed bash permission",
			screen: "testdata/legacy-box.txt",
			want: Question{Kind: KindPermission, Title: "Allow this command?", Command: ptr("rm -rf dist && bun run build"),
				Options: []Option{allowOnce, alwaysRmRf, dontAllow}},
		},
		{
			name:   "bash permission with the transcript",
			screen: "w1-p1.blocked.txt",
			pending: Pending{Command: "rm -rf dist && bun run build", Description: "Clean the old bundle and rebuild",
				Cwd: "~/api", LedHere: "The auth tests pass (212 of 212). It wants to rebuild the bundle."},
			want: Question{Kind: KindPermission, Title: "Allow this command?", Where: "~/api",
				Command: ptr("rm -rf dist && bun run build"),
				LedHere: ptr("The auth tests pass (212 of 212). It wants to rebuild the bundle."),
				Options: []Option{allowOnce, alwaysRmRf, dontAllow}},
		},
		{
			name:    "description stands in for led_here",
			screen:  "w1-p2.blocked.txt",
			pending: Pending{Description: "Run the rate limiter tests", Cwd: "~/api"},
			want: Question{Kind: KindPermission, Title: "Allow this command?", Where: "~/api",
				Command: ptr("bun test test/ratelimit.test.ts --timeout 20000 --rerun-each 3 && bun run lint\n--max-warnings 0"),
				LedHere: ptr("Run the rate limiter tests"),
				Options: []Option{allowOnce,
					{Key: "2", Label: "Always allow", Sub: "Won’t ask again for bun test in this project"}, dontAllow}},
		},
		{
			name:    "AskUserQuestion",
			screen:  "w2-p1.blocked.txt",
			pending: Pending{Cwd: "~/infra"},
			want: Question{Kind: KindAsk, Title: "Which database should I migrate to Postgres 18 first?", Where: "~/infra",
				Steps: []Step{{Header: "Environment"}},
				Options: []Option{
					{Key: "1", Label: "staging", Sub: "Shared staging cluster; a full rehearsal with real-sized data"},
					{Key: "2", Label: "dev", Sub: "Local dev database only; fast, but small data"},
					ownAnswer3,
				}},
		},
		{
			name:   "first of two questions",
			screen: "testdata/ask-first.txt",
			want: Question{Kind: KindAsk, Title: "Which fruit?",
				Steps:   []Step{{Header: "Fruit"}, {Header: "Drink"}},
				Options: []Option{{Key: "1", Label: "Apple", Sub: "Red"}, {Key: "2", Label: "Banana", Sub: "Yellow"}, ownAnswer3}},
		},
		{
			name:   "second of two questions",
			screen: "testdata/ask-second.txt",
			want: Question{Kind: KindAsk, Title: "Which drink?",
				Steps:   []Step{{Header: "Fruit", Done: true}, {Header: "Drink"}},
				Options: []Option{{Key: "1", Label: "Tea", Sub: "Hot"}, {Key: "2", Label: "Juice", Sub: "Cold"}, ownAnswer3}},
		},
		{
			name:   "review step",
			screen: "testdata/ask-review.txt",
			want: Question{Kind: KindAsk, Title: "Send these answers?",
				Steps:  []Step{{Header: "Fruit", Done: true}, {Header: "Drink", Done: true}},
				Review: []ReviewItem{{Question: "Which fruit?", Answer: "Dragon fruit"}, {Question: "Which drink?", Answer: "Tea"}},
				Options: []Option{
					{Key: "1", Label: "Send answers", Sub: "The agent gets them all at once", Primary: true},
					{Key: "2", Label: "Cancel the question", Sub: "Drops every answer; the agent then waits for a message"},
				}},
		},
		{
			name:   "edit permission",
			screen: "blocked.txt",
			want: Question{Kind: KindPermission, Title: "Allow this change?", Where: "CHANGELOG.md",
				Options: []Option{
					{Key: "1", Label: "Allow once", Sub: "Makes this change now", Primary: true},
					{Key: "2", Label: "Allow all edits", Sub: "Won’t ask again for edits this session"},
					dontAllow,
				}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Build("w1:p1", 7, fixture(t, tc.screen), tc.pending)
			if len(got.ID) != 24 {
				t.Errorf("ID = %q, want 24 hex digits", got.ID)
			}
			got.ID = ""
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Build:\n got %s\nwant %s", show(got), show(tc.want))
			}
		})
	}
}

func TestWorkingScreensAreUnknown(t *testing.T) {
	for _, name := range []string{"w1-p2.working.txt", "w2-p2.working.txt", "working.txt", "done.txt", "idle.txt"} {
		screen := fixture(t, name)
		q := Build("w1:p2", 3, screen, Pending{Cwd: "~/api", Command: "bun test"})
		if q.Kind != KindUnknown || q.Screen != screen || len(q.Options) != 0 || q.Options == nil || q.Command != nil {
			t.Errorf("%s: %s", name, show(q))
		}
	}
}

func TestOddScreens(t *testing.T) {
	for _, tc := range []struct {
		name   string
		screen string
		kind   Kind
		title  string
		labels []string
	}{
		{"empty", "", KindUnknown, "Waiting for you", nil},
		{"one option", "Continue?\n❯ 1. Yes\n", KindUnknown, "Waiting for you", nil},
		{"starts at 2", "Pick one\n  2. alpha\n  3. beta\n", KindUnknown, "Waiting for you", nil},
		{"skips a number", "Pick one\n  1. alpha\n  3. beta\n", KindUnknown, "Waiting for you", nil},
		{"ten options", "Pick\n 1. a\n 2. b\n 3. c\n 4. d\n 5. e\n 6. f\n 7. g\n 8. h\n 9. i\n 10. j\n",
			KindAsk, "Pick", []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}},
		{"multi-select", "Which checks?\n❯ 1. [ ] lint\n  2. [✔] test\n", KindUnknown, "Waiting for you", nil},
		{"previews", fixtureText("testdata/ask-preview.txt"), KindUnknown, "Waiting for you", nil},
		{
			"prose list, then a dialog without options",
			"⏺ Plan:\n  1. Read the config\n  2. Patch the loader\n\n────────\n Enter a commit message\n > \n\n Enter to submit · Esc to cancel\n" +
				"────────\n  ⏵⏵ accept edits on\n",
			KindUnknown, "Waiting for you", nil,
		},
		{"generic list", "Which branch should I base it on?\n\n1) main\n2) release/2.4\n", KindAsk,
			"Which branch should I base it on?", []string{"main", "release/2.4"}},
		{
			"prose list above the prompt",
			"⏺ Plan:\n  1. Read the config\n  2. Patch the loader\n  3. Run the tests\n\n" +
				"╭────╮\n│ Bash command │\n│   go test ./... │\n│ │\n│ Do you want to proceed? │\n" +
				"│ ❯ 1. Yes │\n│   2. No, and tell Claude what to do differently (esc) │\n╰────╯\n",
			KindPermission, "Allow this command?", []string{"Allow once", "Don’t allow"},
		},
		{
			"no box, > cursor, CRLF, no description",
			"────────────────\r\n Bash command\r\n\r\n   make deploy\r\n\r\n Do you want to proceed?\r\n" +
				" > 1. Yes\r\n   2. No\r\n\r\n Esc to cancel\r\n",
			KindPermission, "Allow this command?", []string{"Allow once", "Don’t allow"},
		},
		{
			"unknown permission tool keeps its prompt",
			"│ Fetch │\n│   https://example.com/docs │\n│ Do you want to allow Claude to fetch this content? │\n" +
				"│ ❯ 1. Yes │\n│   2. Yes, and don't ask again for example.com │\n│   3. No, and tell Claude what to do differently (esc) │\n",
			KindPermission, "Do you want to allow Claude to fetch this content?",
			[]string{"Allow once", "Always allow", "Don’t allow"},
		},
		{
			"wrapped hint on option 3",
			"Do you want to proceed?\n❯ 1. Yes\n  2. No, and tell Claude what to do\n  differently (esc)\n",
			KindPermission, "Do you want to proceed?", []string{"Allow once", "Don’t allow"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := Build("w2:p1", 1, tc.screen, Pending{})
			var labels []string
			for _, o := range q.Options {
				labels = append(labels, o.Label)
			}
			if q.Kind != tc.kind || q.Title != tc.title || !reflect.DeepEqual(labels, tc.labels) {
				t.Errorf("got %s", show(q))
			}
			if (q.Kind == KindUnknown) != (q.Screen != "" || tc.screen == "") {
				t.Errorf("screen is set only for unknown: %s", show(q))
			}
		})
	}
}

func TestPermissionDetails(t *testing.T) {
	q := Build("w1:p1", 1, "Do you want to proceed?\n❯ 1. Yes\n  2. No\n", Pending{})
	if q.Command != nil || q.Options[0].Sub != "Allows it this time" || q.Options[1] != dontAllow2 {
		t.Errorf("headerless permission: %s", show(q))
	}
	q = Build("w1:p1", 1, "Do you want to proceed?\n❯ 1. Yes\n  2. No\n", Pending{Command: "ls"})
	if q.Command == nil || *q.Command != "ls" || q.Title != "Allow this command?" {
		t.Errorf("transcript command makes it a command prompt: %s", show(q))
	}
	for _, name := range []string{"w1-p1.blocked.txt", "testdata/legacy-box.txt"} {
		s := Parse(fixture(t, name))
		if s.Tool != "Bash command" || s.Command != "rm -rf dist && bun run build" || s.Prompt != "Do you want to proceed?" {
			t.Errorf("Parse(%s): %+v", name, s)
		}
	}
	q = Build("w1:p1", 1, "Do you want to proceed?\n❯ 1. Yes\n  2. Yes, and don't ask again for: git push:*\n  3. No\n", Pending{})
	if q.Options[1].Sub != "Won’t ask again for git push in this project" {
		t.Errorf("rule pattern with :*: %s", show(q))
	}
}

func TestID(t *testing.T) {
	screen := fixture(t, "w1-p1.blocked.txt")
	base := Build("w1:p1", 110, screen, Pending{}).ID
	moved := strings.Replace(strings.Replace(screen, "❯ 1. Yes", "  1. Yes", 1), "  2. Yes,", "❯ 2. Yes,", 1)

	if got := Build("w1:p1", 110, moved, Pending{}).ID; got != base {
		t.Error("moving the ❯ cursor changed the id")
	}
	if got := Build("w1:p1", 110, screen, Pending{Command: "x", LedHere: "y", Cwd: "z"}).ID; got != base {
		t.Error("transcript data changed the id")
	}
	if got := Build("w1:p1", 110, "older output\n"+screen, Pending{}).ID; got != base {
		t.Error("output above the prompt changed the id")
	}
	for name, id := range map[string]string{
		"seq":     Build("w1:p1", 111, screen, Pending{}).ID,
		"pane":    Build("w1:p2", 110, screen, Pending{}).ID,
		"options": Build("w1:p1", 110, strings.Replace(screen, "for: rm -rf dist *", "for: rm *", 1), Pending{}).ID,
		"unknown": Build("w1:p1", 110, "nothing here", Pending{}).ID,
		"command": Build("w1:p1", 110, strings.Replace(screen, "│ rm -rf dist && bun run build", "│ rm -rf ~/ && bun run build", 1), Pending{}).ID,
	} {
		if id == base {
			t.Errorf("a different %s gave the same id", name)
		}
	}
	// Every edit permission has the same options; the file and the diff
	// tell them apart.
	edit := fixture(t, "blocked.txt")
	other := strings.ReplaceAll(edit, "CHANGELOG.md", "go.mod")
	diff := strings.Replace(edit, "cache was empty", "cache was full", 1)
	if a, b, c := Build("w1:p1", 3, edit, Pending{}).ID, Build("w1:p1", 3, other, Pending{}).ID, Build("w1:p1", 3, diff, Pending{}).ID; a == b || a == c {
		t.Error("edit prompts for another file or diff gave the same id")
	}
	// Separators are unambiguous: these differ only in where a boundary falls.
	a := ID("p", 1, Screen{Options: []RawOption{{Key: "1", Label: "ab"}, {Key: "2", Label: "c"}}})
	b := ID("p", 1, Screen{Options: []RawOption{{Key: "1", Label: "a"}, {Key: "2", Label: "bc"}}})
	if a == b {
		t.Error("label boundary not part of the id")
	}
}

// show prints a question as JSON, so pointers print as their values.
func show(q Question) string {
	b, _ := json.Marshal(q)
	return string(b)
}

func TestDialogOpen(t *testing.T) {
	rule := strings.Repeat("─", 40)
	open := map[string]string{
		"question": "Which fruit?\n❯ 1. Apple\n  2. Banana\n  3. Type something.\n" + rule + "\n  4. Chat about this\n\nEnter to select · Tab/Arrow keys to navigate · Esc to cancel\n",
		// The own-answer box focused: herdr reports the agent idle here.
		"own answer": "Which toppings?\n  1. [✔] Honey\n❯ 2. [✔] Maple syrup\n     Submit\n" + rule + "\n  3. Chat about this\n-- INSERT -- · Enter to select · Tab/Arrow keys to navigate · ctrl+g to edit in Nvim\n",
		// The review after several questions has no footer.
		"review": "←  ☒ Fruit  ☒ Toppings  ✔ Submit  →\nReview your answers\n ● Which fruit?\n   → Cherry\n ● Which toppings?\n   → Honey, Yogurt\n❯ 1. Submit answers\n  2. Cancel\n",
		"trust":  " ❯ No, exit\n   Yes, I trust this folder\n Enter to confirm · Esc to cancel\n",
	}
	for name, s := range open {
		if !DialogOpen(s) {
			t.Errorf("%s: not seen as open", name)
		}
	}
	closed := map[string]string{
		"prompt box": "● I received: Cherry.\n✻ Cooked for 8s\n" + rule + "\n❯ \n" + rule + "\n  Opus 5.5 medium · 50k/700k · work\n  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle) · ← 1 agent\n",
		// A footer far up in the output is history, not a dialog.
		"old footer": "Enter to select · Esc to cancel\n" + strings.Repeat("output line\n", 10) + "❯ \n",
		"empty":      "",
	}
	for name, s := range closed {
		if DialogOpen(s) {
			t.Errorf("%s: seen as open", name)
		}
	}
}

// fixtureText reads a testdata file outside a test, for a table.
func fixtureText(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestOwnBox(t *testing.T) {
	for _, tc := range []struct {
		screen, key, text string
		ok                bool
	}{
		{"testdata/ask-own-focused.txt", "3", "", true},
		{"testdata/ask-own-typed.txt", "3", "Dragon fruit", true},
		{"testdata/ask-own-typed.txt", "2", "", false}, // the cursor is on 3
		{"testdata/ask-first.txt", "3", "", false},     // not focused: no INSERT footer
		{"testdata/ask-review.txt", "1", "", false},    // the review has no box
		{"w2-p2.working.txt", "3", "", false},
	} {
		text, ok := OwnBox(fixture(t, tc.screen), tc.key)
		if text != tc.text || ok != tc.ok {
			t.Errorf("OwnBox(%s, %s) = %q, %v; want %q, %v", tc.screen, tc.key, text, ok, tc.text, tc.ok)
		}
	}
}
