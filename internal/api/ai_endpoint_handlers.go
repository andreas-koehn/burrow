package api

// ai_endpoint_handlers.go — GET /ai/endpoints + /:id/metrics
//
// These two handlers surface a derived "AI endpoints" view that the dashboard
// AiEndpoints and AiEndpointDetail pages consume.  The identity fields
// (service_id, name, model_alias, concrete_model, backend_type, api_key_count,
// status, client_session_id) are assembled from live data.  The metering
// aggregates (requests_24h, cache_hits_24h, latency_p95_ms, tokens_in/out_24h,
// cost_usd_24h, requests_per_minute[60]) are zeroed for now.
//
// TODO(follow-up): aggregate metering from the usage_events table.  The SQL
// query shape is: SELECT COUNT(*), COUNT(cache_hit), approx_p95(latency_ms),
// SUM(tokens_in), SUM(tokens_out), SUM(cost_usd) FROM usage_events WHERE
// service_id = ? AND ts >= now() - 24h.  Wire into GetAIEndpoints and
// GetAIEndpointMetrics once the usage_events table is populated by the proxy
// hot-path.

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/db"
)

// aiEndpointResp is the JSON wire shape for one AI endpoint in the list
// response.  Mirrors the TypeScript AiEndpoint interface in AiEndpoints.tsx.
type aiEndpointResp struct {
	ServiceID       string `json:"service_id"`
	Name            string `json:"name"`
	ModelAlias      string `json:"model_alias"`
	ConcreteModel   string `json:"concrete_model"`
	BackendType     string `json:"backend_type"`
	APIKeyCount     int    `json:"api_key_count"`
	Requests24h     int    `json:"requests_24h"`
	CacheHits24h    int    `json:"cache_hits_24h"`
	LatencyP95ms    int    `json:"latency_p95_ms"`
	Status          string `json:"status"`
	ClientSessionID string `json:"client_session_id"`
}

// endpointMetricsResp is the JSON wire shape for the per-endpoint metrics
// endpoint.  Mirrors the TypeScript EndpointMetrics interface in
// AiEndpointDetail.tsx.
type endpointMetricsResp struct {
	Requests24h       int     `json:"requests_24h"`
	TokensIn24h       int     `json:"tokens_in_24h"`
	TokensOut24h      int     `json:"tokens_out_24h"`
	CostUSD24h        float64 `json:"cost_usd_24h"`
	CacheHitRatio24h  float64 `json:"cache_hit_ratio_24h"`
	RequestsPerMinute []int   `json:"requests_per_minute"`
}

// providerToBackendType converts a model-alias provider string to the
// backend_type enum the UI expects.
//
//	"openai"       → "openai-compat"
//	"openai-compat"→ "openai-compat"
//	"ollama"       → "ollama"
//	"vllm"         → "vllm"
//	anything else  → "other"
func providerToBackendType(provider string) string {
	switch provider {
	case "openai", "openai-compat":
		return "openai-compat"
	case "ollama":
		return "ollama"
	case "vllm":
		return "vllm"
	default:
		return "other"
	}
}

