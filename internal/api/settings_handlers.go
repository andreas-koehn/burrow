package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ankoehn/burrow/internal/store"
)

// allowedSettingKeys is the whitelist of non-secret settings the API will
// read back and persist. smtp.password is intentionally absent — the SMTP
// secret comes from BURROW_SMTP_PASSWORD(_FILE) and is NEVER stored in the DB.
//
// connection_logs.rollup_include_top_ips (Q12, v0.5.1 Task 5): when "true" or
// missing, SQLSink.Rollup populates the top 10 source IPs per (day,
// service_id, kind) into connection_log_rollup_top_ips and the rollups read
// path emits a top_source_ips array. When "false", the aggregation is
// skipped AND any pre-existing aux rows are scrubbed. Default-true policy:
// the row need not be seeded.
var allowedSettingKeys = map[string]bool{
	"smtp.host":                              true,
	"smtp.port":                              true,
	"smtp.username":                          true,
	"smtp.from":                              true,
	"smtp.tls":                               true,
	"connection_logs.rollup_include_top_ips": true,
}

// GetSettings returns the non-secret settings map (admin only). GET /api/v1/settings
func (d Deps) GetSettings(w http.ResponseWriter, r *http.Request) {
	m, err := d.Settings.GetSettings(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "get settings failed")
		return
	}
	out := make(map[string]string, len(allowedSettingKeys))
	for k := range allowedSettingKeys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// SaveSettings upserts only whitelisted non-secret keys (admin only).
// PUT /api/v1/settings
func (d Deps) SaveSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var in map[string]string
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	clean := map[string]string{}
	for k, v := range in {
		if allowedSettingKeys[k] {
			clean[k] = v
		}
	}
	if v, ok := clean["smtp.tls"]; ok && v != "none" && v != "starttls" && v != "implicit" {
		writeErr(w, http.StatusBadRequest, "smtp.tls must be none, starttls, or implicit")
		return
	}
	if err := d.Settings.SaveSettings(r.Context(), clean); err != nil {
		writeErr(w, http.StatusInternalServerError, "save settings failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type testEmailReq struct {
	To string `json:"to"`
}

// SendTestEmail sends an SMTP connection-test message (admin only).
// POST /api/v1/settings/test-email — 204 ok, 409 unconfigured, 502 send error.
func (d Deps) SendTestEmail(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	var in testEmailReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.To == "" {
		writeErr(w, http.StatusBadRequest, "to is required")
		return
	}
	err := d.Settings.SendTestEmail(r.Context(), in.To)
	if errors.Is(err, store.ErrSMTPUnconfigured) {
		writeErr(w, http.StatusConflict, "smtp is not configured — set host/port and BURROW_SMTP_PASSWORD")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, "test email failed: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
