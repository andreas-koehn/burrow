// Package metrics is Burrow's hand-rolled Prometheus exposition recorder.
//
// The v0.4.0 metric set is closed (spec Part O): adding a metric requires a
// doc PR. No prometheus/client_golang dep — every counter is a sync.Map of
// atomic.Int64 keyed by the full Prometheus label string ("name{a=\"x\"}").
// Histograms use a fixed default bucket set (cumulative counts plus _count +
// _sum) and on render are translated to per-quantile (0.5/0.95/0.99) summary
// samples via linear interpolation across the bucket boundaries. Gauges are
// stored as float64 under a mutex (a single store per call; the read path is
// snapshot-and-emit).
//
// Concurrency: every Inc/Observe is lock-free in the steady state (atomic
// load/store on the counter, or a single sync.Map entry mutation). WriteText
// takes a read snapshot under each top-level lock and emits without holding
// it — safe to call concurrently with recorders.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// numBuckets is the fixed bucket-count constant — kept as a top-level const
// so the histogramSeries struct can size its `buckets` array literally.
const numBuckets = 11

// defaultBuckets is the histogram bucket set (upper bounds, seconds) used for
// every Burrow latency observation. The values mirror Prometheus's documented
// "default service duration" set — they cover sub-millisecond cache lookups
// up through ten-second timeouts without HDR complexity. MUST stay numBuckets
// long.
var defaultBuckets = [numBuckets]float64{
	0.005, 0.01, 0.025, 0.05, 0.1,
	0.25, 0.5, 1, 2.5, 5, 10,
}

// quantiles is the closed set of quantiles emitted by every histogram-backed
// metric. Spec Part O lists 0.5/0.95/0.99 explicitly.
var quantiles = []float64{0.5, 0.95, 0.99}

// metricKind identifies a series' Prometheus type for the # TYPE line.
type metricKind int

const (
	kindCounter metricKind = iota
	kindGauge
	kindHistogram
)

// metricDef is one entry in the closed metric set: name, type, help text, and
// the ordered label keys that every series under this metric MUST carry.
type metricDef struct {
	name   string
	kind   metricKind
	help   string
	labels []string // ordered; empty for unlabeled metrics like burrow_goroutines
}

