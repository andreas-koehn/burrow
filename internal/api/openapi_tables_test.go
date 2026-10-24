package api

// The tables of the OpenAPI contract tests (openapi_contract_test.go,
// openapi_shapes_test.go). They are the reviewed statement of what the
// document must say; the tests compare the document with them.

import (
	"encoding/json"
	"reflect"

	"github.com/ankoehn/burrow/internal/guardrails"
	"github.com/ankoehn/burrow/internal/mcpgw"
	"github.com/ankoehn/burrow/internal/redact"
)

// openAPIOperationIDs is every operation of the document with its
// operationId, as "METHOD path" → id, in the order of the document. The rule
// (README of the plan, docs/reference/api.md): lowerCamelCase, resource first
// then action. A collection is plural (servicesList), one item singular
// (serviceGet, serviceCreate); a sub-resource carries its parent in the
// singular (serviceApiKeysList); an action is the last path segment
// (webhookPause). An id is a public name: tools and generated clients are
// named after it, so a row is added, never changed.
var openAPIOperationIDs = map[string]string{
	"GET /metrics":                                                              "metricsGet",
	"POST /api/v1/auth/login":                                                   "authLogin",
	"POST /api/v1/auth/logout":                                                  "authLogout",
	"POST /api/v1/auth/reauth":                                                  "authReauth",
	"POST /api/v1/auth/change-password":                                         "authChangePassword",
	"GET /api/v1/me":                                                            "meGet",
	"GET /api/v1/sessions":                                                      "sessionsList",
	"DELETE /api/v1/sessions/{id}":                                              "sessionDelete",
	"POST /api/v1/sessions/revoke-all":                                          "sessionsRevokeAll",
	"GET /api/v1/tokens":                                                        "clientTokensList",
	"POST /api/v1/tokens":                                                       "clientTokenCreate",
	"DELETE /api/v1/tokens/{id}":                                                "clientTokenDelete",
	"GET /api/v1/tunnels":                                                       "tunnelsList",
	"PUT /api/v1/tunnels/{id}/access-mode":                                      "tunnelAccessModeUpdate",
	"GET /api/v1/services":                                                      "servicesList",
	"POST /api/v1/services":                                                     "serviceCreate",
	"GET /api/v1/services/slug-suggestion":                                      "serviceSlugSuggestionGet",
	"GET /api/v1/services/{serviceID}":                                          "serviceGet",
	"DELETE /api/v1/services/{serviceID}":                                       "serviceDelete",
	"PUT /api/v1/services/{serviceID}/access-mode":                              "serviceAccessModeUpdate",
	"PUT /api/v1/services/{serviceID}/slug":                                     "serviceSlugUpdate",
	"PUT /api/v1/services/{serviceID}/gateway-only":                             "serviceGatewayOnlyUpdate",
	"GET /api/v1/services/{serviceID}/api-keys":                                 "serviceApiKeysList",
	"POST /api/v1/services/{serviceID}/api-keys":                                "serviceApiKeyCreate",
	"DELETE /api/v1/services/{serviceID}/api-keys/{id}":                         "serviceApiKeyDelete",
	"GET /api/v1/services/{serviceID}/access-policy":                            "serviceAccessPolicyGet",
	"PUT /api/v1/services/{serviceID}/access-policy":                            "serviceAccessPolicyUpdate",
	"GET /api/v1/services/{serviceID}/ip-geo":                                   "serviceIpGeoGet",
	"PUT /api/v1/services/{serviceID}/ip-geo":                                   "serviceIpGeoUpdate",
	"GET /api/v1/services/{serviceID}/ip-grants":                                "serviceIpGrantsList",
	"POST /api/v1/services/{serviceID}/ip-grants":                               "serviceIpGrantCreate",
	"DELETE /api/v1/services/{serviceID}/ip-grants/{grantID}":                   "serviceIpGrantDelete",
	"PUT /api/v1/services/{serviceID}/max-connections":                          "serviceMaxConnectionsUpdate",
	"GET /api/v1/services/{serviceID}/tcp-settings":                             "serviceTcpSettingsGet",
	"PUT /api/v1/services/{serviceID}/tcp-settings":                             "serviceTcpSettingsUpdate",
	"GET /api/v1/services/{serviceID}/connections":                              "serviceConnectionsList",
	"DELETE /api/v1/services/{serviceID}/connections/{connID}":                  "serviceConnectionDelete",
	"DELETE /api/v1/services/{serviceID}/cache/entries":                         "serviceCacheEntriesDelete",
	"GET /api/v1/services/{serviceID}/ai-config":                                "serviceAiConfigGet",
	"PUT /api/v1/services/{serviceID}/ai-config":                                "serviceAiConfigUpdate",
	"GET /api/v1/services/{serviceID}/inspector/requests":                       "inspectorRequestsList",
	"GET /api/v1/services/{serviceID}/inspector/requests/{rid}":                 "inspectorRequestGet",
	"POST /api/v1/services/{serviceID}/inspector/requests/{rid}/replay":         "inspectorRequestReplay",
	"POST /api/v1/services/{serviceID}/inspector/requests/{rid}/replay-compare": "inspectorRequestReplayCompare",
	"GET /api/v1/services/{serviceID}/inspector/stream":                         "inspectorStream",
	"GET /api/v1/cache/settings":                                                "cacheSettingsGet",
	"PUT /api/v1/cache/settings":                                                "cacheSettingsUpdate",
	"GET /api/v1/cache/stats":                                                   "cacheStatsGet",
	"DELETE /api/v1/cache/entries":                                              "cacheEntriesDelete",
	"DELETE /api/v1/cache/semantic/entries":                                     "cacheSemanticEntriesDelete",
	"GET /api/v1/redaction/rules":                                               "redactionRulesList",
	"POST /api/v1/redaction/rules":                                              "redactionRuleCreate",
	"PUT /api/v1/redaction/rules/{id}":                                          "redactionRuleUpdate",
	"DELETE /api/v1/redaction/rules/{id}":                                       "redactionRuleDelete",
	"GET /api/v1/redaction/settings":                                            "redactionSettingsGet",
	"PUT /api/v1/redaction/settings":                                            "redactionSettingsUpdate",
	"POST /api/v1/redaction/preview":                                            "redactionPreview",
	"GET /api/v1/guardrails/settings":                                           "guardrailSettingsGet",
	"PUT /api/v1/guardrails/settings":                                           "guardrailSettingsUpdate",
	"GET /api/v1/guardrails/patterns":                                           "guardrailPatternsList",
	"POST /api/v1/guardrails/preview":                                           "guardrailPreview",
	"GET /api/v1/rate-limits":                                                   "rateLimitsList",
	"POST /api/v1/rate-limits":                                                  "rateLimitCreate",
	"GET /api/v1/rate-limits/usage":                                             "rateLimitUsageGet",
	"PUT /api/v1/rate-limits/{id}":                                              "rateLimitUpdate",
	"DELETE /api/v1/rate-limits/{id}":                                           "rateLimitDelete",
	"GET /api/v1/cost/pricing":                                                  "costPricingGet",
	"PUT /api/v1/cost/pricing":                                                  "costPricingUpdate",
	"GET /api/v1/cost/summary":                                                  "costSummaryGet",
	"GET /api/v1/cost/export":                                                   "costExport",
	"GET /api/v1/budgets":                                                       "budgetsList",
	"POST /api/v1/budgets":                                                      "budgetCreate",
	"PUT /api/v1/budgets/{id}":                                                  "budgetUpdate",
	"DELETE /api/v1/budgets/{id}":                                               "budgetDelete",
	"GET /api/v1/audit/events":                                                  "auditEventsList",
	"GET /api/v1/audit/export":                                                  "auditExport",
	"GET /api/v1/audit/fingerprint":                                             "auditFingerprintGet",
	"POST /api/v1/audit/verify":                                                 "auditVerify",
	"GET /api/v1/webhooks":                                                      "webhooksList",
	"POST /api/v1/webhooks":                                                     "webhookCreate",
	"GET /api/v1/webhooks/deliveries":                                           "webhookDeliveriesList",
	"PUT /api/v1/webhooks/{id}":                                                 "webhookUpdate",
	"DELETE /api/v1/webhooks/{id}":                                              "webhookDelete",
	"POST /api/v1/webhooks/{id}/test":                                           "webhookTest",
	"POST /api/v1/webhooks/{id}/pause":                                          "webhookPause",
	"POST /api/v1/webhooks/{id}/resume":                                         "webhookResume",
	"POST /api/v1/webhooks/{id}/preview":                                        "webhookPreview",
	"GET /api/v1/automation/presets":                                            "automationPresetsList",
	"GET /api/v1/automation/tokens":                                             "automationTokensList",
	"POST /api/v1/automation/tokens":                                            "automationTokenCreate",
	"DELETE /api/v1/automation/tokens/{id}":                                     "automationTokenDelete",
	"GET /api/v1/backups":                                                       "backupsList",
	"POST /api/v1/backups":                                                      "backupCreate",
	"DELETE /api/v1/backups/{id}":                                               "backupDelete",
	"GET /api/v1/backups/{id}/download":                                         "backupDownload",
	"POST /api/v1/backups/{id}/verify":                                          "backupVerify",
	"POST /api/v1/backups/restore":                                              "backupRestore",
	"GET /api/v1/backups/restores/{id}":                                         "backupRestoreGet",
	"GET /api/v1/roles":                                                         "rolesList",
	"POST /api/v1/roles":                                                        "roleCreate",
	"GET /api/v1/roles/permissions":                                             "rolePermissionsList",
	"GET /api/v1/roles/{name}":                                                  "roleGet",
	"PUT /api/v1/roles/{name}":                                                  "roleUpdate",
	"DELETE /api/v1/roles/{name}":                                               "roleDelete",
	"GET /api/v1/users":                                                         "usersList",
	"POST /api/v1/users":                                                        "userCreate",
	"PATCH /api/v1/users/{id}":                                                  "userUpdate",
	"DELETE /api/v1/users/{id}":                                                 "userDelete",
	"GET /api/v1/settings":                                                      "settingsGet",
	"PUT /api/v1/settings":                                                      "settingsUpdate",
	"POST /api/v1/settings/test-email":                                          "settingsTestEmail",
	"GET /api/v1/settings/retention":                                            "retentionSettingsGet",
	"PUT /api/v1/settings/retention":                                            "retentionSettingsUpdate",
	"GET /api/v1/clients":                                                       "clientsList",
	"GET /api/v1/clients/known":                                                 "knownClientsList",
	"GET /api/v1/clients/{sessionID}":                                           "clientGet",
	"GET /api/v1/clients/connect-info":                                          "clientConnectInfoGet",
	"GET /api/v1/client/discovery":                                              "clientDiscoveryGet",
	"POST /api/v1/client/login/start":                                           "clientLoginStart",
	"POST /api/v1/client/login/poll":                                            "clientLoginPoll",
	"GET /api/v1/client/login/requests/{user_code}":                             "clientLoginRequestGet",
	"POST /api/v1/client/login/requests/{user_code}/approve":                    "clientLoginRequestApprove",
	"POST /api/v1/client/login/requests/{user_code}/deny":                       "clientLoginRequestDeny",
	"GET /api/v1/geo/status":                                                    "geoStatusGet",
	"GET /api/v1/mcp/status":                                                    "mcpStatusGet",
	"GET /api/v1/mcp/tools":                                                     "mcpToolsList",
	"GET /api/v1/upstream-credentials/slots":                                    "upstreamCredentialSlotsList",
	"GET /api/v1/services/{serviceID}/upstream-credential":                      "serviceUpstreamCredentialGet",
	"PUT /api/v1/services/{serviceID}/upstream-credential":                      "serviceUpstreamCredentialUpdate",
	"DELETE /api/v1/services/{serviceID}/upstream-credential":                   "serviceUpstreamCredentialDelete",
	"GET /api/v1/services/{serviceID}/domains":                                  "serviceDomainsList",
	"POST /api/v1/services/{serviceID}/domains":                                 "serviceDomainCreate",
	"GET /api/v1/services/{serviceID}/domains/{did}":                            "serviceDomainGet",
	"PUT /api/v1/services/{serviceID}/domains/{did}":                            "serviceDomainUpdate",
	"DELETE /api/v1/services/{serviceID}/domains/{did}":                         "serviceDomainDelete",
	"GET /api/v1/connection-logs":                                               "connectionLogsList",
	"GET /api/v1/connection-logs/rollups":                                       "connectionLogRollupsList",
	"GET /api/v1/connection-logs/export":                                        "connectionLogsExport",
	"GET /api/v1/events":                                                        "eventsStream",
	"GET /api/v1/openapi/viewer/":                                               "openapiViewerGet",
	"GET /api/v1/openapi/viewer/static/{file}":                                  "openapiViewerStaticGet",
	"GET /api/v1/ai/providers":                                                  "providersList",
	"POST /api/v1/ai/providers":                                                 "providerCreate",
	"GET /api/v1/ai/providers/{slug}":                                           "providerGet",
	"PUT /api/v1/ai/providers/{slug}":                                           "providerUpdate",
	"DELETE /api/v1/ai/providers/{slug}":                                        "providerDelete",
	"PUT /api/v1/ai/providers/{slug}/upstream":                                  "providerUpstreamUpdate",
	"GET /api/v1/mcp/sources":                                                   "mcpSourcesList",
	"POST /api/v1/mcp/sources":                                                  "mcpSourceCreate",
	"GET /api/v1/mcp/sources/{slug}":                                            "mcpSourceGet",
	"PUT /api/v1/mcp/sources/{slug}":                                            "mcpSourceUpdate",
	"DELETE /api/v1/mcp/sources/{slug}":                                         "mcpSourceDelete",
	"POST /api/v1/mcp/sources/{slug}/sync":                                      "mcpSourceSync",
	"GET /api/v1/mcp/sources/{slug}/tools":                                      "mcpSourceToolsList",
	"PUT /api/v1/mcp/sources/{slug}/tools/{name}":                               "mcpSourceToolUpdate",
	"POST /api/v1/mcp/sources/{slug}/tools/{name}/approve":                      "mcpSourceToolApprove",
	"POST /api/v1/mcp/sources/{slug}/approve-all":                               "mcpSourceApproveAll",
	"GET /api/v1/ai/providers/{slug}/models":                                    "providerModelsList",
	"POST /api/v1/ai/providers/{slug}/models":                                   "providerModelCreate",
	"DELETE /api/v1/ai/providers/{slug}/models":                                 "providerModelDelete",
	"POST /api/v1/ai/providers/{slug}/models/sync":                              "providerModelsSync",
	"GET /api/v1/ai/providers/{slug}/metrics":                                   "providerMetricsGet",
	"GET /api/v1/ai/gateway":                                                    "aiGatewayGet",
	"GET /api/v1/ai/models":                                                     "aiModelsList",
	"POST /api/v1/ai/models":                                                    "aiModelCreate",
	"GET /api/v1/ai/models/{name}":                                              "aiModelGet",
	"PUT /api/v1/ai/models/{name}":                                              "aiModelUpdate",
	"DELETE /api/v1/ai/models/{name}":                                           "aiModelDelete",
	"GET /api/v1/ai/keys":                                                       "gatewayKeysList",
	"POST /api/v1/ai/keys":                                                      "gatewayKeyCreate",
	"PUT /api/v1/ai/keys/{id}":                                                  "gatewayKeyUpdate",
	"DELETE /api/v1/ai/keys/{id}":                                               "gatewayKeyDelete",
	"GET /api/v1/ai/requests":                                                   "aiRequestsList",
	"GET /api/v1/ai/requests/{requestID}/attempts":                              "aiRequestAttemptsList",
	"GET /api/v1/database":                                                      "databaseGet",
	"GET /.well-known/oauth-authorization-server":                               "oauthMetadataGet",
	"GET /oauth/authorize":                                                      "oauthAuthorize",
	"POST /oauth/token":                                                         "oauthToken",
	"POST /oauth/revoke":                                                        "oauthRevoke",
	"GET /.well-known/oauth-protected-resource":                                 "oauthProtectedResourceGet",
	"GET /.well-known/oauth-protected-resource/mcp":                             "oauthProtectedResourceGatewayGet",
	"GET /.well-known/oauth-protected-resource/mcp/{slug}":                      "oauthProtectedResourceSourceGet",
	"GET /api/v1/oauth/requests/{id}":                                           "oauthRequestGet",
	"POST /api/v1/oauth/requests/{id}/approve":                                  "oauthRequestApprove",
	"POST /api/v1/oauth/requests/{id}/deny":                                     "oauthRequestDeny",
	"GET /api/v1/oauth/grants":                                                  "oauthGrantsList",
	"DELETE /api/v1/oauth/grants/{id}":                                          "oauthGrantDelete",
	"GET /api/v1/oauth/clients":                                                 "oauthClientsList",
	"POST /api/v1/oauth/clients":                                                "oauthClientCreate",
	"DELETE /api/v1/oauth/clients/{id}":                                         "oauthClientDelete",
}

