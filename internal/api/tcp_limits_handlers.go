package api

// tcp_limits_handlers.go — the bandwidth, daily volume and automatic bans of
// a tcp service, and its bans (tcp complete, E6 and E7).
//
//   - GET/PUT /services/{id}/tcp-limits: the settings, with what the service
//     carried today. A PUT may loosen protection (a limit raised or removed,
//     the bans turned off), so it asks for a recent confirmation and is no
//     management MCP tool, whichever way it goes.
//   - GET/POST /services/{id}/tcp-bans and DELETE …/tcp-bans?source=: the
//     banned sources. A ban by hand narrows who may connect: it is a tool
//     and needs no confirmation. Lifting one lets a source in again: it asks
//     for a confirmation and is no tool.
//
// Every write is audited by the store, and reaches the relay that serves the
// service at once.

import (
	"cmp"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/server"
	"github.com/ankoehn/burrow/internal/store"
)

// TCPGuardRelay is the relay as the limits and bans routes reach it
// (*server.Server): the usage it counted today, whether it refuses the
// service for its daily volume, and the bans it holds. It is found on
// Deps.Connections.
type TCPGuardRelay interface {
	TCPUsageToday(serviceID string) (day string, in, out uint64, ok bool)
	TCPQuotaReached(serviceID string) bool
	BanTCPSource(serviceID, source string, until time.Time, reason string)
	UnbanTCPSource(serviceID, source string) bool
	TCPBans(serviceID string) []server.TCPBan
}

type tcpAutoBanJSON struct {
	Enabled   bool `json:"enabled"`
	ShortS    int  `json:"short_s"`
	Count     int  `json:"count"`
	WindowMin int  `json:"window_min"`
	BanMin    int  `json:"ban_min"`
}

type tcpUsageJSON struct {
	Day      string `json:"day"`
	BytesIn  int64  `json:"bytes_in"`
	BytesOut int64  `json:"bytes_out"`
	Bytes    int64  `json:"bytes"`
}

// tcpLimitsResp is the limits of a tcp service. usage_today is what it
// carried on the UTC day day (the relay's count while a client serves it,
// else what is stored); quota_reached says the relay refuses its new
// connections because the day carried daily_bytes.
type tcpLimitsResp struct {
	RateLimitKbps int            `json:"rate_limit_kbps"`
	DailyBytes    int64          `json:"daily_bytes"`
	AutoBan       tcpAutoBanJSON `json:"autoban"`
	UsageToday    tcpUsageJSON   `json:"usage_today"`
	QuotaReached  bool           `json:"quota_reached"`
}

type tcpAutoBanReq struct {
	Enabled   *bool `json:"enabled"`
	ShortS    *int  `json:"short_s"`
	Count     *int  `json:"count"`
	WindowMin *int  `json:"window_min"`
	BanMin    *int  `json:"ban_min"`
}

// tcpLimitsReq changes the limits it names and keeps the others.
type tcpLimitsReq struct {
	RateLimitKbps *int           `json:"rate_limit_kbps"`
	DailyBytes    *int64         `json:"daily_bytes"`
	AutoBan       *tcpAutoBanReq `json:"autoban"`
}

func (d Deps) tcpLimitsView(serviceID string, l store.TCPLimits, u store.TCPUsage) tcpLimitsResp {
	out := tcpLimitsResp{
		RateLimitKbps: l.RateLimitKbps, DailyBytes: l.DailyBytes,
		AutoBan: tcpAutoBanJSON{
			Enabled: l.AutoBan.Enabled, ShortS: l.AutoBan.ShortS, Count: l.AutoBan.Count,
			WindowMin: l.AutoBan.WindowMin, BanMin: l.AutoBan.BanMin,
		},
		UsageToday: tcpUsageJSON{Day: u.Day, BytesIn: u.BytesIn, BytesOut: u.BytesOut},
	}
	if g, ok := d.Connections.(TCPGuardRelay); ok {
		if day, in, outB, live := g.TCPUsageToday(serviceID); live {
			out.UsageToday = tcpUsageJSON{Day: day, BytesIn: int64(in), BytesOut: int64(outB)}
		}
		out.QuotaReached = g.TCPQuotaReached(serviceID)
	}
	out.UsageToday.Bytes = out.UsageToday.BytesIn + out.UsageToday.BytesOut
	return out
}

