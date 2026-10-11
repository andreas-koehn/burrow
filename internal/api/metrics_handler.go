package api

import (
	"net/http"
	"runtime"

	"github.com/ankoehn/burrow/internal/metrics"
)

// metricsContentType is the canonical Prometheus 0.0.4 plain-text MIME type.
// Prometheus' scrape client honours both the version parameter and the
// charset, so we set both — keeps every popular reverse-proxy and scrape
// configuration happy.
const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// MetricsRecorder is the Deps surface for the /metrics endpoint. The concrete
// *metrics.Recorder satisfies it; tests can substitute a fake to assert the
// gate without exercising the full WriteText path. Kept as an interface so
// the api package never depends on metrics implementation details — only on
// the WriteText shape.
type MetricsRecorder interface {
	// WriteText emits the full closed Prometheus metric set to w. The
	// handler ignores the return for the success path (a partial 200 is
	// preferable to a fully-buffered render that may OOM on long uptimes);
	// errors are logged via Deps.Log.
	WriteText(w http.ResponseWriter) error

	// SetCertExpiryDays sets the burrow_cert_expiry_days{cert} gauge to d.
	// Used by the custom-domain handlers (v0.5.0 Task 7) to record per-domain
	// cert expiry after INSERT / UPDATE.
	SetCertExpiryDays(cert string, d float64)
}

// metricsRecorderAdapter wraps *metrics.Recorder to satisfy MetricsRecorder.
// Kept private — cmd/server constructs it via NewMetricsRecorderAdapter
// (added in Task 25 when the recorder is wired through the binary).
type metricsRecorderAdapter struct{ r *metrics.Recorder }

// NewMetricsRecorderAdapter is the Task 25 seam: cmd/server constructs a
// *metrics.Recorder, wraps it through this adapter, and assigns the result
// to Deps.Metrics. The adapter is a one-liner that calls WriteText with the
// ResponseWriter — kept here (next to the handler) so the metrics package
// stays free of any net/http coupling.
func NewMetricsRecorderAdapter(r *metrics.Recorder) MetricsRecorder {
	return metricsRecorderAdapter{r: r}
}

func (a metricsRecorderAdapter) WriteText(w http.ResponseWriter) error {
	return a.r.WriteText(w)
}

func (a metricsRecorderAdapter) SetCertExpiryDays(cert string, d float64) {
	a.r.SetCertExpiryDays(cert, d)
}

// GetMetrics handles GET /metrics. It writes the full closed Prometheus
// metric set in 0.0.4 exposition format with Content-Type
// "text/plain; version=0.0.4; charset=utf-8". A nil Deps.Metrics is treated
// as 500 "metrics recorder unavailable" — same shape as other Deps surfaces.
//
// The handler also stamps burrow_goroutines at scrape time so the gauge
// always reflects the current process state (rather than whatever the last
// emitter call set). This keeps the metric meaningful even before Task 25
// wires runtime tickers into the recorder.
func (d Deps) GetMetrics(w http.ResponseWriter, r *http.Request) {
	if d.Metrics == nil {
		writeErr(w, http.StatusInternalServerError, "metrics recorder unavailable")
		return
	}
	// Sample goroutine count at scrape time. Cheap (a single atomic load
	// inside the Go runtime) and matches what Prometheus' own Go client
	// does for its process_goroutines counter.
	if rec, ok := d.Metrics.(metricsRecorderAdapter); ok {
		rec.r.SetGoroutines(runtime.NumGoroutine())
	}
	w.Header().Set("Content-Type", metricsContentType)
	w.WriteHeader(http.StatusOK)
	if err := d.Metrics.WriteText(w); err != nil {
		d.Log.Error("metrics write", "err", err)
	}
}
