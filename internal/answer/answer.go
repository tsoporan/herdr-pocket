// Package answer turns a blocked agent's visible screen into a question the
// app can answer with one option number.
//
// Parse reads the screen. Build merges in what the transcript knows about the
// pending call (Pending), maps Claude Code's permission labels to the app's
// plain ones, and computes the question id.
//
// One real Bash permission screen was read through herdr on 2026-11-21
// (structure only): a full-width ── rule, a "Bash command · <title>" header,
// the command on │-prefixed lines between two ╌ rules, a hint line or two,
// "Do you want to proceed?", then "❯ 1. Yes", "2. Yes, and don’t ask again
// for: <pattern> *", "3. No" and an "Esc to cancel · …" footer. Older Claude
// Code drew the same prompt in a │ box with "… commands in <dir>" and "No,
// and tell Claude what to do differently (esc)"; both parse.
//
// AskUserQuestion was read in an isolated herdr session on 2026-11-22: a tab row ("←  ☐ Fruit  ☒ Drink  ✔ Submit  →",
// or " ☐ Fruit" for one question), the question, the options with their
// descriptions, "N+1. Type something.", a rule, "N+2. Chat about this" and
// a footer. After the last of several questions comes "Review your answers"
// with "1. Submit answers" / "2. Cancel". Single choice and the review parse
// as KindAsk; pick-several (tick boxes) and previews ("n to add notes") are
// KindUnknown for now. Anything without a numbered list is KindUnknown too,
// and the app shows the screen and the quick keys instead.
package answer

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Kind is what sort of question a screen holds.
type Kind string

const (
	KindPermission Kind = "permission" // Claude Code's "Do you want to …?" approval
	KindAsk        Kind = "ask"        // AskUserQuestion, or any other numbered list
	KindUnknown    Kind = "unknown"    // no answerable list found
)

// Option is one answer button. Key is the digit to send.
type Option struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Sub       string `json:"sub"`
	Primary   bool   `json:"primary"`
	NeedsNote bool   `json:"needs_note"`
	// Own is AskUserQuestion's "Type something." row: the note is the
	// answer, typed into the box the digit opens (see OwnBox).
	Own bool `json:"own"`
}

// Step is one question of a multi-question ask, from its tab row. Done is
// the tab's ☒: answered at least once.
type Step struct {
	Header string `json:"header"`
	Done   bool   `json:"done"`
}

// ReviewItem is one question and its answer on the review step.
type ReviewItem struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

// Question is the detail frame's question object.
type Question struct {
	ID      string   `json:"id"`
	Kind    Kind     `json:"kind"`
	Title   string   `json:"title"`
	Where   string   `json:"where"`
	Command *string  `json:"command"`
	LedHere *string  `json:"led_here"`
	Options []Option `json:"options"`
	Screen  string   `json:"screen,omitempty"` // only for KindUnknown
	// Steps are an AskUserQuestion's tabs (one for a single question),
	// without the Submit tab. Review is set on the review step.
	Steps  []Step       `json:"steps,omitempty"`
	Review []ReviewItem `json:"review,omitempty"`
}

// Pending is what the transcript knows about the call waiting on the user.
// Every field is optional.
type Pending struct {
	Command     string // the exact Bash command
	Description string // the Bash call's description
	Cwd         string // where it runs, already in display form (such as ~/api)
	LedHere     string // the agent's latest words before it stopped
}

// RawOption is one numbered option as the screen shows it, with Claude
// Code's key hints such as "(esc)" removed. Detail holds the lines under it:
// a wrapped label for a permission prompt, a description for a question.
type RawOption struct {
	Key    string
	Label  string
	Detail []string
}

// Screen is a parsed visible screen.
type Screen struct {
	Kind    Kind
	Prompt  string // the line above the options, such as "Do you want to proceed?"
	Tool    string // the permission box header: "Bash command", "Edit file", …
	Command string // for a Bash permission, the command shown in the box
	Target  string // for an edit permission, the file named in the prompt
	// Dialog is the dialog's non-blank lines above the options, top first:
	// the header, the command or diff and the prompt.
	Dialog  []string
	Options []RawOption
	Steps   []Step       // an AskUserQuestion's tab row
	Review  []ReviewItem // set on its review step
	Own     string       // the key of its "Type something." row
}

