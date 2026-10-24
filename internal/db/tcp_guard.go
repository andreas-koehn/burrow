package db

// tcp_guard.go — the limits of a tcp service and what they keep (tcp
// complete, E6 and E7, migration 0042): the columns rate_limit_kbps,
// daily_bytes and tcp_autoban of services, the banned sources (tcp_bans) and
// the bytes of each UTC day (tcp_daily_usage).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SetServiceTCPLimits sets the bandwidth (kbit/s, 0 none), the daily volume
// (bytes, 0 none) and the automatic ban settings (JSON, "" none: NULL) of a
// tcp service: the ones that are not nil, in one statement. The caller
// validates them. ErrNotFound when there is no such service.
func (x *DB) SetServiceTCPLimits(ctx context.Context, id string, rateKbps *int, dailyBytes *int64, autoban *string) error {
	// An autoban of "" clears the column; nil keeps it.
	setAB, ab := 0, sql.NullString{}
	if autoban != nil {
		setAB = 1
		if *autoban != "" {
			ab = sql.NullString{String: *autoban, Valid: true}
		}
	}
	res, err := x.sqlDB.ExecContext(ctx, `UPDATE services
		SET rate_limit_kbps = COALESCE(CAST(? AS INTEGER), rate_limit_kbps),
		    daily_bytes     = COALESCE(CAST(? AS BIGINT), daily_bytes),
		    tcp_autoban     = CASE WHEN ? = 1 THEN CAST(? AS TEXT) ELSE tcp_autoban END
		WHERE id=?`, rateKbps, dailyBytes, setAB, ab, id)
	if err != nil {
		return fmt.Errorf("set service tcp limits: %w", err)
	}
	return notFoundIfNoRows(res, "set service tcp limits")
}

// TCPBan is a source banned from a tcp service until Until: an IPv4 address
// or an IPv6 /64.
type TCPBan struct {
	ServiceID string
	Source    string
	Until     time.Time
	Reason    string
	CreatedAt time.Time
}

const tcpBanCols = `service_id, source, until, reason, created_at`

func scanTCPBan(sc interface{ Scan(...any) error }) (TCPBan, error) {
	var b TCPBan
	if err := sc.Scan(&b.ServiceID, &b.Source, &b.Until, &b.Reason, &b.CreatedAt); err != nil {
		return TCPBan{}, err
	}
	b.Until, b.CreatedAt = b.Until.UTC(), b.CreatedAt.UTC()
	return b, nil
}

// ErrTCPBanLonger: the source has an active ban that ends later than the
// one written; a ban is extended or renewed, never shortened (lifting it is
// DeleteTCPBan).
var ErrTCPBanLonger = errors.New("db: the source is banned for longer")

// PutTCPBan adds a ban, or replaces the ban of the same source of the
// service, in one statement. An active ban (one that ends after CreatedAt)
// that ends later than b is kept and ErrTCPBanLonger returned, so a ban is
// never shortened. CreatedAt is now when it is zero.
func (x *DB) PutTCPBan(ctx context.Context, b TCPBan) error {
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now()
	}
	res, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO tcp_bans(service_id, source, until, reason, created_at) VALUES(?,?,?,?,?)
		 ON CONFLICT(service_id, source) DO UPDATE SET until=excluded.until, reason=excluded.reason, created_at=excluded.created_at
		 WHERE tcp_bans.until <= excluded.until OR tcp_bans.until <= excluded.created_at`,
		b.ServiceID, b.Source, b.Until.UTC(), b.Reason, b.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("put tcp ban: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("put tcp ban: %w", err)
	}
	if n == 0 {
		return ErrTCPBanLonger
	}
	return nil
}

// ListTCPBans returns the bans of the service that have not expired at now,
// the first to expire first.
func (x *DB) ListTCPBans(ctx context.Context, serviceID string, now time.Time) ([]TCPBan, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT `+tcpBanCols+` FROM tcp_bans WHERE service_id=? AND until > ? ORDER BY until, source`,
		serviceID, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("list tcp bans: %w", err)
	}
	defer rows.Close()
	out := []TCPBan{}
	for rows.Next() {
		b, err := scanTCPBan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan tcp ban: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tcp bans rows: %w", err)
	}
	return out, nil
}

