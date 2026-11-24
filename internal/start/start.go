// Package start starts agents from the phone: it opens a new tab in a
// workspace herdr has open, starts an agent of the chosen kind and name in its root pane,
// and sends the task.
//
// Begin checks a request completely before any herdr call, since a failure
// after tab.create leaves a tab the phone can't close. Run then does the
// steps on the server's context, never a socket's, so a phone that drops
// mid-start still gets the result: the jobs are in the hub's snapshot and
// patches. Jobs are kept in memory only, the finished ones for an hour (at
// most 20). A restart mid-job leaves at most an open tab, which the app's
// "asks first" and "failed" wording already covers.
//
// The task text stays inside the runner; a Job never carries it, so it
// never reaches the snapshot or a log.
package start

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tsoporan/herdr-pocket/internal/herdr"
	"github.com/tsoporan/herdr-pocket/internal/queue"
)

const (
	maxTask    = 4000 // bytes, after trimming; the queue's limit too
	keepFor    = time.Hour
	keepMax    = 20 // finished jobs kept
	startTries = 4  // agent.start, and up to 3 retries on agent_pane_busy
	panePoll   = 100 * time.Millisecond
	relistGap  = time.Second // between re-lists asked for while waiting for the agent
	// nameHold is how long a job that ended after agent.start may have
	// given herdr its name keeps that name from a new start, so a quick
	// retry can't open a second tab only to be refused agent_name_taken.
	nameHold = 2 * time.Minute

	defaultStartTimeout  = 30 * time.Second
	defaultPromptTimeout = 10 * time.Second
	defaultPaneWait      = 10 * time.Second
	defaultBusyRetry     = 500 * time.Millisecond
)

var (
	idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
	// namePattern is herdr's own rule for agent names (checked on herdr
	// 0.9.5): it refuses anything else with invalid_agent_name.
	namePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
)

// Begin's refusals. The server maps each to an ack code.
var (
	ErrInvalid          = errors.New("start: invalid request")
	ErrUnknownWorkspace = errors.New("start: no such workspace")
	ErrUnknownKind      = errors.New("start: kind not offered")
	ErrInvalidName      = errors.New("start: invalid agent name")
	ErrNameTaken        = errors.New("start: agent name taken")
	ErrTooLong          = errors.New("start: task too long")
)

// State is where a job is: running, then one of the endings.
type State string

const (
	Running   State = "running"
	Done      State = "done"       // the agent picked the task up
	AsksFirst State = "asks_first" // the agent asks something first; the task is lined up
	Stalled   State = "stalled"    // agent_prompt_stalled: the task may not have gone; not resent
	Failed    State = "failed"     // see Job.Error and Job.FailedStep
	Unsure    State = "unsure"     // a transport error after a request was written; not retried
)

// The steps, as Job.FailedStep names them.
const (
	StepOpened  = "opened"
	StepStarted = "started"
	StepSent    = "sent"
)

// Job.Error values that aren't herdr's codes.
const (
	ErrorLostPane    = "lost_pane"    // the agent asked first, and its pane never showed in the hub
	ErrorQueueFailed = "queue_failed" // the agent asked first, and lining the task up failed
	ErrorUnsure      = "unsure"       // with State Unsure
	ErrorUnreachable = "unreachable"  // herdr's socket refused the dial: nothing reached it
	ErrorStopped     = "stopped"      // the server stopped between steps
)

// Job is one start as the app sees it. It never carries the task text.
type Job struct {
	ID          string     `json:"id"`
	By          string     `json:"by"`
	WorkspaceID string     `json:"workspace_id"`
	Kind        string     `json:"kind"`
	Name        string     `json:"name"`
	Cwd         string     `json:"cwd,omitempty"`
	PaneID      string     `json:"pane_id,omitempty"`
	TabID       string     `json:"tab_id,omitempty"`
	State       State      `json:"state"`
	Began       time.Time  `json:"began"`
	OpenedAt    *time.Time `json:"opened_at,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	SentAt      *time.Time `json:"sent_at,omitempty"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	// Error is herdr's code, or one of the Error* values; never raw text.
	Error string `json:"error,omitempty"`
	// FailedStep is the step that didn't complete: opened, started or sent.
	FailedStep string `json:"failed_step,omitempty"`
}