var (
	// optionLine matches "❯ 1. Yes" and "  2) dev". herdr only sends single
	// digits, so options 10 and up are not matched.
	optionLine = regexp.MustCompile(`^(\s*(?:[❯›>▶]\s*)?)([1-9])[.)]\s+(\S.*)$`)
	keyHint    = regexp.MustCompile(`\s*\((?:esc|shift\+tab|tab|ctrl\+[a-z])\)$`)
	checkbox   = regexp.MustCompile(`^\[[ xX✔✓]\]`)
	editTarget = regexp.MustCompile(`^Do you want to (?:make this edit to|create|overwrite) (.+?)\?$`)
	alwaysFor  = regexp.MustCompile(`^Yes, and don['’]t ask again for:? (.+?)(?: commands in .+)?$`)
	// tabRow is an AskUserQuestion's tab row; tabItem is one tab in it.
	tabRow  = regexp.MustCompile(`^(?:←\s+)?[☐☒]\s+\S`)
	tabItem = regexp.MustCompile(`([☐☒✔])\s+(.+?)(?:\s{2,}|\s*→|$)`)
)

// ownRow is the label of AskUserQuestion's own-answer row in single choice.
const ownRow = "Type something."

// clean drops box borders and trailing space, keeping indentation outside
// a box.
func clean(line string) string {
	line = strings.TrimRight(line, " \t\r")
	if rest, ok := strings.CutPrefix(strings.TrimLeft(line, " "), "│"); ok {
		line = strings.TrimPrefix(rest, " ")
		line = strings.TrimRight(strings.TrimSuffix(line, "│"), " ")
	}
	return line
}

func indent(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

// maxAfterOptions is how many non-blank lines may follow the options. A
// dialog's options sit at the bottom of the screen, under at most a footer
// and a rule (two lines on the real screen, one in the fixtures). This is a
// heuristic: it keeps a numbered list in the agent's earlier output from
// passing for the question when the dialog itself has no list.
const maxAfterOptions = 4

// Parse reads a visible screen. It uses the last run of options numbered
// 1, 2, 3, … on the screen, so an earlier numbered list in the agent's
// output is skipped, and only if it is near the bottom (maxAfterOptions).
func Parse(text string) Screen {
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = clean(lines[i])
	}

	var block []RawOption
	var firstLine, bestFirst, bestEnd int
	var best []RawOption
	detailOpen := false // still in the lines directly under the last option
	optIndent := 0      // the column of the last option's digit
	for i, l := range lines {
		if m := optionLine.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[2])
			label := keyHint.ReplaceAllString(m[3], "")
			switch {
			case n == 1:
				block, firstLine = []RawOption{{Key: m[2], Label: label}}, i
			case block != nil && n == len(block)+1:
				block = append(block, RawOption{Key: m[2], Label: label})
			default:
				block = nil
			}
			if len(block) >= 2 {
				best, bestFirst, bestEnd = block, firstLine, i
			}
			detailOpen, optIndent = block != nil, utf8.RuneCountInString(m[1])
			continue
		}
		if strings.TrimSpace(l) == "" {
			detailOpen = false
			continue
		}
		if detailOpen && indent(l) >= optIndent {
			last := &block[len(block)-1]
			last.Detail = append(last.Detail, keyHint.ReplaceAllString(strings.TrimSpace(l), ""))
			if len(block) >= 2 {
				bestEnd = i
			}
			continue
		}
		// Anything else ends the run: options must be adjacent, give or
		// take blank lines.
		block, detailOpen = nil, false
	}

	if best == nil || nonBlank(lines[bestEnd+1:]) > maxAfterOptions {
		return Screen{Kind: KindUnknown}
	}
	s := Screen{Kind: KindAsk, Options: best}
	above := nonBlankAbove(lines, bestFirst)
	s.Dialog = slices.Clone(above)
	slices.Reverse(s.Dialog)
	if len(above) > 0 {
		s.Prompt = above[0]
	}
	for _, o := range best {
		if checkbox.MatchString(o.Label) {
			// A multi-select list: a digit toggles instead of answering.
			return Screen{Kind: KindUnknown}
		}
	}
	for _, l := range lines[bestFirst:] {
		if strings.Contains(l, "to add notes") {
			// Options with previews: a digit there did not answer.
			return Screen{Kind: KindUnknown}
		}
	}
	if strings.HasPrefix(s.Prompt, "Do you want to") && strings.HasPrefix(best[0].Label, "Yes") {
		s.Kind = KindPermission
		s.readBox(above)
		if m := editTarget.FindStringSubmatch(s.Prompt); m != nil {
			s.Target = m[1]
		}
		return s
	}
	s.readAsk()
	return s
}

// readAsk reads an AskUserQuestion's tab row, review step and own-answer
// row. A plain numbered list has none of them.
func (s *Screen) readAsk() {
	for i, l := range s.Dialog {
		if !tabRow.MatchString(l) {
			continue
		}
		for _, m := range tabItem.FindAllStringSubmatch(l, -1) {
			if m[1] == "✔" && m[2] == "Submit" {
				continue
			}
			s.Steps = append(s.Steps, Step{Header: m[2], Done: m[1] == "☒"})
		}
		if i+1 < len(s.Dialog) && s.Dialog[i+1] == "Review your answers" {
			s.readReview(s.Dialog[i+2:])
		}
		break
	}
	if s.Review != nil {
		return
	}
	for _, o := range s.Options {
		if o.Label == ownRow {
			s.Own = o.Key
		}
	}
}

