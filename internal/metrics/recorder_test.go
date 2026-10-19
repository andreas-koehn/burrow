package metrics

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// allMetricNames is the closed set of metric names asserted by the contract
// test below. Mirrors spec Part O verbatim — adding a metric requires a doc PR
// AND this slice must grow in lockstep, which is exactly the friction the
// spec wanted to introduce.
var allMetricNames = []string{
	// HTTP per tunnel.
	"burrow_http_requests_total",
	"burrow_http_request_duration_seconds",
	"burrow_http_request_bytes_in_total",
	"burrow_http_request_bytes_out_total",
	// Connection per client.
	"burrow_client_session_count",
	"burrow_client_session_duration_seconds",
	"burrow_client_bytes_in_total",
	"burrow_client_bytes_out_total",
	// AI per service / key.
	"burrow_ai_tokens_in_total",
	"burrow_ai_tokens_out_total",
	"burrow_ai_cost_usd_total",
	"burrow_ai_cache_hits_total",
	"burrow_ai_cache_misses_total",
	"burrow_ai_failover_events_total",
	"burrow_ai_upstream_errors_total",
	// Internal.
	"burrow_goroutines",
	"burrow_db_query_duration_seconds",
	"burrow_control_reconnects_total",
	"burrow_cert_expiry_days",
	"burrow_audit_chain_length",
	"burrow_audit_chain_last_hash",
	// Semantic cache (v0.5.0, spec Part K).
	"burrow_ai_semantic_lookups_total",
	"burrow_ai_semantic_hits_total",
	"burrow_ai_semantic_promotions_total",
	"burrow_ai_semantic_errors_total",
	"burrow_ai_semantic_index_entries",
	"burrow_ai_semantic_index_bytes",
	// Upstream-credential injection (v0.5.0, spec Part K).
	"burrow_ai_credential_injections_total",
	"burrow_ai_credential_misses_total",
	// MCP tool calls (v0.12.0).
	"burrow_mcp_tool_calls_total",
	// Custom domains (v0.5.0, spec Part K).
	"burrow_custom_domain_count",
	// Connection logs (v0.5.0, spec Part K).
	"burrow_connections_total",
	"burrow_connection_bytes_in_total",
	"burrow_connection_bytes_out_total",
	// Retention (v0.5.0, spec Part K).
	"burrow_retention_compact_rows_deleted_total",
	"burrow_retention_compact_last_run_seconds",
	// Database (v0.5.0, spec Part K).
	"burrow_db_backend",
	"burrow_db_pool_active",
	"burrow_db_pool_idle",
}

// parsedSample is one (name, labels, value) sample line from the exposition
// stream. Tests assert on names + label sets; the value is exposed for the
// concurrency test.
type parsedSample struct {
	Name   string
	Labels map[string]string
	Value  string
}

// parsedMetric is one HELP/TYPE/samples block from the exposition stream.
type parsedMetric struct {
	Name    string
	Help    string
	Type    string
	Samples []parsedSample
}

// parseExposition is a tiny ad-hoc parser for the Prometheus 0.0.4 text
// format — JUST enough to validate the recorder output. It handles HELP, TYPE
// and sample lines (with or without labels), ignores blank lines, and rejects
// nothing — its only job is to extract the metric set the recorder emits.
//
// Label-value escaping is reversed (\" → ", \\ → \, \n → newline). Keys are
// expected to be plain identifiers (no escaping needed in the closed set).
func parseExposition(t *testing.T, in string) map[string]*parsedMetric {
	t.Helper()
	out := map[string]*parsedMetric{}
	getOrInit := func(name string) *parsedMetric {
		m, ok := out[name]
		if !ok {
			m = &parsedMetric{Name: name}
			out[name] = m
		}
		return m
	}
	for _, raw := range strings.Split(in, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# HELP ") {
			rest := strings.TrimPrefix(line, "# HELP ")
			name, help, ok := splitFirst(rest, " ")
			if !ok {
				t.Fatalf("malformed HELP line: %q", line)
			}
			getOrInit(name).Help = help
			continue
		}
		if strings.HasPrefix(line, "# TYPE ") {
			rest := strings.TrimPrefix(line, "# TYPE ")
			name, typ, ok := splitFirst(rest, " ")
			if !ok {
				t.Fatalf("malformed TYPE line: %q", line)
			}
			getOrInit(name).Type = typ
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		s, baseName, ok := parseSampleLine(t, line)
		if !ok {
			t.Fatalf("unparseable sample line: %q", line)
		}
		// The base metric for histogram _count/_sum lines is the histogram
		// name itself (strip the suffix). The full sample-name is kept on
		// the sample so callers can find e.g. `burrow_http_requests_total`
		// AND `burrow_http_request_duration_seconds_count` independently.
		getOrInit(baseName).Samples = append(getOrInit(baseName).Samples, s)
	}
	return out
}