// closedSet is the v0.4.0 closed metric registry (spec Part O). The order is
// the emission order in WriteText; adding a metric here requires a doc PR.
var closedSet = []metricDef{
	// HTTP per tunnel (proxy ingress).
	{name: "burrow_http_requests_total", kind: kindCounter,
		help:   "Total HTTP requests through the proxy.",
		labels: []string{"service", "method", "status"}},
	{name: "burrow_http_request_duration_seconds", kind: kindHistogram,
		help:   "Histogram of HTTP request durations (seconds).",
		labels: []string{"service", "method"}},
	{name: "burrow_http_request_bytes_in_total", kind: kindCounter,
		help:   "Total HTTP request bytes received.",
		labels: []string{"service"}},
	{name: "burrow_http_request_bytes_out_total", kind: kindCounter,
		help:   "Total HTTP response bytes sent.",
		labels: []string{"service"}},

	// Connection per client.
	{name: "burrow_client_session_count", kind: kindGauge,
		help:   "Current client control sessions.",
		labels: []string{"user"}},
	{name: "burrow_client_session_duration_seconds", kind: kindHistogram,
		help:   "Histogram of client control session durations (seconds).",
		labels: []string{"user"}},
	{name: "burrow_client_bytes_in_total", kind: kindCounter,
		help:   "Total client bytes received.",
		labels: []string{"user"}},
	{name: "burrow_client_bytes_out_total", kind: kindCounter,
		help:   "Total client bytes sent.",
		labels: []string{"user"}},

	// AI per service / key.
	{name: "burrow_ai_tokens_in_total", kind: kindCounter,
		help:   "Total AI input tokens charged.",
		labels: []string{"service", "api_key"}},
	{name: "burrow_ai_tokens_out_total", kind: kindCounter,
		help:   "Total AI output tokens charged.",
		labels: []string{"service", "api_key"}},
	{name: "burrow_ai_cost_usd_total", kind: kindCounter,
		help:   "Total AI cost in USD.",
		labels: []string{"service", "api_key"}},
	{name: "burrow_ai_cache_hits_total", kind: kindCounter,
		help:   "Total prompt-cache hits.",
		labels: []string{"service"}},
	{name: "burrow_ai_cache_misses_total", kind: kindCounter,
		help:   "Total prompt-cache misses.",
		labels: []string{"service"}},
	{name: "burrow_ai_failover_events_total", kind: kindCounter,
		help:   "Total AI upstream failover events.",
		labels: []string{"service", "from", "to", "cross_provider"}},
	{name: "burrow_ai_upstream_errors_total", kind: kindCounter,
		help:   "Total AI upstream errors.",
		labels: []string{"service", "status"}},

	// Internal.
	{name: "burrow_goroutines", kind: kindGauge,
		help:   "Live goroutine count (sampled at scrape time).",
		labels: nil},
	{name: "burrow_db_query_duration_seconds", kind: kindHistogram,
		help:   "Histogram of DB query durations (seconds).",
		labels: []string{"op"}},
	{name: "burrow_control_reconnects_total", kind: kindCounter,
		help:   "Total control-plane reconnect events.",
		labels: []string{"client"}},
	{name: "burrow_cert_expiry_days", kind: kindGauge,
		help:   "Days remaining until the operator-supplied wildcard cert expires.",
		labels: []string{"cert"}},
	{name: "burrow_audit_chain_length", kind: kindGauge,
		help:   "Total rows in the audit log.",
		labels: nil},
	{name: "burrow_audit_chain_last_hash", kind: kindGauge,
		help:   "Last audit chain hash (gauge value is always 1; the hex digest lives in the hash label).",
		labels: []string{"hash"}},

	// Semantic cache (v0.5.0, spec Part K).
	{name: "burrow_ai_semantic_lookups_total", kind: kindCounter,
		help:   "Total semantic cache lookup attempts.",
		labels: []string{"service"}},
	{name: "burrow_ai_semantic_hits_total", kind: kindCounter,
		help:   "Total semantic cache hits.",
		labels: []string{"service", "policy"}},
	{name: "burrow_ai_semantic_promotions_total", kind: kindCounter,
		help:   "Total semantic cache promotions.",
		labels: []string{"service"}},
	{name: "burrow_ai_semantic_errors_total", kind: kindCounter,
		help:   "Total semantic cache errors.",
		labels: []string{"service", "kind"}},
	{name: "burrow_ai_semantic_index_entries", kind: kindGauge,
		help:   "Current number of entries in the semantic index.",
		labels: []string{"service"}},
	{name: "burrow_ai_semantic_index_bytes", kind: kindGauge,
		help:   "Current byte size of the semantic index.",
		labels: []string{"service"}},

	// Upstream-credential injection (v0.5.0, spec Part K).
	{name: "burrow_ai_credential_injections_total", kind: kindCounter,
		help:   "Total upstream credential injections.",
		labels: []string{"service", "slot"}},
	{name: "burrow_ai_credential_misses_total", kind: kindCounter,
		help:   "Total upstream credential misses (slot env var absent).",
		labels: []string{"service"}},

	// Custom domains (v0.5.0, spec Part K).
	{name: "burrow_custom_domain_count", kind: kindGauge,
		help:   "Current number of provisioned custom domains.",
		labels: nil},

	// Connection logs (v0.5.0, spec Part K).
	{name: "burrow_connections_total", kind: kindCounter,
		help:   "Total connections recorded.",
		labels: []string{"service", "kind", "status"}},
	{name: "burrow_connection_bytes_in_total", kind: kindCounter,
		help:   "Total inbound bytes across all connections.",
		labels: []string{"service", "kind"}},
	{name: "burrow_connection_bytes_out_total", kind: kindCounter,
		help:   "Total outbound bytes across all connections.",
		labels: []string{"service", "kind"}},

	// Retention (v0.5.0, spec Part K).
	{name: "burrow_retention_compact_rows_deleted_total", kind: kindCounter,
		help:   "Total rows deleted by retention compaction.",
		labels: []string{"table"}},
	{name: "burrow_retention_compact_last_run_seconds", kind: kindGauge,
		help:   "UTC Unix timestamp of the last retention compaction run.",
		labels: nil},

	// Database (v0.5.0, spec Part K).
	{name: "burrow_db_backend", kind: kindGauge,
		help:   "Always 1; the driver label identifies the database backend.",
		labels: []string{"driver"}},
	{name: "burrow_db_pool_active", kind: kindGauge,
		help:   "Active database connections in the pool (Postgres only).",
		labels: nil},
	{name: "burrow_db_pool_idle", kind: kindGauge,
		help:   "Idle database connections in the pool (Postgres only).",
		labels: nil},
}

