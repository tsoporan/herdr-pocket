package server_test

import (
	"context"
	"encoding/json"
	"log"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/tsoporan/herdr-pocket/internal/audit"
	"github.com/tsoporan/herdr-pocket/internal/fakeherdr"
	"github.com/tsoporan/herdr-pocket/internal/herdr"
	"github.com/tsoporan/herdr-pocket/internal/hub"
	"github.com/tsoporan/herdr-pocket/internal/queue"
	"github.com/tsoporan/herdr-pocket/internal/server"
	agentstart "github.com/tsoporan/herdr-pocket/internal/start"
)

// Herd pass 3 integration tests for start: fake-herdr, the hub, the
// server's WebSocket, the real start runner and the queue it lines up in.

// startRunner makes and runs a start runner as main does (openStarts), and
// stops it before the audit log closes.
func startRunner(t *testing.T, ctx context.Context, h *hub.Hub, client *herdr.Client, q *queue.Queue, l *audit.Log, readOnly bool) *agentstart.Runner {
	t.Helper()
	r := agentstart.New(agentstart.Config{
		Herdr:     client,
		Kinds:     []string{"claude", "codex"},
		Workspace: h.WorkspacePath,
		NameLive:  h.NameLive,
		HasPane:   h.Has,
		Relist:    h.Relist,
		LineUp: func(by queue.Author, paneID, text string) error {
			_, err := q.Add(by, queue.Target{PaneID: paneID}, text)
			return err
		},
		OnChange: h.StartsChanged,
		OnStep:   server.AuditStart(l),
		Logf:     log.Printf,
	})
	h.SetStarts(r, []hub.StartKind{{Kind: "claude", Label: "Claude Code"}, {Kind: "codex", Label: "Codex"}})
	if readOnly {
		return r
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { r.Run(ctx) })
	t.Cleanup(func() { cancel(); wg.Wait() })
	return r
}

// startDelay is how long the fake's started agent takes to come up, where
// the tests need to see the job running. As on herdr, agent.start answers at
// once and the agent is listed under its name, unknown and launch pending;
// StartAgent's agent.wait returns when it is idle or blocked, and no event
// says so, so the runner's re-list after it is what shows the real status.
const startDelay = 600 * time.Millisecond

func startAgent(t *testing.T, c *websocket.Conn, id string, fields map[string]any) server.Ack {
	t.Helper()
	msg := map[string]any{"type": "start", "id": id, "job_id": "job-00001", "workspace_id": "w1", "kind": "claude", "name": "fix-login", "task": "invented start task"}
	for k, v := range fields {
		msg[k] = v
	}
	send(t, c, msg)
	return ack(t, c, id)
}

func waitJob(t *testing.T, c *websocket.Conn, id string, match func(agentstart.Job) bool) agentstart.Job {
	t.Helper()
	var job agentstart.Job
	waitFor(t, c, "patch", func(p hub.Patch) bool {
		if p.Starts == nil {
			return false
		}
		for _, j := range *p.Starts {
			if j.ID == id && match(j) {
				job = j
				return true
			}
		}
		return false
	})
	return job
}

// jobTrail reads every frame after a start request until the job id ends,
// and returns the ack and each version of the job the patches carried, so a
// test sees the walk from running to the ending, not only the ending.
func jobTrail(t *testing.T, c *websocket.Conn, ackID, jobID string) (server.Ack, []agentstart.Job) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var a *server.Ack
	var trail []agentstart.Job
	for a == nil || len(trail) == 0 || trail[len(trail)-1].State == agentstart.Running {
		var raw json.RawMessage
		if err := wsjson.Read(ctx, c, &raw); err != nil {
			t.Fatalf("job %s: %v (trail %+v)", jobID, err, trail)
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			t.Fatal(err)
		}
		switch head.Type {
		case "ack":
			var v server.Ack
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatal(err)
			}
			if v.ID == ackID {
				if !v.OK {
					t.Fatalf("ack %+v", v)
				}
				a = &v
			}
		case "patch":
			var p hub.Patch
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatal(err)
			}
			if p.Starts == nil {
				continue
			}
			for _, j := range *p.Starts {
				if j.ID == jobID {
					trail = append(trail, j)
				}
			}
		}
	}
	return *a, trail
}

