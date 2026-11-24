package herdr

// Record types, checked against `herdr api schema --json` (protocol 22):
// success_response $defs AgentInfo, WorkspaceInfo and TabInfo, and event $defs
// PaneInfo. herdr omits null fields, so optional strings are plain strings
// with omitempty. Fields this project doesn't use (scroll, worktree,
// restore_error, interactive_ready, ...) are left out.

// Status is an agent state as herdr reports it.
type Status string

const (
	StatusIdle    Status = "idle"
	StatusWorking Status = "working"
	StatusBlocked Status = "blocked" // an approval or question UI is up
	StatusDone    Status = "done"    // finished and not yet seen
	StatusUnknown Status = "unknown"
)

// Pong is the ping result.
type Pong struct {
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
}

// AgentSession identifies the agent's own session; for Claude Code, Value is
// the session id.
type AgentSession struct {
	Source string `json:"source"`
	Agent  string `json:"agent"`
	Kind   string `json:"kind"` // "id" or "path"
	Value  string `json:"value"`
}

// Agent is one agent.list record (AgentInfo).
type Agent struct {
	PaneID                string            `json:"pane_id"`
	TabID                 string            `json:"tab_id"`
	WorkspaceID           string            `json:"workspace_id"`
	TerminalID            string            `json:"terminal_id"`
	Name                  string            `json:"name,omitempty"`
	Agent                 string            `json:"agent,omitempty"`
	DisplayAgent          string            `json:"display_agent,omitempty"`
	AgentStatus           Status            `json:"agent_status"`
	AgentSession          *AgentSession     `json:"agent_session,omitempty"`
	Title                 string            `json:"title,omitempty"`
	TerminalTitle         string            `json:"terminal_title,omitempty"`
	TerminalTitleStripped string            `json:"terminal_title_stripped,omitempty"`
	Cwd                   string            `json:"cwd,omitempty"`
	ForegroundCwd         string            `json:"foreground_cwd,omitempty"`
	Focused               bool              `json:"focused"`
	Tokens                map[string]string `json:"tokens,omitempty"`
	StateLabels           map[string]string `json:"state_labels,omitempty"`
	Revision              uint64            `json:"revision"`
	StateChangeSeq        uint64            `json:"state_change_seq"`
	CompletionSeq         uint64            `json:"completion_seq,omitempty"`
	// LaunchPending is true from agent.start until the agent is ready;
	// it stays true while the agent is blocked at startup (a trust
	// dialog) or its executable never ran (seen on herdr 0.9.5).
	LaunchPending bool `json:"launch_pending,omitempty"`
	// InteractiveReady is true once a started agent is ready for input.
	InteractiveReady bool `json:"interactive_ready,omitempty"`
}

// Pane is the pane record carried by pane.created and pane.updated (PaneInfo).
type Pane struct {
	PaneID                string            `json:"pane_id"`
	TabID                 string            `json:"tab_id"`
	WorkspaceID           string            `json:"workspace_id"`
	TerminalID            string            `json:"terminal_id"`
	Label                 string            `json:"label,omitempty"`
	Agent                 string            `json:"agent,omitempty"`
	DisplayAgent          string            `json:"display_agent,omitempty"`
	AgentStatus           Status            `json:"agent_status"`
	AgentSession          *AgentSession     `json:"agent_session,omitempty"`
	Title                 string            `json:"title,omitempty"`
	TerminalTitle         string            `json:"terminal_title,omitempty"`
	TerminalTitleStripped string            `json:"terminal_title_stripped,omitempty"`
	Cwd                   string            `json:"cwd,omitempty"`
	ForegroundCwd         string            `json:"foreground_cwd,omitempty"`
	Focused               bool              `json:"focused"`
	Tokens                map[string]string `json:"tokens,omitempty"`
	StateLabels           map[string]string `json:"state_labels,omitempty"`
	Revision              uint64            `json:"revision"`
}

// Workspace is one workspace.list record (WorkspaceInfo).
type Workspace struct {
	WorkspaceID string            `json:"workspace_id"`
	Number      int               `json:"number"`
	Label       string            `json:"label"`
	Focused     bool              `json:"focused"`
	PaneCount   int               `json:"pane_count"`
	TabCount    int               `json:"tab_count"`
	ActiveTabID string            `json:"active_tab_id"`
	AgentStatus Status            `json:"agent_status"`
	Tokens      map[string]string `json:"tokens,omitempty"`
}

// Tab is one tab.list record (TabInfo).
type Tab struct {
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Number      int    `json:"number"`
	Label       string `json:"label"`
	Focused     bool   `json:"focused"`
	PaneCount   int    `json:"pane_count"`
	AgentStatus Status `json:"agent_status"`
}
