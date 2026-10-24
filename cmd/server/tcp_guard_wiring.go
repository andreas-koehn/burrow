package main

// tcp_guard_wiring.go — the bandwidth, daily volume and automatic bans of tcp
// services (tcp complete, E6 and E7) as the relay reads them: the limits of
// a service row, and the bans and daily usage it keeps in the database.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/server"
	"github.com/ankoehn/burrow/internal/store"
)

// The API reaches the relay's bans and usage through Deps.Connections.
var _ api.TCPGuardRelay = (*server.Server)(nil)

// tcpLimitsOf is a tcp service's limits as the relay applies them.
func tcpLimitsOf(svc db.Service) server.TCPLimits {
	l := store.TCPLimitsOf(svc)
	return server.TCPLimits{
		RateLimitKbps: l.RateLimitKbps, DailyBytes: l.DailyBytes,
		AutoBan: server.TCPAutoBan{
			Enabled: l.AutoBan.Enabled, Count: l.AutoBan.Count,
			Short:  time.Duration(l.AutoBan.ShortS) * time.Second,
			Window: time.Duration(l.AutoBan.WindowMin) * time.Minute,
			Ban:    time.Duration(l.AutoBan.BanMin) * time.Minute,
		},
	}
}

// tcpGuardDB is what tcpGuardStore reads and writes. *db.DB satisfies it.
type tcpGuardDB interface {
	ListTCPBans(ctx context.Context, serviceID string, now time.Time) ([]db.TCPBan, error)
	PutTCPBan(ctx context.Context, b db.TCPBan) error
	TCPDailyUsage(ctx context.Context, serviceID, day string) (in, out int64, err error)
	AddTCPDailyUsage(ctx context.Context, serviceID, day string, in, out int64) error
	PruneTCPBans(ctx context.Context, serviceID string, now time.Time) (int64, error)
	PruneTCPDailyUsage(ctx context.Context, before string) (int64, error)
}

// tcpGuardStore keeps the relay's bans and daily usage of tcp services in the
// database (server.TCPGuardStore).
type tcpGuardStore struct{ db tcpGuardDB }

var (
	_ server.TCPGuardStore  = tcpGuardStore{}
	_ server.TCPUsagePruner = tcpGuardStore{}
)

func (a tcpGuardStore) ActiveTCPBans(ctx context.Context, serviceID string, now time.Time) ([]server.TCPBan, error) {
	rows, err := a.db.ListTCPBans(ctx, serviceID, now)
	if err != nil {
		return nil, err
	}
	out := make([]server.TCPBan, 0, len(rows))
	for _, b := range rows {
		out = append(out, server.TCPBan{Source: b.Source, Until: b.Until, Reason: b.Reason, CreatedAt: b.CreatedAt})
	}
	return out, nil
}

// SaveTCPBan writes b; a stored ban of the source that ends later holds,
// and that is no error.
func (a tcpGuardStore) SaveTCPBan(ctx context.Context, serviceID string, b server.TCPBan) error {
	err := a.db.PutTCPBan(ctx, db.TCPBan{ServiceID: serviceID, Source: b.Source, Until: b.Until, Reason: b.Reason, CreatedAt: b.CreatedAt})
	if errors.Is(err, db.ErrTCPBanLonger) {
		return nil
	}
	return err
}

func (a tcpGuardStore) TCPUsage(ctx context.Context, serviceID, day string) (uint64, uint64, error) {
	in, out, err := a.db.TCPDailyUsage(ctx, serviceID, day)
	return uint64(max(in, 0)), uint64(max(out, 0)), err
}

// AddTCPUsage adds to the service's day; a service that is gone is
// server.ErrTCPServiceGone.
func (a tcpGuardStore) AddTCPUsage(ctx context.Context, serviceID, day string, in, out uint64) error {
	err := a.db.AddTCPDailyUsage(ctx, serviceID, day, int64(in), int64(out))
	if errors.Is(err, db.ErrNotFound) {
		return fmt.Errorf("%w: %s", server.ErrTCPServiceGone, serviceID)
	}
	return err
}

func (a tcpGuardStore) PruneTCPUsage(ctx context.Context, before string) error {
	_, err := a.db.PruneTCPDailyUsage(ctx, before)
	return err
}

func (a tcpGuardStore) PruneTCPBans(ctx context.Context, serviceID string, now time.Time) error {
	_, err := a.db.PruneTCPBans(ctx, serviceID, now)
	return err
}