func methods(f *fakeherdr.Server, names ...string) []fakeherdr.Input {
	var out []fakeherdr.Input
	for _, in := range f.InputLog() {
		if slices.Contains(names, in.Method) {
			out = append(out, in)
		}
	}
	return out
}

func TestStartAgentDone(t *testing.T) {
	e := startWith(t, options{starts: true})
	// Long enough to see running, and longer than two of the hub's re-list
	// waits.
	e.fake.SetStartDelay(startDelay)
	c := dial(t, e)
	snap := read[json.RawMessage](t, c)
	for _, want := range []string{`"starts":[]`, `"start_kinds":[{"kind":"claude","label":"Claude Code"},{"kind":"codex","label":"Codex"}]`, `"path":"/home/dev/src/api","agent_count":3`} {
		if !strings.Contains(string(snap), want) {
			t.Errorf("snapshot lacks %s: %s", want, snap)
		}
	}

	task := "invented start task"
	send(t, c, map[string]any{"type": "start", "id": "s1", "job_id": "job-00001", "workspace_id": "w1", "kind": "claude", "name": "fix-login", "task": "  " + task + "\n"})
	_, trail := jobTrail(t, c, "s1", "job-00001")
	// The patches walk running -> done, and each step, once set, stays.
	if trail[0].State != agentstart.Running || len(trail) < 2 {
		t.Fatalf("trail %+v", trail)
	}
	for i := 1; i < len(trail); i++ {
		prev, cur := trail[i-1], trail[i]
		if (prev.OpenedAt != nil && cur.OpenedAt == nil) || (prev.StartedAt != nil && cur.StartedAt == nil) || (prev.SentAt != nil && cur.SentAt == nil) {
			t.Fatalf("a step went back: %+v -> %+v", prev, cur)
		}
		if cur.StartedAt != nil && cur.OpenedAt == nil || cur.SentAt != nil && cur.StartedAt == nil {
			t.Fatalf("steps out of order: %+v", cur)
		}
	}
	j := trail[len(trail)-1]
	// Held running until the hub lists the agent, so its name is taken.
	if !e.hub.NameLive("fix-login") {
		t.Fatal("done, but the hub doesn't list the agent")
	}
	if j.State != agentstart.Done || j.PaneID == "" || j.TabID == "" || j.Cwd != "/home/dev/src/api" || j.By != owner.Login ||
		j.OpenedAt == nil || j.StartedAt == nil || j.SentAt == nil {
		t.Fatalf("job %+v", j)
	}

	in := methods(e.fake, "tab.create", "agent.start", "agent.prompt")
	if len(in) != 3 {
		t.Fatalf("inputs %+v", in)
	}
	var params map[string]any
	if err := json.Unmarshal(in[0].Params, &params); err != nil {
		t.Fatal(err)
	}
	if in[0].Method != "tab.create" || in[0].Label != "fix-login" || in[0].PaneID != j.PaneID || params["focus"] != false || params["cwd"] != "/home/dev/src/api" || params["env"] != nil {
		t.Errorf("tab.create %+v %s", in[0], in[0].Params)
	}
	if err := json.Unmarshal(in[1].Params, &params); err != nil {
		t.Fatal(err)
	}
	if in[1].Method != "agent.start" || in[1].Name != "fix-login" || in[1].Kind != "claude" || in[1].PaneID != j.PaneID || params["args"] != nil {
		t.Errorf("agent.start %+v %s", in[1], in[1].Params)
	}
	if in[2].Method != "agent.prompt" || in[2].PaneID != j.PaneID || in[2].Text != task {
		t.Errorf("prompt %+v", in[2])
	}
	// Nothing else reached herdr's input side: no keys, text or rename.
	if n := len(e.fake.InputLog()); n != 3 {
		t.Errorf("inputs %+v", e.fake.InputLog())
	}
	// StartAgent waited with agent.wait, once.
	if n := e.fake.WaitCount(); n != 1 {
		t.Errorf("agent.wait sent %d times", n)
	}

	// A resend with the same job id opens no second tab.
	if a := startAgent(t, c, "s2", nil); !a.OK {
		t.Fatalf("resend ack %+v", a)
	}
	if n := len(methods(e.fake, "tab.create")); n != 1 {
		t.Fatalf("tab.create %d times", n)
	}
	// The job has ended, so only the live agent holds the name now.
	wantCode(t, startAgent(t, c, "s3", map[string]any{"job_id": "job-00002"}), server.CodeNameTaken)
	if n := len(methods(e.fake, "tab.create")); n != 1 {
		t.Fatalf("tab.create %d times", n)
	}

	type row struct{ method, pane, ws, item, kind, name, via, result string }
	var got []row
	var sums []string
	for _, l := range auditLines(t, e, task) {
		got = append(got, row{l.Method, l.PaneID, l.WorkspaceID, l.ItemID, l.Kind, l.Name, l.Via, l.Result})
		if l.Method == "start" || l.Method == "prompt" {
			sums = append(sums, l.SHA256)
		}
		if l.Login != owner.Login {
			t.Errorf("audit login %+v", l)
		}
	}
	want := []row{
		{"start", "", "w1", "job-00001", "claude", "fix-login", "", "ok"},
		{"tab.create", j.PaneID, "w1", "job-00001", "claude", "fix-login", "start", "ok"},
		{"agent.start", j.PaneID, "w1", "job-00001", "claude", "fix-login", "start", "ok"},
		{"prompt", j.PaneID, "w1", "job-00001", "claude", "fix-login", "start", "ok"},
		{"start", "", "w1", "job-00001", "claude", "fix-login", "", "ok"},
		{"start", "", "w1", "job-00002", "claude", "fix-login", "", "name_taken"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("audit\n got %+v\nwant %+v", got, want)
	}
	// The start lines hash the trimmed task, so they match the prompt's.
	if len(sums) != 4 || len(slices.Compact(slices.Clone(sums))) != 1 {
		t.Errorf("start and prompt hashes %q", sums)
	}

	// A reconnect's snapshot carries the job.
	if s := read[hub.Snapshot](t, dial(t, e)); len(s.Starts) != 1 || s.Starts[0].State != agentstart.Done {
		t.Errorf("snapshot starts %+v", s.Starts)
	}
}

// An agent that asks first (the trust dialog) gets the task through the
// queue once it is idle.
func TestStartAgentAsksFirst(t *testing.T) {
	e := startWith(t, options{starts: true})
	e.fake.SetStartOutcome("claude", fakeherdr.StartNotReady)
	e.fake.SetStartDelay(startDelay)
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	task := "invented trusting task"
	if a := startAgent(t, c, "s1", map[string]any{"task": task}); !a.OK {
		t.Fatalf("ack %+v", a)
	}
	j := waitJob(t, c, "job-00001", func(j agentstart.Job) bool { return j.State != agentstart.Running })
	if j.State != agentstart.AsksFirst || j.StartedAt == nil || j.SentAt != nil {
		t.Fatalf("job %+v", j)
	}
	items := e.queue.List()
	if len(items) != 1 || items[0].Target.PaneID != j.PaneID || items[0].State != queue.Waiting ||
		items[0].Author == nil || items[0].Author.UserID != ownerID || items[0].Text != task {
		t.Fatalf("queue %+v", items)
	}
	if n := len(methods(e.fake, "agent.prompt")); n != 0 {
		t.Fatalf("prompted a blocked agent %d times", n)
	}

	// Trust answered on the desktop: the queue sends it.
	if err := e.fake.SetStatus(j.PaneID, herdr.StatusIdle); err != nil {
		t.Fatal(err)
	}
	waitQueue(t, c, "the task sent", func(q []queue.Item) bool { return len(q) == 1 && q[0].State == queue.PickedUp })
	if got := prompts(e, j.PaneID); !slices.Equal(got, []string{task}) {
		t.Fatalf("prompts %q", got)
	}
}

// A kind whose executable isn't installed: agent.start succeeds, agent.wait
// times out, and the agent stays herdr's under its name, launch pending.
func TestStartAgentNotInstalled(t *testing.T) {
	e := startWith(t, options{starts: true})
	e.fake.SetStartOutcome("codex", fakeherdr.StartTimeout)
	e.fake.SetWaitLimit(300 * time.Millisecond) // not the runner's 30 s timeout_ms
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	if a := startAgent(t, c, "s1", map[string]any{"kind": "codex"}); !a.OK {
		t.Fatalf("ack %+v", a)
	}
	j := waitJob(t, c, "job-00001", func(j agentstart.Job) bool { return j.State != agentstart.Running })
	if j.State != agentstart.Failed || j.Error != herdr.CodeTimeout || j.FailedStep != agentstart.StepStarted || j.PaneID == "" {
		t.Fatalf("job %+v", j)
	}
	if n := len(methods(e.fake, "agent.prompt")); n != 0 {
		t.Fatalf("prompted %d times", n)
	}
	if n, w := len(methods(e.fake, "agent.start")), e.fake.WaitCount(); n != 1 || w != 1 {
		t.Fatalf("agent.start %d times, agent.wait %d times; want one each, never a second start", n, w)
	}
	// The name stays taken: by the job's hold, and by herdr's listing.
	wantCode(t, startAgent(t, c, "s2", map[string]any{"job_id": "job-00002", "kind": "codex"}), server.CodeNameTaken)
	// The runner asked for a re-list after StartAgent; it may still be on
	// its way.
	for deadline := time.Now().Add(2 * time.Second); !e.hub.NameLive("fix-login"); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the hub doesn't list the launched agent under its name")
		}
	}
	if n := len(methods(e.fake, "tab.create")); n != 1 {
		t.Fatalf("tab.create %d times", n)
	}
}