// histogramSeries holds the bucket-aggregated state for one labeled series of
// a histogram metric. Each bucket is cumulative — bucket i is "observations
// with value ≤ defaultBuckets[i]". sumBits is the float64 sum, stored as its
// raw bits so atomic.Uint64.CompareAndSwap can do lock-free additive updates.
type histogramSeries struct {
	buckets [numBuckets]atomic.Uint64
	count   atomic.Uint64
	sumBits atomic.Uint64
}

// observe records a single value into the histogram's cumulative buckets.
// Observations that fall beyond the last finite bucket increment only count
// + sum; quantile() then returns the last finite bound for any target that
// sits in the +Inf tail. This is intentional: we emit summary-style per-
// quantile samples (not _bucket samples), so a separate +Inf counter would
// be inert.
func (h *histogramSeries) observe(v float64) {
	for i, ub := range defaultBuckets {
		if v <= ub {
			h.buckets[i].Add(1)
		}
	}
	h.count.Add(1)
	// Lock-free float-sum add via CAS on the raw bits.
	for {
		old := h.sumBits.Load()
		nw := math.Float64bits(math.Float64frombits(old) + v)
		if h.sumBits.CompareAndSwap(old, nw) {
			return
		}
	}
}

// quantile returns the linearly interpolated quantile q∈[0,1] over the
// cumulative buckets, mirroring Prometheus' histogram_quantile() semantics
// at scrape time. Returns 0 when no observations have been recorded yet.
func (h *histogramSeries) quantile(q float64) float64 {
	total := h.count.Load()
	if total == 0 {
		return 0
	}
	target := q * float64(total)
	var prevCum uint64 = 0
	var prevUB float64 = 0
	for i, ub := range defaultBuckets {
		cum := h.buckets[i].Load()
		if float64(cum) >= target {
			width := ub - prevUB
			delta := float64(cum) - float64(prevCum)
			if delta == 0 {
				return ub
			}
			frac := (target - float64(prevCum)) / delta
			if frac < 0 {
				frac = 0
			}
			if frac > 1 {
				frac = 1
			}
			return prevUB + frac*width
		}
		prevCum = cum
		prevUB = ub
	}
	// Quantile sits in the +Inf bucket — return the last finite upper bound.
	// (Prometheus would return +Inf; the dashboard render is friendlier with
	// a finite value, and the _count/_sum samples still convey the truth.)
	return defaultBuckets[len(defaultBuckets)-1]
}

// sum reads the running sum of every observation in this series.
func (h *histogramSeries) sum() float64 {
	return math.Float64frombits(h.sumBits.Load())
}

// Recorder is the process-wide metrics sink. All fields are concurrent-safe.
// The zero value is NOT usable — call New().
type Recorder struct {
	// counters maps the fully-rendered series key (e.g.
	// `burrow_http_requests_total{service="foo",method="GET",status="200"}`)
	// to its atomic counter. sync.Map gives us a lock-free hot path; the
	// LoadOrStore on first observation pays a single allocation.
	counters sync.Map // map[string]*atomic.Int64

	// histograms maps the per-label-set key (e.g.
	// `burrow_http_request_duration_seconds{service="foo",method="GET"}`) to
	// its histogramSeries.
	histograms sync.Map // map[string]*histogramSeries

	// gauges maps the per-label-set key to a float64 (stored as raw bits in
	// an atomic.Uint64 so set/read stay lock-free).
	gauges sync.Map // map[string]*atomic.Uint64

	// chainHash holds the audit chain's last hex digest. The hex string lives
	// in a label of the burrow_audit_chain_last_hash series; the sample value
	// is the constant 1.
	chainHashMu sync.RWMutex
	chainHash   string
}

// New returns a fresh Recorder ready for concurrent use.
func New() *Recorder {
	return &Recorder{}
}

// --- Label-key construction ------------------------------------------------