// openAPIReservedOperationIDs are ids that belong to operations other plans
// add, with the path they belong to. They are not rows of the table above.
var openAPIReservedOperationIDs = map[string]string{
	// plan mcp-server: the management MCP endpoint, three operations
	// (POST mcpManagement, GET mcpManagementGet, DELETE mcpManagementDelete).
	// An id is reserved by prefix, so the one entry covers all three.
	"mcpManagement": "/mcp/burrow",
	// plan mcp-gateway: one tool source behind gateway keys, three
	// operations (mcpToolSource, mcpToolSourceGet, mcpToolSourceDelete).
	"mcpToolSource": "/mcp/{slug}",
	// the same plan: every tool source behind /mcp (mcpAggregated,
	// mcpAggregatedGet, mcpAggregatedDelete).
	"mcpAggregated": "/mcp",
}

// openAPIMCPDisabled is the complete set of management-API operations that
// carry x-burrow-mcp: false, as operationId → "METHOD path". They must never
// become agent tools; the reason of each group is in docs/reference/api.md.
var openAPIMCPDisabled = map[string]string{
	"authLogin":          "POST /api/v1/auth/login",
	"authLogout":         "POST /api/v1/auth/logout",
	"authReauth":         "POST /api/v1/auth/reauth",
	"authChangePassword": "POST /api/v1/auth/change-password",
	// An agent must not approve tool text that may have been written to steer
	// it, nor rename or reclassify tools.
	"mcpSourceToolApprove": "POST /api/v1/mcp/sources/{slug}/tools/{name}/approve",
	"mcpSourceApproveAll":  "POST /api/v1/mcp/sources/{slug}/approve-all",
	"mcpSourceToolUpdate":  "PUT /api/v1/mcp/sources/{slug}/tools/{name}",
	// Nor point a source, and its credential, at another host, nor read the
	// tool text that could steer it into doing so.
	"mcpSourceCreate":    "POST /api/v1/mcp/sources",
	"mcpSourceUpdate":    "PUT /api/v1/mcp/sources/{slug}",
	"mcpSourceToolsList": "GET /api/v1/mcp/sources/{slug}/tools",
	// An agent steered by untrusted text must not re-route credentials or
	// switch off inspection: no provider, upstream credential, SMTP or
	// custom-domain write, no outbound webhook, and no change to guardrails,
	// redaction, the inspector, retention or gateway-only.
	"providerCreate":                  "POST /api/v1/ai/providers",
	"providerUpdate":                  "PUT /api/v1/ai/providers/{slug}",
	"providerUpstreamUpdate":          "PUT /api/v1/ai/providers/{slug}/upstream",
	"serviceUpstreamCredentialUpdate": "PUT /api/v1/services/{serviceID}/upstream-credential",
	"serviceUpstreamCredentialDelete": "DELETE /api/v1/services/{serviceID}/upstream-credential",
	"settingsUpdate":                  "PUT /api/v1/settings",
	"serviceDomainCreate":             "POST /api/v1/services/{serviceID}/domains",
	"serviceDomainUpdate":             "PUT /api/v1/services/{serviceID}/domains/{did}",
	"webhookCreate":                   "POST /api/v1/webhooks",
	"webhookUpdate":                   "PUT /api/v1/webhooks/{id}",
	"webhookDelete":                   "DELETE /api/v1/webhooks/{id}",
	"webhookTest":                     "POST /api/v1/webhooks/{id}/test",
	"webhookPause":                    "POST /api/v1/webhooks/{id}/pause",
	"webhookResume":                   "POST /api/v1/webhooks/{id}/resume",
	"guardrailSettingsUpdate":         "PUT /api/v1/guardrails/settings",
	"redactionSettingsUpdate":         "PUT /api/v1/redaction/settings",
	"redactionRuleCreate":             "POST /api/v1/redaction/rules",
	"redactionRuleUpdate":             "PUT /api/v1/redaction/rules/{id}",
	"redactionRuleDelete":             "DELETE /api/v1/redaction/rules/{id}",
	"serviceAiConfigUpdate":           "PUT /api/v1/services/{serviceID}/ai-config",
	"retentionSettingsUpdate":         "PUT /api/v1/settings/retention",
	"serviceGatewayOnlyUpdate":        "PUT /api/v1/services/{serviceID}/gateway-only",
	// Nor let an address reach a tcp service (an agent's own, say).
	"serviceIpGrantsList":  "GET /api/v1/services/{serviceID}/ip-grants",
	"serviceIpGrantCreate": "POST /api/v1/services/{serviceID}/ip-grants",
	"serviceIpGrantDelete": "DELETE /api/v1/services/{serviceID}/ip-grants/{grantID}",
	// Nor cut off the people who use a tcp service.
	"serviceConnectionDelete": "DELETE /api/v1/services/{serviceID}/connections/{connID}",
	// Nor change who may reach a service: its ip rules and its role list.
	"serviceIpGeoUpdate":        "PUT /api/v1/services/{serviceID}/ip-geo",
	"serviceAccessPolicyUpdate": "PUT /api/v1/services/{serviceID}/access-policy",
	// Nor widen access, grant privileges, put a data-plane secret into the
	// model's context or plant an instruction in other users' requests.
	"serviceAccessModeUpdate": "PUT /api/v1/services/{serviceID}/access-mode",
	"tunnelAccessModeUpdate":  "PUT /api/v1/tunnels/{id}/access-mode",
	"gatewayKeyUpdate":        "PUT /api/v1/ai/keys/{id}",
	"clientTokenCreate":       "POST /api/v1/tokens",
	"serviceApiKeyCreate":     "POST /api/v1/services/{serviceID}/api-keys",
	"userCreate":              "POST /api/v1/users",
	"userUpdate":              "PATCH /api/v1/users/{id}",
	"userDelete":              "DELETE /api/v1/users/{id}",
	"roleCreate":              "POST /api/v1/roles",
	"roleUpdate":              "PUT /api/v1/roles/{name}",
	"roleDelete":              "DELETE /api/v1/roles/{name}",
	"aiModelCreate":           "POST /api/v1/ai/models",
	"aiModelUpdate":           "PUT /api/v1/ai/models/{name}",
	// Nor create a service, whose access mode it would set.
	"serviceCreate": "POST /api/v1/services",
	// Nor register where authorization codes are sent.
	"oauthClientCreate":         "POST /api/v1/oauth/clients",
	"clientDiscoveryGet":        "GET /api/v1/client/discovery",
	"clientLoginStart":          "POST /api/v1/client/login/start",
	"clientLoginPoll":           "POST /api/v1/client/login/poll",
	"clientLoginRequestGet":     "GET /api/v1/client/login/requests/{user_code}",
	"clientLoginRequestApprove": "POST /api/v1/client/login/requests/{user_code}/approve",
	"clientLoginRequestDeny":    "POST /api/v1/client/login/requests/{user_code}/deny",
	"backupDownload":            "GET /api/v1/backups/{id}/download",
	"auditExport":               "GET /api/v1/audit/export",
	"costExport":                "GET /api/v1/cost/export",
	"connectionLogsExport":      "GET /api/v1/connection-logs/export",
	"eventsStream":              "GET /api/v1/events",
	"inspectorStream":           "GET /api/v1/services/{serviceID}/inspector/stream",
	"backupRestore":             "POST /api/v1/backups/restore",
	"backupRestoreGet":          "GET /api/v1/backups/restores/{id}",
	"openapiViewerGet":          "GET /api/v1/openapi/viewer/",
	"openapiViewerStaticGet":    "GET /api/v1/openapi/viewer/static/{file}",
	// An agent must not consent in a person's name, nor read what it would
	// consent to.
	"oauthRequestGet":     "GET /api/v1/oauth/requests/{id}",
	"oauthRequestApprove": "POST /api/v1/oauth/requests/{id}/approve",
	"oauthRequestDeny":    "POST /api/v1/oauth/requests/{id}/deny",
}