// readReview reads the review's "● question" and "→ answer" lines.
func (s *Screen) readReview(lines []string) {
	s.Review = []ReviewItem{}
	for _, l := range lines {
		if q, ok := strings.CutPrefix(l, "● "); ok {
			s.Review = append(s.Review, ReviewItem{Question: q})
		} else if a, ok := strings.CutPrefix(l, "→ "); ok && len(s.Review) > 0 {
			s.Review[len(s.Review)-1].Answer = a
		}
	}
}

func nonBlank(lines []string) int {
	n := 0
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}

// nonBlankAbove returns the trimmed non-blank lines above line i, nearest
// first, up to the top of the prompt (a ── rule or a ╭ box corner).
func nonBlankAbove(lines []string, i int) []string {
	var out []string
	for j := i - 1; j >= 0 && len(out) < 40; j-- {
		l := strings.TrimSpace(lines[j])
		if strings.HasPrefix(l, "╭") || strings.HasPrefix(l, "──") {
			break
		}
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// readBox finds the permission header and, for Bash, the command under it.
// above is nearest first, so the header is the furthest one.
func (s *Screen) readBox(above []string) {
	for i := len(above) - 1; i >= 1; i-- {
		tool, _, _ := strings.Cut(above[i], " · ")
		switch tool {
		case "Bash command":
			s.Tool = tool
			s.Command = bashCommand(above[1:i])
			return
		case "Edit file", "Write file", "Create file":
			s.Tool = tool
			return
		}
	}
}

// bashCommand picks the command out of the lines between the header and the
// prompt (nearest the prompt first). Between two ╌ rules, it is every line
// there, joined as shown; otherwise it is the line right under the header.
func bashCommand(between []string) string {
	var rules []int
	for i, l := range between {
		if strings.HasPrefix(l, "╌") {
			rules = append(rules, i)
		}
	}
	if len(rules) >= 2 {
		lines := slices.Clone(between[rules[len(rules)-2]+1 : rules[len(rules)-1]])
		slices.Reverse(lines)
		return strings.Join(lines, "\n")
	}
	if len(between) > 0 {
		return between[len(between)-1]
	}
	return ""
}

// Build parses screen and returns the question for the agent at paneID.
// seq is the hub's epoch for the agent, which changes with every status
// change and re-list the hub sees (see ID).
func Build(paneID string, seq uint64, screen string, p Pending) Question {
	s := Parse(screen)
	q := Question{
		ID:      ID(paneID, seq, s),
		Kind:    s.Kind,
		Where:   p.Cwd,
		Options: []Option{},
	}
	if led := cmp.Or(p.LedHere, p.Description); led != "" {
		q.LedHere = &led
	}
	bash := s.Kind == KindPermission && (s.Tool == "Bash command" || p.Command != "")
	if cmd := cmp.Or(p.Command, s.Command); bash && cmd != "" {
		q.Command = &cmd
	}

	switch s.Kind {
	case KindUnknown:
		q.Title = "Waiting for you"
		q.Screen = screen
		return q
	case KindPermission:
		switch {
		case bash:
			q.Title = "Allow this command?"
		case s.Target != "":
			q.Title = "Allow this change?"
			q.Where = s.Target
		default:
			q.Title = s.Prompt
		}
	default:
		q.Title = s.Prompt
		if s.Review != nil {
			q.Title = "Send these answers?"
		}
	}
	q.Steps, q.Review = s.Steps, s.Review
	for i, o := range s.Options {
		opt := Option{Key: o.Key, Label: o.Label}
		switch {
		case s.Kind == KindPermission:
			opt.Primary = i == 0
			if len(o.Detail) > 0 {
				opt.Label = strings.Join(append([]string{o.Label}, o.Detail...), " ")
			}
			plainPermission(&opt, bash, s.Target != "")
		case s.Review != nil:
			plainReview(&opt)
		case o.Key == s.Own:
			opt.Label, opt.Own, opt.NeedsNote = "Your own answer", true, true
		default:
			opt.Sub = strings.Join(o.Detail, " ")
		}
		q.Options = append(q.Options, opt)
	}
	return q
}

// plainReview words the review step's two options as the mockup does. Verified 2026-11-23: 1 sends every answer;
// 2 drops the whole ask ("Declined to answer") and the agent waits.
func plainReview(o *Option) {
	switch o.Label {
	case "Submit answers":
		o.Label, o.Sub, o.Primary = "Send answers", "The agent gets them all at once", true
	case "Cancel":
		o.Label, o.Sub = "Cancel the question", "Drops every answer; the agent then waits for a message"
	}
}

// plainPermission maps Claude Code's permission labels to the mockup's. Labels it doesn't know are kept.
func plainPermission(o *Option, bash, edit bool) {
	label := o.Label
	switch {
	case label == "Yes":
		o.Label = "Allow once"
		switch {
		case bash:
			o.Sub = "Runs this command now"
		case edit:
			o.Sub = "Makes this change now"
		default:
			o.Sub = "Allows it this time"
		}
	case strings.HasPrefix(label, "Yes, and don't ask again"), strings.HasPrefix(label, "Yes, and don’t ask again"):
		o.Label = "Always allow"
		if m := alwaysFor.FindStringSubmatch(label); m != nil {
			o.Sub = "Won’t ask again for " + strings.TrimRight(strings.TrimSuffix(m[1], "*"), " :") + " in this project"
		} else {
			o.Sub = "Won’t ask again for this in this project"
		}
	case strings.HasPrefix(label, "Yes, allow all edits"):
		o.Label = "Allow all edits"
		o.Sub = "Won’t ask again for edits this session"
	case label == "No" || strings.HasPrefix(label, "No,"):
		// Plain "No" leaves the agent waiting for input too, so the note
		// can follow as a prompt either way.
		o.Label = "Don’t allow"
		o.Sub = "Then tell it what to do instead"
		o.NeedsNote = true
	}
}

// ID identifies a question by what the screen shows the user: the kind, the
// dialog above the options (the header, the command or the diff, and the
// prompt) and the options. So two prompts with the same options, such as two
// edit permissions for different files, get different ids. seq, the hub's
// epoch for the agent, makes a second, identical prompt a new question, so a
// double tap can't answer it. The ❯ position and the transcript are left
// out, so re-reading an unchanged prompt gives the same id.
func ID(paneID string, seq uint64, s Screen) string {
	parts := []any{paneID, seq, s.Kind, s.Tool, s.Prompt, s.Target, s.Command, s.Dialog}
	for _, o := range s.Options {
		parts = append(parts, o.Key, o.Label, o.Detail)
	}
	b, _ := json.Marshal(parts) // strings and numbers only; cannot fail
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12])
}