// Request is one start as the app asks for it. ID is made by the app, so a
// resend or a double tap names the same job.
type Request struct {
	ID, WorkspaceID, Kind, Name, Task string
}

// Herdr is what the runner calls: *herdr.Client's start methods.
type Herdr interface {
	CreateTab(ctx context.Context, workspaceID, cwd, label string) (herdr.Tab, herdr.Pane, error)
	StartAgent(ctx context.Context, name, kind, paneID string, timeout time.Duration) (herdr.Agent, error)
	PromptWait(ctx context.Context, target, text string, until []herdr.Status, timeout time.Duration) (herdr.Agent, error)
}

// Config configures a Runner. Herdr, Workspace, NameLive, HasPane and
// LineUp are required. Workspace, NameLive, HasPane and Relist are the
// hub's and are never called under the runner's lock, so the hub may call
// List under its own.
type Config struct {
	Now   func() time.Time
	Herdr Herdr
	// Kinds are the agent kinds offered.
	Kinds []string
	// Workspace reports whether the hub lists the workspace, and the folder
	// a new tab there opens in ("" leaves it to herdr: the workspace's
	// active pane's folder).
	Workspace func(id string) (path string, ok bool)
	// NameLive reports whether an agent herdr lists has that name.
	NameLive func(name string) bool
	// HasPane reports whether the hub lists an agent in the pane, which
	// means the queue has seen it too.
	HasPane func(paneID string) bool
	// Relist asks the hub to re-list soon. herdr sends no event once a new
	// agent is up (idle or blocked), so the runner asks after StartAgent
	// returns, and again while it waits for the agent. Optional.
	Relist func()
	// LineUp lines the task up for the pane in the work queue, by its
	// author, for an agent that asks something first.
	LineUp func(by queue.Author, paneID, text string) error
	// OnChange is called, without the runner's lock, after a job changes.
	OnChange func()
	// OnStep is called after each herdr call, for the audit: method is
	// tab.create, agent.start or prompt; payload is the task for prompt
	// and nil otherwise, for its size and hash.
	OnStep func(j Job, method string, payload []byte, err error)
	Logf   func(string, ...any)

	StartTimeout  time.Duration // agent.start's timeout_ms; 0 means 30 s
	PromptTimeout time.Duration // the prompt's wait; 0 means 10 s
	PaneWait      time.Duration // how long an asking agent's pane may take to show; 0 means 10 s
	BusyRetry     time.Duration // between agent.start tries on agent_pane_busy; 0 means 0.5 s
}

type job struct {
	Job
	task     string
	author   queue.Author
	launched bool // Run has picked it up
	// agentUp: agent.start succeeded, so herdr lists the agent under its
	// name even if the wait for it to come up then failed.
	agentUp bool
}

// Runner holds the jobs and runs them.
type Runner struct {
	cfg  Config
	wake chan struct{}

	mu   sync.Mutex
	jobs []*job // in the order begun
}

