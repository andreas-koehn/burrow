package main

import (
	"context"
	"sync/atomic"

	"github.com/ankoehn/burrow/internal/aigateway"
	"github.com/ankoehn/burrow/internal/aigw"
)

// judgeAdapter hands the chain's judge calls (aigw.Chain.Judge) to the AI
// gateway, which asks the judge model through its own resolution, chain and
// fallback (aigateway.Gateway.Judge). The chain is built before the gateway
// (buildV04Stack, then main), so the gateway is handed over with set once it
// exists; until then the judge is unavailable. Safe for concurrent use.
type judgeAdapter struct {
	g atomic.Pointer[aigateway.Gateway]
}

// set hands the gateway over.
func (j *judgeAdapter) set(g *aigateway.Gateway) { j.g.Store(g) }

// judge is the chain's Judge function.
func (j *judgeAdapter) judge(ctx context.Context, serviceID, model, policy, text string) (string, error) {
	g := j.g.Load()
	if g == nil {
		return "", aigw.ErrJudgeUnavailable
	}
	return g.Judge(ctx, serviceID, model, policy, text)
}
