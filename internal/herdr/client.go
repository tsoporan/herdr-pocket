// Package herdr is a client for herdr's local socket API (protocol 22).
//
// Wire behaviour, checked against herdr 0.9.5 on 2026-11-20 with read-only
// probes:
//
//   - A request is one JSON line, {"id","method","params"}. herdr writes one
//     response line, {"id","result"} or {"id","error":{"code","message"}},
//     and then closes the connection. So every Call dials its own connection.
//   - events.subscribe answers {"id","result":{"type":"subscription_started"}}
//     and then streams event lines with no id. See Subscribe for the rest.
package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"
)

// MinProtocol is the oldest herdr socket protocol this client supports.
const MinProtocol = 22

// Error codes herdr returns that callers branch on.
const (
	CodeInvalidRequest = "invalid_request"
	CodeAgentBlocked   = "agent_blocked"
	CodePaneNotFound   = "pane_not_found"
	CodeAgentNotFound  = "agent_not_found" // an agent.* target that matches no agent (seen on herdr 0.9.5)
)

// Error is an error response from herdr, carrying its error code.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return "herdr: " + e.Code + ": " + e.Message }

// requestTimeout bounds one request: the dial, the write and the response
// read. A herdr that accepts and never answers then fails the call instead of
// blocking it forever.
var requestTimeout = 5 * time.Second

// ErrProtocolTooOld reports that herdr speaks an older protocol than MinProtocol.
var ErrProtocolTooOld = errors.New("herdr protocol too old")

// Client talks to one herdr server over its unix socket.
type Client struct {
	socketPath string
	nextID     atomic.Uint64
}

// NewClient returns a client for the herdr socket at socketPath.
func NewClient(socketPath string) *Client {
	return &Client{socketPath: socketPath}
}

// SocketPath returns the socket this client dials.
func (c *Client) SocketPath() string { return c.socketPath }

// ResolveSocket picks the herdr socket: the flag value, then
// $HERDR_SOCKET_PATH, then ~/.config/herdr/herdr.sock.
func ResolveSocket(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if env := os.Getenv("HERDR_SOCKET_PATH"); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find herdr socket: %w", err)
	}
	return filepath.Join(home, ".config", "herdr", "herdr.sock"), nil
}

type request struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

// response is one line from herdr: a result, an error, or (on a stream) an event.
type response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *Error          `json:"error"`
	Event  string          `json:"event"`
	Data   json.RawMessage `json:"data"`
}

// dial connects and sends one request. The connection is closed when ctx is
// done; the returned stop func detaches that, and the caller must call it.
func (c *Client) dial(ctx context.Context, method string, params any) (net.Conn, *json.Decoder, string, func() bool, error) {
	if params == nil {
		params = struct{}{}
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("herdr %s: %w", method, err)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	id := strconv.FormatUint(c.nextID.Add(1), 10)
	line, err := json.Marshal(request{ID: id, Method: method, Params: params})
	if err == nil {
		_, err = conn.Write(append(line, '\n'))
	}
	if err != nil {
		stop()
		conn.Close()
		return nil, nil, "", nil, fmt.Errorf("herdr %s: %w", method, ctxErr(ctx, err))
	}
	// json.Decoder has no line-length limit, unlike bufio.Scanner's 64 KB.
	return conn, json.NewDecoder(conn), id, stop, nil
}

// readResponse reads the response to request id and decodes its result into out.
func readResponse(ctx context.Context, dec *json.Decoder, method, id string, out any) error {
	var resp response
	if err := dec.Decode(&resp); err != nil {
		return fmt.Errorf("herdr %s: read response: %w", method, ctxErr(ctx, err))
	}
	if resp.Error != nil {
		return resp.Error
	}
	if resp.ID != id {
		return fmt.Errorf("herdr %s: response id %q, want %q", method, resp.ID, id)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Result, out); err != nil {
		return fmt.Errorf("herdr %s: decode result: %w", method, err)
	}
	return nil
}

func ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// Call sends one request and decodes the result into out (which may be nil).
// A herdr error response comes back as *Error.
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	return c.call(ctx, requestTimeout, method, params, out)
}

// call is Call with its own deadline, for methods herdr answers only after
// waiting (agent.start, agent.prompt with wait).
func (c *Client) call(ctx context.Context, timeout time.Duration, method string, params, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, dec, id, stop, err := c.dial(ctx, method, params)
	if err != nil {
		return err
	}
	defer conn.Close()
	defer stop()
	return readResponse(ctx, dec, method, id, out)
}

// Ping returns herdr's version and protocol.
func (c *Client) Ping(ctx context.Context) (Pong, error) {
	var p Pong
	err := c.Call(ctx, "ping", nil, &p)
	return p, err
}

// CheckProtocol pings herdr and fails with ErrProtocolTooOld when its
// protocol is older than MinProtocol.
func (c *Client) CheckProtocol(ctx context.Context) (Pong, error) {
	p, err := c.Ping(ctx)
	if err != nil {
		return p, err
	}
	if p.Protocol < MinProtocol {
		return p, fmt.Errorf("%w: herdr %s speaks protocol %d, herdr-pocket needs %d or later; update herdr",
			ErrProtocolTooOld, p.Version, p.Protocol, MinProtocol)
	}
	return p, nil
}

// Agents returns every agent pane (agent.list).
func (c *Client) Agents(ctx context.Context) ([]Agent, error) {
	var r struct {
		Agents []Agent `json:"agents"`
	}
	err := c.Call(ctx, "agent.list", nil, &r)
	return r.Agents, err
}

// Workspaces returns every workspace (workspace.list).
func (c *Client) Workspaces(ctx context.Context) ([]Workspace, error) {
	var r struct {
		Workspaces []Workspace `json:"workspaces"`
	}
	err := c.Call(ctx, "workspace.list", nil, &r)
	return r.Workspaces, err
}

// Tabs returns the tabs of one workspace, or of all workspaces when
// workspaceID is empty (tab.list).
func (c *Client) Tabs(ctx context.Context, workspaceID string) ([]Tab, error) {
	params := struct {
		WorkspaceID string `json:"workspace_id,omitempty"`
	}{workspaceID}
	var r struct {
		Tabs []Tab `json:"tabs"`
	}
	err := c.Call(ctx, "tab.list", params, &r)
	return r.Tabs, err
}
