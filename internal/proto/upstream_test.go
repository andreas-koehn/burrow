package proto

import "testing"

// EffectiveUpstream is the one rule the relay and the API both apply: the
// client's choice, else the stored setting, else http1.
func TestEffectiveUpstream(t *testing.T) {
	for _, c := range []struct{ client, stored, want string }{
		{"", "", UpstreamHTTP1},
		{"", UpstreamH2, UpstreamH2},
		{UpstreamH2C, "", UpstreamH2C},
		{UpstreamH2C, UpstreamH2, UpstreamH2C},
		{UpstreamHTTP1, UpstreamH2C, UpstreamHTTP1},
	} {
		if got := EffectiveUpstream(c.client, c.stored); got != c.want {
			t.Errorf("EffectiveUpstream(%q, %q) = %q, want %q", c.client, c.stored, got, c.want)
		}
	}
}
