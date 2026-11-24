package fakeherdr

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/herdr"
)

// Step is one timed change in a script. A script is a JSON array of steps:
//
//	[{"after": "0s", "start_delay": "4s"},
//	 {"after": "0s", "kind": "codex", "start": "timeout"},
//	 {"after": "8s", "pane_id": "w1:p2", "status": "blocked"},
//	 {"after": "24s", "pane_id": "w1:p1", "status": "working", "title": "Running the auth tests"}]
//
// After is a Go duration measured from the start of the run (not from the
// previous step), and steps must be in order. A pane step names a pane and
// sets at least one of Status, Title and Focus: a status change emits
// pane.agent_status_changed, a title change pane.updated, and focus moves the
// desktop's focus to the pane (pane.focused, plus tab.focused and
// workspace.focused when they change), as if someone clicked it, which drives
// push suppression. A start step names no pane and sets what agent.start does
// from then on: Start with Kind (SetStartOutcome), StartDelay
// (SetStartDelay), or both.
type Step struct {
	After      Duration     `json:"after"`
	PaneID     string       `json:"pane_id,omitempty"`
	Status     herdr.Status `json:"status,omitempty"`
	Title      string       `json:"title,omitempty"`
	Focus      bool         `json:"focus,omitempty"`
	Kind       string       `json:"kind,omitempty"`
	Start      StartOutcome `json:"start,omitempty"`
	StartDelay *Duration    `json:"start_delay,omitempty"`
}

// Duration is a time.Duration written as a string such as "8s" or "1m30s".
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"8s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

var validStatus = map[herdr.Status]bool{
	herdr.StatusIdle: true, herdr.StatusWorking: true, herdr.StatusBlocked: true,
	herdr.StatusDone: true, herdr.StatusUnknown: true,
}

// ParseScript decodes and checks a script.
func ParseScript(data []byte) ([]Step, error) {
	var steps []Step
	if err := json.Unmarshal(data, &steps); err != nil {
		return nil, fmt.Errorf("fakeherdr script: %w", err)
	}
	for i, st := range steps {
		paneStep := st.Status != "" || st.Title != "" || st.Focus
		startStep := st.Start != "" || st.Kind != "" || st.StartDelay != nil
		switch {
		case paneStep && startStep:
			return nil, fmt.Errorf("fakeherdr script step %d: set a pane's status, title or focus, or agent.start's start or start_delay, not both", i)
		case startStep && st.PaneID != "":
			return nil, fmt.Errorf("fakeherdr script step %d: a start step names no pane_id", i)
		case startStep && (st.Start == "") != (st.Kind == ""):
			return nil, fmt.Errorf("fakeherdr script step %d: start and kind go together", i)
		case st.Start != "" && !validOutcome[st.Start]:
			return nil, fmt.Errorf("fakeherdr script step %d: unknown start outcome %q", i, st.Start)
		case st.Kind != "" && !slices.Contains(herdr.Kinds, st.Kind):
			return nil, fmt.Errorf("fakeherdr script step %d: unknown kind %q", i, st.Kind)
		case startStep:
		case st.PaneID == "":
			return nil, fmt.Errorf("fakeherdr script step %d: pane_id is required", i)
		case !paneStep:
			return nil, fmt.Errorf("fakeherdr script step %d: set status, title or focus", i)
		case st.Status != "" && !validStatus[st.Status]:
			return nil, fmt.Errorf("fakeherdr script step %d: unknown status %q", i, st.Status)
		case i > 0 && st.After < steps[i-1].After:
			return nil, fmt.Errorf("fakeherdr script step %d: after %s is earlier than the step before", i, time.Duration(st.After))
		}
	}
	return steps, nil
}

// Run applies the steps at their times. It returns when the script ends,
// when ctx is done (with ctx's error), or on the first failing step.
// onStep, when non-nil, is called after each applied step.
func (s *Server) Run(ctx context.Context, steps []Step, onStep func(Step)) error {
	start := time.Now()
	for _, st := range steps {
		t := time.NewTimer(time.Until(start.Add(time.Duration(st.After))))
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		if st.Start != "" {
			s.SetStartOutcome(st.Kind, st.Start)
		}
		if st.StartDelay != nil {
			s.SetStartDelay(time.Duration(*st.StartDelay))
		}
		if st.Status != "" {
			if err := s.SetStatus(st.PaneID, st.Status); err != nil {
				return err
			}
		}
		if st.Title != "" {
			if err := s.SetTitle(st.PaneID, st.Title); err != nil {
				return err
			}
		}
		if st.Focus {
			if err := s.SetFocus(st.PaneID); err != nil {
				return err
			}
		}
		if onStep != nil {
			onStep(st)
		}
	}
	return nil
}