// openAPINonJSONBodies are the request bodies that are not application/json,
// as "METHOD path" → media type.
var openAPINonJSONBodies = map[string]string{
	"POST /api/v1/backups/restore": "multipart/form-data",
}

// openAPIEmptyAllowed lists, by operationId, the operations that may
// reference the Empty schema because their handler really answers {}, each
// with the place in the handler that does. No handler does today.
var openAPIEmptyAllowed = map[string]string{}

func oaType[T any]() reflect.Type { return reflect.TypeOf((*T)(nil)).Elem() }

// openAPIRequestBody says what the handler of an operation decodes its JSON
// body into. Exactly one of the three is set.
type openAPIRequestBody struct {
	typ    reflect.Type // the handler's own struct
	keys   []string     // the handler's own closed key set (a map body)
	except string       // why there is no type to name
}

// openAPIRequestBodies has a row for every operation of the management API
// with a JSON request body, by operationId.
var openAPIRequestBodies = map[string]openAPIRequestBody{
	// auth, sessions, tokens, clients, users, roles (O03)
	"authLogin":                 {typ: oaType[loginReq]()},
	"oauthRequestApprove":       {typ: oaType[oauthApproveReq]()},
	"oauthClientCreate":         {typ: oaType[postOAuthClientReq]()},
	"authReauth":                {typ: oaType[reauthReq]()},
	"authChangePassword":        {typ: oaType[changePasswordReq]()},
	"clientTokenCreate":         {typ: oaType[createTokenReq]()},
	"clientLoginStart":          {typ: oaType[clientLoginStartReq]()},
	"clientLoginPoll":           {typ: oaType[clientLoginPollReq]()},
	"clientLoginRequestApprove": {typ: oaType[clientLoginApproveReq]()},
	"userCreate":                {typ: oaType[createUserReq]()},
	"userUpdate":                {typ: oaType[updateUserReq]()},
	"roleCreate":                {typ: oaType[createRoleReq]()},
	"roleUpdate":                {typ: oaType[updateRoleReq]()},
	"automationTokenCreate":     {typ: oaType[postAutomationTokenReq]()},

	// services, tunnels, domains, upstream credentials (O01, O02)
	"serviceCreate":                   {typ: oaType[postServiceReq]()},
	"tunnelAccessModeUpdate":          {typ: oaType[accessModeReq]()},
	"serviceAccessModeUpdate":         {typ: oaType[setAccessModeReq]()},
	"serviceSlugUpdate":               {typ: oaType[setSlugReq]()},
	"serviceGatewayOnlyUpdate":        {typ: oaType[setGatewayOnlyReq]()},
	"serviceApiKeyCreate":             {typ: oaType[createAPIKeyReq]()},
	"serviceAccessPolicyUpdate":       {typ: oaType[setAccessPolicyReq]()},
	"serviceIpGeoUpdate":              {typ: oaType[ipGeoReq]()},
	"serviceIpGrantCreate":            {typ: oaType[ipGrantReq]()},
	"serviceMaxConnectionsUpdate":     {typ: oaType[maxConnectionsReq]()},
	"serviceTcpSettingsUpdate":        {typ: oaType[tcpSettingsReq]()},
	"serviceUpstreamCredentialUpdate": {typ: oaType[putCredReq]()},
	"serviceDomainCreate":             {typ: oaType[postDomainReq]()},
	"serviceDomainUpdate":             {typ: oaType[putDomainReq]()},
	// PutServiceAIConfig (semantic_handlers.go) stores the body as a raw
	// JSON blob; it is decoded later, section by section, into inline
	// structs in cmd/server/v04_loader.go.
	"serviceAiConfigUpdate": {except: "PutServiceAIConfig keeps the body as a raw JSON blob (map[string]json.RawMessage)"},

	// AI providers, models, gateway keys (O04)
	"providerCreate":         {typ: oaType[postProviderReq]()},
	"providerUpdate":         {typ: oaType[putProviderReq]()},
	"providerUpstreamUpdate": {typ: oaType[upstreamReq]()},
	"mcpSourceCreate":        {typ: oaType[postMCPSourceReq]()},
	"mcpSourceUpdate":        {typ: oaType[putMCPSourceReq]()},
	"mcpSourceToolUpdate":    {typ: oaType[putMCPToolReq]()},
	"mcpSourceToolApprove":   {typ: oaType[approveMCPToolReq]()},
	"mcpSourceApproveAll":    {typ: oaType[approveAllMCPToolsReq]()},
	// PostAIProviderModel (ai_provider_handlers.go) decodes into
	// `var in struct { ID string `json:"id"` }`.
	// TestOpenAPI_ProviderModelBody probes the handler instead.
	"providerModelCreate": {except: "PostAIProviderModel decodes into an inline struct (var in struct{ ID })"},
	"aiModelCreate":       {typ: oaType[aiModelReq]()},
	"aiModelUpdate":       {typ: oaType[aiModelReq]()},
	"gatewayKeyCreate":    {typ: oaType[postAIKeyReq]()},
	"gatewayKeyUpdate":    {typ: oaType[putAIKeyReq]()},

	// cache, rate limits, cost, budgets (O05)
	// PutCacheSettings (cache_handlers.go) hands the body to
	// exact.SettingsFromJSON, which decodes into an inline struct.
	// TestOpenAPI_CacheSettingsBody probes that function instead.
	"cacheSettingsUpdate": {except: "PutCacheSettings decodes through exact.SettingsFromJSON into an inline struct of package exact"},
	"rateLimitCreate":     {typ: oaType[rateLimitReq]()},
	"rateLimitUpdate":     {typ: oaType[rateLimitReq]()},
	"costPricingUpdate":   {typ: oaType[pricingResp]()},
	"budgetCreate":        {typ: oaType[budgetReq]()},
	"budgetUpdate":        {typ: oaType[budgetReq]()},

	// redaction, guardrails, inspector (O06)
	"redactionRuleCreate":           {typ: oaType[redactionRuleReq]()},
	"redactionRuleUpdate":           {typ: oaType[redactionRuleReq]()},
	"redactionSettingsUpdate":       {typ: oaType[redactionGlobalJSON]()},
	"redactionPreview":              {typ: oaType[redactionPreviewReq]()},
	"guardrailSettingsUpdate":       {typ: oaType[guardrails.Settings]()},
	"guardrailPreview":              {typ: oaType[guardrailPreviewReq]()},
	"inspectorRequestReplay":        {typ: oaType[replayReq]()},
	"inspectorRequestReplayCompare": {typ: oaType[replayReq]()},

	// audit, webhooks (O07)
	"auditVerify":    {typ: oaType[auditVerifyReq]()},
	"webhookCreate":  {typ: oaType[webhookReq]()},
	"webhookUpdate":  {typ: oaType[webhookReq]()},
	"webhookPreview": {typ: oaType[previewReq]()},

	// settings (O08)
	// SaveSettings (settings_handlers.go) decodes into map[string]string and
	// keeps the keys of allowedSettingKeys.
	"settingsUpdate":          {keys: oaKeys(allowedSettingKeys)},
	"settingsTestEmail":       {typ: oaType[testEmailReq]()},
	"retentionSettingsUpdate": {typ: oaType[retentionSettingsPutReq]()},
}