// dialogFooters are the key hints Claude Code draws under a dialog that
// takes keys (a permission, a question, its own-answer box, the folder trust
// check), and never under its plain prompt box. Seen on 2026-11-22: "Enter
// to select · Tab/Arrow keys to navigate · Esc to cancel", "-- INSERT -- ·
// Enter to select · …" (the own-answer box focused, which herdr reports as
// idle), "Enter to confirm · Esc to cancel", "Esc to cancel · …".
//
// The review step after the last of several questions has no footer: it
// ends with "❯ 1. Submit answers" and "2. Cancel", under "Review your
// answers", so those count too.
var dialogFooters = []string{"Esc to cancel", "Enter to select", "Enter to confirm", "Submit answers", "Review your answers"}

// OwnBox reports whether the screen shows the own-answer row of option key
// focused for typing (a footer reading "-- INSERT --"), and what it holds:
// "" while it still reads "Type something.", else the typed text's first
// screen line. Verified 2026-11-23: the row's digit focuses it, typed text
// replaces the label, and Enter answers the tab with it.
func OwnBox(screen, key string) (text string, ok bool) {
	if !DialogOpen(screen) {
		return "", false
	}
	lines := strings.Split(strings.TrimRight(screen, "\n "), "\n")
	insert := false
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-6; i-- {
		insert = insert || strings.Contains(lines[i], "-- INSERT --")
	}
	if !insert {
		return "", false
	}
	for _, l := range lines {
		rest, ok := strings.CutPrefix(strings.TrimSpace(clean(l)), "❯")
		if !ok {
			continue
		}
		if label, ok := strings.CutPrefix(strings.TrimSpace(rest), key+". "); ok {
			if label = strings.TrimSpace(label); label == ownRow {
				label = ""
			}
			return label, true
		}
	}
	return "", false
}

// DialogOpen reports whether the bottom of a visible screen shows a dialog
// that would take typed text or Enter as an answer, so a prompt must not be
// typed into it.
func DialogOpen(screen string) bool {
	lines := strings.Split(strings.TrimRight(screen, "\n "), "\n")
	seen := 0
	for i := len(lines) - 1; i >= 0 && seen < 6; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		seen++
		for _, f := range dialogFooters {
			if strings.Contains(l, f) {
				return true
			}
		}
	}
	return false
}
