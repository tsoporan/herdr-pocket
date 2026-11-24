package transcript

import (
	"encoding/json"
	"slices"
	"strings"
	"time"
	"unicode"
)

// StepKind is how a step is drawn in the agent view.
type StepKind string

const (
	KindYou  StepKind = "you"  // a prompt the person typed
	KindOK   StepKind = "ok"   // a tool call that finished
	KindFail StepKind = "fail" // a tool call that errored or was interrupted
	KindNow  StepKind = "now"  // a tool call with no result yet
)

// Step is one line of the agent view's Steps list.
type Step struct {
	Kind StepKind
	Text string
	Meta string
	At   time.Time // when the prompt was sent or the call was made
}

// Option is one AskUserQuestion choice.
type Option struct {
	Label       string
	Description string
}

// Pending is the last tool call with no result yet: what the agent is
// running, or what it's waiting for permission to run. Which of the two is
// up to herdr's status.
type Pending struct {
	ToolUseID string
	Tool      string
	At        time.Time
	Cwd       string
	LedHere   string // the assistant text just before the call, this turn

	Command     string // Bash
	Description string // Bash

	Question    string // AskUserQuestion (the first question)
	Header      string
	MultiSelect bool
	Options     []Option

	Path string // Edit, Write, Read
}

// Snapshot is what the transcript says about a session right now.
type Snapshot struct {
	Now     string // the latest assistant text, one or two sentences
	Steps   []Step // oldest first, at most MaxSteps
	Pending *Pending
	Cwd     string

	// LastMessage is the latest assistant text of the current turn in full,
	// trimmed and bounded by BoundMessage; "" until the turn says something.
	LastMessage string
	// LastAsks is whether that text, before the bound, ends in a question.
	LastAsks bool

	TurnStart     time.Time // the last prompt the person typed
	LastAssistant time.Time // the last assistant record
	PRNumber      int       // the last PR the session linked, or 0
	PRURL         string
}

// clone copies the slices, so callers can't change the cached snapshot.
func (s Snapshot) clone() Snapshot {
	s.Steps = slices.Clone(s.Steps)
	if s.Pending != nil {
		p := *s.Pending
		p.Options = slices.Clone(p.Options)
		s.Pending = &p
	}
	return s
}

// MaxSteps bounds the steps kept per session.
const MaxSteps = 50

type call struct {
	step    *Step
	name    string
	input   json.RawMessage
	cwd     string
	ledHere string
	seq     int
}

// state folds records into a Snapshot, one record at a time.
type state struct {
	records  int
	steps    []*Step
	open     map[string]*call // unresolved tool calls of the current turn
	seq      int
	now      string
	turnText string // the latest assistant text of the current turn, summarized
	lastMsg  string // the same text in full, bounded
	lastAsks bool   // the full text ends in a question

	cwd           string
	turnStart     time.Time
	lastAssistant time.Time
	prNumber      int
	prURL         string
}

func newState() *state { return &state{open: map[string]*call{}} }

func (s *state) apply(r record) {
	s.records++
	if r.Type == "pr-link" {
		s.prNumber, s.prURL = r.PRNumber, r.PRURL
		return
	}
	if r.Cwd != "" {
		s.cwd = r.Cwd
	}
	switch r.Type {
	case "user":
		if r.isRealPrompt() {
			s.startTurn(r)
			return
		}
		s.applyResults(r)
	case "assistant":
		s.lastAssistant = r.at
		for _, b := range r.blocks {
			switch b.Type {
			case "text":
				if t := summarize(b.Text); t != "" {
					s.now, s.turnText = t, t
				}
				if t := strings.TrimSpace(b.Text); t != "" {
					s.lastMsg, s.lastAsks = BoundMessage(t), Asks(t)
				}
			case "tool_use":
				if b.ID == "" || b.Name == "" {
					continue
				}
				st := &Step{Kind: KindNow, Text: phrase(b.Name, b.Input, false), At: r.at}
				s.push(st)
				s.seq++
				s.open[b.ID] = &call{step: st, name: b.Name, input: b.Input, cwd: s.cwd, ledHere: s.turnText, seq: s.seq}
			}
		}
	}
}

// startTurn records a typed prompt. Calls left open by the previous turn
// were interrupted: they will never get a result.
func (s *state) startTurn(r record) {
	for id, c := range s.open {
		c.step.Kind, c.step.Meta = KindFail, "Interrupted"
		delete(s.open, id)
	}
	s.turnStart = r.at
	s.turnText, s.lastMsg, s.lastAsks = "", "", false
	s.push(&Step{Kind: KindYou, Text: clip(oneLine(r.prompt), 200), Meta: "You", At: r.at})
}