// labelString renders the canonical Prometheus label set for a series.
// Keys are emitted in the order supplied (matching closedSet's labels slice),
// values are escaped per the exposition format (\ → \\, " → \", \n → \n).
func labelString(keys, values []string) string {
	if len(keys) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(k)
		sb.WriteString(`="`)
		sb.WriteString(escapeLabelValue(values[i]))
		sb.WriteByte('"')
	}
	sb.WriteByte('}')
	return sb.String()
}

// escapeLabelValue applies the three required escapes from the Prometheus
// 0.0.4 exposition format.
func escapeLabelValue(v string) string {
	if !strings.ContainsAny(v, `\"`+"\n") {
		return v
	}
	var sb strings.Builder
	sb.Grow(len(v) + 4)
	for _, r := range v {
		switch r {
		case '\\':
			sb.WriteString(`\\`)
		case '"':
			sb.WriteString(`\"`)
		case '\n':
			sb.WriteString(`\n`)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// seriesKey returns "name{k1=v1,k2=v2,...}" — the canonical full key used by
// every counter / gauge / histogram map.
func seriesKey(name string, labelKeys, labelValues []string) string {
	return name + labelString(labelKeys, labelValues)
}

// incCounter increments the counter named `name` with the given labels by
// delta. Allocates a new *atomic.Int64 on first observation.
func (r *Recorder) incCounter(name string, labelKeys, labelValues []string, delta int64) {
	key := seriesKey(name, labelKeys, labelValues)
	v, ok := r.counters.Load(key)
	if !ok {
		v, _ = r.counters.LoadOrStore(key, new(atomic.Int64))
	}
	v.(*atomic.Int64).Add(delta)
}

// setGauge stores v as the gauge value for the named series.
func (r *Recorder) setGauge(name string, labelKeys, labelValues []string, v float64) {
	key := seriesKey(name, labelKeys, labelValues)
	cell, ok := r.gauges.Load(key)
	if !ok {
		cell, _ = r.gauges.LoadOrStore(key, new(atomic.Uint64))
	}
	cell.(*atomic.Uint64).Store(math.Float64bits(v))
}

// observeHistogram records v into the named histogram's series.
func (r *Recorder) observeHistogram(name string, labelKeys, labelValues []string, v float64) {
	key := seriesKey(name, labelKeys, labelValues)
	h, ok := r.histograms.Load(key)
	if !ok {
		h, _ = r.histograms.LoadOrStore(key, &histogramSeries{})
	}
	h.(*histogramSeries).observe(v)
}

// --- Spec Part O recorder API ---------------------------------------------
//
// One Inc/Observe/Set per metric in the closed set. Wiring the recorder into
// every emitter (proxy, aimeter, route, audit, db, control) is Task 25's job;
// for Task 21 the API + tests are the deliverable.

// IncHTTPRequest records one HTTP request through the proxy: it increments
// burrow_http_requests_total{service,method,status}. Status is converted to
// the canonical decimal string.
func (r *Recorder) IncHTTPRequest(service, method string, status int) {
	r.incCounter("burrow_http_requests_total",
		[]string{"service", "method", "status"},
		[]string{service, method, strconv.Itoa(status)},
		1)
}

// ObserveHTTPDuration records one HTTP request duration sample (seconds).
func (r *Recorder) ObserveHTTPDuration(service, method string, seconds float64) {
	r.observeHistogram("burrow_http_request_duration_seconds",
		[]string{"service", "method"},
		[]string{service, method},
		seconds)
}

// AddHTTPBytesIn adds n bytes to burrow_http_request_bytes_in_total{service}.
func (r *Recorder) AddHTTPBytesIn(service string, n int64) {
	r.incCounter("burrow_http_request_bytes_in_total",
		[]string{"service"}, []string{service}, n)
}

// AddHTTPBytesOut adds n bytes to burrow_http_request_bytes_out_total{service}.
func (r *Recorder) AddHTTPBytesOut(service string, n int64) {
	r.incCounter("burrow_http_request_bytes_out_total",
		[]string{"service"}, []string{service}, n)
}

// SetClientSessionCount sets burrow_client_session_count{user} to n.
func (r *Recorder) SetClientSessionCount(user string, n int) {
	r.setGauge("burrow_client_session_count",
		[]string{"user"}, []string{user}, float64(n))
}

// ObserveClientSessionDuration records one client control-session duration.
func (r *Recorder) ObserveClientSessionDuration(user string, seconds float64) {
	r.observeHistogram("burrow_client_session_duration_seconds",
		[]string{"user"}, []string{user}, seconds)
}

// AddClientBytesIn adds n bytes to burrow_client_bytes_in_total{user}.
func (r *Recorder) AddClientBytesIn(user string, n int64) {
	r.incCounter("burrow_client_bytes_in_total",
		[]string{"user"}, []string{user}, n)
}

// AddClientBytesOut adds n bytes to burrow_client_bytes_out_total{user}.
func (r *Recorder) AddClientBytesOut(user string, n int64) {
	r.incCounter("burrow_client_bytes_out_total",
		[]string{"user"}, []string{user}, n)
}

// AddAITokensIn adds n input tokens to burrow_ai_tokens_in_total{service,api_key}.
func (r *Recorder) AddAITokensIn(service, apiKey string, n int64) {
	r.incCounter("burrow_ai_tokens_in_total",
		[]string{"service", "api_key"}, []string{service, apiKey}, n)
}

// AddAITokensOut adds n output tokens to burrow_ai_tokens_out_total{service,api_key}.
func (r *Recorder) AddAITokensOut(service, apiKey string, n int64) {
	r.incCounter("burrow_ai_tokens_out_total",
		[]string{"service", "api_key"}, []string{service, apiKey}, n)
}

// AddAICostUSD adds USD cost to burrow_ai_cost_usd_total{service,api_key}. The
// counter stores micro-USD as int64 internally (1e6 = $1) to preserve the
// atomic discipline; render-time divides back to USD.
func (r *Recorder) AddAICostUSD(service, apiKey string, usd float64) {
	micro := int64(math.Round(usd * 1_000_000))
	r.incCounter("burrow_ai_cost_usd_total",
		[]string{"service", "api_key"}, []string{service, apiKey}, micro)
}

// IncAICacheHit records one prompt-cache hit.
func (r *Recorder) IncAICacheHit(service string) {
	r.incCounter("burrow_ai_cache_hits_total",
		[]string{"service"}, []string{service}, 1)
}

// IncAICacheMiss records one prompt-cache miss.
func (r *Recorder) IncAICacheMiss(service string) {
	r.incCounter("burrow_ai_cache_misses_total",
		[]string{"service"}, []string{service}, 1)
}

// IncAIFailover records a single failover event from→to inside service.
// crossProvider is "true" when the from and to backends have different
// provider values (e.g. ollama→anthropic), "false" when they are the same
// provider. This label was added in v0.5.0 (Task 6).
func (r *Recorder) IncAIFailover(service, from, to, crossProvider string) {
	r.incCounter("burrow_ai_failover_events_total",
		[]string{"service", "from", "to", "cross_provider"},
		[]string{service, from, to, crossProvider}, 1)
}

// IncAIUpstreamError records one upstream error for service at HTTP status.
func (r *Recorder) IncAIUpstreamError(service string, status int) {
	r.incCounter("burrow_ai_upstream_errors_total",
		[]string{"service", "status"},
		[]string{service, strconv.Itoa(status)}, 1)
}

// SetGoroutines sets burrow_goroutines to n. Callers typically pass
// runtime.NumGoroutine() at WriteText time.
func (r *Recorder) SetGoroutines(n int) {
	r.setGauge("burrow_goroutines", nil, nil, float64(n))
}

// ObserveDBQueryDuration records one DB query duration sample (seconds).
func (r *Recorder) ObserveDBQueryDuration(op string, seconds float64) {
	r.observeHistogram("burrow_db_query_duration_seconds",
		[]string{"op"}, []string{op}, seconds)
}

// IncControlReconnect records one client control-plane reconnect.
func (r *Recorder) IncControlReconnect(client string) {
	r.incCounter("burrow_control_reconnects_total",
		[]string{"client"}, []string{client}, 1)
}

// SetCertExpiryDays sets burrow_cert_expiry_days{cert} to d. Negative values
// indicate the cert has already expired.
func (r *Recorder) SetCertExpiryDays(cert string, d float64) {
	r.setGauge("burrow_cert_expiry_days",
		[]string{"cert"}, []string{cert}, d)
}

// SetAuditChainLength sets burrow_audit_chain_length to n.
func (r *Recorder) SetAuditChainLength(n int64) {
	r.setGauge("burrow_audit_chain_length", nil, nil, float64(n))
}

// SetAuditChainLastHash stores the audit chain's last hex digest. The next
// WriteText emits burrow_audit_chain_last_hash{hash="<hex>"} 1.
func (r *Recorder) SetAuditChainLastHash(hex string) {
	r.chainHashMu.Lock()
	r.chainHash = hex
	r.chainHashMu.Unlock()
}

// --- v0.5.0 metric methods (spec Part K) ----------------------------------

// IncAISemanticLookups records one semantic cache lookup attempt for service.
func (r *Recorder) IncAISemanticLookups(service string) {
	r.incCounter("burrow_ai_semantic_lookups_total",
		[]string{"service"}, []string{service}, 1)
}

// IncAISemanticHits records one semantic cache hit. policy must be one of
// "treat_as_miss" or "return_cached_marked".
func (r *Recorder) IncAISemanticHits(service, policy string) {
	r.incCounter("burrow_ai_semantic_hits_total",
		[]string{"service", "policy"}, []string{service, policy}, 1)
}

// IncAISemanticPromotions records one semantic cache promotion for service.
func (r *Recorder) IncAISemanticPromotions(service string) {
	r.incCounter("burrow_ai_semantic_promotions_total",
		[]string{"service"}, []string{service}, 1)
}

// IncAISemanticErrors records one semantic cache error. kind must be one of
// "timeout", "model", or "index".
func (r *Recorder) IncAISemanticErrors(service, kind string) {
	r.incCounter("burrow_ai_semantic_errors_total",
		[]string{"service", "kind"}, []string{service, kind}, 1)
}

// SetAISemanticIndexEntries sets burrow_ai_semantic_index_entries{service} to n.
func (r *Recorder) SetAISemanticIndexEntries(service string, n int64) {
	r.setGauge("burrow_ai_semantic_index_entries",
		[]string{"service"}, []string{service}, float64(n))
}

// SetAISemanticIndexBytes sets burrow_ai_semantic_index_bytes{service} to n.
func (r *Recorder) SetAISemanticIndexBytes(service string, n int64) {
	r.setGauge("burrow_ai_semantic_index_bytes",
		[]string{"service"}, []string{service}, float64(n))
}

// IncAICredentialInjections records one upstream credential injection for
// service at the given slot.
func (r *Recorder) IncAICredentialInjections(service, slot string) {
	r.incCounter("burrow_ai_credential_injections_total",
		[]string{"service", "slot"}, []string{service, slot}, 1)
}

// IncAICredentialMisses records one upstream credential miss (slot env var
// absent) for service.
func (r *Recorder) IncAICredentialMisses(service string) {
	r.incCounter("burrow_ai_credential_misses_total",
		[]string{"service"}, []string{service}, 1)
}

// SetCustomDomainCount sets burrow_custom_domain_count to n.
func (r *Recorder) SetCustomDomainCount(n int64) {
	r.setGauge("burrow_custom_domain_count", nil, nil, float64(n))
}

// IncConnections records one connection for the given service, kind, and
// status label tuple.
func (r *Recorder) IncConnections(service, kind, status string) {
	r.incCounter("burrow_connections_total",
		[]string{"service", "kind", "status"},
		[]string{service, kind, status}, 1)
}

// AddConnectionBytesIn adds n bytes to
// burrow_connection_bytes_in_total{service,kind}.
func (r *Recorder) AddConnectionBytesIn(service, kind string, n int64) {
	r.incCounter("burrow_connection_bytes_in_total",
		[]string{"service", "kind"}, []string{service, kind}, n)
}

// AddConnectionBytesOut adds n bytes to
// burrow_connection_bytes_out_total{service,kind}.
func (r *Recorder) AddConnectionBytesOut(service, kind string, n int64) {
	r.incCounter("burrow_connection_bytes_out_total",
		[]string{"service", "kind"}, []string{service, kind}, n)
}

// AddRetentionCompactRowsDeleted adds n to
// burrow_retention_compact_rows_deleted_total{table}.
func (r *Recorder) AddRetentionCompactRowsDeleted(table string, n int64) {
	r.incCounter("burrow_retention_compact_rows_deleted_total",
		[]string{"table"}, []string{table}, n)
}

// SetRetentionCompactLastRunSeconds sets
// burrow_retention_compact_last_run_seconds to the UTC Unix timestamp ts.
func (r *Recorder) SetRetentionCompactLastRunSeconds(ts int64) {
	r.setGauge("burrow_retention_compact_last_run_seconds", nil, nil, float64(ts))
}

// SetDBBackend sets burrow_db_backend{driver} to 1. The value is always 1;
// the driver label carries the identity ("sqlite", "postgres").
func (r *Recorder) SetDBBackend(driver string) {
	r.setGauge("burrow_db_backend",
		[]string{"driver"}, []string{driver}, 1)
}

// SetDBPoolActive sets burrow_db_pool_active to n. For SQLite this is always
// 1; call it with 0 when no pool is active.
func (r *Recorder) SetDBPoolActive(n int64) {
	r.setGauge("burrow_db_pool_active", nil, nil, float64(n))
}

// SetDBPoolIdle sets burrow_db_pool_idle to n. For SQLite this is always 0.
func (r *Recorder) SetDBPoolIdle(n int64) {
	r.setGauge("burrow_db_pool_idle", nil, nil, float64(n))
}

// --- WriteText -------------------------------------------------------------

// WriteText emits the full closed metric set in Prometheus 0.0.4 exposition
// format to w. Series are sorted by key inside each metric so the output is
// stable across scrapes (downstream golden tests can rely on it).
//
// The function is safe to call concurrently with any Inc/Observe/Set call:
// each metric's snapshot is taken via an atomic load; the rendered text is
// emitted without holding any lock.
func (r *Recorder) WriteText(w io.Writer) error {
	bw := &writeErr{w: w}

	for _, def := range closedSet {
		bw.printf("# HELP %s %s\n", def.name, def.help)
		bw.printf("# TYPE %s %s\n", def.name, kindName(def.kind))
		if bw.err != nil {
			return bw.err
		}
		switch def.kind {
		case kindCounter:
			r.writeCounter(bw, def)
		case kindGauge:
			r.writeGauge(bw, def)
		case kindHistogram:
			r.writeHistogram(bw, def)
		}
		if bw.err != nil {
			return bw.err
		}
	}
	return bw.err
}

// writeCounter walks every series under a counter metric and emits one
// sample line per series, sorted by full series key.
func (r *Recorder) writeCounter(bw *writeErr, def metricDef) {
	type sample struct {
		key string
		val int64
	}
	var rows []sample
	r.counters.Range(func(k, v any) bool {
		ks := k.(string)
		// Filter by metric name — the key may belong to a different metric.
		if !strings.HasPrefix(ks, def.name) {
			return true
		}
		// The label-set form is "name{...}" — guard against a same-prefix
		// metric (e.g. burrow_http_requests vs burrow_http_request_bytes_in;
		// HasPrefix is not enough, so we require the next byte to be '{' or
		// end-of-string for unlabeled metrics).
		if len(ks) > len(def.name) && ks[len(def.name)] != '{' {
			return true
		}
		rows = append(rows, sample{key: ks, val: v.(*atomic.Int64).Load()})
		return true
	})
	sort.Slice(rows, func(i, j int) bool { return rows[i].key < rows[j].key })
	for _, s := range rows {
		// burrow_ai_cost_usd_total is stored as int64 micro-USD; render in USD.
		if def.name == "burrow_ai_cost_usd_total" {
			bw.printf("%s %s\n", s.key, formatFloat(float64(s.val)/1_000_000))
			continue
		}
		bw.printf("%s %d\n", s.key, s.val)
	}
	// Unlabeled counters with no observations emit no sample lines (only the
	// HELP/TYPE preamble). Prometheus tolerates that — the metric is still
	// declared, just not yet observed.
}

// writeGauge walks every gauge series and emits sample lines, plus the
// special burrow_audit_chain_last_hash series synthesised from chainHash.
func (r *Recorder) writeGauge(bw *writeErr, def metricDef) {
	if def.name == "burrow_audit_chain_last_hash" {
		r.chainHashMu.RLock()
		h := r.chainHash
		r.chainHashMu.RUnlock()
		if h != "" {
			bw.printf("%s{hash=\"%s\"} 1\n", def.name, escapeLabelValue(h))
		}
		return
	}
	type sample struct {
		key string
		val float64
	}
	var rows []sample
	r.gauges.Range(func(k, v any) bool {
		ks := k.(string)
		if !strings.HasPrefix(ks, def.name) {
			return true
		}
		if len(ks) > len(def.name) && ks[len(def.name)] != '{' {
			return true
		}
		rows = append(rows, sample{key: ks, val: math.Float64frombits(v.(*atomic.Uint64).Load())})
		return true
	})
	sort.Slice(rows, func(i, j int) bool { return rows[i].key < rows[j].key })
	for _, s := range rows {
		bw.printf("%s %s\n", s.key, formatFloat(s.val))
	}
}

// writeHistogram walks every histogram series and emits, for each:
//   - one sample per quantile in {0.5, 0.95, 0.99}, with the quantile label
//     appended to the existing label set (Prometheus 0.0.4 summary form).
//   - one <name>_count sample with the total observation count.
//   - one <name>_sum sample with the running sum.
func (r *Recorder) writeHistogram(bw *writeErr, def metricDef) {
	type entry struct {
		key string
		h   *histogramSeries
	}
	var rows []entry
	r.histograms.Range(func(k, v any) bool {
		ks := k.(string)
		if !strings.HasPrefix(ks, def.name) {
			return true
		}
		if len(ks) > len(def.name) && ks[len(def.name)] != '{' {
			return true
		}
		rows = append(rows, entry{key: ks, h: v.(*histogramSeries)})
		return true
	})
	sort.Slice(rows, func(i, j int) bool { return rows[i].key < rows[j].key })
	for _, e := range rows {
		// e.key looks like `name{a="x",b="y"}` (or just `name` for an
		// unlabeled metric — none in v0.4.0, but the code handles it).
		base, labelInner := splitLabels(e.key, def.name)
		for _, q := range quantiles {
			qKey := base + appendLabel(labelInner, "quantile", strconv.FormatFloat(q, 'g', -1, 64))
			bw.printf("%s %s\n", qKey, formatFloat(e.h.quantile(q)))
		}
		// _count and _sum carry the metric's original label set (no quantile).
		bw.printf("%s_count%s %d\n", def.name, labelInner, e.h.count.Load())
		bw.printf("%s_sum%s %s\n", def.name, labelInner, formatFloat(e.h.sum()))
	}
}

// splitLabels splits a full series key "name{a=...,b=...}" into its metric
// base ("name") and the brace-wrapped inner block (`{a=...,b=...}` or "").
func splitLabels(key, name string) (string, string) {
	if len(key) == len(name) || key[len(name)] != '{' {
		return name, ""
	}
	return name, key[len(name):]
}

// appendLabel inserts (k=v) into an existing brace-wrapped label block,
// preserving the existing keys. The new key is appended to the end so it
// always shows up as the last label — matches what Prometheus emits when
// summary-style quantiles are added to a series.
func appendLabel(inner, k, v string) string {
	if inner == "" {
		return "{" + k + `="` + escapeLabelValue(v) + `"}`
	}
	// inner is "{a=\"x\",b=\"y\"}" — strip the trailing '}' and append.
	return inner[:len(inner)-1] + "," + k + `="` + escapeLabelValue(v) + `"}`
}

// formatFloat renders v with %g (no trailing-zero noise). Special values are
// emitted exactly as Prometheus expects: NaN / +Inf / -Inf.
func formatFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// kindName converts a metricKind to its Prometheus exposition TYPE token.
// Histograms in our hand-rolled flavor are emitted as "summary" because each
// series has per-quantile samples (no _bucket samples) — exactly the shape
// Grafana expects from a pre-binned summary.
func kindName(k metricKind) string {
	switch k {
	case kindCounter:
		return "counter"
	case kindGauge:
		return "gauge"
	case kindHistogram:
		return "summary"
	}
	return "untyped"
}

// writeErr is a thin error-sticky writer: once any Write fails, subsequent
// printf calls become no-ops. Keeps WriteText's main loop linear.
type writeErr struct {
	w   io.Writer
	err error
}

func (we *writeErr) printf(format string, args ...any) {
	if we.err != nil {
		return
	}
	_, we.err = fmt.Fprintf(we.w, format, args...)
}
