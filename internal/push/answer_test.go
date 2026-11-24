package push

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/answer"
)

// The answer buttons (herd pass 1): a blocked alert for a Bash permission
// carries its two option keys, only for a subscription CanAct allows.

func bashQuestion() *answer.Question {
	cmd := "rm -rf dist && bun run build"
	return &answer.Question{ID: "q-bash", Kind: answer.KindPermission, Title: "Allow this command?", Command: &cmd, Options: []answer.Option{
		{Key: "1", Label: "Allow once"}, {Key: "2", Label: "Always allow"}, {Key: "3", Label: "Don’t allow", NeedsNote: true},
	}}
}

// answerRig is a rig whose Service has the answer hooks.
func answerRig(t *testing.T, question func(context.Context, string) (*answer.Question, error), canAct func(Who) bool) *rig {
	t.Helper()
	r := newRig(t)
	svc, err := Open(t.TempDir(), Config{Origin: testOrigin, HTTPClient: r.ps.client(), Now: r.clk.Now, Logf: r.logs.Logf, Question: question, CanAct: canAct})
	if err != nil {
		t.Fatal(err)
	}
	r.svc = svc
	return r
}

// alertOnce subscribes d as the owner, identifies it, blocks the agent and
// returns the one push.
func alertOnce(t *testing.T, r *rig, d *device) decoded {
	t.Helper()
	r.subscribe(d, noDesk())
	r.svc.Identify(owner, "phone", 1001, nil)
	r.svc.Observe([]Agent{with(blocked(1), "working", 1)})
	r.svc.Observe([]Agent{blocked(2)})
	got := r.decode(d, r.at(3*time.Second))
	if len(got) != 1 {
		t.Fatalf("got %d pushes, want 1", len(got))
	}
	return got[0]
}

func ownerOnly(w Who) bool { return w.Login == owner && w.UserID == 1001 }

func TestBlockedAlertCarriesAnswerKeys(t *testing.T) {
	var asked []string
	r := answerRig(t, func(_ context.Context, pane string) (*answer.Question, error) {
		asked = append(asked, pane)
		return bashQuestion(), nil
	}, ownerOnly)
	d := newDevice(t, "dev-a")
	got := alertOnce(t, r, d)
	want := answerKeys{PaneID: "w1-p2", QuestionID: "q-bash", Allow: "1", Deny: "3"}
	if got.msg.Answer == nil || *got.msg.Answer != want {
		t.Fatalf("answer %+v, want %+v", got.msg.Answer, want)
	}
	if got.msg.Body != "Wants to run a command · orchard. Tap to see it first." {
		t.Errorf("body %q", got.msg.Body)
	}
	if len(asked) != 1 || asked[0] != "w1-p2" {
		t.Errorf("Question asked for %q", asked)
	}
}

func TestNoAnswerKeys(t *testing.T) {
	ask := &answer.Question{ID: "q-ask", Kind: answer.KindAsk, Options: []answer.Option{{Key: "1", Label: "staging"}, {Key: "2", Label: "dev"}}}
	edit := bashQuestion()
	edit.Command = nil
	noDeny := bashQuestion()
	noDeny.Options = noDeny.Options[:2]
	cases := []struct {
		name     string
		question func(context.Context, string) (*answer.Question, error)
		canAct   func(Who) bool
	}{
		{"an AskUserQuestion", func(context.Context, string) (*answer.Question, error) { return ask, nil }, ownerOnly},
		{"an edit permission", func(context.Context, string) (*answer.Question, error) { return edit, nil }, ownerOnly},
		{"no Don't allow", func(context.Context, string) (*answer.Question, error) { return noDeny, nil }, ownerOnly},
		{"no longer blocked", func(context.Context, string) (*answer.Question, error) { return nil, nil }, ownerOnly},
		{"a failing Question", func(context.Context, string) (*answer.Question, error) {
			return nil, errors.New("herdr: invented failure")
		}, ownerOnly},
		{"a reader", func(context.Context, string) (*answer.Question, error) { return bashQuestion(), nil }, func(Who) bool { return false }},
		{"no hooks", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := answerRig(t, tc.question, tc.canAct)
			got := alertOnce(t, r, newDevice(t, "dev-a"))
			if got.msg.Answer != nil || got.msg.Body != "Asks you a question · orchard. Tap to answer." {
				t.Errorf("payload %+v", got.msg)
			}
		})
	}
}

// A Question that ignores its context still can't hold the alert past
// questionTimeout; the alert goes without buttons.
func TestSlowQuestionStillSends(t *testing.T) {
	old := questionTimeout
	questionTimeout = 50 * time.Millisecond
	t.Cleanup(func() { questionTimeout = old })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	r := answerRig(t, func(context.Context, string) (*answer.Question, error) {
		<-release
		return bashQuestion(), nil
	}, ownerOnly)
	start := time.Now()
	got := alertOnce(t, r, newDevice(t, "dev-a"))
	if got.msg.Answer != nil {
		t.Errorf("answer %+v from a timed-out Question", got.msg.Answer)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the alert took %v", d)
	}
	if !strings.Contains(r.logs.String(), "timed out") {
		t.Errorf("logs %q", r.logs.String())
	}
}

// CanAct sees the identity Identify stored, and it survives a restart.
func TestIdentifyIsStored(t *testing.T) {
	r := newRig(t)
	d := newDevice(t, "dev-a")
	r.subscribe(d, noDesk())
	r.svc.Identify(owner, "phone", 1001, []string{"tag:phone"})
	r.svc.Identify(owner, "laptop", 7, nil) // another device: not this one
	var seen []Who
	svc, err := Open(r.dir, Config{Origin: testOrigin, HTTPClient: r.ps.client(), Now: r.clk.Now,
		Question: func(context.Context, string) (*answer.Question, error) { return bashQuestion(), nil },
		CanAct:   func(w Who) bool { seen = append(seen, w); return true }})
	if err != nil {
		t.Fatal(err)
	}
	r.svc = svc
	r.svc.Observe([]Agent{with(blocked(1), "working", 1)})
	r.svc.Observe([]Agent{blocked(2)})
	if got := r.decode(d, r.at(3*time.Second)); len(got) != 1 || got[0].msg.Answer == nil {
		t.Fatalf("pushes %+v", got)
	}
	if len(seen) != 1 || seen[0].Login != owner || seen[0].Node != "phone" || seen[0].UserID != 1001 || len(seen[0].Tags) != 1 {
		t.Errorf("CanAct saw %+v", seen)
	}
}

func TestNotifyKeys(t *testing.T) {
	if a, d, ok := NotifyKeys(bashQuestion()); !ok || a != "1" || d != "3" {
		t.Errorf("bash: %q %q %v", a, d, ok)
	}
	straight := bashQuestion()
	straight.Options[2].Label = "Don't allow" // not the label answer.Build writes
	if _, _, ok := NotifyKeys(straight); ok {
		t.Error("an unknown deny label passed")
	}
	moved := bashQuestion()
	moved.Options[0].Label, moved.Options[1].Label = moved.Options[1].Label, moved.Options[0].Label
	if _, _, ok := NotifyKeys(moved); ok {
		t.Error("Allow once as option 2 passed")
	}
	if _, _, ok := NotifyKeys(nil); ok {
		t.Error("nil passed")
	}
}
