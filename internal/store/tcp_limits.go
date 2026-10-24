package store

// tcp_limits.go — the bandwidth, daily volume and automatic bans of a tcp
// service, and its bans (tcp complete, E6 and E7). The relay applies them
// (internal/server/tcp_guard.go); the bounds here mirror the relay's
// (server.Min/MaxTCP…), and a test in cmd/server pins them equal.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
)

// The bounds of the limits of a tcp service.
const (
	MinTCPRateLimitKbps = 64
	MaxTCPRateLimitKbps = 10_000_000
	MinTCPDailyBytes    = int64(1) << 20
	MaxTCPDailyBytes    = int64(1) << 50

	MinTCPAutoBanShortS    = 1
	MaxTCPAutoBanShortS    = 300
	MinTCPAutoBanCount     = 2
	MaxTCPAutoBanCount     = 100
	MinTCPAutoBanWindowMin = 1
	MaxTCPAutoBanWindowMin = 1440
	MinTCPAutoBanBanMin    = 1
	MaxTCPAutoBanBanMin    = 43200

	// MaxTCPBans bounds the active bans of one service (server.MaxTCPBans).
	MaxTCPBans = 4096
	// maxTCPBanNote bounds the note of a ban by hand.
	maxTCPBanNote = 200
)

// ErrInvalidTCPLimits: a limit is out of its bounds; the error names the
// field and the bounds (maps to HTTP 400). ErrInvalidTCPBan: a ban by hand
// with a source that is no address or IPv6 /64, a duration out of bounds or
// a note too long (400). ErrTooManyTCPBans: the service has MaxTCPBans
// active bans (409).
var (
	ErrInvalidTCPLimits = errors.New("store: invalid tcp limits")
	ErrInvalidTCPBan    = errors.New("store: invalid tcp ban")
	ErrTooManyTCPBans   = errors.New("store: the service has its maximum of active bans")
)

// TCPAutoBan are the automatic ban settings of a tcp service: a source whose
// connections end within ShortS seconds at least Count times in WindowMin
// minutes is banned for BanMin minutes, while Enabled.
type TCPAutoBan struct {
	Enabled   bool `json:"enabled"`
	ShortS    int  `json:"short_s"`
	Count     int  `json:"count"`
	WindowMin int  `json:"window_min"`
	BanMin    int  `json:"ban_min"`
}

// DefaultTCPAutoBan are the settings of a service that has none stored:
// off, 3 s, 10 times, 10 minutes, 60 minutes (spec E6).
func DefaultTCPAutoBan() TCPAutoBan {
	return TCPAutoBan{ShortS: 3, Count: 10, WindowMin: 10, BanMin: 60}
}

// ParseTCPAutoBan reads the stored settings (services.tcp_autoban): ""
// (NULL) is DefaultTCPAutoBan, and a field missing from the JSON keeps its
// default. Settings that cannot be read are off.
func ParseTCPAutoBan(s string) TCPAutoBan {
	ab := DefaultTCPAutoBan()
	if s == "" {
		return ab
	}
	if err := json.Unmarshal([]byte(s), &ab); err != nil || ab.validate() != nil {
		return DefaultTCPAutoBan()
	}
	return ab
}

func (ab TCPAutoBan) validate() error {
	check := func(field string, v, lo, hi int, unit string) error {
		if v < lo || v > hi {
			return fmt.Errorf("%w: autoban.%s must be %d to %d %s", ErrInvalidTCPLimits, field, lo, hi, unit)
		}
		return nil
	}
	return errors.Join(
		check("short_s", ab.ShortS, MinTCPAutoBanShortS, MaxTCPAutoBanShortS, "seconds"),
		check("count", ab.Count, MinTCPAutoBanCount, MaxTCPAutoBanCount, "connections"),
		check("window_min", ab.WindowMin, MinTCPAutoBanWindowMin, MaxTCPAutoBanWindowMin, "minutes"),
		check("ban_min", ab.BanMin, MinTCPAutoBanBanMin, MaxTCPAutoBanBanMin, "minutes"),
	)
}