// openAPIResponseBody says what the handler of an operation writes as its
// JSON success response.
type openAPIResponseBody struct {
	status   string       // the success status the handler answers with
	also     []string     // other 2xx statuses with a JSON body of their own
	typ      reflect.Type // the handler's own struct (the element type of a list)
	list     bool         // the handler writes a JSON array
	keys     []string     // the handler's own closed key set (a map answer): pins the document's property names only, not their types
	optional []string     // fields left out of some answers although always tagged
	except   string       // why there is no type to name
}

// openAPIResponses has a row for every operation of the management API with
// a JSON success response, by operationId.
var openAPIResponses = map[string]openAPIResponseBody{
	// auth, sessions, tokens, clients, users, roles (O03)
	"authLogin":            {status: "200", typ: oaType[userResp]()},
	"oauthRequestGet":      {status: "200", typ: oaType[oauthRequestResp]()},
	"oauthRequestApprove":  {status: "200", typ: oaType[oauthRedirectResp]()},
	"oauthRequestDeny":     {status: "200", typ: oaType[oauthRedirectResp]()},
	"oauthGrantsList":      {status: "200", typ: oaType[oauthGrantResp](), list: true},
	"oauthClientsList":     {status: "200", typ: oaType[oauthClientResp](), list: true},
	"oauthClientCreate":    {status: "201", typ: oaType[oauthClientResp]()},
	"authReauth":           {status: "200", typ: oaType[reauthResp]()},
	"meGet":                {status: "200", typ: oaType[meResp]()},
	"sessionsList":         {status: "200", typ: oaType[sessionResp](), list: true},
	"sessionsRevokeAll":    {status: "200", keys: []string{"revoked"}},
	"clientTokensList":     {status: "200", typ: oaType[tokenResp](), list: true},
	"clientTokenCreate":    {status: "201", typ: oaType[newTokenResp]()},
	"clientsList":          {status: "200", typ: oaType[ClientView](), list: true},
	"knownClientsList":     {status: "200", typ: oaType[KnownClient](), list: true},
	"clientGet":            {status: "200", typ: oaType[ClientDetail]()},
	"clientConnectInfoGet": {status: "200", typ: oaType[ConnectInfo]()},
	"clientDiscoveryGet":   {status: "200", typ: oaType[ClientDiscovery]()},
	"clientLoginStart":     {status: "200", typ: oaType[clientLoginStartResp]()},
	// 202 {"status": "pending"} while the request is undecided.
	"clientLoginPoll":           {status: "200", also: []string{"202"}, typ: oaType[clientLoginPollResp]()},
	"clientLoginRequestGet":     {status: "200", typ: oaType[clientLoginRequestResp]()},
	"clientLoginRequestApprove": {status: "200", typ: oaType[clientLoginRequestResp]()},
	"clientLoginRequestDeny":    {status: "200", typ: oaType[clientLoginRequestResp]()},
	"usersList":                 {status: "200", typ: oaType[usersPageResp]()},
	"userCreate":                {status: "201", typ: oaType[userAdminResp]()},
	"rolesList":                 {status: "200", typ: oaType[roleResp](), list: true},
	"roleCreate":                {status: "201", typ: oaType[roleDetailResp]()},
	"rolePermissionsList":       {status: "200", typ: oaType[permissionInfo](), list: true},
	"roleGet":                   {status: "200", typ: oaType[roleDetailResp]()},
	"automationPresetsList":     {status: "200", typ: oaType[automationPresetResp](), list: true},
	"automationTokensList":      {status: "200", typ: oaType[automationTokenResp](), list: true},
	"automationTokenCreate":     {status: "201", typ: oaType[createTokenWrap]()},

	// services, tunnels, domains, upstream credentials, geo (O01, O02)
	"tunnelsList":              {status: "200", typ: oaType[TunnelView](), list: true},
	"servicesList":             {status: "200", typ: oaType[serviceResp](), list: true},
	"serviceCreate":            {status: "201", keys: []string{"id", "slug", "created_at"}},
	"serviceSlugSuggestionGet": {status: "200", keys: []string{"slug"}},
	"serviceGet":               {status: "200", typ: oaType[serviceDetailResp]()},
	"serviceSlugUpdate":        {status: "200", keys: []string{"slug", "url"}},
	"serviceGatewayOnlyUpdate": {status: "200", keys: []string{"gateway_only"}},
	"serviceApiKeysList":       {status: "200", typ: oaType[apiKeyResp](), list: true},
	"serviceApiKeyCreate":      {status: "201", typ: oaType[createAPIKeyResp]()},
	"serviceAccessPolicyGet":   {status: "200", typ: oaType[accessPolicyResp]()},
	"serviceIpGeoGet":          {status: "200", typ: oaType[ipGeoResp]()},
	"serviceIpGrantsList":      {status: "200", typ: oaType[ipGrantResp](), list: true},
	"serviceConnectionsList":   {status: "200", typ: oaType[serviceConnectionResp](), list: true},
	"serviceTcpSettingsGet":    {status: "200", typ: oaType[tcpSettingsResp]()},
	"serviceTcpSettingsUpdate": {status: "200", typ: oaType[tcpSettingsResp]()},
	"serviceIpGrantCreate":     {status: "201", typ: oaType[ipGrantCreatedResp]()},
	// The one answer that stays an exception: the handler writes the stored
	// blob, a JSON object with no closed key set and no type to name. The nine
	// other map-literal answers are pinned by keys, so that at least the
	// document's property names cannot drift.
	"serviceAiConfigGet":          {status: "200", except: "GetServiceAIConfig writes the stored JSON blob as it is"},
	"geoStatusGet":                {status: "200", typ: oaType[geoStatusResp]()},
	"upstreamCredentialSlotsList": {status: "200", typ: oaType[slotsResp]()},
	// Bound: credBindingResp. Unbound: credUnboundResp, which has slot_present only.
	"serviceUpstreamCredentialGet": {status: "200", typ: oaType[credBindingResp](), optional: []string{"slot", "header_name", "header_format"}},
	"serviceDomainsList":           {status: "200", typ: oaType[domainResp](), list: true},
	"serviceDomainCreate":          {status: "201", typ: oaType[domainResp]()},
	"serviceDomainGet":             {status: "200", typ: oaType[domainResp]()},

	// AI providers, models, gateway keys, attempts (O04)
	"providersList":          {status: "200", typ: oaType[aiProviderResp](), list: true},
	"providerCreate":         {status: "201", typ: oaType[aiProviderResp]()},
	"providerGet":            {status: "200", typ: oaType[aiProviderResp]()},
	"providerUpdate":         {status: "200", typ: oaType[aiProviderResp]()},
	"providerUpstreamUpdate": {status: "200", typ: oaType[aiProviderResp]()},
	"mcpSourcesList":         {status: "200", typ: oaType[mcpSourceResp](), list: true},
	"mcpSourceCreate":        {status: "201", typ: oaType[mcpSourceResp]()},
	"mcpSourceGet":           {status: "200", typ: oaType[mcpSourceResp]()},
	"mcpSourceUpdate":        {status: "200", typ: oaType[mcpSourceResp]()},
	"mcpSourceSync":          {status: "200", typ: oaType[mcpgw.SyncResult]()},
	"mcpSourceToolsList":     {status: "200", typ: oaType[mcpSourceToolResp](), list: true},
	"mcpSourceToolUpdate":    {status: "200", typ: oaType[putMCPToolResp]()},
	"mcpSourceToolApprove":   {status: "200", typ: oaType[mcpSourceToolResp]()},
	"mcpSourceApproveAll":    {status: "200", typ: oaType[mcpApproveAllResp]()},
	"providerModelsList":     {status: "200", typ: oaType[aiProviderModelResp](), list: true},
	"providerModelsSync":     {status: "200", keys: []string{"count"}},
	"providerMetricsGet":     {status: "200", typ: oaType[endpointMetricsResp]()},
	"aiGatewayGet":           {status: "200", keys: []string{"endpoints"}},
	"aiModelsList":           {status: "200", typ: oaType[aiModelResp](), list: true},
	"aiModelCreate":          {status: "201", typ: oaType[aiModelResp]()},
	"aiModelGet":             {status: "200", typ: oaType[aiModelResp]()},
	"aiModelUpdate":          {status: "200", typ: oaType[aiModelResp]()},
	"gatewayKeysList":        {status: "200", typ: oaType[aiKeyResp](), list: true},
	"gatewayKeyCreate":       {status: "201", typ: oaType[aiKeyCreatedResp]()},
	"gatewayKeyUpdate":       {status: "200", typ: oaType[aiKeyResp]()},
	"aiRequestAttemptsList":  {status: "200", typ: oaType[aiAttemptResp](), list: true},
	"aiRequestsList":         {status: "200", typ: oaType[inspectorEntryJSON](), list: true},

	// cache, rate limits, cost, budgets (O05)
	"cacheSettingsGet":  {status: "200", typ: oaType[cacheSettingsResp]()},
	"cacheStatsGet":     {status: "200", typ: oaType[cacheStatsResp]()},
	"rateLimitsList":    {status: "200", typ: oaType[rateLimitResp](), list: true},
	"rateLimitCreate":   {status: "201", typ: oaType[rateLimitResp]()},
	"rateLimitUsageGet": {status: "200", typ: oaType[usageResp]()},
	"costPricingGet":    {status: "200", typ: oaType[pricingResp]()},
	// Without group_by the handler writes cost.Summary; with it,
	// groupedSummaryResp, which adds the two fields.
	"costSummaryGet": {status: "200", typ: oaType[groupedSummaryResp](), optional: []string{"group_by", "groups"}},
	"budgetsList":    {status: "200", typ: oaType[budgetResp](), list: true},
	"budgetCreate":   {status: "201", typ: oaType[budgetResp]()},

	// redaction, guardrails, inspector (O06)
	"redactionRulesList":            {status: "200", typ: oaType[redactionRulesResp]()},
	"redactionRuleCreate":           {status: "201", typ: oaType[redact.Rule]()},
	"redactionSettingsGet":          {status: "200", typ: oaType[redactionSettingsResp]()},
	"redactionPreview":              {status: "200", typ: oaType[redactionPreviewResp]()},
	"guardrailSettingsGet":          {status: "200", typ: oaType[guardrailSettingsResp]()},
	"guardrailPatternsList":         {status: "200", typ: oaType[guardrailPatternResp](), list: true},
	"guardrailPreview":              {status: "200", typ: oaType[guardrailPreviewResp]()},
	"inspectorRequestsList":         {status: "200", typ: oaType[inspectorEntryJSON](), list: true},
	"inspectorRequestGet":           {status: "200", typ: oaType[inspectorEntryJSON]()},
	"inspectorRequestReplay":        {status: "200", keys: []string{"new_entry"}},
	"inspectorRequestReplayCompare": {status: "200", keys: []string{"original", "replayed", "diff"}},

	// audit, webhooks, connection logs (O07)
	"auditEventsList":          {status: "200", typ: oaType[auditEventResp](), list: true},
	"auditFingerprintGet":      {status: "200", typ: oaType[auditFingerprintResp]()},
	"auditVerify":              {status: "200", typ: oaType[auditVerifyResp]()},
	"webhooksList":             {status: "200", typ: oaType[webhookResp](), list: true},
	"webhookCreate":            {status: "201", typ: oaType[webhookResp]()},
	"webhookDeliveriesList":    {status: "200", typ: oaType[deliveryResp](), list: true},
	"webhookPreview":           {status: "200", typ: oaType[previewResp]()},
	"connectionLogsList":       {status: "200", typ: oaType[connectionLogResp](), list: true},
	"connectionLogRollupsList": {status: "200", typ: oaType[connectionLogRollupResp](), list: true},

	// settings, backups, MCP status, database (O08)
	// GetSettings (settings_handlers.go) writes a map with the keys of
	// allowedSettingKeys that have a value.
	// Plus services_domain, which it adds from the environment.
	"settingsGet":          {status: "200", keys: append(oaKeys(allowedSettingKeys), "services_domain")},
	"retentionSettingsGet": {status: "200", typ: oaType[retentionSettingsResp]()},
	"backupsList":          {status: "200", typ: oaType[backupRow](), list: true},
	"backupCreate":         {status: "202", typ: oaType[startBackupResp]()},
	"backupVerify":         {status: "200", typ: oaType[verifyResp]()},
	"backupRestore":        {status: "202", typ: oaType[restoreStartResp]()},
	"backupRestoreGet":     {status: "200", typ: oaType[restoreStatusResp]()},
	"mcpStatusGet":         {status: "200", typ: oaType[mcpStatusResp]()},
	"mcpToolsList":         {status: "200", typ: oaType[mcpToolResp](), list: true},
	"databaseGet":          {status: "200", typ: oaType[dbStatusResp]()},
}

