package api

import (
	"fmt"
	"net/http"
	"time"
)

// EventsStream is a Server-Sent Events stream of content-free "refetch"
// notifications, the refresh channel of the dashboard pages.
func (d Deps) EventsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok || d.Events == nil {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := d.Events.Subscribe(userID(r.Context()))
	defer cancel()

	keepAlive := time.NewTicker(25 * time.Second)
	defer keepAlive.Stop()
	// Write errors are intentionally ignored: r.Context().Done() is the
	// authoritative exit (client disconnect), and a write to a dead conn
	// returns promptly rather than blocking.
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			fmt.Fprint(w, "event: tunnels\ndata: {}\n\n")
			flusher.Flush()
		case <-keepAlive.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		}
	}
}