// parseSampleLine extracts `name{labels} value` from one exposition line.
// It returns the sample plus the "base" metric name (stripping _count / _sum
// suffixes so histogram-companion lines roll up under the parent metric).
func parseSampleLine(t *testing.T, line string) (parsedSample, string, bool) {
	t.Helper()
	// Find the start of labels '{' or the start of the value (first space).
	brace := strings.IndexByte(line, '{')
	var name, labelInner, valueStr string
	if brace == -1 {
		// "name value"
		i := strings.LastIndexByte(line, ' ')
		if i == -1 {
			return parsedSample{}, "", false
		}
		name = line[:i]
		valueStr = line[i+1:]
	} else {
		name = line[:brace]
		close := strings.IndexByte(line[brace:], '}')
		if close == -1 {
			return parsedSample{}, "", false
		}
		labelInner = line[brace+1 : brace+close]
		// after '}' there should be a space then the value
		rest := strings.TrimLeft(line[brace+close+1:], " ")
		valueStr = rest
	}
	labels, ok := parseLabels(labelInner)
	if !ok {
		t.Fatalf("malformed labels in %q", line)
	}
	base := name
	// Histogram companion samples (`<name>_count`, `<name>_sum`) roll up
	// under the parent histogram metric, BUT only if the trimmed name is
	// itself in the closed set — otherwise `burrow_client_session_count`
	// (a gauge) would wrongly get rolled into `burrow_client_session`.
	if trimmed, ok := stripIfHistogramCompanion(name); ok {
		base = trimmed
	}
	return parsedSample{Name: name, Labels: labels, Value: valueStr}, base, true
}

// parseLabels decodes `a="x",b="y"` into a map. Reverses the three required
// exposition-format escapes.
func parseLabels(s string) (map[string]string, bool) {
	out := map[string]string{}
	if s == "" {
		return out, true
	}
	i := 0
	for i < len(s) {
		// key
		eq := strings.IndexByte(s[i:], '=')
		if eq == -1 {
			return nil, false
		}
		key := s[i : i+eq]
		i += eq + 1
		if i >= len(s) || s[i] != '"' {
			return nil, false
		}
		i++ // skip opening quote
		var sb strings.Builder
		for i < len(s) && s[i] != '"' {
			if s[i] == '\\' && i+1 < len(s) {
				switch s[i+1] {
				case '\\':
					sb.WriteByte('\\')
				case '"':
					sb.WriteByte('"')
				case 'n':
					sb.WriteByte('\n')
				default:
					sb.WriteByte(s[i+1])
				}
				i += 2
				continue
			}
			sb.WriteByte(s[i])
			i++
		}
		if i >= len(s) {
			return nil, false
		}
		i++ // skip closing quote
		out[key] = sb.String()
		if i < len(s) && s[i] == ',' {
			i++
		}
	}
	return out, true
}

// stripIfHistogramCompanion returns (base, true) if name ends in _count or
// _sum AND the resulting base is a closed-set histogram. Otherwise (name,
// false) — the name stands on its own (e.g. a gauge that happens to end in
// _count).
func stripIfHistogramCompanion(name string) (string, bool) {
	histograms := map[string]bool{
		"burrow_http_request_duration_seconds":   true,
		"burrow_client_session_duration_seconds": true,
		"burrow_db_query_duration_seconds":       true,
	}
	for _, sfx := range []string{"_count", "_sum"} {
		if strings.HasSuffix(name, sfx) {
			base := strings.TrimSuffix(name, sfx)
			if histograms[base] {
				return base, true
			}
		}
	}
	return name, false
}