func (s *state) applyResults(r record) {
	n := 0
	for _, b := range r.blocks {
		if b.Type == "tool_result" {
			n++
		}
	}
	for _, b := range r.blocks {
		if b.Type != "tool_result" {
			continue
		}
		c, ok := s.open[b.ToolUseID]
		if !ok {
			continue
		}
		delete(s.open, b.ToolUseID)
		var tur toolUseResult
		if n == 1 { // toolUseResult belongs to the record's only result
			tur = decodeResult(r.Result)
		}
		finish(c, b.IsError, resultText(b.Content), tur, r.at)
	}
}

func (s *state) push(st *Step) {
	s.steps = append(s.steps, st)
	if len(s.steps) > MaxSteps {
		s.steps = append(s.steps[:0:0], s.steps[len(s.steps)-MaxSteps:]...)
	}
}

func (s *state) snapshot() Snapshot {
	snap := Snapshot{
		Now:           s.now,
		LastMessage:   s.lastMsg,
		LastAsks:      s.lastAsks,
		Steps:         make([]Step, len(s.steps)),
		Cwd:           s.cwd,
		TurnStart:     s.turnStart,
		LastAssistant: s.lastAssistant,
		PRNumber:      s.prNumber,
		PRURL:         s.prURL,
	}
	for i, st := range s.steps {
		snap.Steps[i] = *st
	}
	var last *call
	var lastID string
	for id, c := range s.open {
		if last == nil || c.seq > last.seq {
			last, lastID = c, id
		}
	}
	if last != nil {
		snap.Pending = pendingOf(lastID, last)
	}
	return snap
}

func pendingOf(id string, c *call) *Pending {
	p := &Pending{ToolUseID: id, Tool: c.name, At: c.step.At, Cwd: c.cwd, LedHere: c.ledHere}
	var in struct {
		Command     string `json:"command"`
		Description string `json:"description"`
		FilePath    string `json:"file_path"`
		Questions   []struct {
			Question    string `json:"question"`
			Header      string `json:"header"`
			MultiSelect bool   `json:"multiSelect"`
			Options     []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	}
	_ = json.Unmarshal(c.input, &in)
	p.Command, p.Description, p.Path = in.Command, in.Description, in.FilePath
	if len(in.Questions) > 0 {
		q := in.Questions[0]
		p.Question, p.Header, p.MultiSelect = q.Question, q.Header, q.MultiSelect
		for _, o := range q.Options {
			p.Options = append(p.Options, Option{Label: o.Label, Description: o.Description})
		}
	}
	return p
}

// MaxMessage bounds LastMessage, in characters.
const MaxMessage = 8000

// maxTail bounds the end BoundMessage keeps of a cut message, in characters.
const maxTail = 1000

// BoundMessage fits text longer than MaxMessage characters: it keeps the
// start, cut at its last line break that fits (or mid-line when there is
// none), a "…" line, and the final paragraph, where an agent puts its
// question. A final paragraph over maxTail characters keeps only its end.
func BoundMessage(text string) string {
	r := []rune(text)
	if len(r) <= MaxMessage {
		return text
	}
	body := strings.TrimRight(text, " \t\n")
	tail := body
	if i := strings.LastIndex(body, "\n\n"); i >= 0 {
		tail = body[i:]
	} else if i := strings.LastIndexByte(body, '\n'); i >= 0 {
		tail = body[i:]
	}
	tr := []rune(strings.TrimSpace(tail))
	if len(tr) > maxTail {
		tr = tr[len(tr)-maxTail:]
	}
	tail = strings.TrimLeft(string(tr), " \t")
	const sep = "\n…\n\n"
	head := string(r[:MaxMessage-len([]rune(tail))-len([]rune(sep))])
	if i := strings.LastIndexByte(head, '\n'); i > 0 {
		head = head[:i]
	}
	return strings.TrimRight(head, " \t\n") + sep + tail
}

// Asks reports whether a message ends in a question: a "?" last, past
// whitespace, markdown emphasis, closing brackets, quotes and emoji. A
// backtick is not passed over, so a "?" inside code doesn't count.
func Asks(text string) bool {
	t := strings.TrimRightFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("*_)]\"'”’", r) ||
			unicode.Is(unicode.So, r) || r == '\uFE0F' || r == '\u200D' || (r >= 0x1F3FB && r <= 0x1F3FF)
	})
	return strings.HasSuffix(t, "?")
}
