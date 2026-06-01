// test/harness/upstream/main.go
// Tiny stdlib-only HTTP service used as the upstream behind a burrow tunnel
// in the basic e2e Docker Compose harness. Endpoints: /healthz, /echo, and
// /redirect -> /redirected (exercises the proxy's prefix-aware Location
// rewrite for single-origin /t/{id} path routing).
// No external dependencies — keeps the test-infra footprint zero.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const maxEchoBody = 64 * 1024 // 64 KiB cap to keep tests deterministic

func handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	// /redirect issues a 302 to the path-absolute "/redirected". A reverse
	// proxy that path-mounts this upstream under /t/<id> must rewrite a
	// path-absolute Location to /t/<id>/redirected — this endpoint exists to
	// prove that prefix-aware Location rewrite (see proxy.rewriteLocationPrefix,
	// which only rewrites path-absolute, non-protocol-relative Locations).
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/redirected", http.StatusFound)
	})
	mux.HandleFunc("/redirected", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"landed": "redirected"})
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, maxEchoBody))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"method":  r.Method,
			"path":    r.URL.RequestURI(),
			"headers": r.Header,
			"body":    string(body),
		})
	})
	return mux
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8081", "listen address")
	flag.Parse()
	srv := &http.Server{Addr: *addr, Handler: handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		<-c
		_ = srv.Close()
	}()
	log.Printf("upstream listening on %s", *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}
