package aigateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/guardrails"
)

// judgePath is the endpoint of a judge call: Chat Completions, in the OpenAI
// dialect.
const judgePath = "/v1/chat/completions"

// maxJudgeAnswer bounds how much of the judge model's answer is kept. A
// verdict is a line of JSON; an answer this large is no verdict anyway.
const maxJudgeAnswer = 1 << 20

// Judge sends the judge model one Chat Completions request (system:
// JudgeInstruction + "\n\nPolicy:\n" + policy; user: text) and returns the
// assistant's text. It resolves the model like any synthetic model (targets,
// routing, fallback), runs the chain with aigw.WithoutGuardrails and
// aigw.WithoutCache and without system text, and attributes usage to
// serviceID with no gateway key. The model must have an OpenAI-format target
// or translation turned on; otherwise it is an error.
//
// The call is served in-process, through the path ServeDialect takes after
// the gateway key: resolution, candidates, the chain, the fallback chain. No
// gateway key, allow-list or budget applies: it is the relay's own call. Of
// ctx only the deadline and the cancellation are used, never its values (the
// judged request's route, system text and flags); Judge returns when ctx
// ends, whatever the upstream does. A status other than 2xx is an error.
func (g *Gateway) Judge(ctx context.Context, serviceID, model, policy, text string) (string, error) {
	body, err := json.Marshal(judgeRequest{
		Model: model,
		Messages: []judgeMessage{
			{Role: "system", Content: guardrails.JudgeInstruction + "\n\nPolicy:\n" + policy},
			{Role: "user", Content: text},
		},
	})
	if err != nil {
		return "", err
	}

	// A context of its own, with the caller's deadline and cancellation.
	jctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if deadline, ok := ctx.Deadline(); ok {
		var cancelDeadline context.CancelFunc
		jctx, cancelDeadline = context.WithDeadline(jctx, deadline)
		defer cancelDeadline()
	}
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	jctx = aigw.WithPurpose(aigw.WithoutCache(aigw.WithoutGuardrails(jctx)), aigw.PurposeGuardrailJudge)

	req, err := http.NewRequestWithContext(jctx, http.MethodPost, judgePath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	rec := &judgeRecorder{header: http.Header{}}
	// The judge call's own request id: the judged request's, marked.
	if ri, ok := aigw.RouteFrom(ctx); ok && ri.RequestID != "" {
		rec.header.Set(headerRequestID, ri.RequestID+"-judge")
	}

	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		g.serveJudge(rec, req, serviceID)
	}()
	select {
	case p := <-done:
		if p != nil {
			return "", fmt.Errorf("aigateway: judge call panicked: %v", p)
		}
	case <-jctx.Done():
		// The call may go on for a moment; nothing it writes is read.
		return "", jctx.Err()
	}
	if rec.status < 200 || rec.status > 299 {
		return "", fmt.Errorf("aigateway: judge model answered %d", rec.status)
	}
	answer, ok := guardrails.AnswerText(guardrails.FormatChat, rec.body.Bytes())
	if !ok {
		return "", errors.New("aigateway: judge model's answer has no text")
	}
	return answer, nil
}

// serveJudge is ServeDialect for the relay's own judge call: the body is
// built here and trusted, there is no gateway key, and the usage goes to
// serviceID.
func (g *Gateway) serveJudge(w http.ResponseWriter, r *http.Request, serviceID string) {
	d := DialectOpenAI
	r = r.WithContext(aigw.WithKind(aigw.WithErrorWriter(r.Context(), d.WriteError), aigw.Kind(d.Name)))
	body, ok := g.readBodyOrFail(w, r)
	if !ok {
		return
	}
	res, ok := g.resolveOrFail(w, r, body.Model(), d)
	if !ok {
		return
	}
	candidates, why := g.candidatesForRequest(r.Context(), res, d, judgePath)
	if len(candidates) == 0 {
		g.failNoCandidate(w, r, res, d, body.Model(), why)
		return
	}
	g.dispatch(w, r, d, judgePath, res, candidates, body, "", "", serviceID)
}

type judgeRequest struct {
	Model    string         `json:"model"`
	Messages []judgeMessage `json:"messages"`
}

type judgeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// judgeRecorder is the response writer of a judge call: it keeps the status
// and up to maxJudgeAnswer bytes of the body.
type judgeRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (j *judgeRecorder) Header() http.Header { return j.header }

func (j *judgeRecorder) WriteHeader(code int) {
	if j.status == 0 && code >= 200 {
		j.status = code
	}
}

func (j *judgeRecorder) Write(p []byte) (int, error) {
	if j.status == 0 {
		j.status = http.StatusOK
	}
	if room := maxJudgeAnswer - j.body.Len(); room > 0 {
		j.body.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (j *judgeRecorder) Flush() {}
