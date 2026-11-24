package transcript

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type toolInput struct {
	FilePath    string `json:"file_path"`
	Path        string `json:"path"`
	Pattern     string `json:"pattern"`
	Command     string `json:"command"`
	Description string `json:"description"`
	Content     string `json:"content"`
	Questions   []struct {
		Question string `json:"question"`
	} `json:"questions"`
}

// phrase is the plain-language text of a tool call: past tense once it has
// a result, present tense while it runs.
func phrase(tool string, raw json.RawMessage, past bool) string {
	var in toolInput
	_ = json.Unmarshal(raw, &in)
	verb := func(done, doing string) string {
		if past {
			return done
		}
		return doing
	}
	file := path.Base(in.FilePath)
	switch tool {
	case "Read":
		if in.FilePath != "" {
			return verb("Read ", "Reading ") + file
		}
	case "Write":
		if in.FilePath != "" {
			return verb("Changed ", "Writing ") + file // finish says Created
		}
	case "Edit", "MultiEdit", "NotebookEdit":
		if in.FilePath != "" {
			return verb("Changed ", "Changing ") + file
		}
	case "Bash":
		what := in.Description
		if what == "" {
			what = clip(oneLine(in.Command), 80)
		} else if rest, ok := cutPrefixFold(what, "run "); ok {
			what = rest
		} else {
			what = lowerFirst(what)
		}
		if what != "" {
			return verb("Ran ", "Running ") + what
		}
	case "Grep", "Glob":
		if in.Pattern != "" {
			return verb("Searched for ", "Searching for ") + clip(in.Pattern, 60)
		}
		return verb("Searched", "Searching")
	case "Task", "Agent":
		if in.Description != "" {
			return verb("Asked a helper agent to ", "Asking a helper agent to ") + lowerFirst(in.Description)
		}
		return verb("Asked a helper agent", "Asking a helper agent")
	case "AskUserQuestion":
		if len(in.Questions) > 0 && in.Questions[0].Question != "" {
			return verb("Asked you: ", "Asking you: ") + clip(oneLine(in.Questions[0].Question), 120)
		}
	}
	return verb("Used ", "Using ") + tool
}

// finish turns an open call's step into a finished one.
func finish(c *call, isError bool, text string, tur toolUseResult, at time.Time) {
	st := c.step
	st.Kind = KindOK
	st.Text = phrase(c.name, c.input, true)
	var in toolInput
	_ = json.Unmarshal(c.input, &in)
	rel := relPath(in.FilePath, c.cwd)

	switch c.name {
	case "Read":
		st.Meta = rel
	case "Write":
		if tur.Type == "create" {
			st.Text = "Created " + path.Base(in.FilePath)
		}
		st.Meta = join(rel, plural(countLines(in.Content), "line"))
	case "Edit", "MultiEdit":
		st.Meta = rel
		if n := changedLines(tur.StructuredPatch); n > 0 {
			st.Meta = join(rel, plural(n, "line")+" changed")
		}
	case "Bash":
		out := tur.Stdout
		if out == "" {
			out = text
		}
		if counts := testCounts(out); counts != "" {
			st.Text += ": " + counts
		}
		st.Meta = duration(at.Sub(st.At))
	case "Grep", "Glob":
		st.Meta = relPath(in.Path, c.cwd)
	}
	if isError {
		st.Kind = KindFail
		if line := firstLine(text); line != "" {
			st.Meta = clip(line, 100)
		}
	}
}

var (
	passRe = regexp.MustCompile(`\b(\d+) (?:pass|passed|passing)\b`)
	failRe = regexp.MustCompile(`\b(\d+) (?:fail|failed|failing)\b`)
)

// testCounts finds pass and fail counts in test output, like "18 passed" or
// "16 passed, 2 failed". It returns "" when the output has no pass count.
func testCounts(out string) string {
	m := passRe.FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	s := m[1] + " passed"
	if f := failRe.FindStringSubmatch(out); f != nil && f[1] != "0" {
		s += ", " + f[1] + " failed"
	}
	return s
}

// changedLines is the larger of the added and removed line counts.
func changedLines(ps []patch) int {
	var add, del int
	for _, p := range ps {
		for _, l := range p.Lines {
			switch {
			case strings.HasPrefix(l, "+"):
				add++
			case strings.HasPrefix(l, "-"):
				del++
			}
		}
	}
	return max(add, del)
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(s, "\n"), "\n") + 1
}

func plural(n int, word string) string {
	if n == 0 {
		return ""
	}
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

func join(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + " · " + b
}

func duration(d time.Duration) string {
	switch {
	case d <= 0:
		return ""
	case d < 10*time.Second:
		return fmt.Sprintf("%.1f s", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	}
	return fmt.Sprintf("%d min", int(d.Minutes()))
}

// relPath shows a path relative to the session's cwd when it's inside it.
func relPath(p, cwd string) string {
	if cwd != "" {
		if rest, ok := strings.CutPrefix(p, strings.TrimSuffix(cwd, "/")+"/"); ok {
			return rest
		}
	}
	return p
}

// ShortDir is how a directory is shown to the person: "~/" plus its last
// element, as the mockups show /home/dev/src/api as ~/api.
func ShortDir(dir string) string {
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		return ""
	}
	return "~/" + path.Base(dir)
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}

// lowerFirst lowercases the first letter unless it starts an acronym.
func lowerFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	if next, _ := utf8.DecodeRuneInString(s[n:]); unicode.IsUpper(next) {
		return s
	}
	return string(unicode.ToLower(r)) + s[n:]
}

func firstLine(s string) string {
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clip shortens s to at most n runes, ending in "…".
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

var mdNoise = strings.NewReplacer("**", "", "__", "", "`", "")

// summarize trims assistant text to the Now line: the first paragraph, with
// markdown emphasis removed, cut to two sentences.
func summarize(text string) string {
	para := strings.TrimSpace(text)
	if i := strings.Index(para, "\n\n"); i >= 0 {
		para = para[:i]
	}
	para = strings.TrimLeft(para, "#>-* ")
	s := oneLine(mdNoise.Replace(para))
	return clip(firstSentences(s, 2), 280)
}

// firstSentences returns the first n sentences. A sentence ends at ".", "!"
// or "?" followed by a space and an upper-case letter or digit, so "0.4 s",
// "/v2" and "e.g. this" don't split.
func firstSentences(s string, n int) string {
	rs := []rune(s)
	for i := 0; i < len(rs)-2; i++ {
		if (rs[i] == '.' || rs[i] == '!' || rs[i] == '?') && rs[i+1] == ' ' &&
			(unicode.IsUpper(rs[i+2]) || unicode.IsDigit(rs[i+2])) {
			if n--; n == 0 {
				return string(rs[:i+1])
			}
		}
	}
	return s
}
