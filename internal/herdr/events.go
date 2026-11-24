package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// Event kinds, in dotted form. herdr writes global events with an underscore
// name ({"event":"pane_focused","data":{"type":"pane_focused",...}}) and
// per-pane subscription events dotted
// ({"event":"pane.agent_status_changed","data":{...}}, no data.type).
// Stream.Next normalises both to the dotted form, which is also the
// subscription type. Every EventKind in the protocol 22 schema maps this way
// (first underscore becomes a dot).
const (
	EventWorkspaceCreated         = "workspace.created"
	EventWorkspaceUpdated         = "workspace.updated"
	EventWorkspaceMetadataUpdated = "workspace.metadata_updated"
	EventWorkspaceRenamed         = "workspace.renamed"
	EventWorkspaceMoved           = "workspace.moved"
	EventWorkspaceReordered       = "workspace.reordered"
	EventWorkspaceClosed          = "workspace.closed"
	EventWorkspaceFocused         = "workspace.focused"
	EventTabCreated               = "tab.created"
	EventTabClosed                = "tab.closed"
	EventTabFocused               = "tab.focused"
	EventTabRenamed               = "tab.renamed"
	EventTabMoved                 = "tab.moved"
	EventPaneCreated              = "pane.created"
	EventPaneClosed               = "pane.closed"
	EventPaneUpdated              = "pane.updated"
	EventPaneFocused              = "pane.focused"
	EventPaneAgentDetected        = "pane.agent_detected"
	EventPaneAgentStatusChanged   = "pane.agent_status_changed"
)

// Subscription is one entry of events.subscribe's subscriptions list.
// PaneID is required for pane.agent_status_changed (herdr answers
// invalid_request "missing field `pane_id`" otherwise).
type Subscription struct {
	Type   string `json:"type"`
	PaneID string `json:"pane_id,omitempty"`
}

// GlobalSubscriptions returns the global event types the inventory needs:
// panes, tabs and workspaces. tab.* and workspace.* are not valid types, so
// each is listed.
func GlobalSubscriptions() []Subscription {
	types := []string{
		EventPaneCreated, EventPaneClosed, EventPaneUpdated, EventPaneFocused, EventPaneAgentDetected,
		EventTabCreated, EventTabClosed, EventTabFocused, EventTabRenamed, EventTabMoved,
		EventWorkspaceCreated, EventWorkspaceUpdated, EventWorkspaceMetadataUpdated, EventWorkspaceRenamed,
		EventWorkspaceMoved, EventWorkspaceReordered, EventWorkspaceClosed, EventWorkspaceFocused,
	}
	subs := make([]Subscription, len(types))
	for i, t := range types {
		subs[i] = Subscription{Type: t}
	}
	return subs
}

// AgentStatusSubscription subscribes to status changes of one agent pane.
func AgentStatusSubscription(paneID string) Subscription {
	return Subscription{Type: EventPaneAgentStatusChanged, PaneID: paneID}
}

// Event is one streamed event. Kind is dotted (see the Event* constants);
// Data is the raw payload, decoded with Decode into one of the payload types
// below.
type Event struct {
	Kind string
	Data json.RawMessage
}

// Decode unmarshals the event payload into v.
func (e Event) Decode(v any) error {
	if err := json.Unmarshal(e.Data, v); err != nil {
		return fmt.Errorf("herdr event %s: %w", e.Kind, err)
	}
	return nil
}

// AgentStatusChanged is the pane.agent_status_changed payload.
type AgentStatusChanged struct {
	PaneID       string            `json:"pane_id"`
	WorkspaceID  string            `json:"workspace_id"`
	AgentStatus  Status            `json:"agent_status"`
	Agent        string            `json:"agent,omitempty"`
	DisplayAgent string            `json:"display_agent,omitempty"`
	Title        string            `json:"title,omitempty"`
	StateLabels  map[string]string `json:"state_labels,omitempty"`
}

// PaneEvent is the pane.created and pane.updated payload.
type PaneEvent struct {
	Pane Pane `json:"pane"`
}

// PaneRef is the pane.closed and pane.focused payload.
type PaneRef struct {
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
}