// TCPLimits are the limits of a tcp service: the bandwidth of each
// direction in kbit/s, the bytes in and out a UTC day may carry (0 for
// none, both) and the automatic ban settings.
type TCPLimits struct {
	RateLimitKbps int        `json:"rate_limit_kbps"`
	DailyBytes    int64      `json:"daily_bytes"`
	AutoBan       TCPAutoBan `json:"autoban"`
}

// TCPLimitsOf reads the limits of a service row.
func TCPLimitsOf(svc db.Service) TCPLimits {
	return TCPLimits{RateLimitKbps: svc.RateLimitKbps, DailyBytes: svc.DailyBytes, AutoBan: ParseTCPAutoBan(svc.TCPAutoBan)}
}

// TCPAutoBanUpdate names the ban settings to change; a nil field is kept.
type TCPAutoBanUpdate struct {
	Enabled                          *bool
	ShortS, Count, WindowMin, BanMin *int
}

// TCPLimitsUpdate names the limits to change; a nil field is kept.
type TCPLimitsUpdate struct {
	RateLimitKbps *int
	DailyBytes    *int64
	AutoBan       *TCPAutoBanUpdate
}

// TCPUsage is what a tcp service carried on the UTC day Day, as stored.
type TCPUsage struct {
	Day               string
	BytesIn, BytesOut int64
}

// ServiceTCPLimits reads the limits of a tcp service and what it carried
// today (UTC) as stored. Same permission rule as SetServiceMaxConnections;
// ErrServiceNotTCP for any other type.
func (s *Store) ServiceTCPLimits(ctx context.Context, callerID, callerRole, serviceID string) (TCPLimits, TCPUsage, error) {
	svc, err := s.canConfigure(ctx, callerID, callerRole, serviceID)
	if err != nil {
		return TCPLimits{}, TCPUsage{}, err
	}
	if svc.Type != "tcp" {
		return TCPLimits{}, TCPUsage{}, ErrServiceNotTCP
	}
	day := time.Now().UTC().Format(time.DateOnly)
	in, out, err := s.q.TCPDailyUsage(ctx, serviceID, day)
	if err != nil {
		return TCPLimits{}, TCPUsage{}, err
	}
	return TCPLimitsOf(svc), TCPUsage{Day: day, BytesIn: in, BytesOut: out}, nil
}

