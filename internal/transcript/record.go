package transcript

// The structure of Claude Code's session transcripts, as observed in recent
// real files (keys and nesting only; Claude Code 2.1.x, November 2026). The file is
// <config dir>/projects/<cwd with "/" replaced by "-">/<sessionId>.jsonl, one
// JSON object per line, appended as the session runs.
//
// Every record has "type". Types seen: user, assistant, system, attachment,
// pr-link, last-prompt, ai-title, permission-mode, mode, session-owner,
// file-history-snapshot, file-history-delta, queue-operation, tools-used,
// data-flow-records, api-request, api-request-blob, api-request-shape,
// atis-latch, cost-state and more. Only user, assistant and pr-link matter
// here; everything else is skipped.
//
// user and assistant records carry: uuid, parentUuid, sessionId, cwd,
// gitBranch, version, userType, isSidechain, timestamp (RFC 3339 with
// milliseconds, UTC, e.g. 2026-11-21T09:10:00.000Z) and message.
//
//   - assistant: message = {id, type:"message", role, model, content[],
//     stop_reason ("tool_use" | "end_turn" | …), usage, …}. Each record holds
//     ONE content block; a single API message is split over several records
//     that share message.id. Block types: text {text}, thinking, tool_use
//     {id, name, input, caller}, server_tool_use, advisor_tool_result.
//   - tool_use input keys: Bash {command, description, timeout?,
//     run_in_background?}; Read {file_path, offset?, limit?}; Edit {file_path,
//     old_string, new_string, replace_all}; Write {file_path, content}; Agent
//     {description, prompt, subagent_type, run_in_background?};
//     AskUserQuestion {questions: [{question, header, multiSelect,
//     options: [{label, description}]}]}.
//   - user, a prompt: message.content is a string (or an array of text
//     blocks). Typed prompts carry promptSource:"typed". Injected ones carry
//     isMeta:true or promptSource:"system", or start with a tag
//     such as <system-reminder>, <task-notification> or <bash-stdout>. After
//     a compaction, the summary is a user string with isCompactSummary:true.
//     Other sources seen: "queued", "suggestion_accepted" (both typed by the
//     person). Many user records have no promptSource at all.
//   - user, a tool result: message.content = [{type:"tool_result",
//     tool_use_id, content (string, or [{type:"text",text}|image|…]),
//     is_error?}], plus sourceToolAssistantUUID and toolUseResult: an object
//     ({stdout, stderr, interrupted, …} for Bash; {type:"create", filePath,
//     content, structuredPatch} for Write; {filePath, oldString, newString,
//     structuredPatch: [{oldStart, oldLines, newStart, newLines, lines}]} for
//     Edit, lines prefixed " ", "+" or "-"), or a plain string on error.
//     Bash results carry no duration.
//   - pr-link: {prNumber, prUrl, prRepository, sessionId, timestamp}.
//
// A pending tool call (running, or waiting for permission) is an assistant
// tool_use block whose id has no tool_result yet. The transcript can't tell
// those two apart; herdr's status does.

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

type record struct {
	Type        string          `json:"type"`
	Timestamp   string          `json:"timestamp"`
	Cwd         string          `json:"cwd"`
	IsMeta      bool            `json:"isMeta"`
	IsSidechain bool            `json:"isSidechain"`
	Source      string          `json:"promptSource"`
	IsCompact   bool            `json:"isCompactSummary"`
	Message     *message        `json:"message"`
	Result      json.RawMessage `json:"toolUseResult"`
	PRNumber    int             `json:"prNumber"`
	PRURL       string          `json:"prUrl"`

	at     time.Time
	blocks []block
	prompt string // set for a plain-text user message
}

type message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	StopReason string          `json:"stop_reason"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// toolUseResult is the subset of the record-level toolUseResult object used
// for step meta.
type toolUseResult struct {
	Type            string  `json:"type"`
	Stdout          string  `json:"stdout"`
	Content         string  `json:"content"`
	StructuredPatch []patch `json:"structuredPatch"`
}

type patch struct {
	Lines []string `json:"lines"`
}

// decodeLine parses one transcript line. It reports false for blank,
// malformed or irrelevant lines; it never panics.
func decodeLine(line []byte) (record, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return record{}, false
	}
	var r record
	if err := json.Unmarshal(line, &r); err != nil {
		return record{}, false
	}
	switch r.Type {
	case "pr-link":
		return r, r.PRNumber > 0
	case "user", "assistant":
	default:
		return record{}, false
	}
	if r.IsSidechain || r.Message == nil {
		return record{}, false
	}
	r.at, _ = time.Parse(time.RFC3339Nano, r.Timestamp)
	c := bytes.TrimSpace(r.Message.Content)
	switch {
	case len(c) > 0 && c[0] == '"':
		var s string
		if json.Unmarshal(c, &s) != nil {
			return record{}, false
		}
		r.prompt = s
	case len(c) > 0 && c[0] == '[':
		if json.Unmarshal(c, &r.blocks) != nil {
			return record{}, false
		}
		if r.Type == "user" && !hasToolResult(r.blocks) {
			var texts []string
			for _, b := range r.blocks {
				if b.Type == "text" {
					texts = append(texts, b.Text)
				}
			}
			r.prompt = strings.Join(texts, "\n")
		}
	default:
		return record{}, false
	}
	return r, true
}

func hasToolResult(bs []block) bool {
	for _, b := range bs {
		if b.Type == "tool_result" {
			return true
		}
	}
	return false
}

// isRealPrompt reports whether a user record is something the person typed,
// rather than an injected reminder, command output or notification.
func (r *record) isRealPrompt() bool {
	if r.Type != "user" || r.IsMeta || r.IsCompact {
		return false
	}
	p := strings.TrimSpace(r.prompt)
	if p == "" {
		return false
	}
	if r.Source == "typed" {
		return true
	}
	return !strings.HasPrefix(p, "<") && !strings.HasPrefix(p, "[Request interrupted")
}

// resultText flattens a tool_result's content (a string or text blocks).
func resultText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var bs []block
	if json.Unmarshal(raw, &bs) != nil {
		return ""
	}
	var parts []string
	for _, b := range bs {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func decodeResult(raw json.RawMessage) toolUseResult {
	var t toolUseResult
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '{' {
		_ = json.Unmarshal(raw, &t)
	}
	return t
}
