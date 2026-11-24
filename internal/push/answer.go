package push

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/answer"
)

// questionTimeout bounds the Question hooks of one step, so reading the
// screens never holds up a blocked alert by more than this (sendTimeout and
// this stay well inside the 10 s an alert has to reach the phone). A var
// for the tests.
var questionTimeout = 2 * time.Second

// The option labels a notification offers, as answer.Build maps them.
const (
	labelAllow = "Allow once"
	labelDeny  = "Don’t allow" // U+2019, as answer.Build writes it
)

// Who is a subscription's tailnet identity, as last seen by the server
// (Service.Identify): what Config.CanAct decides a role from. Login is the
// subscription's owner string, which for a tagged node is login/node.
type Who struct {
	Login, Node string
	UserID      int64
	Tags        []string
}

// answerKeys is a blocked push's optional answer object: the two option
// digits the notification's buttons send. Never labels or command text.
type answerKeys struct {
	PaneID     string `json:"pane_id"`
	QuestionID string `json:"question_id"`
	Allow      string `json:"allow"`
	Deny       string `json:"deny"`
}

// NotifyKeys returns the keys of a question's "Allow once" and "Don't allow"
// options when it may be answered from a notification: a permission prompt
// for a Bash command whose option 1 is "Allow once". Anything else, such as
// AskUserQuestion or an edit, is answered in the app.
func NotifyKeys(q *answer.Question) (allow, deny string, ok bool) {
	if q == nil || q.Kind != answer.KindPermission || q.Command == nil || *q.Command == "" {
		return "", "", false
	}
	key := func(label string) string {
		if i := slices.IndexFunc(q.Options, func(o answer.Option) bool { return o.Label == label }); i >= 0 {
			return q.Options[i].Key
		}
		return ""
	}
	allow, deny = key(labelAllow), key(labelDeny)
	if allow != "1" || !isDigit(deny) || allow == deny {
		return "", "", false
	}
	return allow, deny, true
}

func isDigit(k string) bool { return len(k) == 1 && k[0] >= '1' && k[0] <= '9' }

// addAnswers gives the blocked alerts that may carry buttons their answer
// object and body. It calls the hooks outside s.mu (the hub calls Observe
// with its own lock held, and Question takes that lock), reads each pane's
// question once, all at once, and waits at most questionTimeout in all. A
// failed, slow or missing hook only means no buttons.
func (s *Service) addAnswers(ctx context.Context, jobs []job) {
	if s.cfg.Question == nil || s.cfg.CanAct == nil {
		return
	}
	var want []int
	panes := map[string]bool{}
	for i, j := range jobs {
		if j.msg.Kind == kindBlocked && s.cfg.CanAct(j.who) {
			want = append(want, i)
			panes[j.agent.PaneID] = true
		}
	}
	if len(want) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, questionTimeout)
	defer cancel()
	var mu sync.Mutex
	questions := map[string]*answer.Question{}
	done := make(chan struct{}, len(panes))
	for pane := range panes {
		// Not waited for: a hook that ignores ctx may outlive the step, and
		// its result is then dropped.
		go func() {
			defer func() { done <- struct{}{} }()
			q, err := s.cfg.Question(ctx, pane)
			if err != nil {
				s.cfg.Logf("push: question for %s: %v; no answer buttons", pane, err)
				return
			}
			mu.Lock()
			questions[pane] = q
			mu.Unlock()
		}()
	}
wait:
	for range panes {
		select {
		case <-done:
		case <-ctx.Done():
			s.cfg.Logf("push: reading the questions timed out; no answer buttons for some alerts")
			break wait
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, i := range want {
		j := &jobs[i]
		q := questions[j.agent.PaneID]
		allow, deny, ok := NotifyKeys(q)
		if !ok {
			continue
		}
		j.msg.Answer = &answerKeys{PaneID: j.agent.PaneID, QuestionID: q.ID, Allow: allow, Deny: deny}
		j.msg.Body = withWorkspace("Wants to run a command", j.agent.Workspace) + ". Tap to see it first."
	}
}