// AgentDetected is the pane.agent_detected payload. Released is true when the
// agent left the pane; Agent is empty then.
type AgentDetected struct {
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	Agent       string `json:"agent,omitempty"`
	FinalStatus Status `json:"final_status,omitempty"`
	Released    bool   `json:"released,omitempty"`
}

// TabEvent covers the tab.* payloads: Tab for created; TabID and WorkspaceID
// for closed, focused, renamed (with Label) and moved (with Tabs).
type TabEvent struct {
	Tab         *Tab   `json:"tab,omitempty"`
	TabID       string `json:"tab_id,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	Label       string `json:"label,omitempty"`
	Tabs        []Tab  `json:"tabs,omitempty"`
}

// WorkspaceEvent covers the workspace.* payloads: Workspace for created,
// updated and metadata_updated (and sometimes closed); WorkspaceID for closed,
// focused and renamed (with Label); Workspaces for moved and reordered.
type WorkspaceEvent struct {
	Workspace   *Workspace  `json:"workspace,omitempty"`
	WorkspaceID string      `json:"workspace_id,omitempty"`
	Label       string      `json:"label,omitempty"`
	Workspaces  []Workspace `json:"workspaces,omitempty"`
}

// Stream is one events.subscribe connection.
type Stream struct {
	ctx       context.Context
	conn      net.Conn
	dec       *json.Decoder
	stop      func() bool
	closeOnce sync.Once
}

// Subscribe opens a dedicated connection and subscribes it to subs. It returns
// once herdr has answered subscription_started, or with *Error when herdr
// rejects the request.
//
// The subscription set is fixed for the connection's life. Probed on herdr
// 0.9.5 (2026-11-20): after subscription_started, a second events.subscribe,
// or any other request, on the same connection makes herdr reset the
// connection (ECONNRESET), which ends the stream; the probe did not see
// whether an error line came first. One events.subscribe can carry many
// subscriptions, global and per-pane mixed, but the whole request fails with
// pane_not_found if any pane_id doesn't exist (for example a pane that closed
// after agent.list). So to watch a new agent pane,
// open another Stream (or replace the old one), and re-read agent.list after
// every (re)subscribe so a change in the gap isn't lost.
//
// The stream is closed when ctx is done or Close is called.
func (c *Client) Subscribe(ctx context.Context, subs []Subscription) (*Stream, error) {
	params := struct {
		Subscriptions []Subscription `json:"subscriptions"`
	}{subs}
	conn, dec, id, stop, err := c.dial(ctx, "events.subscribe", params)
	if err != nil {
		return nil, err
	}
	var ack struct {
		Type string `json:"type"`
	}
	// Bound the ack like any request, then clear the deadline: the stream
	// itself may be quiet for as long as herdr is.
	err = conn.SetReadDeadline(time.Now().Add(requestTimeout))
	if err == nil {
		err = readResponse(ctx, dec, "events.subscribe", id, &ack)
	}
	if err == nil {
		err = conn.SetReadDeadline(time.Time{})
	}
	if err != nil {
		stop()
		conn.Close()
		return nil, err
	}
	if ack.Type != "subscription_started" {
		stop()
		conn.Close()
		return nil, fmt.Errorf("herdr events.subscribe: unexpected result type %q", ack.Type)
	}
	return &Stream{ctx: ctx, conn: conn, dec: dec, stop: stop}, nil
}

// Next blocks until the next event. It returns an error when the stream ends
// (herdr closed it, ctx was cancelled, or Close was called).
func (s *Stream) Next() (Event, error) {
	var line response
	if err := s.dec.Decode(&line); err != nil {
		return Event{}, fmt.Errorf("herdr event stream: %w", ctxErr(s.ctx, err))
	}
	if line.Error != nil {
		return Event{}, line.Error
	}
	if line.Event == "" {
		return Event{}, fmt.Errorf("herdr event stream: line without an event")
	}
	return Event{Kind: normalizeKind(line.Event), Data: line.Data}, nil
}

// Close ends the stream.
func (s *Stream) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.stop()
		err = s.conn.Close()
	})
	return err
}

// normalizeKind turns "pane_focused" into "pane.focused" and leaves dotted
// kinds alone.
func normalizeKind(kind string) string {
	if strings.Contains(kind, ".") {
		return kind
	}
	return strings.Replace(kind, "_", ".", 1)
}