// storeErrText is the text of a validation error of the store without its
// sentinel's prefix ("store: invalid tcp limits: "), one line.
func storeErrText(err, sentinel error) string {
	parts := strings.Split(err.Error(), "\n")
	for i, p := range parts {
		parts[i] = strings.TrimPrefix(p, sentinel.Error()+": ")
	}
	return strings.Join(parts, "; ")
}

// GetServiceTCPLimits handles GET /api/v1/services/{serviceID}/tcp-limits.
func (d Deps) GetServiceTCPLimits(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	l, u, err := d.Services.ServiceTCPLimits(r.Context(), userID(r.Context()), role, serviceID)
	if err != nil {
		if !mapServiceErr(w, err, "service not found") {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	writeJSON(w, http.StatusOK, d.tcpLimitsView(serviceID, l, u))
}

// PutServiceTCPLimits handles PUT /api/v1/services/{serviceID}/tcp-limits:
// rate_limit_kbps (0, or 64 to 10 000 000), daily_bytes (0, or 1 MiB to
// 1 PiB) and autoban {enabled, short_s, count, window_min, ban_min}, any of
// them; a field left out is kept. The relay applies them at once, to the
// open connections as well. The store audits the write
// (service.tcp_limits.update).
func (d Deps) PutServiceTCPLimits(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	r.Body = http.MaxBytesReader(w, r.Body, 2048)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var in tcpLimitsReq
	if err := dec.Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if in.RateLimitKbps == nil && in.DailyBytes == nil && in.AutoBan == nil {
		writeErr(w, http.StatusBadRequest, "rate_limit_kbps, daily_bytes or autoban is required")
		return
	}
	u := store.TCPLimitsUpdate{RateLimitKbps: in.RateLimitKbps, DailyBytes: in.DailyBytes}
	if a := in.AutoBan; a != nil {
		if a.Enabled == nil && a.ShortS == nil && a.Count == nil && a.WindowMin == nil && a.BanMin == nil {
			writeErr(w, http.StatusBadRequest, "autoban needs one of enabled, short_s, count, window_min, ban_min")
			return
		}
		u.AutoBan = &store.TCPAutoBanUpdate{Enabled: a.Enabled, ShortS: a.ShortS, Count: a.Count, WindowMin: a.WindowMin, BanMin: a.BanMin}
	}
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	_, after, err := d.Services.SetServiceTCPLimits(r.Context(), userID(r.Context()), role, serviceID, u)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInvalidTCPLimits):
			writeErr(w, http.StatusBadRequest, storeErrText(err, store.ErrInvalidTCPLimits))
		case !mapServiceErr(w, err, "service not found"):
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	d.refreshTCP(r.Context(), serviceID)
	// The usage as stored; the relay's count replaces it while it serves.
	_, usage, uerr := d.Services.ServiceTCPLimits(r.Context(), userID(r.Context()), role, serviceID)
	if uerr != nil {
		usage = store.TCPUsage{Day: time.Now().UTC().Format(time.DateOnly)}
	}
	writeJSON(w, http.StatusOK, d.tcpLimitsView(serviceID, after, usage))
}

// tcpBanResp is a banned source of a tcp service: an IPv4 address or an IPv6
// /64, until when, and why (auto: the relay banned it; manual: by hand).
type tcpBanResp struct {
	Source    string    `json:"source"`
	Until     time.Time `json:"until"`
	Reason    string    `json:"reason"`
	Auto      bool      `json:"auto"`
	CreatedAt time.Time `json:"created_at"`
}

func banResp(b db.TCPBan) tcpBanResp {
	return tcpBanResp{
		Source: b.Source, Until: b.Until, Reason: b.Reason, CreatedAt: b.CreatedAt,
		Auto: b.Reason == server.TCPBanReasonAuto || strings.HasPrefix(b.Reason, server.TCPBanReasonAuto+":"),
	}
}

// tcpBanReq bans a source for ban_min minutes, with an optional note.
type tcpBanReq struct {
	Source string `json:"source"`
	BanMin int    `json:"ban_min"`
	Note   string `json:"note"`
}

func mapBanErr(w http.ResponseWriter, err error, notFound string) {
	switch {
	case errors.Is(err, store.ErrInvalidTCPBan):
		writeErr(w, http.StatusBadRequest, storeErrText(err, store.ErrInvalidTCPBan))
	case errors.Is(err, store.ErrTooManyTCPBans):
		writeErr(w, http.StatusConflict, "the service has its maximum of 4096 active bans")
	case errors.Is(err, store.ErrNoTCPBan):
		writeErr(w, http.StatusNotFound, "no ban of that source")
	case !mapServiceErr(w, err, notFound):
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

// ListServiceTCPBans handles GET /api/v1/services/{serviceID}/tcp-bans: the
// active bans, the first to expire first: the stored ones and those the
// relay holds that the store does not have (a write that failed).
func (d Deps) ListServiceTCPBans(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	bans, err := d.Services.ListTCPBans(r.Context(), userID(r.Context()), role, serviceID)
	if err != nil {
		mapBanErr(w, err, "service not found")
		return
	}
	if g, ok := d.Connections.(TCPGuardRelay); ok {
		stored := make(map[string]bool, len(bans))
		for _, b := range bans {
			stored[b.Source] = true
		}
		n := len(bans)
		for _, b := range g.TCPBans(serviceID) {
			if !stored[b.Source] {
				bans = append(bans, db.TCPBan{ServiceID: serviceID, Source: b.Source, Until: b.Until, Reason: b.Reason, CreatedAt: b.CreatedAt})
			}
		}
		if len(bans) > n {
			slices.SortFunc(bans, func(a, b db.TCPBan) int {
				return cmp.Or(a.Until.Compare(b.Until), cmp.Compare(a.Source, b.Source))
			})
		}
	}
	out := make([]tcpBanResp, 0, len(bans))
	for _, b := range bans {
		out = append(out, banResp(b))
	}
	writeJSON(w, http.StatusOK, out)
}

// PostServiceTCPBan handles POST /api/v1/services/{serviceID}/tcp-bans:
// source (an IP address, or an IPv6 address or prefix of its /64) is banned
// for ban_min minutes (1 to 43200). Its live visitors are closed at once.
// Audited as tcp.ban.add.
func (d Deps) PostServiceTCPBan(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var in tcpBanReq
	if err := dec.Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if strings.TrimSpace(in.Source) == "" {
		writeErr(w, http.StatusBadRequest, "source is required")
		return
	}
	if in.BanMin == 0 {
		writeErr(w, http.StatusBadRequest, "ban_min is required: 1 to 43200 minutes")
		return
	}
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	b, err := d.Services.AddTCPBan(r.Context(), userID(r.Context()), role, serviceID, in.Source, in.BanMin, in.Note)
	if err != nil {
		mapBanErr(w, err, "service not found")
		return
	}
	if g, ok := d.Connections.(TCPGuardRelay); ok {
		g.BanTCPSource(serviceID, b.Source, b.Until, b.Reason)
	}
	writeJSON(w, http.StatusCreated, banResp(b))
}

// DeleteServiceTCPBan handles DELETE
// /api/v1/services/{serviceID}/tcp-bans?source=…: the ban of the source (an
// address of a banned IPv6 /64 lifts the /64) ends now, in the store and on
// the relay (which may hold a ban the store does not have). 404 when there
// is none. Audited as tcp.ban.remove.
func (d Deps) DeleteServiceTCPBan(w http.ResponseWriter, r *http.Request) {
	serviceID := chi.URLParam(r, "serviceID")
	source := strings.TrimSpace(r.URL.Query().Get("source"))
	if source == "" {
		writeErr(w, http.StatusBadRequest, "the query parameter source is required")
		return
	}
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	var held func(string) bool
	if g, ok := d.Connections.(TCPGuardRelay); ok {
		held = func(src string) bool { return g.UnbanTCPSource(serviceID, src) }
	}
	if _, err := d.Services.RemoveTCPBan(r.Context(), userID(r.Context()), role, serviceID, source, held); err != nil {
		mapBanErr(w, err, "service not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
