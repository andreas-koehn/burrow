package aimeter_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/aimeter"
	"github.com/ankoehn/burrow/internal/db"
)

// TestAnthropicStreamUsageAccumulates — scenario 3 of the plan: feed
// message_start + 3 content_block_delta + message_delta (with usage) +
// message_stop and assert the accumulator returns the Anthropic-shaped
// tokens.
func TestAnthropicStreamUsageAccumulates(t *testing.T) {
	raw := mustReadFixture(t, "anthropic_stream.sse")
	frames := splitSSEFrames(t, raw)
	if len(frames) < 5 {
		t.Fatalf("fixture should have >=5 frames, got %d", len(frames))
	}

	visitor := newRecordingWriter()
	s := aimeter.WrapResponse(visitor, aimeter.KindAnthropic)

	reader := &frameDripReader{frames: frames, gap: 15 * time.Millisecond}
	if _, err := io.Copy(s, reader); err != nil {
		t.Fatalf("io.Copy: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got := s.Tokens()
	if got.In != 12 || got.Out != 7 {
		t.Fatalf("tokens: got %+v want In=12 Out=7", got)
	}

	if !visitor.NonBuffered(10*time.Millisecond, len(frames)-1) {
		t.Fatal("buffered Anthropic stream — SSE invariant violated (not enough wide gaps between writes)")
	}
}

// TestParseAnthropicBody — non-stream Anthropic /v1/messages response.
func TestParseAnthropicBody(t *testing.T) {
	body := []byte(`{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":12,"output_tokens":7}}`)
	got := aimeter.ParseAnthropicBody(body)
	want := aimeter.Tokens{In: 12, Out: 7, Total: 19}
	if got != want {
		t.Fatalf("ParseAnthropicBody: got %+v want %+v", got, want)
	}
}

// TestStreamPassthroughKind — Kind values without a dedicated parser
// (e.g. MCP, unknown) still forward bytes verbatim and flush.
func TestStreamPassthroughKind(t *testing.T) {
	visitor := newRecordingWriter()
	s := aimeter.WrapResponse(visitor, aimeter.KindMCP)
	payload := []byte("arbitrary opaque bytes\n")
	n, err := s.Write(payload)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(payload) {
		t.Fatalf("short write: n=%d want %d", n, len(payload))
	}
	if string(visitor.Bytes()) != string(payload) {
		t.Fatalf("passthrough body differs: %q vs %q", visitor.Bytes(), payload)
	}
	if len(visitor.flushes) == 0 {
		t.Fatal("passthrough did not flush")
	}
}

// TestStreamKindAccessor confirms the Kind diagnostic accessor.
func TestStreamKindAccessor(t *testing.T) {
	s := aimeter.WrapResponse(newRecordingWriter(), aimeter.KindOpenAI)
	if s.Kind() != aimeter.KindOpenAI {
		t.Fatalf("Kind: got %q want %q", s.Kind(), aimeter.KindOpenAI)
	}
}

// --- SQLSink ----------------------------------------------------------------

// testDB opens an isolated, fully-migrated sqlite database in a temp dir.
// We bootstrap our own copy here (mirroring the db package's internal
// testDB) rather than importing the db package's unexported helper.
func testDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	x := db.Wrap(d)
	t.Cleanup(func() { _ = x.Close() })
	return x
}

// seedService inserts a row into services so the FK on usage_events is
// satisfied. Returns the service id.
func seedService(t *testing.T, x *db.DB) string {
	t.Helper()
	ctx := context.Background()
	// Need a user first (services.user_id FK).
	if err := x.CreateUser(ctx, db.User{ID: "u-sink", Email: "sink@test", PasswordHash: "h", Role: "admin"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	svc, err := x.GetOrCreateService(ctx, "u-sink", "openai-svc", "http")
	if err != nil {
		t.Fatalf("seed service: %v", err)
	}
	return svc.ID
}

func TestSQLSinkRecord(t *testing.T) {
	ctx := context.Background()
	x := testDB(t)
	serviceID := seedService(t, x)

	sink := aimeter.NewSQLSink(x)
	sample := aimeter.Sample{
		ServiceID:      serviceID,
		APIKeyID:       "", // optional
		Model:          "gpt-4o-mini",
		Kind:           aimeter.KindOpenAI,
		TokensIn:       12,
		TokensOut:      7,
		BytesIn:        1024,
		BytesOut:       4096,
		Streamed:       true,
		CacheHit:       false,
		UpstreamStatus: 200,
	}
	if err := sink.Record(ctx, sample); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Read the row back via raw SQL — the db package doesn't expose a
	// typed read helper for usage_events yet (Task 21 owns the query layer).
	var (
		id, svcID, apiKeyID, kind string
		tokensIn, tokensOut       int64
		bytesIn, bytesOut         int64
		streamed, cacheHit        int
		upstreamStatus            int
	)
	err := x.DB().QueryRowContext(ctx, `
		SELECT id, service_id, api_key_id, kind,
		       tokens_in, tokens_out, bytes_in, bytes_out,
		       streamed, cache_hit, upstream_status
		  FROM usage_events
		 WHERE service_id=?`, serviceID).Scan(
		&id, &svcID, &apiKeyID, &kind,
		&tokensIn, &tokensOut, &bytesIn, &bytesOut,
		&streamed, &cacheHit, &upstreamStatus)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if id == "" {
		t.Fatal("id should be populated")
	}
	if svcID != serviceID {
		t.Fatalf("service_id: got %q want %q", svcID, serviceID)
	}
	if apiKeyID != "" {
		t.Fatalf("api_key_id: got %q want empty", apiKeyID)
	}
	if kind != "openai" {
		t.Fatalf("kind: got %q want openai", kind)
	}
	if tokensIn != 12 || tokensOut != 7 {
		t.Fatalf("tokens: got in=%d out=%d want 12/7", tokensIn, tokensOut)
	}
	if bytesIn != 1024 || bytesOut != 4096 {
		t.Fatalf("bytes: got in=%d out=%d want 1024/4096", bytesIn, bytesOut)
	}
	if streamed != 1 || cacheHit != 0 {
		t.Fatalf("bool flags: streamed=%d cache_hit=%d want 1/0", streamed, cacheHit)
	}
	if upstreamStatus != 200 {
		t.Fatalf("upstream_status: got %d want 200", upstreamStatus)
	}
}

// TestSQLSinkRecord_NilSafeguards — Record on a nil sink or nil DB is a
// no-op (used so call sites can safely skip metering without a guard).
func TestSQLSinkRecord_NilSafeguards(t *testing.T) {
	ctx := context.Background()
	var s *aimeter.SQLSink
	if err := s.Record(ctx, aimeter.Sample{}); err != nil {
		t.Fatalf("nil sink Record: %v", err)
	}
	s = &aimeter.SQLSink{DB: nil}
	if err := s.Record(ctx, aimeter.Sample{}); err != nil {
		t.Fatalf("nil DB Record: %v", err)
	}
}

// A reported cost is stored as it came; a sample without one leaves the
// column NULL so the price table applies to its tokens.
func TestSQLSinkRecord_CostUSD(t *testing.T) {
	ctx := context.Background()
	x := testDB(t)
	serviceID := seedService(t, x)
	sink := aimeter.NewSQLSink(x)

	reported, zero := 0.00042, 0.0
	for key, cost := range map[string]*float64{"k-reported": &reported, "k-zero": &zero, "k-none": nil} {
		if err := sink.Record(ctx, aimeter.Sample{ServiceID: serviceID, APIKeyID: key, Kind: aimeter.KindOpenAI, TokensIn: 1, TokensOut: 1, CostUSD: cost}); err != nil {
			t.Fatalf("Record %s: %v", key, err)
		}
		var got sql.NullFloat64
		if err := x.DB().QueryRowContext(ctx, `SELECT cost_usd FROM usage_events WHERE api_key_id=?`, key).Scan(&got); err != nil {
			t.Fatalf("read back %s: %v", key, err)
		}
		switch {
		case cost == nil && got.Valid:
			t.Errorf("%s: cost_usd = %v, want NULL", key, got.Float64)
		case cost != nil && (!got.Valid || got.Float64 != *cost):
			t.Errorf("%s: cost_usd = %+v, want %v", key, got, *cost)
		}
	}
}

// The sink is the last stop before the database: a cost that is not a sane
// amount is dropped there even if a caller handed it over, and the row is
// still written.
func TestSQLSinkRecord_DropsInsaneCost(t *testing.T) {
	ctx := context.Background()
	x := testDB(t)
	serviceID := seedService(t, x)
	sink := aimeter.NewSQLSink(x)
	for i, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -0.01, 1e9} {
		bad := bad
		key := fmt.Sprintf("k-%d", i)
		if err := sink.Record(ctx, aimeter.Sample{ServiceID: serviceID, APIKeyID: key, Kind: aimeter.KindOpenAI, TokensIn: 3, TokensOut: 2, CostUSD: &bad}); err != nil {
			t.Fatalf("Record %v: %v", bad, err)
		}
		var got sql.NullFloat64
		var in int64
		if err := x.DB().QueryRowContext(ctx, `SELECT cost_usd, tokens_in FROM usage_events WHERE api_key_id=?`, key).Scan(&got, &in); err != nil {
			t.Fatalf("row for cost %v missing: %v", bad, err)
		}
		if got.Valid || in != 3 {
			t.Errorf("cost %v: stored %+v with tokens_in=%d, want NULL and 3", bad, got, in)
		}
	}
}

// The route of a request and its latency are stored on the usage row; a
// sample without them leaves the columns at their defaults.
func TestSQLSinkRecord_RouteAndLatency(t *testing.T) {
	ctx := context.Background()
	x := testDB(t)
	serviceID := seedService(t, x)
	sink := aimeter.NewSQLSink(x)

	if err := sink.Record(ctx, aimeter.Sample{
		ServiceID: serviceID, APIKeyID: "k-route", Kind: aimeter.KindOpenAI, UpstreamStatus: 200,
		GatewayKeyID: "gk1", Dialect: "openai", ProviderSlug: "openrouter",
		RequestedModel: "burrow-medium", TargetModel: "google/gemini-x", RequestID: "req-9",
		LatencyMs: 1234,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := sink.Record(ctx, aimeter.Sample{ServiceID: serviceID, APIKeyID: "k-plain", Kind: aimeter.KindOpenAI, UpstreamStatus: 200}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	read := func(apiKeyID string) (got [6]string, latency int64) {
		t.Helper()
		err := x.DB().QueryRowContext(ctx, `
			SELECT gateway_key_id, dialect, provider_slug, requested_model, target_model, request_id, latency_ms
			  FROM usage_events WHERE api_key_id = ?`, apiKeyID).
			Scan(&got[0], &got[1], &got[2], &got[3], &got[4], &got[5], &latency)
		if err != nil {
			t.Fatalf("select %s: %v", apiKeyID, err)
		}
		return got, latency
	}
	got, latency := read("k-route")
	want := [6]string{"gk1", "openai", "openrouter", "burrow-medium", "google/gemini-x", "req-9"}
	if got != want || latency != 1234 {
		t.Fatalf("route row = %v latency=%d, want %v 1234", got, latency, want)
	}
	if got, latency := read("k-plain"); got != [6]string{} || latency != 0 {
		t.Fatalf("plain row = %v latency=%d, want empty and 0", got, latency)
	}
}
