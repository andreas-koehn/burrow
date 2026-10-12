package api

// What is not converted yet. Every area task deletes its own block from each
// list; the last task sets openAPIConversionDone and the lists are empty.
// A list must not grow: an operation that is added to the API gets its
// operationId, its schemas and its table rows in the commit that adds it.

// openAPIConversionDone turns TestOpenAPI_NothingPending on. The verify task
// sets it to true once the three lists below are empty.
const openAPIConversionDone = false

// openAPIPendingOperationIDs lists the operations that have no operationId
// yet, as "METHOD path".
var openAPIPendingOperationIDs = []string{
	// O05: cache, per-service AI configuration, rate limits, cost, budgets
	"DELETE /api/v1/services/{serviceID}/cache/entries",
	"GET /api/v1/services/{serviceID}/ai-config",
	"PUT /api/v1/services/{serviceID}/ai-config",
	"GET /api/v1/cache/settings",
	"PUT /api/v1/cache/settings",
	"GET /api/v1/cache/stats",
	"DELETE /api/v1/cache/entries",
	"DELETE /api/v1/cache/semantic/entries",
	"GET /api/v1/rate-limits",
	"POST /api/v1/rate-limits",
	"GET /api/v1/rate-limits/usage",
	"PUT /api/v1/rate-limits/{id}",
	"DELETE /api/v1/rate-limits/{id}",
	"GET /api/v1/cost/pricing",
	"PUT /api/v1/cost/pricing",
	"GET /api/v1/cost/summary",
	"GET /api/v1/cost/export",
	"GET /api/v1/budgets",
	"POST /api/v1/budgets",
	"PUT /api/v1/budgets/{id}",
	"DELETE /api/v1/budgets/{id}",
	// O06: redaction, guardrails, inspector
	"GET /api/v1/services/{serviceID}/inspector/requests",
	"GET /api/v1/services/{serviceID}/inspector/requests/{rid}",
	"POST /api/v1/services/{serviceID}/inspector/requests/{rid}/replay",
	"POST /api/v1/services/{serviceID}/inspector/requests/{rid}/replay-compare",
	"GET /api/v1/services/{serviceID}/inspector/stream",
	"GET /api/v1/redaction/rules",
	"POST /api/v1/redaction/rules",
	"PUT /api/v1/redaction/rules/{id}",
	"DELETE /api/v1/redaction/rules/{id}",
	"GET /api/v1/redaction/settings",
	"PUT /api/v1/redaction/settings",
	"POST /api/v1/redaction/preview",
	"GET /api/v1/guardrails/settings",
	"PUT /api/v1/guardrails/settings",
	"GET /api/v1/guardrails/patterns",
	// O07: audit, webhooks, connection logs
	"GET /api/v1/audit/events",
	"GET /api/v1/audit/export",
	"GET /api/v1/audit/fingerprint",
	"POST /api/v1/audit/verify",
	"GET /api/v1/webhooks",
	"POST /api/v1/webhooks",
	"GET /api/v1/webhooks/deliveries",
	"PUT /api/v1/webhooks/{id}",
	"DELETE /api/v1/webhooks/{id}",
	"POST /api/v1/webhooks/{id}/test",
	"POST /api/v1/webhooks/{id}/pause",
	"POST /api/v1/webhooks/{id}/resume",
	"POST /api/v1/webhooks/{id}/preview",
	"GET /api/v1/connection-logs",
	"GET /api/v1/connection-logs/rollups",
	"GET /api/v1/connection-logs/export",
	// O08: settings, backups, MCP status, database, metrics, events, OpenAPI viewer
	"GET /metrics",
	"GET /api/v1/backups",
	"POST /api/v1/backups",
	"DELETE /api/v1/backups/{id}",
	"GET /api/v1/backups/{id}/download",
	"POST /api/v1/backups/{id}/verify",
	"POST /api/v1/backups/restore",
	"GET /api/v1/backups/restores/{id}",
	"GET /api/v1/settings",
	"PUT /api/v1/settings",
	"POST /api/v1/settings/test-email",
	"GET /api/v1/settings/retention",
	"PUT /api/v1/settings/retention",
	"GET /api/v1/mcp/status",
	"GET /api/v1/mcp/tools",
	"GET /api/v1/events",
	"GET /api/v1/openapi/viewer/",
	"GET /api/v1/openapi/viewer/static/{file}",
	"GET /api/v1/database",
}

// openAPIPendingEmpty lists the operations that still reference the Empty
// schema in a request body or a response.
var openAPIPendingEmpty = []string{
	// O05: cache, per-service AI configuration, rate limits, cost, budgets
	"GET /api/v1/services/{serviceID}/ai-config",
	"PUT /api/v1/services/{serviceID}/ai-config",
	"GET /api/v1/cache/settings",
	"PUT /api/v1/cache/settings",
	"GET /api/v1/cache/stats",
	"GET /api/v1/rate-limits",
	"POST /api/v1/rate-limits",
	"GET /api/v1/rate-limits/usage",
	"PUT /api/v1/rate-limits/{id}",
	"GET /api/v1/cost/pricing",
	"PUT /api/v1/cost/pricing",
	"GET /api/v1/cost/summary",
	"GET /api/v1/budgets",
	"POST /api/v1/budgets",
	"PUT /api/v1/budgets/{id}",
	// O06: redaction, guardrails, inspector
	"POST /api/v1/services/{serviceID}/inspector/requests/{rid}/replay",
	"POST /api/v1/services/{serviceID}/inspector/requests/{rid}/replay-compare",
	"GET /api/v1/redaction/rules",
	"POST /api/v1/redaction/rules",
	"PUT /api/v1/redaction/rules/{id}",
	"GET /api/v1/redaction/settings",
	"PUT /api/v1/redaction/settings",
	"POST /api/v1/redaction/preview",
	"GET /api/v1/guardrails/settings",
	"PUT /api/v1/guardrails/settings",
	"GET /api/v1/guardrails/patterns",
	// O07: audit, webhooks, connection logs
	"GET /api/v1/audit/events",
	"GET /api/v1/audit/fingerprint",
	"POST /api/v1/audit/verify",
	"GET /api/v1/webhooks",
	"POST /api/v1/webhooks",
	"GET /api/v1/webhooks/deliveries",
	"PUT /api/v1/webhooks/{id}",
	"POST /api/v1/webhooks/{id}/test",
	"POST /api/v1/webhooks/{id}/preview",
	// O08: settings, backups, MCP status, database, metrics, events, OpenAPI viewer
	"GET /api/v1/backups",
	"POST /api/v1/backups",
	"POST /api/v1/backups/{id}/verify",
	"GET /api/v1/backups/restores/{id}",
	"GET /api/v1/settings",
	"PUT /api/v1/settings",
	"GET /api/v1/settings/retention",
	"PUT /api/v1/settings/retention",
}

// openAPIPendingResponseContent lists the operations whose 200/201/202
// response declares no content yet.
var openAPIPendingResponseContent = []string{}