// GetAIEndpoints handles GET /api/v1/ai/endpoints.
//
// Returns one entry for every service whose access_mode == "api_key".  Identity
// fields (name, model_alias, concrete_model, backend_type, api_key_count,
// status, client_session_id) are populated from live data.  Metering fields
// (requests_24h, cache_hits_24h, latency_p95_ms) are zeroed pending
// usage_events aggregation (see TODO above).
func (d Deps) GetAIEndpoints(w http.ResponseWriter, r *http.Request) {
	role, err := d.callerRole(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	uid := userID(r.Context())

	svcs, err := d.Services.ListServices(r.Context(), uid, role)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Build a service_id → first alias mapping from the model alias registry.
	// We pick the alias with the highest priority (lowest priority number)
	// for each service.  If ModelAliases is nil or the list fails, we proceed
	// with an empty map — every endpoint will show empty alias fields.
	aliasForService := map[string]modelAliasResp{}
	if d.ModelAliases != nil {
		if aliases, err := d.ModelAliases.ListModelAliases(r.Context()); err == nil {
			for _, a := range aliases {
				if a.ServiceID == "" {
					continue
				}
				existing, seen := aliasForService[a.ServiceID]
				// Lower priority number = higher priority.  Pick the first
				// (lowest Priority) alias seen for each service.
				if !seen || a.Priority < existing.Priority {
					aliasForService[a.ServiceID] = toModelAliasResp(a)
				}
			}
		}
	}

	// Trailing-24h request + cache-hit counts per service. Degrades to zeros
	// when the metrics store isn't wired (early-wiring / handler tests).
	counts := map[string]db.AIEndpointCount{}
	if d.AIMetrics != nil {
		if c, err := d.AIMetrics.AIEndpointCounts24h(r.Context()); err == nil {
			counts = c
		}
	}

	// Build response: include only api_key-mode services.
	out := make([]aiEndpointResp, 0)
	for _, sv := range svcs {
		if sv.AccessMode != "api_key" {
			continue
		}

		// Count API keys for this service via ListAPIKeys.  On error, default
		// to 0 (non-fatal: the endpoint still renders).
		keyCount := 0
		if keys, err := d.Services.ListAPIKeys(r.Context(), uid, role, sv.ID); err == nil {
			keyCount = len(keys)
		}

		// Live tunnel snapshot: determines status + client_session_id.
		// LiveTunnels may be nil in some wiring configurations; composeLive
		// handles that gracefully (returns zero-value snapshot).
		snap := d.composeLive(sv.ID)
		status := "Offline"
		sessionID := ""
		if snap.Connected {
			status = "Connected"
			// The session ID surfaces as part of the live snapshot's local_addr
			// in some wiring configurations, but there is no dedicated SessionID
			// field on LiveTunnelSnapshot today.  For now we leave it empty for
			// connected tunnels — the dashboard handles "" gracefully.
			// TODO: expose SessionID on LiveTunnelSnapshot when the tunnel
			// registry tracks it (follow-up task).
			_ = sessionID
		}

		alias := aliasForService[sv.ID]
		backendType := providerToBackendType(alias.Provider)

		out = append(out, aiEndpointResp{
			ServiceID:       sv.ID,
			Name:            sv.Name,
			ModelAlias:      alias.Alias,
			ConcreteModel:   alias.ConcreteModel,
			BackendType:     backendType,
			APIKeyCount:     keyCount,
			Requests24h:     counts[sv.ID].Requests,
			CacheHits24h:    counts[sv.ID].CacheHits,
			LatencyP95ms:    0, // usage_events has no latency column — not derivable
			Status:          status,
			ClientSessionID: sessionID,
		})
	}

	writeJSON(w, http.StatusOK, out)
}

// GetAIEndpointMetrics handles GET /api/v1/ai/endpoints/{serviceID}/metrics.
//
// TODO(follow-up): replace zeroed values with real aggregations from
// usage_events.  SQL sketch:
//
//	SELECT
//	  COUNT(*)                                          AS requests_24h,
//	  SUM(tokens_in)                                    AS tokens_in_24h,
//	  SUM(tokens_out)                                   AS tokens_out_24h,
//	  SUM(cost_usd)                                     AS cost_usd_24h,
//	  AVG(CASE WHEN cache_hit THEN 1.0 ELSE 0.0 END)   AS cache_hit_ratio_24h
//	FROM usage_events
//	WHERE service_id = ?
//	  AND ts >= strftime('%s','now') - 86400;
//
// The requests_per_minute array requires a per-minute bucket join.
func (d Deps) GetAIEndpointMetrics(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")

	// Validate the service exists and is accessible to the caller.
	role, err := d.callerRole(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	uid := userID(r.Context())
	_, err = d.Services.GetService(r.Context(), uid, role, serviceID)
	if err != nil {
		if !mapServiceErr(w, err, "service not found") {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	// 60 buckets — one request count per minute over the trailing hour.
	rpm := make([]int, 60)
	resp := endpointMetricsResp{RequestsPerMinute: rpm}

	// Aggregate from usage_events when the metrics store is wired. Degrades to
	// zeros on any read error (metrics are non-critical to the page rendering).
	if d.AIMetrics != nil {
		if agg, err := d.AIMetrics.AIEndpointMetrics24h(r.Context(), serviceID); err == nil {
			resp.Requests24h = agg.Requests
			resp.TokensIn24h = int(agg.TokensIn)
			resp.TokensOut24h = int(agg.TokensOut)
			if agg.Requests > 0 {
				resp.CacheHitRatio24h = float64(agg.CacheHits) / float64(agg.Requests)
			}
			copy(rpm, agg.PerMinute[:])
			// Cost from per-kind token subtotals via the pricing table.
			if d.CostEngine != nil {
				var usd float64
				for _, k := range agg.ByKind {
					usd += d.CostEngine.UsdFor(k.Kind, int(k.TokensIn), int(k.TokensOut))
				}
				resp.CostUSD24h = usd
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
}