func splitFirst(s, sep string) (string, string, bool) {
	i := strings.Index(s, sep)
	if i == -1 {
		return "", "", false
	}
	return s[:i], s[i+len(sep):], true
}

// TestWriteTextClosedSetPresent asserts every metric in the spec Part O closed
// set has BOTH `# HELP` and `# TYPE` lines emitted, even when no observation
// has been recorded — the registry is closed at startup, not at first-use.
func TestWriteTextClosedSetPresent(t *testing.T) {
	r := New()
	// Touch one series under every metric so the renderer emits a sample
	// (and so we exercise the path with a non-empty value for the parser).
	r.IncHTTPRequest("svc", "GET", 200)
	r.ObserveHTTPDuration("svc", "GET", 0.123)
	r.AddHTTPBytesIn("svc", 100)
	r.AddHTTPBytesOut("svc", 200)
	r.SetClientSessionCount("u", 3)
	r.ObserveClientSessionDuration("u", 1.5)
	r.AddClientBytesIn("u", 10)
	r.AddClientBytesOut("u", 20)
	r.AddAITokensIn("svc", "k", 5)
	r.AddAITokensOut("svc", "k", 7)
	r.AddAICostUSD("svc", "k", 0.0125)
	r.IncAICacheHit("svc")
	r.IncAICacheMiss("svc")
	r.IncAIFailover("svc", "anthropic", "openai", "true")
	r.IncAIUpstreamError("svc", 502)
	r.SetGoroutines(42)
	r.ObserveDBQueryDuration("select", 0.01)
	r.IncControlReconnect("c1")
	r.SetCertExpiryDays("wildcard.example.com", 30)
	r.SetAuditChainLength(17)
	r.SetAuditChainLastHash("deadbeef")
	// v0.5.0 — semantic cache.
	r.IncAISemanticLookups("svc")
	r.IncAISemanticHits("svc", "return_cached_marked")
	r.IncAISemanticPromotions("svc")
	r.IncAISemanticErrors("svc", "timeout")
	r.SetAISemanticIndexEntries("svc", 12)
	r.SetAISemanticIndexBytes("svc", 4096)
	// v0.5.0 — upstream-credential injection.
	r.IncAICredentialInjections("svc", "OPENAI_API_KEY")
	r.IncAICredentialMisses("svc")
	// v0.12.0 — MCP tool calls.
	r.IncMCPToolCall("burrow", "ok")
	// v0.5.0 — custom domains.
	r.SetCustomDomainCount(3)
	// v0.5.0 — connection logs.
	r.IncConnections("svc", "http_proxy", "closed_clean")
	r.AddConnectionBytesIn("svc", "http_proxy", 1024)
	r.AddConnectionBytesOut("svc", "http_proxy", 2048)
	// v0.5.0 — retention.
	r.AddRetentionCompactRowsDeleted("audit_logs", 50)
	r.SetRetentionCompactLastRunSeconds(1_748_000_000)
	// v0.5.0 — database.
	r.SetDBBackend("sqlite")
	r.SetDBPoolActive(1)
	r.SetDBPoolIdle(0)

	var buf bytes.Buffer
	if err := r.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	got := parseExposition(t, buf.String())

	for _, name := range allMetricNames {
		m, ok := got[name]
		if !ok {
			t.Errorf("missing metric %q in output", name)
			continue
		}
		if m.Help == "" {
			t.Errorf("metric %q missing HELP line", name)
		}
		if m.Type == "" {
			t.Errorf("metric %q missing TYPE line", name)
		}
		if len(m.Samples) == 0 {
			t.Errorf("metric %q has no samples", name)
		}
	}

	// Sanity: HTTP cost converted from micro-USD back to USD.
	costSeries := got["burrow_ai_cost_usd_total"]
	if costSeries == nil || len(costSeries.Samples) == 0 {
		t.Fatal("burrow_ai_cost_usd_total missing")
	}
	if v := costSeries.Samples[0].Value; v != "0.0125" {
		t.Errorf("ai cost rendered as %q want 0.0125", v)
	}
}

