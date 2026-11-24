package herdr

import (
	"context"
	"time"
)

// Opening a tab, starting an agent in it and prompting with a wait. Param
// names are checked against `herdr api schema --json` (protocol 22): request
// $defs TabCreateParams, AgentStartParams, AgentPromptParams and
// AgentPromptWaitOptions. Behaviour was checked in an isolated herdr 0.9.5
// session on 2026-11-22, and agent.start and agent.wait over the raw socket
// on 2026-11-23.

// Error codes from tab.create, agent.start, agent.wait and agent.prompt with
// wait.
const (
	// CodeAgentNotReady is the herdr CLI's word, not a socket code:
	// StartAgent returns it when agent.wait finds the new agent blocked at
	// startup (a trust dialog). The name stays the agent's.
	CodeAgentNotReady      = "agent_not_ready"
	CodeAgentNameTaken     = "agent_name_taken"     // agent.start: a live agent has the name
	CodeInvalidAgentName   = "invalid_agent_name"   // agent.start: the name doesn't match [a-z][a-z0-9_-]{0,31}
	CodeAgentPaneBusy      = "agent_pane_busy"      // agent.start: the pane isn't an available shell
	CodeAgentPromptStalled = "agent_prompt_stalled" // agent.prompt with wait: no working or blocked within 5 s
	CodeTimeout            = "timeout"              // agent.wait or a waiting prompt ran out of timeout_ms
	// CodeWorkspaceNotFound is unverified: a guess at tab.create's code for an
	// unknown workspace_id. The protocol 22 schema lists no error codes, and
	// the probe never sent an unknown workspace.
	CodeWorkspaceNotFound = "workspace_not_found"
)

// Kinds are the agent kinds herdr 0.9.5 can start. herdr doesn't check that
// a kind's executable is installed (it types the command, and agent.wait
// answers CodeTimeout), and the executable isn't always the kind's name (kiro runs
// kiro-cli).
var Kinds = []string{
	"pi", "claude", "codex", "gemini", "cursor", "devin", "agy", "cline", "omp", "mastracode", "opencode", "copilot",
	"kimi", "kiro", "droid", "amp", "grok", "hermes", "kilo", "qodercli", "qwen", "letta", "maki", "muse",
}

// waitSlack is added to herdr's own timeout for a call that waits server
// side, so herdr's answer (CodeTimeout) arrives before the client gives up.
const waitSlack = 5 * time.Second

// CreateTab opens a tab with one shell pane in a workspace (tab.create). An
// empty cwd is omitted: herdr then opens the tab in the live cwd of the
// workspace's active pane. It always sends focus:false, so the desktop's
// layout and focus stay as they are, and never env.
func (c *Client) CreateTab(ctx context.Context, workspaceID, cwd, label string) (Tab, Pane, error) {
	params := struct {
		WorkspaceID string `json:"workspace_id"`
		Cwd         string `json:"cwd,omitempty"`
		Label       string `json:"label"`
		Focus       bool   `json:"focus"`
	}{WorkspaceID: workspaceID, Cwd: cwd, Label: label, Focus: false}
	var r struct {
		Tab      Tab  `json:"tab"`
		RootPane Pane `json:"root_pane"`
	}
	err := c.Call(ctx, "tab.create", params, &r)
	return r.Tab, r.RootPane, err
}

// StartAgent starts an agent of kind in a shell pane, names it, and waits
// until it is ready or blocked at startup. It is two calls, as the herdr
// CLI makes them (checked on herdr 0.9.5, 2026-11-23):
//
//  1. agent.start answers at once (normal request deadline) with the agent
//     listed under its name, status unknown and launch_pending. herdr's
//     refusals (CodeAgentNameTaken, CodeInvalidAgentName, CodeAgentPaneBusy)
//     come from here, before anything is typed; agent.wait is then never
//     sent.
//  2. agent.wait on the pane until idle, blocked, done or working, with
//     timeout_ms; its deadline is timeout plus 5 s. herdr answers
//     CodeTimeout after timeout when the agent never comes up (its
//     executable isn't installed); the agent stays listed, launch pending,
//     and the name stays taken.
//
// CodeAgentNotReady is made here, not by herdr: the socket never returns it.
// It is the CLI's word for an agent that agent.wait finds blocked at
// startup (a trust dialog, say), and StartAgent returns it with that agent.
//
// Once agent.start has succeeded, the returned Agent has its pane id even
// when err is not nil: the agent was launched and holds its name, so the
// caller must not start it again. On a refusal from agent.start the Agent
// is empty. It never sends args, and herdr takes timeout_ms (more than 3 s,
// at most 300 s) on both calls.
func (c *Client) StartAgent(ctx context.Context, name, kind, paneID string, timeout time.Duration) (Agent, error) {
	start := struct {
		Name      string `json:"name"`
		Kind      string `json:"kind"`
		PaneID    string `json:"pane_id"`
		TimeoutMS int64  `json:"timeout_ms"`
	}{name, kind, paneID, timeout.Milliseconds()}
	var started struct {
		Agent Agent `json:"agent"`
	}
	if err := c.Call(ctx, "agent.start", start, &started); err != nil {
		return Agent{}, err
	}
	launched := started.Agent
	if launched.PaneID == "" {
		launched.PaneID = paneID
	}

	wait := struct {
		Target    string   `json:"target"`
		Until     []Status `json:"until"`
		TimeoutMS int64    `json:"timeout_ms"`
	}{paneID, []Status{StatusIdle, StatusBlocked, StatusDone, StatusWorking}, timeout.Milliseconds()}
	var waited struct {
		Agent Agent `json:"agent"`
	}
	if err := c.call(ctx, timeout+waitSlack, "agent.wait", wait, &waited); err != nil {
		return launched, err
	}
	a := waited.Agent
	if a.PaneID == "" {
		a.PaneID = paneID
	}
	if a.AgentStatus == StatusBlocked {
		return a, &Error{Code: CodeAgentNotReady, Message: "agent " + name + " started but is blocked at startup"}
	}
	return a, nil
}

// PromptWait submits text plus Enter and waits (agent.prompt with wait). It
// returns once the agent's status is in until; an empty until waits for the
// turn to end (idle, done or blocked). From a non-working agent herdr answers
// CodeAgentPromptStalled when it sees neither working nor blocked within 5 s,
// and CodeTimeout after timeout. A blocked agent is refused with
// CodeAgentBlocked before anything is sent. The call's deadline is timeout
// plus 5 s.
func (c *Client) PromptWait(ctx context.Context, target, text string, until []Status, timeout time.Duration) (Agent, error) {
	type wait struct {
		Until     []Status `json:"until,omitempty"`
		TimeoutMS int64    `json:"timeout_ms"`
	}
	params := struct {
		Target string `json:"target"`
		Text   string `json:"text"`
		Wait   wait   `json:"wait"`
	}{target, text, wait{until, timeout.Milliseconds()}}
	var r struct {
		Agent Agent `json:"agent"`
	}
	err := c.call(ctx, timeout+waitSlack, "agent.prompt", params, &r)
	return r.Agent, err
}
