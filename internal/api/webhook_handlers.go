package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/webhook"
	wtemplate "github.com/ankoehn/burrow/internal/webhook/template"
)

// WebhookStore is the narrow CRUD surface the webhook handlers consume.
// *db.DB satisfies it; tests provide an in-memory fake.
type WebhookStore interface {
	ListWebhooks(ctx context.Context) ([]db.Webhook, error)
	GetWebhook(ctx context.Context, id string) (db.Webhook, error)
	CreateWebhook(ctx context.Context, w db.Webhook) error
	UpdateWebhook(ctx context.Context, w db.Webhook) error
	DeleteWebhook(ctx context.Context, id string) error
	SetWebhookPaused(ctx context.Context, id string, paused bool) error
	ListWebhookDeliveries(ctx context.Context, q db.WebhookDeliveryQuery) ([]db.WebhookDelivery, error)
}

// WebhookDispatcher is the narrow surface the handlers use to register
// secrets on POST and to deliver test events on .../test. The dispatcher
// also implements cost.Dispatcher (Publish), but the JSON API only needs
// the secret-registry + on-demand delivery seams.
//
// *webhook.Dispatcher (paired with *webhook.InMemorySecrets) satisfies
// this directly; tests provide a thin stub.
type WebhookDispatcher interface {
	// DeliverNow runs one full retry cycle synchronously for the given
	// webhook. Used by POST /webhooks/{id}/test.
	DeliverNow(ctx context.Context, webhookID, event string, payload any) (int, int, error)

	// Publish enqueues an event for async fan-out to all subscribed webhooks.
	// Fire-and-forget; used by the custom-domain handler to emit cert.expiring
	// events (v0.5.0 Task 7) and by the cost engine for budget.exceeded events.
	Publish(ctx context.Context, event string, payload any)
}

// WebhookSecretRegistry is the secret-plaintext side of the dispatcher.
// *webhook.InMemorySecrets satisfies it. Separate from WebhookDispatcher
// so the dispatcher's Publish/DeliverNow surface stays minimal.
type WebhookSecretRegistry interface {
	Set(id, plaintext string)
	Delete(id string)
}

// --- Permission gate --------------------------------------------------------
//
// Spec Part H: every webhook route requires admin OR webhooks:manage.

// --- Wire shapes ------------------------------------------------------------