// openAPILenientSchemas are component schemas whose `required` list is not
// compared with the response type, each with the reason. Property names and
// nullability are compared all the same.
var openAPILenientSchemas = map[string]string{
	"AIModel":       "also the request body of aiModelCreate and aiModelUpdate, where most fields are optional",
	"AIModelTarget": "also part of that request body, where dialect is optional and available is not accepted",
}

// openAPIRequiredProbe checks a `required` list against the handler's own
// validation function: valid is a body the function accepts, with every
// required field and any optional ones worth probing.
type openAPIRequiredProbe struct {
	valid    string
	validate func(body []byte) string // "" when accepted, else the 400 message
}

func oaProbeRateLimit(body []byte) string {
	var in rateLimitReq
	if err := json.Unmarshal(body, &in); err != nil {
		return "invalid JSON"
	}
	return validateRateLimit(in)
}

func oaProbeBudget(body []byte) string {
	var in budgetReq
	if err := json.Unmarshal(body, &in); err != nil {
		return "invalid JSON"
	}
	return validateBudget(in)
}

func oaProbeWebhook(body []byte) string {
	var in webhookReq
	if err := json.Unmarshal(body, &in); err != nil {
		return "invalid JSON"
	}
	return validateWebhookReq(in)
}

func oaProbeRedactionRule(body []byte) string {
	var in redactionRuleReq
	if err := json.Unmarshal(body, &in); err != nil {
		return "invalid JSON"
	}
	if _, err := validateRuleReq(in); err != nil {
		return err.Error()
	}
	return ""
}

