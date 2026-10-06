package aimeter

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/ankoehn/burrow/internal/db"
)

// BudgetChecker is the narrow surface SQLSink uses to fire the cost engine's
// budget check after each successful insert. *cost.Engine satisfies it.
// Kept here as a Sample-shaped value (not a cost.Subjects struct) to avoid
// importing internal/cost from internal/aimeter (which would put cost on
// the proxy hot path's import graph).
type BudgetChecker interface {
	// CheckBudgetsForSample inspects the just-recorded usage and triggers
	// any exceeded budgets (alert_webhook / throttle_zero / disable_key)
	// exactly once per UTC day. Errors are logged + swallowed.
	CheckBudgetsForSample(ctx context.Context, serviceID, apiKeyID string)
}

// SQLSink writes one usage_events row per recorded Sample. Errors are
// logged at warn level via the configured logger and swallowed (the proxy
// path must never fail on a metering write). If Budgets is non-nil, the
// sink calls it after each successful insert so the cost engine sees the
// new row before the next request.
type SQLSink struct {
	DB      *db.DB
	Log     *slog.Logger // optional; if nil, slog.Default() is used
	Budgets BudgetChecker
}

// NewSQLSink constructs a SQLSink using slog.Default for diagnostics. Callers
// who want a scoped logger may set Log directly on the returned struct.
func NewSQLSink(d *db.DB) *SQLSink {
	return &SQLSink{DB: d, Log: slog.Default()}
}

// Record inserts one usage_events row. The id is generated via uuid.NewString
// (same scheme other v0.4.0 tables use — see internal/db/services.go and the
// services/api-keys path). The Ts column defaults to time.Now().UTC().
//
// Non-blocking semantics: any sqlite error is logged + swallowed. The caller
// (proxy hot path) treats the returned error as informational only. Callers
// may still propagate it in tests via errcheck patterns.
func (s *SQLSink) Record(ctx context.Context, sm Sample) error {
	if s == nil || s.DB == nil {
		return nil
	}
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	row := db.UsageEvent{
		ID:             uuid.NewString(),
		ServiceID:      sm.ServiceID,
		APIKeyID:       sm.APIKeyID,
		Ts:             time.Now().UTC(),
		Kind:           string(sm.Kind),
		TokensIn:       int64(sm.TokensIn),
		TokensOut:      int64(sm.TokensOut),
		BytesIn:        sm.BytesIn,
		BytesOut:       sm.BytesOut,
		Streamed:       sm.Streamed,
		CacheHit:       sm.CacheHit,
		UpstreamStatus: sm.UpstreamStatus,
		GatewayKeyID:   sm.GatewayKeyID,
		Dialect:        sm.Dialect,
		ProviderSlug:   sm.ProviderSlug,
		RequestedModel: sm.RequestedModel,
		TargetModel:    sm.TargetModel,
		RequestID:      sm.RequestID,
		LatencyMs:      sm.LatencyMs,
	}
	// The parsers only hand over a validated cost; checking again here keeps
	// NaN, an infinity, a negative or an absurd amount out of the table and
	// out of the budget check whoever built the Sample. Such a row is stored
	// without a cost, so the price table applies to it.
	var cost sql.NullFloat64
	if sm.CostUSD != nil && saneCost(*sm.CostUSD) {
		v := *sm.CostUSD
		row.CostUSD = &v
		cost = sql.NullFloat64{Float64: v, Valid: true}
	}
	_, err := s.DB.DB().ExecContext(ctx, `
		INSERT INTO usage_events
		  (id, service_id, api_key_id, ts, kind,
		   tokens_in, tokens_out, bytes_in, bytes_out,
		   streamed, cache_hit, upstream_status, cost_usd,
		   gateway_key_id, dialect, provider_slug, requested_model, target_model,
		   request_id, latency_ms)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		row.ID, row.ServiceID, row.APIKeyID, row.Ts, row.Kind,
		row.TokensIn, row.TokensOut, row.BytesIn, row.BytesOut,
		boolToInt(row.Streamed), boolToInt(row.CacheHit), row.UpstreamStatus, cost,
		row.GatewayKeyID, row.Dialect, row.ProviderSlug, row.RequestedModel, row.TargetModel,
		row.RequestID, row.LatencyMs,
	)
	if err != nil {
		log.Warn("aimeter: usage_events insert failed",
			slog.String("service_id", sm.ServiceID),
			slog.String("api_key_id", sm.APIKeyID),
			slog.String("kind", string(sm.Kind)),
			slog.String("err", err.Error()),
		)
		// Swallow: non-blocking per v0.4.0 spec.
		return nil
	}
	// Optional budget check — fires exactly once per UTC day per exceeded
	// budget. Implemented by *cost.Engine; nil-safe so existing call sites
	// that don't set Budgets keep their original behaviour.
	if s.Budgets != nil {
		s.Budgets.CheckBudgetsForSample(ctx, sm.ServiceID, sm.APIKeyID)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
