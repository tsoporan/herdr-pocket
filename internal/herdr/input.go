package herdr

import (
	"context"
	"fmt"
)

// Agent read and input methods. Param names are checked against
// `herdr api schema --json` (protocol 22): request $defs AgentReadParams,
// AgentPromptParams, AgentSendKeysParams, AgentRenameParams, AgentTarget and
// PaneSendTextParams. A target is a pane id or a unique agent name; herdr
// answers agent_not_found for anything else.

// ReadSource selects what agent.read returns.
type ReadSource string

const (
	SourceVisible         ReadSource = "visible"          // the screen as shown
	SourceRecent          ReadSource = "recent"           // recent scrollback, wrapped
	SourceRecentUnwrapped ReadSource = "recent_unwrapped" // recent scrollback, lines joined
	SourceDetection       ReadSource = "detection"        // what herdr's state detection sees
)

// ReadResult is an agent.read result (PaneReadResult).
type ReadResult struct {
	PaneID      string     `json:"pane_id"`
	WorkspaceID string     `json:"workspace_id"`
	TabID       string     `json:"tab_id"`
	Source      ReadSource `json:"source"`
	Format      string     `json:"format"`
	Text        string     `json:"text"`
	Revision    uint64     `json:"revision"`
	Truncated   bool       `json:"truncated"`
}

// Read returns an agent's terminal text, ANSI stripped (agent.read with
// format text). lines limits recent reads; 0 leaves herdr's default.
func (c *Client) Read(ctx context.Context, target string, source ReadSource, lines int) (ReadResult, error) {
	params := struct {
		Target    string     `json:"target"`
		Source    ReadSource `json:"source"`
		Format    string     `json:"format"`
		Lines     *int       `json:"lines,omitempty"`
		StripANSI bool       `json:"strip_ansi"`
	}{Target: target, Source: source, Format: "text", StripANSI: true}
	if lines > 0 {
		params.Lines = &lines
	}
	var r struct {
		Read ReadResult `json:"read"`
	}
	err := c.Call(ctx, "agent.read", params, &r)
	return r.Read, err
}

// Prompt submits text plus Enter to an agent (agent.prompt, without wait).
// herdr refuses a blocked agent with CodeAgentBlocked before sending anything.
func (c *Client) Prompt(ctx context.Context, target, text string) error {
	params := struct {
		Target string `json:"target"`
		Text   string `json:"text"`
	}{target, text}
	return c.Call(ctx, "agent.prompt", params, nil)
}

// SendKeys sends logical key presses to an agent (agent.send_keys). Keys are
// checked with ValidateKeys first, so a bad name never reaches herdr.
func (c *Client) SendKeys(ctx context.Context, target string, keys []string) error {
	if err := ValidateKeys(keys); err != nil {
		return err
	}
	params := struct {
		Target string   `json:"target"`
		Keys   []string `json:"keys"`
	}{target, keys}
	return c.Call(ctx, "agent.send_keys", params, nil)
}

// SendText types text into a pane without pressing Enter (pane.send_text).
// It takes a pane id, not an agent target.
func (c *Client) SendText(ctx context.Context, paneID, text string) error {
	params := struct {
		PaneID string `json:"pane_id"`
		Text   string `json:"text"`
	}{paneID, text}
	return c.Call(ctx, "pane.send_text", params, nil)
}

// Rename names an agent (agent.rename). An empty name sends null, which
// clears it.
func (c *Client) Rename(ctx context.Context, target, name string) error {
	params := struct {
		Target string  `json:"target"`
		Name   *string `json:"name"`
	}{Target: target}
	if name != "" {
		params.Name = &name
	}
	return c.Call(ctx, "agent.rename", params, nil)
}

// Focus brings an agent's pane to the front on the desktop (agent.focus).
// herdr marks a focused agent seen, so a done agent becomes idle.
func (c *Client) Focus(ctx context.Context, target string) error {
	params := struct {
		Target string `json:"target"`
	}{target}
	return c.Call(ctx, "agent.focus", params, nil)
}

// validKeys are the logical key names herdr-pocket sends. herdr validates
// every key before writing and has more names than these, but neither the
// protocol 22 schema nor `herdr agent send-keys --help` lists them; the help
// names only esc (escape also accepted). This is the narrow set the app needs
// (SPEC: Enter, Esc, Ctrl-C, arrows, digits); widen it only from herdr's own
// list.
var validKeys = map[string]bool{
	"esc": true, "escape": true, "enter": true, "ctrl+c": true,
	"up": true, "down": true, "left": true, "right": true,
	"0": true, "1": true, "2": true, "3": true, "4": true,
	"5": true, "6": true, "7": true, "8": true, "9": true,
}

// ValidKey reports whether key is a logical key name herdr-pocket may send.
func ValidKey(key string) bool { return validKeys[key] }

// ValidateKeys returns an error naming the first key ValidKey refuses, or for
// an empty list.
func ValidateKeys(keys []string) error {
	if len(keys) == 0 {
		return fmt.Errorf("herdr: no keys to send")
	}
	for _, k := range keys {
		if !ValidKey(k) {
			return fmt.Errorf("herdr: unsupported key %q", k)
		}
	}
	return nil
}
