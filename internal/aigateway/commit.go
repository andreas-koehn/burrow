package aigateway

import "net/http"

// commitWriter decides, at the moment an attempt produces its status line,
// whether the client gets this response or the gateway tries the next
// candidate. Until then nothing reaches the client: the attempt's headers
// collect in a map of their own. The decision needs the status only, so
// nothing of a body is ever held back; once committed the writer is a plain
// pass-through, and a stream is never interrupted or replayed.
//
// It offers Flush and deliberately no Unwrap: through Unwrap a flush would
// reach the client while the attempt is still undecided.
type commitWriter struct {
	w        http.ResponseWriter
	header   http.Header
	retry    func(status int) bool
	onCommit func(status int)

	status    int
	committed bool
	discarded bool
	// decided: retry was asked ahead of the status line (see Decide) and
	// said no. The attempt is the client's; nothing is written yet.
	decided bool
	// upstreamTimeout: the upstream handler said its transport gave up
	// waiting for the response (see aiprovider.TimeoutNoter).
	upstreamTimeout bool
}

// NoteUpstreamTimeout implements aiprovider.TimeoutNoter.
func (c *commitWriter) NoteUpstreamTimeout() { c.upstreamTimeout = true }

// newCommitWriter wraps w. retry is asked once, with the attempt's status:
// true discards the attempt. onCommit runs just before the status line of a
// committed attempt is written.
func newCommitWriter(w http.ResponseWriter, retry func(int) bool, onCommit func(int)) *commitWriter {
	return &commitWriter{w: w, header: http.Header{}, retry: retry, onCommit: onCommit}
}

// Decide asks, now, whether an attempt that will answer status is discarded,
// without writing anything: for a writer above this one that learns the
// upstream's status before it can write the client's response (a translated
// error, whose body must be read and re-shaped first). true: the attempt is
// discarded, as by WriteHeader. false: the response is the client's; the
// WriteHeader that follows commits it without asking again. retry is asked
// once per attempt either way.
func (c *commitWriter) Decide(status int) (discarded bool) {
	if c.committed || c.discarded || c.decided {
		return c.discarded
	}
	c.status = status
	if c.retry(status) {
		c.discarded = true
		return true
	}
	c.decided = true
	return false
}

func (c *commitWriter) Header() http.Header {
	if c.committed {
		return c.w.Header()
	}
	return c.header
}

func (c *commitWriter) WriteHeader(status int) {
	if c.committed || c.discarded {
		return
	}
	if status >= 100 && status < 200 {
		return // informational responses are not a decision
	}
	c.status = status
	if !c.decided && c.retry(status) {
		c.discarded = true
		return
	}
	dst := c.w.Header()
	for k, v := range c.header {
		if k == headerRequestID && len(dst[k]) > 0 {
			continue // the relay's id, not an upstream's
		}
		dst[k] = v
	}
	c.onCommit(status)
	c.committed = true
	c.w.WriteHeader(status)
}

func (c *commitWriter) Write(p []byte) (int, error) {
	if !c.committed && !c.discarded {
		c.WriteHeader(http.StatusOK)
	}
	if c.discarded {
		return len(p), nil
	}
	return c.w.Write(p)
}

// Flush forwards to the client only once the response is committed.
func (c *commitWriter) Flush() {
	if c.committed {
		_ = http.NewResponseController(c.w).Flush()
	}
}