// Every refusal is acked before anything reaches herdr.
func TestStartRefused(t *testing.T) {
	e := startWith(t, options{starts: true, readers: []string{friend.Login}})
	e.fake.SetStartDelay(2 * time.Second) // the first job stays running
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	if a := startAgent(t, c, "s0", nil); !a.OK {
		t.Fatalf("ack %+v", a)
	}
	for _, tc := range []struct {
		fields map[string]any
		code   string
	}{
		{map[string]any{"job_id": "short"}, server.CodeInvalidRequest},
		{map[string]any{"job_id": "job-00002", "task": " "}, server.CodeInvalidRequest},
		{map[string]any{"job_id": "job-00002", "workspace_id": "w404"}, server.CodeNotFound},
		{map[string]any{"job_id": "job-00002", "kind": "gemini"}, server.CodeUnknownKind},
		{map[string]any{"job_id": "job-00002", "name": "Fix Login"}, server.CodeInvalidName},
		{map[string]any{"job_id": "job-00002"}, server.CodeNameTaken}, // job-00001 is starting under it
		{map[string]any{"job_id": "job-00002", "name": "other", "task": strings.Repeat("x", 4001)}, server.CodeTooLong},
	} {
		wantCode(t, startAgent(t, c, "s1", tc.fields), tc.code)
	}
	e.who.set(friend, nil)
	rc := dial(t, e)
	read[hub.Snapshot](t, rc)
	wantCode(t, startAgent(t, rc, "s2", map[string]any{"job_id": "job-00003", "name": "other"}), server.CodeForbidden)
	// Only job-00001 reached herdr: one tab, one start, under its name.
	if in := methods(e.fake, "tab.create"); len(in) != 1 || in[0].Label != "fix-login" {
		t.Fatalf("tab.create %+v", in)
	}
	if in := methods(e.fake, "agent.start"); len(in) > 1 || (len(in) == 1 && in[0].Name != "fix-login") {
		t.Fatalf("agent.start %+v", in)
	}
	if n := e.fake.WaitCount(); n > 1 {
		t.Fatalf("agent.wait sent %d times", n)
	}
}

func TestStartReadOnly(t *testing.T) {
	e := startWith(t, options{starts: true, readOnly: true})
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	wantCode(t, startAgent(t, c, "s1", nil), server.CodeReadOnly)
	if n := len(methods(e.fake, "tab.create")); n != 0 {
		t.Fatalf("tab.create %d times", n)
	}
}

// Without a runner, start is refused.
func TestStartOff(t *testing.T) {
	e := start(t)
	c := dial(t, e)
	read[hub.Snapshot](t, c)
	wantCode(t, startAgent(t, c, "s1", nil), server.CodeInvalidRequest)
}