// webhookResp is the wire shape for GET responses. signing_secret is
// only populated on the POST response (and explicitly omitted from list
// + GET responses — the plaintext is never stored).
type webhookResp struct {
	ID                  string     `json:"id"`
	Name                string     `json:"name"`
	URL                 string     `json:"url"`
	Events              []string   `json:"events"`
	Paused              bool       `json:"paused"`
	Status              string     `json:"status"` // "ok" | "failing"
	ConsecutiveFailures int        `json:"consecutive_failures"`
	FirstFailureAt      *time.Time `json:"first_failure_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	SigningSecret       string     `json:"signing_secret,omitempty"` // POST response only
}

// statusFor returns the UI status tag derived from the failure counter.
func statusFor(w db.Webhook) string {
	if w.ConsecutiveFailures >= webhook.FailingThreshold {
		return "failing"
	}
	return "ok"
}

// toWebhookResp converts the db.Webhook row to the wire shape, decoding
// the events JSON array. Malformed events (corrupt DB) decode to an
// empty array so the response is still well-formed.
func toWebhookResp(w db.Webhook) webhookResp {
	var events []string
	if strings.TrimSpace(w.Events) != "" {
		_ = json.Unmarshal([]byte(w.Events), &events)
	}
	if events == nil {
		events = []string{}
	}
	return webhookResp{
		ID:                  w.ID,
		Name:                w.Name,
		URL:                 w.URL,
		Events:              events,
		Paused:              w.Paused,
		Status:              statusFor(w),
		ConsecutiveFailures: w.ConsecutiveFailures,
		FirstFailureAt:      w.FirstFailureAt,
		CreatedAt:           w.CreatedAt,
	}
}

// webhookReq is the wire shape for POST + PUT bodies.
type webhookReq struct {
	Name            string   `json:"name"`
	URL             string   `json:"url"`
	Events          []string `json:"events"`
	Paused          bool     `json:"paused"`
	PayloadTemplate string   `json:"payload_template,omitempty"` // v0.5.0: optional Go template
}

// validateWebhookReq returns "" on success or a user-visible 400 message.
// Events must be non-empty and every entry must be in the spec-closed
// vocabulary. An optional payload_template is validated via wtemplate.Validate.
func validateWebhookReq(in webhookReq) string {
	if strings.TrimSpace(in.Name) == "" {
		return "name is required"
	}
	if len(in.Name) > 200 {
		return "name too long (max 200 chars)"
	}
	if strings.TrimSpace(in.URL) == "" {
		return "url is required"
	}
	u, err := url.Parse(in.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "url must be a valid http(s) URL"
	}
	if len(in.Events) == 0 {
		return "events must be a non-empty array"
	}
	for _, e := range in.Events {
		if !webhook.IsClosedEvent(e) {
			return "events contains an unknown event: " + e
		}
	}
	// Validate the payload_template against the first subscribed event so
	// function-misuse is caught at create/update time, not at delivery time.
	if in.PayloadTemplate != "" {
		event := ""
		if len(in.Events) > 0 {
			event = in.Events[0]
		}
		if err := wtemplate.Validate(event, in.PayloadTemplate); err != nil {
			return "payload_template invalid: " + err.Error()
		}
	}
	return ""
}

// deliveryResp is the wire shape for one webhook_deliveries row on
// GET /webhooks/deliveries.
type deliveryResp struct {
	ID                  string    `json:"id"`
	WebhookID           string    `json:"webhook_id"`
	Event               string    `json:"event"`
	TS                  time.Time `json:"ts"`
	StatusCode          int       `json:"status_code"`
	Attempt             int       `json:"attempt"`
	LatencyMS           int       `json:"latency_ms"`
	RequestBodyPreview  string    `json:"request_body_preview"`
	ResponseBodyPreview string    `json:"response_body_preview"`
}

func toDeliveryResp(d db.WebhookDelivery) deliveryResp {
	req := ""
	if d.RequestPreview != nil {
		req = *d.RequestPreview
	}
	resp := ""
	if d.ResponsePreview != nil {
		resp = *d.ResponsePreview
	}
	return deliveryResp{
		ID:                  d.ID,
		WebhookID:           d.WebhookID,
		Event:               d.Event,
		TS:                  d.Ts,
		StatusCode:          d.StatusCode,
		Attempt:             d.Attempt,
		LatencyMS:           d.LatencyMs,
		RequestBodyPreview:  req,
		ResponseBodyPreview: resp,
	}
}

// --- GET /api/v1/webhooks ---------------------------------------------------

// GetWebhooks handles GET /api/v1/webhooks. Returns the list of every
// configured webhook (signing_secret is never included in this response).
func (d Deps) GetWebhooks(w http.ResponseWriter, r *http.Request) {
	if d.Webhooks == nil {
		writeJSON(w, http.StatusOK, []webhookResp{})
		return
	}
	rows, err := d.Webhooks.ListWebhooks(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list webhooks failed")
		return
	}
	out := make([]webhookResp, len(rows))
	for i, row := range rows {
		out[i] = toWebhookResp(row)
	}
	writeJSON(w, http.StatusOK, out)
}

// --- POST /api/v1/webhooks --------------------------------------------------

// PostWebhook handles POST /api/v1/webhooks. The response includes the
// plaintext signing_secret exactly once — clients MUST capture it because
// only the sha256 hash is stored. The dispatcher's in-memory secret
// registry is updated synchronously.
func (d Deps) PostWebhook(w http.ResponseWriter, r *http.Request) {
	if d.Webhooks == nil {
		writeErr(w, http.StatusInternalServerError, "webhook store unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var in webhookReq
	if err := json.Unmarshal(raw, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.URL = strings.TrimSpace(in.URL)
	if msg := validateWebhookReq(in); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	secret, err := generateSigningSecret()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "generate secret failed")
		return
	}
	hash := sha256.Sum256([]byte(secret))
	eventsJSON, _ := json.Marshal(in.Events)
	row := db.Webhook{
		ID:              uuid.NewString(),
		Name:            in.Name,
		URL:             in.URL,
		SecretHash:      hex.EncodeToString(hash[:]),
		Events:          string(eventsJSON),
		Paused:          in.Paused,
		PayloadTemplate: in.PayloadTemplate,
	}
	if err := d.Webhooks.CreateWebhook(r.Context(), row); err != nil {
		writeErr(w, http.StatusInternalServerError, "create webhook failed")
		return
	}
	if d.WebhookSecrets != nil {
		d.WebhookSecrets.Set(row.ID, secret)
	}
	// Read-back so created_at reflects the SQLite default.
	created, err := d.Webhooks.GetWebhook(r.Context(), row.ID)
	if err != nil {
		// Created but we can't read back: still return the secret so the
		// caller doesn't lose it.
		resp := toWebhookResp(row)
		resp.SigningSecret = secret
		writeJSON(w, http.StatusCreated, resp)
		return
	}
	resp := toWebhookResp(created)
	resp.SigningSecret = secret
	writeJSON(w, http.StatusCreated, resp)
}

// --- PUT /api/v1/webhooks/{id} ----------------------------------------------

// PutWebhook handles PUT /api/v1/webhooks/{id}. Mutates name, url,
// events, paused. Secret rotation is NOT supported by this route —
// callers must delete + recreate to rotate (the spec's "shown once"
// invariant applies to the new value too).
func (d Deps) PutWebhook(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	if d.Webhooks == nil {
		writeErr(w, http.StatusInternalServerError, "webhook store unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var in webhookReq
	if err := json.Unmarshal(raw, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.URL = strings.TrimSpace(in.URL)
	if msg := validateWebhookReq(in); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	eventsJSON, _ := json.Marshal(in.Events)
	row := db.Webhook{
		ID:              id,
		Name:            in.Name,
		URL:             in.URL,
		Events:          string(eventsJSON),
		Paused:          in.Paused,
		PayloadTemplate: in.PayloadTemplate,
	}
	if err := d.Webhooks.UpdateWebhook(r.Context(), row); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "webhook not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "update webhook failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- DELETE /api/v1/webhooks/{id} -------------------------------------------

// DeleteWebhook handles DELETE /api/v1/webhooks/{id}. Cascades to
// webhook_deliveries via the FK.
func (d Deps) DeleteWebhook(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	if d.Webhooks == nil {
		writeErr(w, http.StatusInternalServerError, "webhook store unavailable")
		return
	}
	if err := d.Webhooks.DeleteWebhook(r.Context(), id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "webhook not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "delete webhook failed")
		return
	}
	if d.WebhookSecrets != nil {
		d.WebhookSecrets.Delete(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- POST /api/v1/webhooks/{id}/test ---------------------------------------

// PostWebhookTest handles POST /api/v1/webhooks/{id}/test. Synchronously
// dispatches one webhook.test event through the dispatcher's full
// attempt+retry cycle. Returns 204 on completion (even if delivery
// failed — the webhook_deliveries row records the actual outcome).
func (d Deps) PostWebhookTest(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	if d.Webhooks == nil {
		writeErr(w, http.StatusInternalServerError, "webhook store unavailable")
		return
	}
	if _, err := d.Webhooks.GetWebhook(r.Context(), id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "webhook not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	if d.WebhookDispatcher == nil {
		writeErr(w, http.StatusInternalServerError, "dispatcher unavailable")
		return
	}
	if _, _, err := d.WebhookDispatcher.DeliverNow(r.Context(), id, "webhook.test",
		map[string]any{"hello": "burrow"}); err != nil {
		writeErr(w, http.StatusInternalServerError, "delivery failed: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- POST /api/v1/webhooks/{id}/preview ------------------------------------

// previewReq is the wire shape for the preview endpoint.
type previewReq struct {
	Event  string         `json:"event"`
	Fields map[string]any `json:"fields"`
}

// previewResp is the wire shape for the preview response.
type previewResp struct {
	Rendered  string `json:"rendered"`
	SizeBytes int    `json:"size_bytes"`
}

// PostWebhookPreview handles POST /api/v1/webhooks/{id}/preview.
//
// If the webhook has a non-empty PayloadTemplate, it is rendered with the
// caller-supplied fields. If the template is empty, the default v0.4.0
// {"event":…,"data":…} body is produced using the provided fields.
//
// Returns 200 {"rendered":"…","size_bytes":N} on success, 400 when the
// template fails to render, 404 when the webhook is not found.
func (d Deps) PostWebhookPreview(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	if d.Webhooks == nil {
		writeErr(w, http.StatusInternalServerError, "webhook store unavailable")
		return
	}
	wh, err := d.Webhooks.GetWebhook(r.Context(), id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "webhook not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "lookup failed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var in previewReq
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
	}
	if in.Fields == nil {
		in.Fields = map[string]any{}
	}

	var rendered []byte
	var sizeBytes int

	if wh.PayloadTemplate != "" {
		// Render the webhook's stored template with the caller-supplied fields.
		rendered, sizeBytes, err = wtemplate.Render(wh.PayloadTemplate, in.Fields)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "template render error: "+err.Error())
			return
		}
	} else {
		// Default body: mirror the dispatcher's default {"event":…,"data":…} envelope.
		defaultBody, merr := json.Marshal(map[string]any{
			"event": in.Event,
			"data":  in.Fields,
		})
		if merr != nil {
			writeErr(w, http.StatusInternalServerError, "marshal default body failed")
			return
		}
		rendered = defaultBody
		sizeBytes = len(defaultBody)
	}

	writeJSON(w, http.StatusOK, previewResp{
		Rendered:  string(rendered),
		SizeBytes: sizeBytes,
	})
}

// --- POST /api/v1/webhooks/{id}/pause + /resume -----------------------------

// PostWebhookPause handles POST /api/v1/webhooks/{id}/pause.
func (d Deps) PostWebhookPause(w http.ResponseWriter, r *http.Request) {
	d.setPausedFlag(w, r, true)
}

// PostWebhookResume handles POST /api/v1/webhooks/{id}/resume.
func (d Deps) PostWebhookResume(w http.ResponseWriter, r *http.Request) {
	d.setPausedFlag(w, r, false)
}

func (d Deps) setPausedFlag(w http.ResponseWriter, r *http.Request, paused bool) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	if d.Webhooks == nil {
		writeErr(w, http.StatusInternalServerError, "webhook store unavailable")
		return
	}
	if err := d.Webhooks.SetWebhookPaused(r.Context(), id, paused); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "webhook not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "update pause flag failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- GET /api/v1/webhooks/deliveries ----------------------------------------

// GetWebhookDeliveries handles GET /api/v1/webhooks/deliveries.
// Query params: ?webhook_id=<id>&limit=<n>.
func (d Deps) GetWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	if d.Webhooks == nil {
		writeJSON(w, http.StatusOK, []deliveryResp{})
		return
	}
	q := db.WebhookDeliveryQuery{
		WebhookID: r.URL.Query().Get("webhook_id"),
	}
	if ls := r.URL.Query().Get("limit"); ls != "" {
		if n, err := strconv.Atoi(ls); err == nil {
			q.Limit = n
		}
	}
	rows, err := d.Webhooks.ListWebhookDeliveries(r.Context(), q)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list deliveries failed")
		return
	}
	out := make([]deliveryResp, len(rows))
	for i, row := range rows {
		out[i] = toDeliveryResp(row)
	}
	writeJSON(w, http.StatusOK, out)
}

// --- helpers ----------------------------------------------------------------

// generateSigningSecret returns a fresh 32-byte (256-bit) URL-safe
// random string formatted as "whsec_" + 64-char hex. The format matches
// the spec's "looks like a Stripe webhook secret" convention so admins
// can recognise it in their environment configs.
func generateSigningSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "whsec_" + hex.EncodeToString(b[:]), nil
}