// TestIncHTTPRequestTwoLabelSetsTwoSeries asserts two distinct label tuples
// produce two distinct series (spec Part O: per-label-set counter shape).
func TestIncHTTPRequestTwoLabelSetsTwoSeries(t *testing.T) {
	r := New()
	r.IncHTTPRequest("svc-a", "GET", 200)
	r.IncHTTPRequest("svc-a", "GET", 200) // same series → +1
	r.IncHTTPRequest("svc-b", "POST", 500)

	var buf bytes.Buffer
	if err := r.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	got := parseExposition(t, buf.String())
	reqs := got["burrow_http_requests_total"]
	if reqs == nil {
		t.Fatal("missing burrow_http_requests_total")
	}
	if len(reqs.Samples) != 2 {
		t.Fatalf("want 2 series, got %d: %+v", len(reqs.Samples), reqs.Samples)
	}
	// Find both series by their label triple.
	want := map[string]string{
		"svc-a|GET|2xx":  "2",
		"svc-b|POST|5xx": "1",
	}
	for _, s := range reqs.Samples {
		k := s.Labels["service"] + "|" + s.Labels["method"] + "|" + s.Labels["status"]
		exp, ok := want[k]
		if !ok {
			t.Errorf("unexpected series %s", k)
			continue
		}
		if s.Value != exp {
			t.Errorf("series %s value=%s want %s", k, s.Value, exp)
		}
		delete(want, k)
	}
	for k := range want {
		t.Errorf("missing series %s", k)
	}
}

// TestConcurrentIncCounter runs 1000 goroutines × 1000 Inc calls each against
// the same series and asserts the final atomic count is exactly 1,000,000 —
// the property the sync.Map[*atomic.Int64] hot path is designed to guarantee.
func TestConcurrentIncCounter(t *testing.T) {
	r := New()
	const goroutines = 1000
	const each = 1000
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				r.IncHTTPRequest("svc", "GET", 200)
			}
		}()
	}
	wg.Wait()

	v, ok := r.counters.Load(`burrow_http_requests_total{service="svc",method="GET",status="2xx"}`)
	if !ok {
		t.Fatal("series missing after concurrent Inc")
	}
	got := v.(*atomic.Int64).Load()
	if got != goroutines*each {
		t.Fatalf("got %d want %d", got, goroutines*each)
	}
}

// TestHistogramQuantilesMonotonic asserts the linear interpolation across
// buckets returns values inside the histogram's observed range, with 0.5 ≤
// 0.95 ≤ 0.99. This is the property summary consumers (Grafana) depend on.
func TestHistogramQuantilesMonotonic(t *testing.T) {
	r := New()
	// 100 observations spread across the bucket range.
	for i := 0; i < 100; i++ {
		r.ObserveHTTPDuration("svc", "GET", float64(i)*0.05) // 0..5 sec
	}
	var buf bytes.Buffer
	if err := r.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	got := parseExposition(t, buf.String())
	m := got["burrow_http_request_duration_seconds"]
	if m == nil {
		t.Fatal("missing histogram metric")
	}
	// Build a quantile→value map for the only series.
	q := map[string]float64{}
	for _, s := range m.Samples {
		if s.Name == "burrow_http_request_duration_seconds" {
			// summary quantile sample
			parseFloat := func(str string) float64 {
				var f float64
				if _, err := fmt.Sscan(str, &f); err != nil {
					t.Fatalf("bad value %q: %v", str, err)
				}
				return f
			}
			q[s.Labels["quantile"]] = parseFloat(s.Value)
		}
	}
	if len(q) != 3 {
		t.Fatalf("want 3 quantile samples got %d (%+v)", len(q), q)
	}
	if !(q["0.5"] <= q["0.95"] && q["0.95"] <= q["0.99"]) {
		t.Errorf("quantiles not monotonic: %+v", q)
	}
	// _count and _sum must be present too.
	var sawCount, sawSum bool
	for _, s := range m.Samples {
		if s.Name == "burrow_http_request_duration_seconds_count" {
			sawCount = true
			if s.Value != "100" {
				t.Errorf("_count=%s want 100", s.Value)
			}
		}
		if s.Name == "burrow_http_request_duration_seconds_sum" {
			sawSum = true
		}
	}
	if !sawCount || !sawSum {
		t.Errorf("missing _count(%v) or _sum(%v)", sawCount, sawSum)
	}
}