// SetServiceTCPLimits changes the limits named in u and keeps the others.
// Same permission rule as SetServiceMaxConnections. ErrServiceNotTCP for
// any other type, ErrInvalidTCPLimits (naming the field) for a value out of
// bounds; nothing is written then. Every write is audited as
// service.tcp_limits.update with the limits before and after, and they are
// returned.
func (s *Store) SetServiceTCPLimits(ctx context.Context, callerID, callerRole, serviceID string, u TCPLimitsUpdate) (before, after TCPLimits, err error) {
	svc, err := s.canConfigure(ctx, callerID, callerRole, serviceID)
	if err != nil {
		return TCPLimits{}, TCPLimits{}, err
	}
	if svc.Type != "tcp" {
		return TCPLimits{}, TCPLimits{}, ErrServiceNotTCP
	}
	if n := u.RateLimitKbps; n != nil && *n != 0 && (*n < MinTCPRateLimitKbps || *n > MaxTCPRateLimitKbps) {
		return TCPLimits{}, TCPLimits{}, fmt.Errorf("%w: rate_limit_kbps must be 0 (none) or %d to %d", ErrInvalidTCPLimits, MinTCPRateLimitKbps, MaxTCPRateLimitKbps)
	}
	if n := u.DailyBytes; n != nil && *n != 0 && (*n < MinTCPDailyBytes || *n > MaxTCPDailyBytes) {
		return TCPLimits{}, TCPLimits{}, fmt.Errorf("%w: daily_bytes must be 0 (none) or %d (1 MiB) to %d (1 PiB)", ErrInvalidTCPLimits, MinTCPDailyBytes, MaxTCPDailyBytes)
	}
	before = TCPLimitsOf(svc)
	var abJSON *string
	if a := u.AutoBan; a != nil {
		ab := before.AutoBan
		set := func(dst *int, v *int) {
			if v != nil {
				*dst = *v
			}
		}
		if a.Enabled != nil {
			ab.Enabled = *a.Enabled
		}
		set(&ab.ShortS, a.ShortS)
		set(&ab.Count, a.Count)
		set(&ab.WindowMin, a.WindowMin)
		set(&ab.BanMin, a.BanMin)
		if err := ab.validate(); err != nil {
			return TCPLimits{}, TCPLimits{}, err
		}
		b, _ := json.Marshal(ab)
		js := string(b)
		abJSON = &js
	}
	if err := s.q.SetServiceTCPLimits(ctx, serviceID, u.RateLimitKbps, u.DailyBytes, abJSON); err != nil {
		return TCPLimits{}, TCPLimits{}, err
	}
	now, err := s.q.GetServiceByID(ctx, serviceID)
	if err != nil {
		return TCPLimits{}, TCPLimits{}, err
	}
	after = TCPLimitsOf(now)
	s.emitAudit(withAuditDeadline(ctx), audit.ActionServiceTCPLimitsUpdate, func(e *audit.Event) {
		e.SubjectID = serviceID
		e.SubjectLabel = svc.Name
		e.Payload = audit.MustJSON(map[string]TCPLimits{"from": before, "to": after})
	})
	return before, after, nil
}

// NormalizeTCPBanSource is the source a ban is for, as the relay counts
// visitors: an IPv4 address (an address, a /32, or IPv4 written as IPv6),
// or the /64 of an IPv6 address (an address, or a prefix of 64 bits or
// more). ok is false for anything else, a wider range included.
func NormalizeTCPBanSource(s string) (string, bool) {
	s = strings.TrimSpace(s)
	var a netip.Addr
	if ad, err := netip.ParseAddr(s); err == nil {
		a = ad.WithZone("")
	} else if p, err := netip.ParsePrefix(s); err == nil {
		switch {
		case p.Addr().Is4() && p.Bits() == 32, p.Addr().Is4In6() && p.Bits() == 128:
		case p.Addr().Is6() && !p.Addr().Is4In6() && p.Bits() >= 64:
		default:
			return "", false
		}
		a = p.Addr()
	} else {
		return "", false
	}
	a = a.Unmap()
	if a.Is4() {
		return a.String(), true
	}
	p, err := a.Prefix(64)
	if err != nil {
		return "", false
	}
	return p.String(), true
}

// ListTCPBans lists the active bans of a tcp service, the first to expire
// first, and prunes the expired ones. Same permission rule as
// SetServiceMaxConnections.
func (s *Store) ListTCPBans(ctx context.Context, callerID, callerRole, serviceID string) ([]db.TCPBan, error) {
	svc, err := s.canConfigure(ctx, callerID, callerRole, serviceID)
	if err != nil {
		return nil, err
	}
	if svc.Type != "tcp" {
		return nil, ErrServiceNotTCP
	}
	now := time.Now()
	if _, err := s.q.PruneTCPBans(ctx, serviceID, now); err != nil {
		return nil, err
	}
	return s.q.ListTCPBans(ctx, serviceID, now)
}