// New returns a runner; jobs begun before Run wait for it.
func New(cfg Config) *Runner {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.OnChange == nil {
		cfg.OnChange = func() {}
	}
	if cfg.OnStep == nil {
		cfg.OnStep = func(Job, string, []byte, error) {}
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Relist == nil {
		cfg.Relist = func() {}
	}
	cfg.StartTimeout = cmp.Or(cfg.StartTimeout, defaultStartTimeout)
	cfg.PromptTimeout = cmp.Or(cfg.PromptTimeout, defaultPromptTimeout)
	cfg.PaneWait = cmp.Or(cfg.PaneWait, defaultPaneWait)
	cfg.BusyRetry = cmp.Or(cfg.BusyRetry, defaultBusyRetry)
	return &Runner{cfg: cfg, wake: make(chan struct{}, 1)}
}

// Begin checks a request and queues it for Run. A known ID returns that
// job and starts nothing. Nothing reaches herdr before every check passes.
//
// Only a job the runner still holds is known: an ID is forgotten with its
// job, an hour after it ends, past the last 20 finished, or on a restart.
// The app doesn't resend an ID the server took, nor one older than an hour.
func (r *Runner) Begin(by queue.Author, req Request) (Job, error) {
	if by.Login == "" || !idPattern.MatchString(req.ID) {
		return Job{}, fmt.Errorf("%w: id must be 8 to 64 of A-Z a-z 0-9 _ -", ErrInvalid)
	}
	if j, ok := r.find(req.ID); ok {
		return j, nil
	}
	task := strings.TrimSpace(req.Task)
	switch {
	case task == "" || !utf8.ValidString(task):
		return Job{}, fmt.Errorf("%w: no task", ErrInvalid)
	case len(task) > maxTask:
		return Job{}, fmt.Errorf("%w: at most %d bytes", ErrTooLong, maxTask)
	}
	cwd, ok := r.cfg.Workspace(req.WorkspaceID)
	if !ok {
		return Job{}, fmt.Errorf("%w: %q", ErrUnknownWorkspace, req.WorkspaceID)
	}
	if !slices.Contains(r.cfg.Kinds, req.Kind) {
		return Job{}, fmt.Errorf("%w: %q", ErrUnknownKind, req.Kind)
	}
	if !namePattern.MatchString(req.Name) {
		return Job{}, fmt.Errorf("%w: a lowercase letter, then up to 31 of a-z 0-9 _ -", ErrInvalidName)
	}
	if r.cfg.NameLive(req.Name) {
		return Job{}, fmt.Errorf("%w: %q", ErrNameTaken, req.Name)
	}

	r.mu.Lock()
	// Checked again under the lock: two Begins may race.
	if j := r.findLocked(req.ID); j != nil {
		out := j.Job
		r.mu.Unlock()
		return out, nil
	}
	now := r.cfg.Now()
	if slices.ContainsFunc(r.jobs, func(j *job) bool { return j.Name == req.Name && holdsName(j, now) }) {
		r.mu.Unlock()
		return Job{}, fmt.Errorf("%w: %q is starting, or may still be herdr's", ErrNameTaken, req.Name)
	}
	r.trimLocked(now)
	by.Tags = slices.Clone(by.Tags)
	j := &job{
		Job: Job{
			ID: req.ID, By: by.Login, WorkspaceID: req.WorkspaceID, Kind: req.Kind, Name: req.Name,
			Cwd: cwd, State: Running, Began: now,
		},
		task:   task,
		author: by,
	}
	r.jobs = append(r.jobs, j)
	out := j.Job
	r.mu.Unlock()

	r.poke()
	r.cfg.OnChange()
	return out, nil
}

// List returns the running jobs and those finished in the last hour (at
// most 20), in the order begun. It takes only the runner's short lock, so
// the hub may call it under its own.
func (r *Runner) List() []Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trimLocked(r.cfg.Now())
	out := make([]Job, len(r.jobs))
	for i, j := range r.jobs {
		out[i] = j.Job
	}
	return out
}

// Run runs each begun job on ctx until ctx ends, then waits for the jobs
// under way: a step already sent to herdr runs to its own deadline, so its
// result is recorded and audited.
func (r *Runner) Run(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		r.mu.Lock()
		var todo []*job
		for _, j := range r.jobs {
			if !j.launched {
				j.launched = true
				todo = append(todo, j)
			}
		}
		r.mu.Unlock()
		for _, j := range todo {
			wg.Go(func() { r.run(ctx, j) })
		}
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		}
	}
}

func (r *Runner) poke() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Runner) find(id string) (Job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if j := r.findLocked(id); j != nil {
		return j.Job, true
	}
	return Job{}, false
}

