package webhook

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
)

// lockedBuffer is a log sink the worker and the test may use at once.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// silentReceiver is a webhook receiver that takes a request and never
// answers it. arrived gets one value per request, gone one when the sender
// gave that request up.
func silentReceiver(t *testing.T) (url string, arrived, gone chan struct{}) {
	t.Helper()
	arrived, gone = make(chan struct{}, 64), make(chan struct{}, 64)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// The server notices a sender that hangs up once the body is read.
		_, _ = io.Copy(io.Discard, r.Body)
		arrived <- struct{}{}
		select {
		case <-r.Context().Done():
			gone <- struct{}{}
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	return srv.URL, arrived, gone
}

// Close is bounded as a whole. With a receiver that does not answer, the
// delivery in flight is cancelled through its request when the budget is
// spent, the queued events are given up, and the log says how many.
func TestClose_ReceiverThatNeverAnswers(t *testing.T) {
	url, arrived, gone := silentReceiver(t)
	store := newFakeStore()
	store.addWebhook(db.Webhook{ID: "wh1", Name: "hook", URL: url, Events: `["tunnel.disconnected"]`})
	logs := &lockedBuffer{}
	d := New(store, &fakeSecrets{id: "wh1", plaintext: "s"}, nil, slog.New(slog.NewTextHandler(logs, nil)))
	const budget = 300 * time.Millisecond
	d.closeBudget = budget
	d.Start()

	const events = 5
	for range events {
		d.Publish(context.Background(), "tunnel.disconnected", map[string]any{"TunnelID": "t"})
	}
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the first delivery never reached the receiver")
	}

	start := time.Now()
	d.Close()
	took := time.Since(start)
	if took < budget {
		t.Fatalf("Close returned after %s, before its budget of %s was spent on a delivery still in flight", took, budget)
	}
	if took > budget+3*time.Second {
		t.Fatalf("Close took %s with a budget of %s", took, budget)
	}
	// The request of the delivery in flight was cancelled, not left running.
	select {
	case <-gone:
	case <-time.After(3 * time.Second):
		t.Fatal("the delivery in flight was not cancelled when Close returned")
	}
	select {
	case <-arrived:
		t.Fatal("a queued event was still sent after the budget was spent")
	case <-time.After(100 * time.Millisecond):
	}
	// One line, with the number and nothing of the events or the receiver.
	out := logs.String()
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "not delivered") {
			line = l
		}
	}
	if !strings.Contains(line, "count=5") {
		t.Fatalf("the log does not say that 5 events were not delivered:\n%s", out)
	}
	if strings.Contains(out, "TunnelID") || strings.Contains(line, "url=") || strings.Contains(line, "event=") {
		t.Fatalf("the shutdown log carries more than a count:\n%s", out)
	}
	// What was cut off says nothing about the receiver: no failure is
	// recorded against the webhook.
	if n := store.webhookSnapshot("wh1").ConsecutiveFailures; n != 0 {
		t.Fatalf("ConsecutiveFailures = %d after a delivery Close cut off, want 0", n)
	}
	// A second Close returns at once, and so does a Publish after it.
	d.Close()
	d.Publish(context.Background(), "tunnel.disconnected", nil)
}

// With a receiver that answers, everything queued is delivered before Close
// returns, and nothing is logged as lost.
func TestClose_DeliversWhatIsQueued(t *testing.T) {
	var received atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	store := newFakeStore()
	store.addWebhook(db.Webhook{ID: "wh1", Name: "hook", URL: srv.URL, Events: `["tunnel.disconnected"]`})
	logs := &lockedBuffer{}
	d := New(store, &fakeSecrets{id: "wh1", plaintext: "s"}, nil, slog.New(slog.NewTextHandler(logs, nil)))

	// Queued before the worker runs, so Close finds all of them waiting.
	const events = 8
	for range events {
		d.Publish(context.Background(), "tunnel.disconnected", nil)
	}
	d.Start()
	d.Close()
	if n := received.Load(); n != events {
		t.Fatalf("%d of %d queued events were delivered before Close returned", n, events)
	}
	if n := store.deliveryCount(); n != events {
		t.Fatalf("%d delivery rows, want %d", n, events)
	}
	if out := logs.String(); strings.Contains(out, "not delivered") {
		t.Fatalf("a clean shutdown logged lost events:\n%s", out)
	}
}

// A receiver that fails while the relay stops is not retried: Close does not
// sit out the retry schedule.
func TestClose_DoesNotRetry(t *testing.T) {
	var received atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	store := newFakeStore()
	store.addWebhook(db.Webhook{ID: "wh1", Name: "hook", URL: srv.URL, Events: `["tunnel.disconnected"]`})
	d := New(store, &fakeSecrets{id: "wh1", plaintext: "s"}, nil, nil)
	d.Publish(context.Background(), "tunnel.disconnected", nil)
	d.Publish(context.Background(), "tunnel.disconnected", nil)
	d.Start()
	start := time.Now()
	d.Close()
	if took := time.Since(start); took > 900*time.Millisecond {
		t.Fatalf("Close took %s: it waited for a retry (the first is due after 1s)", took)
	}
	// Each event is tried once: the wait for a retry ends when Close is called.
	if n := received.Load(); n != 2 {
		t.Fatalf("%d attempts for 2 events, want one each", n)
	}
}

// Publish after Close is dropped: it neither panics nor blocks, with or
// without a worker.
func TestPublish_AfterClose(t *testing.T) {
	for _, started := range []bool{true, false} {
		store := newFakeStore()
		d := New(store, &fakeSecrets{}, nil, nil)
		if started {
			d.Start()
		}
		d.Close()
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range queueCapacity + 10 {
				d.Publish(context.Background(), "webhook.test", nil)
			}
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("Publish after Close blocked (started=%v)", started)
		}
		if n := len(d.queue); n != 0 {
			t.Fatalf("%d events were queued after Close (started=%v)", n, started)
		}
	}
}

// Events that were queued when a dispatcher without a worker is closed are
// counted as lost too.
func TestClose_CountsWhatNoWorkerTook(t *testing.T) {
	logs := &lockedBuffer{}
	d := New(newFakeStore(), &fakeSecrets{}, nil, slog.New(slog.NewTextHandler(logs, nil)))
	for range 3 {
		d.Publish(context.Background(), "webhook.test", nil)
	}
	d.Close()
	if out := logs.String(); !strings.Contains(out, "count=3") {
		t.Fatalf("the log does not count the 3 events nobody took:\n%s", out)
	}
}
