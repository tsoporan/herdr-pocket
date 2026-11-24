package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"slices"

	"github.com/tsoporan/herdr-pocket/internal/audit"
	"github.com/tsoporan/herdr-pocket/internal/push"
	"github.com/tsoporan/herdr-pocket/internal/tailnet"
)

// maxAnswerBody bounds POST /api/answer's body; a real one is about 100 bytes.
const maxAnswerBody = 1 << 10

// viaNotification is the audit's via for POST /api/answer.
const viaNotification = "notification"

// answerRequest is POST /api/answer's body.
type answerRequest struct {
	PaneID     string `json:"pane_id"`
	QuestionID string `json:"question_id"`
	Option     string `json:"option"`
}

// answerResponse is POST /api/answer's reply. Code is an ack error code.
type answerResponse struct {
	OK      bool   `json:"ok"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// answerHTTP is POST /api/answer: a notification's "Allow once" or "Don't
// allow". The service worker sends
// it, having no WebSocket. It takes the same path as the WebSocket's answer,
// limited to a Bash permission's two buttons, and is audited with via
// "notification".
func (s *wsHandler) answerHTTP(w http.ResponseWriter, r *http.Request) {
	who, ok := viewerFrom(r.Context())
	if !ok { // authenticate runs first; this only guards against miswiring
		refuse(w)
		return
	}
	// As on the WebSocket: once shutdown has begun the audit log may close,
	// so nothing new reaches herdr.
	if !s.requests.begin() {
		writeAnswer(w, fail(CodeHerdrError, "the server is stopping"), http.StatusServiceUnavailable)
		return
	}
	defer s.requests.end()
	// A started answer runs to its end even if the worker goes away, so the
	// audit line has its real result.
	ctx := context.WithoutCancel(r.Context())
	req, e := s.notificationAnswer(ctx, w, r, who)
	result := "ok"
	if e != nil {
		result = e.Code
		log.Printf("server: answer via notification %s: %s", req.PaneID, e.Code)
	}
	entry := audit.Entry{Login: who.peer.Login, Node: who.peer.Node, Method: "answer", PaneID: req.PaneID, Result: result, Via: viaNotification}
	// Hashed as the WebSocket answer's payload with no note, so the same
	// choice has the same size and hash by either path.
	if err := s.audit.Record(entry, []byte(req.Option+"\n")); err != nil {
		log.Printf("server: audit: %v", err)
	}
	writeAnswer(w, e, answerStatus(e))
}

// notificationAnswer checks a POST /api/answer and carries it out. The
// request it returns is what was read of the body, for the audit line.
func (s *wsHandler) notificationAnswer(ctx context.Context, w http.ResponseWriter, r *http.Request, who viewer) (answerRequest, *AckError) {
	var req answerRequest
	// Only the app's own service worker: a fetch from it is same-origin
	// and carries the app's Origin. Unlike the GET endpoints, a
	// user-initiated "none" is not enough to act.
	if r.Header.Get("Sec-Fetch-Site") != "same-origin" || !slices.Contains(s.origins, r.Header.Get("Origin")) {
		return req, fail(CodeForbidden, "cross-site request refused")
	}
	// A form can't send this type cross-site without a CORS preflight.
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return req, fail(CodeInvalidRequest, "want application/json")
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAnswerBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return answerRequest{}, fail(CodeInvalidRequest, "malformed request")
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return answerRequest{}, fail(CodeInvalidRequest, "malformed request")
	}
	if req.PaneID == "" || req.QuestionID == "" || req.Option == "" {
		return req, fail(CodeInvalidRequest, "pane_id, question_id and option are required")
	}
	if who.role != Controller {
		return req, fail(CodeForbidden, "view only")
	}
	if s.input == nil {
		return req, fail(CodeReadOnly, "this server is read-only")
	}
	if !s.hub.Has(req.PaneID) {
		return req, fail(CodeNotFound, "no agent %q", req.PaneID)
	}
	// No push.Seen here: the clear still goes to this device. The service
	// worker keeps a sent outcome through it (notify.ts, closesOnClear), and
	// a refused or failed tap's notice is closed by it once the agent is
	// answered elsewhere.
	return req, s.answer(ctx, answerArgs{paneID: req.PaneID, questionID: req.QuestionID, option: req.Option, notification: true})
}

// answerStatus is the HTTP status for an answer's result.
func answerStatus(e *AckError) int {
	if e == nil {
		return http.StatusOK
	}
	switch e.Code {
	case CodeQuestionChanged, CodeAgentBlocked:
		return http.StatusConflict
	case CodeForbidden, CodeReadOnly:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeInvalidRequest:
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
}

func writeAnswer(w http.ResponseWriter, e *AckError, status int) {
	res := answerResponse{OK: e == nil}
	if e != nil {
		res.Code, res.Message = e.Code, e.Message
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(res); err != nil {
		log.Printf("server: answer response: %v", err)
	}
}

// CanAct returns the push engine's CanAct hook (push.Config.CanAct) for
// cfg's access rules: whether a subscription's owner may answer from a
// notification, that is, is a controller of a server that isn't read-only.
// A subscription not yet identified (push.Service.Identify) gets no buttons.
// POST /api/answer checks the viewer again; this only decides whether a push
// offers the buttons.
func CanAct(cfg Config) (func(push.Who) bool, error) {
	a, err := newAccess(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.Input == nil {
		return func(push.Who) bool { return false }, nil
	}
	return func(w push.Who) bool {
		if w.UserID == 0 && len(w.Tags) == 0 {
			return false
		}
		// A tagged node's Login is login/node, but role decides a tagged
		// peer by its tags alone.
		role, ok := a.role(tailnet.Peer{Login: w.Login, Node: w.Node, UserID: w.UserID, Tags: w.Tags})
		return ok && role == Controller
	}, nil
}