func (r *Runner) findLocked(id string) *job {
	for _, j := range r.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

// holdsName reports whether a job keeps its name from a new start: while it
// runs, and for nameHold after an ending where herdr may hold the name but
// the hub may not list the agent: the agent started (asking first, say, or
// it never came up), or agent.start went unanswered.
func holdsName(j *job, now time.Time) bool {
	if j.State == Running {
		return true
	}
	mayHold := j.StartedAt != nil || j.agentUp ||
		j.FailedStep == StepStarted && (j.Error == ErrorUnsure || j.Error == herdr.CodeTimeout)
	return mayHold && now.Sub(*j.EndedAt) < nameHold
}

// trimLocked forgets jobs finished over an hour ago, and all but the last
// 20 finished. Running jobs are kept. Callers hold mu.
func (r *Runner) trimLocked(now time.Time) {
	finished := 0
	keep := make([]*job, 0, len(r.jobs))
	for i := len(r.jobs) - 1; i >= 0; i-- {
		j := r.jobs[i]
		if j.State != Running {
			if now.Sub(*j.EndedAt) >= keepFor {
				continue
			}
			if finished++; finished > keepMax {
				continue
			}
		}
		keep = append(keep, j)
	}
	if len(keep) == len(r.jobs) {
		return
	}
	slices.Reverse(keep)
	r.jobs = keep
}

// update changes a job under the lock and returns the result, then tells
// OnChange.
func (r *Runner) update(j *job, f func(j *Job, now time.Time)) Job {
	r.mu.Lock()
	f(&j.Job, r.cfg.Now())
	out := j.Job
	r.mu.Unlock()
	r.cfg.OnChange()
	return out
}

// end finishes a job.
func (r *Runner) end(j *job, s State, code, step string) Job {
	return r.update(j, func(x *Job, now time.Time) {
		x.State, x.Error, x.FailedStep, x.EndedAt = s, code, step, &now
	})
}

// fail ends a job after a herdr call at step failed: herdr's own refusal is
// failed with its code; a dial that failed never reached herdr; anything
// else may have, so the job is unsure.
func (r *Runner) fail(j *job, step string, err error) Job {
	var herr *herdr.Error
	var dial *net.OpError
	switch {
	case errors.As(err, &herr):
		return r.end(j, Failed, herr.Code, step)
	case errors.As(err, &dial) && dial.Op == "dial":
		return r.end(j, Failed, ErrorUnreachable, step)
	}
	r.cfg.Logf("start: job %s: %s: %v; it may have gone, so it is not retried", j.ID, step, err)
	return r.end(j, Unsure, ErrorUnsure, step)
}

func code(err error) string {
	var herr *herdr.Error
	if errors.As(err, &herr) {
		return herr.Code
	}
	return ""
}

// sleep waits d, or reports false when ctx ends first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// run does a job's steps. Each herdr call runs on a context that ctx's end
// doesn't cancel: a call abandoned midway may still have reached herdr, and
// the client bounds each call itself. ctx is checked between steps.
func (r *Runner) run(ctx context.Context, j *job) {
	call := context.WithoutCancel(ctx)
	if ctx.Err() != nil {
		r.end(j, Failed, ErrorStopped, StepOpened)
		return
	}

	// 1. Open a tab, labelled with the agent's name, never focused.
	tab, pane, err := r.cfg.Herdr.CreateTab(call, j.WorkspaceID, j.Cwd, j.Name)
	var out Job
	switch {
	case err != nil:
		out = r.fail(j, StepOpened, err)
	case pane.PaneID == "":
		// A tab may be open, but there is no pane to start in.
		r.cfg.Logf("start: job %s: tab.create returned no pane", j.ID)
		out = r.end(j, Unsure, ErrorUnsure, StepOpened)
	default:
		out = r.update(j, func(x *Job, now time.Time) {
			x.TabID, x.PaneID, x.OpenedAt = tab.TabID, pane.PaneID, &now
			if pane.Cwd != "" {
				x.Cwd = pane.Cwd
			}
		})
	}
	r.cfg.OnStep(out, "tab.create", nil, err)
	if out.State != Running {
		return
	}
	if ctx.Err() != nil {
		r.end(j, Failed, ErrorStopped, StepStarted)
		return
	}

	// 2. Start the agent and wait for it to come up (StartAgent is
	// agent.start, then agent.wait). agent_pane_busy (the shell isn't ready
	// yet) is the only refusal retried; herdr refuses before typing
	// anything. An agent that was launched (a pane id back) is never
	// started again, whatever the wait said.
	for try := 1; ; try++ {
		var a herdr.Agent
		a, err = r.cfg.Herdr.StartAgent(call, j.Name, j.Kind, out.PaneID, r.cfg.StartTimeout)
		r.cfg.OnStep(out, "agent.start", nil, err)
		if a.PaneID != "" {
			r.mu.Lock()
			j.agentUp = true // read by Begin, under mu
			r.mu.Unlock()
			break
		}
		if code(err) != herdr.CodeAgentPaneBusy || try == startTries {
			break
		}
		if !sleep(ctx, r.cfg.BusyRetry) {
			r.end(j, Failed, ErrorStopped, StepStarted)
			return
		}
	}
	// herdr lists the agent by name from agent.start on, but its start
	// events come before it is up and none comes after, so a re-list they
	// caused shows it unknown, launch pending.
	r.cfg.Relist()
	switch {
	case err == nil:
		out = r.update(j, func(x *Job, now time.Time) { x.StartedAt = &now })
	case code(err) == herdr.CodeAgentNotReady:
		// Started, but asking something first (often "Do you trust this
		// folder?"). The queue sends the task once it is idle.
		r.update(j, func(x *Job, now time.Time) { x.StartedAt = &now })
		r.lineUp(ctx, j)
		return
	default:
		// Refused before anything was typed (no hold), or launched but
		// never came up: herdr's timeout when the executable isn't
		// installed, or another error from the wait. A launched agent
		// keeps its name (agentUp), and it is not started again.
		r.fail(j, StepStarted, err)
		return
	}
	// Hold the job running until the hub lists the agent: until then only
	// this job keeps its name taken (NameLive can't see it yet), and "Open
	// it" needs an agent the app knows. The prompt goes to herdr, not the
	// hub, so it is sent even if the pane never shows.
	if !r.waitAgent(ctx, out.PaneID, j.Name) && ctx.Err() == nil {
		r.cfg.Logf("start: job %s: pane %s not listed yet; sending the task anyway", j.ID, out.PaneID)
	}
	if ctx.Err() != nil {
		r.end(j, Failed, ErrorStopped, StepSent)
		return
	}

	// 3. Send the task to the pane (not the name, which may change) and
	// wait until the agent picks it up.
	until := []herdr.Status{herdr.StatusWorking, herdr.StatusBlocked}
	_, err = r.cfg.Herdr.PromptWait(call, out.PaneID, j.task, until, r.cfg.PromptTimeout)
	switch {
	case err == nil:
		out = r.update(j, func(x *Job, now time.Time) {
			x.SentAt, x.State, x.EndedAt = &now, Done, &now
		})
	case code(err) == herdr.CodeAgentBlocked:
		// It began asking something between the start and the prompt;
		// herdr typed nothing, so the task is lined up as for not_ready.
		out = r.copy(j)
	case code(err) == herdr.CodeAgentPromptStalled, code(err) == herdr.CodeTimeout:
		// A waiting prompt submits the text before it waits, so on a
		// timeout too the task may have gone: not resent.
		out = r.end(j, Stalled, code(err), StepSent)
	default:
		out = r.fail(j, StepSent, err)
	}
	r.cfg.OnStep(out, "prompt", []byte(j.task), err)
	if code(err) == herdr.CodeAgentBlocked {
		r.lineUp(ctx, j)
	}
}

func (r *Runner) copy(j *job) Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	return j.Job
}

