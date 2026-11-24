package hub

import (
	"context"
	"reflect"

	"github.com/tsoporan/herdr-pocket/internal/start"
)

// Starter is the start runner (*start.Runner) as the hub carries it: its
// jobs, for the snapshot and patches. List takes only the runner's short
// lock, so the hub calls it under its own.
type Starter interface {
	List() []start.Job
}

// StartKind is one agent kind the app may start, with the app's label.
type StartKind struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

// SetStarts attaches the start runner and the kinds it offers. The
// runner's Config.OnChange must call StartsChanged.
func (h *Hub) SetStarts(s Starter, kinds []StartKind) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.starter = s
	h.startKinds = append([]StartKind{}, kinds...)
	h.starts = h.startsListLocked()
}

// StartsChanged tells the hub the start jobs changed; it patches the app.
// It only signals, so it may be called from anywhere.
func (h *Hub) StartsChanged() {
	select {
	case h.startsKick <- struct{}{}:
	default:
	}
}

// Relist asks for a re-list soon. herdr sends no event once a new agent
// has its name and status, so the start runner asks after agent.start
// returns. It only signals, so it may be called from anywhere.
func (h *Hub) Relist() {
	select {
	case h.relistKick <- struct{}{}:
	default:
	}
}

// startsLoop publishes the jobs each time StartsChanged signals.
func (h *Hub) startsLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.startsKick:
		}
		h.mu.Lock()
		h.publishStartsLocked()
		h.mu.Unlock()
	}
}

// publishStartsLocked patches subscribers with the jobs when they differ
// from the last published. Callers hold h.mu.
func (h *Hub) publishStartsLocked() {
	jobs := h.startsListLocked()
	if reflect.DeepEqual(jobs, h.starts) {
		return
	}
	h.starts = jobs
	p := Patch{Type: "patch", Starts: &jobs}
	for s := range h.subs {
		h.sendLocked(s, p)
	}
}

// startsListLocked is the runner's jobs, never nil. Callers hold h.mu.
func (h *Hub) startsListLocked() []start.Job {
	if h.starter == nil {
		return []start.Job{}
	}
	jobs := h.starter.List()
	if jobs == nil {
		jobs = []start.Job{}
	}
	return jobs
}

// WorkspacePath reports whether the hub lists the workspace, and its path
// (the most common cwd among its agents, "" when it has none), which a tab
// started from the app opens in: the folder the app shows.
func (h *Hub) WorkspacePath(workspaceID string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.rawWS[workspaceID]; !ok {
		return "", false
	}
	return h.workspaces[workspaceID].Path, true
}

// NameLive reports whether an agent herdr lists has that name (herdr's own
// name, not a terminal title the app may show instead).
func (h *Hub) NameLive(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.raw.Name == name {
			return true
		}
	}
	return false
}

// commonPath is the cwd most agents share, ties to the smallest, so a patch
// doesn't flap with map order; and the number of agents. Agents with no
// cwd count but never name the path.
func commonPath(cwds map[string]int) (string, int) {
	path, best, total := "", 0, 0
	for cwd, n := range cwds {
		total += n
		if cwd == "" {
			continue
		}
		if n > best || (n == best && cwd < path) {
			path, best = cwd, n
		}
	}
	return path, total
}
