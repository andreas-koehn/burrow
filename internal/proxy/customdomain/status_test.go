package customdomain

import (
	"testing"
	"time"
)

// TestComputeStatus pins the state machine that drives the daily-tick
// transitions in StatusTick (v0.5.2 Task 10).
func TestComputeStatus(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		delta time.Duration
		want  string
	}{
		{"60d remaining -> active", 60 * 24 * time.Hour, StatusActive},
		{"14d edge -> cert_expiring", 14 * 24 * time.Hour, StatusCertExpiring},
		{"1d remaining -> cert_expiring", 24 * time.Hour, StatusCertExpiring},
		{"0d exact -> cert_expired", 0, StatusCertExpired},
		{"past -> cert_expired", -24 * time.Hour, StatusCertExpired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			notAfter := now.Add(c.delta)
			got := ComputeStatus(notAfter, now)
			if got != c.want {
				t.Errorf("ComputeStatus(now+%v, now) = %q; want %q",
					c.delta, got, c.want)
			}
		})
	}
}