// lineUp hands the task to the queue once the hub lists the agent's pane.
// The queue refuses a pane it hasn't seen, and drops an item for a pane
// missing from the agent set it last observed, so Add must not run before
// the hub has listed the new agent.
func (r *Runner) lineUp(ctx context.Context, j *job) {
	if !r.waitAgent(ctx, j.PaneID, j.Name) {
		if ctx.Err() != nil {
			r.end(j, Failed, ErrorStopped, StepSent)
			return
		}
		r.cfg.Logf("start: job %s: pane %s never showed; the task wasn't lined up", j.ID, j.PaneID)
		r.end(j, Failed, ErrorLostPane, StepSent)
		return
	}
	if err := r.cfg.LineUp(j.author, j.PaneID, j.task); err != nil {
		r.cfg.Logf("start: job %s: lining up for %s: %v", j.ID, j.PaneID, err)
		r.end(j, Failed, ErrorQueueFailed, StepSent)
		return
	}
	r.end(j, AsksFirst, "", "")
}

// waitAgent polls the hub until it lists the agent in the pane under its
// name, for up to PaneWait, asking for a re-list each second meanwhile.
func (r *Runner) waitAgent(ctx context.Context, paneID, name string) bool {
	listed := func() bool { return r.cfg.HasPane(paneID) && r.cfg.NameLive(name) }
	deadline := time.NewTimer(r.cfg.PaneWait)
	defer deadline.Stop()
	tick := time.NewTicker(min(panePoll, r.cfg.PaneWait))
	defer tick.Stop()
	asked := r.cfg.Now()
	for {
		if listed() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return listed()
		case <-tick.C:
		}
		if now := r.cfg.Now(); now.Sub(asked) >= relistGap {
			r.cfg.Relist()
			asked = now
		}
	}
}
