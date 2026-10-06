package cost

import "time"

// Invalidate is what the engine calls when a budget fires.
func (g *Guard) Invalidate() { g.invalidate() }

// SetReadTimeout shortens the time a refresh may take, for tests.
func (g *Guard) SetReadTimeout(d time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.readTimeout = d
}