func oaProbeGuardrails(body []byte) string {
	var in guardrails.Settings
	if err := json.Unmarshal(body, &in); err != nil {
		return "invalid JSON"
	}
	if !guardrails.ValidActions[in.Action] {
		return "invalid action"
	}
	return ""
}

func oaProbeRoleCreate(body []byte) string {
	var in createRoleReq
	if err := json.Unmarshal(body, &in); err != nil {
		return "invalid JSON"
	}
	if !isValidRoleName(in.Name) {
		return "name must be a slug"
	}
	return ""
}

// openAPIRequiredProbes, by operationId.
var openAPIRequiredProbes = map[string]openAPIRequiredProbe{
	"roleCreate":              {valid: `{"name":"curator","description":"d","permissions":[],"default_for_new_users":false}`, validate: oaProbeRoleCreate},
	"rateLimitCreate":         {valid: `{"scope":"global","dimension":"rpm","limit":10,"burst":10,"window":"day"}`, validate: oaProbeRateLimit},
	"rateLimitUpdate":         {valid: `{"scope":"global","dimension":"rpm","limit":10,"burst":10,"window":"day"}`, validate: oaProbeRateLimit},
	"budgetCreate":            {valid: `{"scope":"global","action_on_exceed":"alert_webhook","daily_usd":1,"daily_tokens":1}`, validate: oaProbeBudget},
	"budgetUpdate":            {valid: `{"scope":"global","action_on_exceed":"alert_webhook","daily_usd":1,"daily_tokens":1}`, validate: oaProbeBudget},
	"redactionRuleCreate":     {valid: `{"name":"n","pattern":"a+","action":"mask","scope":"both"}`, validate: oaProbeRedactionRule},
	"redactionRuleUpdate":     {valid: `{"name":"n","pattern":"a+","action":"mask","scope":"both"}`, validate: oaProbeRedactionRule},
	"guardrailSettingsUpdate": {valid: `{"enabled":true,"action":"log_only"}`, validate: oaProbeGuardrails},
	"webhookCreate":           {valid: `{"name":"n","url":"https://example.com/hook","events":["webhook.test"],"paused":false}`, validate: oaProbeWebhook},
	"webhookUpdate":           {valid: `{"name":"n","url":"https://example.com/hook","events":["webhook.test"],"paused":false}`, validate: oaProbeWebhook},
}