// AddTCPBan bans source (see NormalizeTCPBanSource) from a tcp service for
// banMin minutes (MinTCPAutoBanBanMin to MaxTCPAutoBanBanMin), with an
// optional note of at most 200 bytes; a ban of the same source is replaced.
// Same permission rule as SetServiceMaxConnections. ErrInvalidTCPBan,
// ErrTooManyTCPBans, ErrServiceNotTCP. Audited as tcp.ban.add.
func (s *Store) AddTCPBan(ctx context.Context, callerID, callerRole, serviceID, source string, banMin int, note string) (db.TCPBan, error) {
	svc, err := s.canConfigure(ctx, callerID, callerRole, serviceID)
	if err != nil {
		return db.TCPBan{}, err
	}
	if svc.Type != "tcp" {
		return db.TCPBan{}, ErrServiceNotTCP
	}
	src, ok := NormalizeTCPBanSource(source)
	if !ok {
		return db.TCPBan{}, fmt.Errorf("%w: source must be an IP address, or an IPv6 /64; a ban is for one source", ErrInvalidTCPBan)
	}
	if banMin < MinTCPAutoBanBanMin || banMin > MaxTCPAutoBanBanMin {
		return db.TCPBan{}, fmt.Errorf("%w: ban_min must be %d to %d", ErrInvalidTCPBan, MinTCPAutoBanBanMin, MaxTCPAutoBanBanMin)
	}
	note = strings.TrimSpace(note)
	if len(note) > maxTCPBanNote {
		return db.TCPBan{}, fmt.Errorf("%w: note must be at most %d bytes", ErrInvalidTCPBan, maxTCPBanNote)
	}
	now := time.Now().UTC().Truncate(time.Second)
	active, err := s.q.ListTCPBans(ctx, serviceID, now)
	if err != nil {
		return db.TCPBan{}, err
	}
	if len(active) >= MaxTCPBans {
		renew := false
		for _, b := range active {
			renew = renew || b.Source == src
		}
		if !renew {
			return db.TCPBan{}, ErrTooManyTCPBans
		}
	}
	reason := "manual"
	if note != "" {
		reason += ": " + note
	}
	b := db.TCPBan{ServiceID: serviceID, Source: src, Until: now.Add(time.Duration(banMin) * time.Minute), Reason: reason, CreatedAt: now}
	if err := s.q.PutTCPBan(ctx, b); err != nil {
		return db.TCPBan{}, err
	}
	s.emitAudit(withAuditDeadline(ctx), audit.ActionTCPBanAdd, func(e *audit.Event) {
		e.SubjectID = serviceID
		e.SubjectLabel = svc.Name
		e.Payload = audit.MustJSON(map[string]any{
			"source": src, "until": b.Until.Format(time.RFC3339), "ban_min": banMin, "reason": reason, "by": "manual",
		})
	})
	return b, nil
}

// RemoveTCPBan lifts the ban of source (normalised as for AddTCPBan) from a
// tcp service and returns it; db.ErrNotFound when there is none. Same
// permission rule as SetServiceMaxConnections. Audited as tcp.ban.remove.
func (s *Store) RemoveTCPBan(ctx context.Context, callerID, callerRole, serviceID, source string) (db.TCPBan, error) {
	svc, err := s.canConfigure(ctx, callerID, callerRole, serviceID)
	if err != nil {
		return db.TCPBan{}, err
	}
	if svc.Type != "tcp" {
		return db.TCPBan{}, ErrServiceNotTCP
	}
	src, ok := NormalizeTCPBanSource(source)
	if !ok {
		return db.TCPBan{}, fmt.Errorf("%w: source must be an IP address, or an IPv6 /64", ErrInvalidTCPBan)
	}
	b, err := s.q.DeleteTCPBan(ctx, serviceID, src)
	if err != nil {
		return db.TCPBan{}, err
	}
	s.emitAudit(withAuditDeadline(ctx), audit.ActionTCPBanRemove, func(e *audit.Event) {
		e.SubjectID = serviceID
		e.SubjectLabel = svc.Name
		e.Payload = audit.MustJSON(map[string]any{
			"source": src, "until": b.Until.Format(time.RFC3339), "reason": b.Reason,
		})
	})
	return b, nil
}
