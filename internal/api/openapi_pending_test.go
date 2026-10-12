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