// CountTCPBans counts the bans of the service that have not expired at now.
func (x *DB) CountTCPBans(ctx context.Context, serviceID string, now time.Time) (int, error) {
	var n int
	if err := x.sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tcp_bans WHERE service_id=? AND until > ?`, serviceID, now.UTC()).Scan(&n); err != nil {
		return 0, fmt.Errorf("count tcp bans: %w", err)
	}
	return n, nil
}

// DeleteTCPBan removes the ban of source from the service and returns it,
// expired or not; ErrNotFound when there is none.
func (x *DB) DeleteTCPBan(ctx context.Context, serviceID, source string) (TCPBan, error) {
	b, err := scanTCPBan(x.sqlDB.QueryRowContext(ctx,
		`SELECT `+tcpBanCols+` FROM tcp_bans WHERE service_id=? AND source=?`, serviceID, source))
	if errors.Is(err, sql.ErrNoRows) {
		return TCPBan{}, ErrNotFound
	}
	if err != nil {
		return TCPBan{}, fmt.Errorf("get tcp ban: %w", err)
	}
	res, err := x.sqlDB.ExecContext(ctx, `DELETE FROM tcp_bans WHERE service_id=? AND source=?`, serviceID, source)
	if err != nil {
		return TCPBan{}, fmt.Errorf("delete tcp ban: %w", err)
	}
	if err := notFoundIfNoRows(res, "delete tcp ban"); err != nil {
		return TCPBan{}, err
	}
	return b, nil
}

// PruneTCPBans deletes the bans of the service that expired at or before
// now, and returns how many.
func (x *DB) PruneTCPBans(ctx context.Context, serviceID string, now time.Time) (int64, error) {
	res, err := x.sqlDB.ExecContext(ctx, `DELETE FROM tcp_bans WHERE service_id=? AND until <= ?`, serviceID, now.UTC())
	if err != nil {
		return 0, fmt.Errorf("prune tcp bans: %w", err)
	}
	return res.RowsAffected()
}

// TCPDailyUsage is what the service carried on day (YYYY-MM-DD, UTC); 0 and
// 0 for a day without a row.
func (x *DB) TCPDailyUsage(ctx context.Context, serviceID, day string) (in, out int64, err error) {
	err = x.sqlDB.QueryRowContext(ctx,
		`SELECT bytes_in, bytes_out FROM tcp_daily_usage WHERE service_id=? AND day=?`, serviceID, day).Scan(&in, &out)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("tcp daily usage: %w", err)
	}
	return in, out, nil
}

// AddTCPDailyUsage adds in and out to the service's day, in one statement.
// ErrNotFound when there is no such service (it was deleted): the bytes have
// nowhere to go and are not to be tried again.
func (x *DB) AddTCPDailyUsage(ctx context.Context, serviceID, day string, in, out int64) error {
	res, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO tcp_daily_usage(service_id, day, bytes_in, bytes_out)
		 SELECT id, CAST(? AS TEXT), CAST(? AS BIGINT), CAST(? AS BIGINT) FROM services WHERE id=?
		 ON CONFLICT(service_id, day) DO UPDATE SET
		   bytes_in  = tcp_daily_usage.bytes_in + excluded.bytes_in,
		   bytes_out = tcp_daily_usage.bytes_out + excluded.bytes_out`,
		day, in, out, serviceID)
	if err != nil {
		return fmt.Errorf("add tcp daily usage: %w", err)
	}
	return notFoundIfNoRows(res, "add tcp daily usage")
}

// PruneTCPDailyUsage deletes the usage rows of every service for the days
// before before (YYYY-MM-DD), and returns how many.
func (x *DB) PruneTCPDailyUsage(ctx context.Context, before string) (int64, error) {
	res, err := x.sqlDB.ExecContext(ctx, `DELETE FROM tcp_daily_usage WHERE day < ?`, before)
	if err != nil {
		return 0, fmt.Errorf("prune tcp daily usage: %w", err)
	}
	return res.RowsAffected()
}