// TestV050MetricAdditions is the golden test for the v0.5.0 spec Part K metric
// additions. It asserts that every new metric:
//   - has a HELP line
//   - has a TYPE line
//   - has at least one sample with the correct label keys
//
// Mirror of TestWriteTextClosedSetPresent but isolated so failures are clearly
// attributable to the Part K additions.
func TestV050MetricAdditions(t *testing.T) {
	r := New()

	// Semantic cache.
	r.IncAISemanticLookups("svcA")
	r.IncAISemanticHits("svcA", "return_cached_marked")
	r.IncAISemanticHits("svcA", "treat_as_miss")
	r.IncAISemanticPromotions("svcA")
	r.IncAISemanticErrors("svcA", "timeout")
	r.IncAISemanticErrors("svcA", "model")
	r.IncAISemanticErrors("svcA", "index")
	r.SetAISemanticIndexEntries("svcA", 100)
	r.SetAISemanticIndexBytes("svcA", 81920)

	// Upstream-credential injection.
	r.IncAICredentialInjections("svcA", "OPENAI_KEY")
	r.IncAICredentialInjections("svcA", "ANTHROPIC_KEY")
	r.IncAICredentialMisses("svcA")

	// Custom domains.
	r.SetCustomDomainCount(5)

	// Connection logs.
	r.IncConnections("svcA", "http_proxy", "closed_clean")
	r.IncConnections("svcA", "http_proxy", "closed_error")
	r.IncConnections("svcA", "control", "rejected")
	r.AddConnectionBytesIn("svcA", "http_proxy", 512)
	r.AddConnectionBytesOut("svcA", "http_proxy", 1024)

	// Retention.
	r.AddRetentionCompactRowsDeleted("audit_logs", 200)
	r.AddRetentionCompactRowsDeleted("connection_logs", 100)
	r.SetRetentionCompactLastRunSeconds(1_748_000_000)

	// Database.
	r.SetDBBackend("sqlite")
	r.SetDBPoolActive(1)
	r.SetDBPoolIdle(0)

	var buf bytes.Buffer
	if err := r.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	got := parseExposition(t, buf.String())

	type labelCheck struct {
		metric string
		labels map[string]string // subset that must be present on at least one sample
	}
	checks := []labelCheck{
		// Semantic cache.
		{"burrow_ai_semantic_lookups_total", map[string]string{"service": "svcA"}},
		{"burrow_ai_semantic_hits_total", map[string]string{"service": "svcA", "policy": "return_cached_marked"}},
		{"burrow_ai_semantic_hits_total", map[string]string{"service": "svcA", "policy": "treat_as_miss"}},
		{"burrow_ai_semantic_promotions_total", map[string]string{"service": "svcA"}},
		{"burrow_ai_semantic_errors_total", map[string]string{"service": "svcA", "kind": "timeout"}},
		{"burrow_ai_semantic_errors_total", map[string]string{"service": "svcA", "kind": "model"}},
		{"burrow_ai_semantic_errors_total", map[string]string{"service": "svcA", "kind": "index"}},
		{"burrow_ai_semantic_index_entries", map[string]string{"service": "svcA"}},
		{"burrow_ai_semantic_index_bytes", map[string]string{"service": "svcA"}},
		// Upstream-credential injection.
		{"burrow_ai_credential_injections_total", map[string]string{"service": "svcA", "slot": "OPENAI_KEY"}},
		{"burrow_ai_credential_injections_total", map[string]string{"service": "svcA", "slot": "ANTHROPIC_KEY"}},
		{"burrow_ai_credential_misses_total", map[string]string{"service": "svcA"}},
		// Custom domains — unlabeled gauge.
		{"burrow_custom_domain_count", nil},
		// Connection logs.
		{"burrow_connections_total", map[string]string{"service": "svcA", "kind": "http_proxy", "status": "closed_clean"}},
		{"burrow_connections_total", map[string]string{"service": "svcA", "kind": "http_proxy", "status": "closed_error"}},
		{"burrow_connections_total", map[string]string{"service": "svcA", "kind": "control", "status": "rejected"}},
		{"burrow_connection_bytes_in_total", map[string]string{"service": "svcA", "kind": "http_proxy"}},
		{"burrow_connection_bytes_out_total", map[string]string{"service": "svcA", "kind": "http_proxy"}},
		// Retention.
		{"burrow_retention_compact_rows_deleted_total", map[string]string{"table": "audit_logs"}},
		{"burrow_retention_compact_rows_deleted_total", map[string]string{"table": "connection_logs"}},
		{"burrow_retention_compact_last_run_seconds", nil},
		// Database.
		{"burrow_db_backend", map[string]string{"driver": "sqlite"}},
		{"burrow_db_pool_active", nil},
		{"burrow_db_pool_idle", nil},
	}

	for _, c := range checks {
		m, ok := got[c.metric]
		if !ok {
			t.Errorf("metric %q missing from output", c.metric)
			continue
		}
		if m.Help == "" {
			t.Errorf("metric %q missing HELP line", c.metric)
		}
		if m.Type == "" {
			t.Errorf("metric %q missing TYPE line", c.metric)
		}
		if c.labels == nil {
			// Unlabeled gauge — just need at least one sample.
			if len(m.Samples) == 0 {
				t.Errorf("metric %q has no samples", c.metric)
			}
			continue
		}
		// Find a sample that contains all required labels.
		found := false
		for _, s := range m.Samples {
			match := true
			for k, v := range c.labels {
				if s.Labels[k] != v {
					match = false
					break
				}
			}
			if match {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("metric %q: no sample with labels %v (got %+v)", c.metric, c.labels, m.Samples)
		}
	}

	// Spot-check specific counter values.
	connTotal := got["burrow_connections_total"]
	if connTotal == nil {
		t.Fatal("burrow_connections_total missing")
	}
	for _, s := range connTotal.Samples {
		if s.Labels["kind"] == "http_proxy" && s.Labels["status"] == "closed_clean" {
			if s.Value != "1" {
				t.Errorf("burrow_connections_total{http_proxy,closed_clean}=%s want 1", s.Value)
			}
		}
	}

	retentionRows := got["burrow_retention_compact_rows_deleted_total"]
	if retentionRows == nil {
		t.Fatal("burrow_retention_compact_rows_deleted_total missing")
	}
	for _, s := range retentionRows.Samples {
		if s.Labels["table"] == "audit_logs" && s.Value != "200" {
			t.Errorf("retention audit_logs=%s want 200", s.Value)
		}
	}
}

// TestLabelEscaping exercises the three required exposition-format escapes
// (\ → \\, " → \", \n → \n) end-to-end (write → parse round-trip).
func TestLabelEscaping(t *testing.T) {
	r := New()
	r.IncHTTPRequest(`svc with "quote" \ and `+"\n", "GET", 200)
	var buf bytes.Buffer
	if err := r.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	got := parseExposition(t, buf.String())
	m := got["burrow_http_requests_total"]
	if m == nil || len(m.Samples) != 1 {
		t.Fatalf("want exactly one series, got %+v", m)
	}
	wantSvc := `svc with "quote" \ and ` + "\n"
	if m.Samples[0].Labels["service"] != wantSvc {
		t.Errorf("escaped service mismatch: got %q want %q", m.Samples[0].Labels["service"], wantSvc)
	}
}

// TestLabelValuesAreBounded feeds every recording method that takes a value
// a visitor, a client or an upstream chooses a thousand different ones, and
// asserts the families stay at a handful of series: the label values come
// from fixed vocabularies, whatever is passed in.
func TestLabelValuesAreBounded(t *testing.T) {
	r := New()
	for i := 0; i < 1000; i++ {
		junk := "x" + strconv.Itoa(i)
		r.IncHTTPRequest("svc", junk, 100+i)
		r.ObserveHTTPDuration("svc", junk, 0.01)
		r.IncAIUpstreamError("svc", 100+i)
		r.IncAISemanticHits("svc", junk)
		r.IncAISemanticErrors("svc", junk)
		r.IncConnections("svc", junk, junk)
		r.AddConnectionBytesIn("svc", junk, 1)
		r.AddConnectionBytesOut("svc", junk, 1)
		r.IncAIFailover("svc", "a", "b", junk)
	}
	var buf bytes.Buffer
	if err := r.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	got := parseExposition(t, buf.String())
	for name, max := range map[string]int{
		"burrow_http_requests_total":           6, // one method ("other") × five classes and "none"
		"burrow_http_request_duration_seconds": 5, // one series: three quantiles, _count, _sum
		"burrow_ai_upstream_errors_total":      6,
		"burrow_ai_semantic_hits_total":        1,
		"burrow_ai_semantic_errors_total":      1,
		"burrow_connections_total":             1,
		"burrow_connection_bytes_in_total":     1,
		"burrow_connection_bytes_out_total":    1,
		"burrow_ai_failover_events_total":      1,
	} {
		if n := len(got[name].Samples); n == 0 || n > max {
			t.Errorf("%s has %d series after 1000 different values, want 1..%d", name, n, max)
		}
	}
	for in, want := range map[int]string{0: "none", 99: "none", 100: "1xx", 204: "2xx", 302: "3xx", 404: "4xx", 503: "5xx", 600: "none", -1: "none"} {
		if got := StatusClass(in); got != want {
			t.Errorf("StatusClass(%d) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"GET": "GET", "PATCH": "PATCH", "get": "other", "": "other", "PROPFIND": "other", "GET\n": "other"} {
		if got := Method(in); got != want {
			t.Errorf("Method(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCollectorsRunOnWriteText: a collector runs on every scrape, before the
// text is rendered; one that panics does not break the scrape or the others.
func TestCollectorsRunOnWriteText(t *testing.T) {
	r := New()
	n := 0
	r.AddCollector(func() { panic("broken collector") })
	r.AddCollector(func() { n++; r.SetCustomDomainCount(int64(n)) })
	r.AddCollector(nil)
	for want := 1; want <= 2; want++ {
		var buf bytes.Buffer
		if err := r.WriteText(&buf); err != nil {
			t.Fatal(err)
		}
		got := parseExposition(t, buf.String())["burrow_custom_domain_count"]
		if got == nil || len(got.Samples) != 1 || got.Samples[0].Value != strconv.Itoa(want) {
			t.Fatalf("scrape %d: burrow_custom_domain_count = %+v", want, got)
		}
	}
}

// A tool call is counted per source and outcome. The tool is never a label,
// and an outcome outside the closed set is "other".
func TestMCPToolCallMetric(t *testing.T) {
	r := New()
	r.IncMCPToolCall("github", "ok")
	r.IncMCPToolCall("github", "ok")
	r.IncMCPToolCall("github", "denied")
	r.IncMCPToolCall("burrow", "error")
	r.IncMCPToolCall("burrow", "rate-limited!")
	var buf strings.Builder
	if err := r.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`burrow_mcp_tool_calls_total{source="github",outcome="ok"} 2`,
		`burrow_mcp_tool_calls_total{source="github",outcome="denied"} 1`,
		`burrow_mcp_tool_calls_total{source="burrow",outcome="error"} 1`,
		`burrow_mcp_tool_calls_total{source="burrow",outcome="other"} 1`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, `tool="`) {
		t.Error("a tool label appeared")
	}
}
