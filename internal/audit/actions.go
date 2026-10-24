package audit

// Action is one of Burrow's closed set of audit action strings (spec Part G.4).
//
// New audit call sites MUST use one of these constants — the verifier and
// admin UI key off them. Adding a new auditable mutation means:
//
//   1. Add a constant here.
//   2. Add the constant to AllActions.
//   3. Add (or extend) a helper in sites.go.
//   4. Call the helper from the store/handler that performs the mutation.
//
// String values match the spec verbatim (dot.separated, lowercase). The
// verbatim spec values are part of the wire contract: the admin UI filter
// drop-down and the export NDJSON consumers depend on them.

const (
	ActionUserCreate          = "user.create"
	ActionUserUpdate          = "user.update"
	ActionUserDelete          = "user.delete"
	ActionUserSuspend         = "user.suspend"
	ActionUserReactivate      = "user.reactivate"
	ActionRoleCreate          = "role.create"
	ActionRoleUpdate          = "role.update"
	ActionRoleDelete          = "role.delete"
	ActionTokenMint           = "token.mint"
	ActionTokenRevoke         = "token.revoke"
	ActionSessionCreate       = "session.create"
	ActionSessionDelete       = "session.delete"
	ActionSessionRevokeOthers = "session.revoke_others"

	ActionServiceCreate                = "service.create"
	ActionServiceDelete                = "service.delete"
	ActionServiceAccessModeUpdate      = "service.access_mode.update"
	ActionServiceSlugChange            = "service.slug_changed"
	ActionServiceGatewayOnlyUpdate     = "service.gateway_only.update"
	ActionServiceTCPPortReclaimed      = "service.tcp_port.reclaimed" // a tcp registration took an offline service's automatic port
	ActionServiceTCPAccessUpdate       = "service.tcp_access.update"  // the access mode of a tcp service: open, ip, private
	ActionServiceIPGrantAdd            = "service.ip_grant.add"       // a just-in-time grant of a tcp service in mode ip
	ActionServiceIPGrantRevoke         = "service.ip_grant.revoke"
	ActionServiceMaxConnectionsUpdate  = "service.max_connections.update"   // the cap on connected visitors of a tcp service
	ActionServiceTCPSettingsUpdate     = "service.tcp_settings.update"      // the PROXY protocol or idle timeout of a tcp service
	ActionServiceHTTPSettingsUpdate    = "service.http_settings.update"     // the upstream protocol, TLS verification, CA or gRPC cap of an http service
	ActionServiceTCPHostnameUpdate     = "service.tcp_hostname.update"      // the TLS passthrough host name of a tcp service
	ActionServiceTCPHostnameOnlyUpdate = "service.tcp_hostname_only.update" // a tcp service reached by host name only (no public port), or by port again
	ActionServiceConnectionClose       = "service.connection.close"         // a live connection of a tcp service was closed from the dashboard
	ActionTCPForwardOpen               = "tcp.forward.open"                 // a forward stream to a tcp service was opened (one per user and service a minute)
	ActionServiceTCPLimitsUpdate       = "service.tcp_limits.update"        // the bandwidth, daily volume or automatic bans of a tcp service
	ActionTCPBanAdd                    = "tcp.ban.add"                      // a source was banned from a tcp service (by the relay or by hand)
	ActionTCPBanRemove                 = "tcp.ban.remove"                   // a ban of a tcp service was lifted by hand
	ActionServiceAccessPolicyUpdate    = "service.access_policy.update"
	ActionServiceAPIKeyCreate          = "service.api_key.create"
	ActionServiceAPIKeyRevoke          = "service.api_key.revoke"
	ActionServiceAIConfigUpdate        = "service.ai_config.update"
	ActionAIProviderCreate             = "ai_provider.create"
	ActionAIProviderUpdate             = "ai_provider.update"
	ActionAIProviderDelete             = "ai_provider.delete"
	ActionAIProviderModelsSync         = "ai_provider.models_sync"
	ActionAIProviderModelAdd           = "ai_provider.model_add"
	ActionAIProviderModelRemove        = "ai_provider.model_remove"
	ActionAIModelCreate                = "ai_model.create"
	ActionAIModelUpdate                = "ai_model.update"
	ActionAIModelDelete                = "ai_model.delete"
	ActionAIGatewayKeyCreate           = "ai_gateway_key.create"
	ActionAIGatewayKeyRevoke           = "ai_gateway_key.revoke"
	ActionAIGatewayKeyUpdate           = "ai_gateway_key.update"

	ActionCacheClear          = "cache.clear"
	ActionCacheSettingsUpdate = "cache.settings.update"

	ActionRedactionRuleCreate = "redaction.rule.create"
	ActionRedactionRuleUpdate = "redaction.rule.update"
	ActionRedactionRuleDelete = "redaction.rule.delete"
	// ActionRedactionApplied is aggregated 1/hour per (subject_id, action) —
	// see Logger.Append's sample-rate gate.
	ActionRedactionApplied = "redaction.applied"

	ActionGuardrailSettingsUpdate = "guardrail.settings.update"
	// ActionGuardrailRefused is aggregated 1/hour per (subject_id, action),
	// or per Event.AggregationKey when the caller sets one (the gateway
	// does: service, pattern, action and gateway key).
	ActionGuardrailRefused = "guardrail.refused"
	// ActionGuardrailFlagged is written when an answer pattern matched an
	// answer that was delivered (answer action "flag"); a withheld answer is
	// a guardrail.refused with direction "answer". Aggregated like
	// guardrail.refused.
	ActionGuardrailFlagged = "guardrail.flagged"
	// ActionGuardrailJudgeFailed is written when the judge model could not
	// judge a request or an answer (a timeout, an error, an answer that is no
	// verdict, no judge): the payload names the class and what on_failure
	// did, never the judged text. Aggregated like guardrail.refused.
	ActionGuardrailJudgeFailed = "guardrail.judge_failed"

	ActionBudgetCreate   = "budget.create"
	ActionBudgetUpdate   = "budget.update"
	ActionBudgetDelete   = "budget.delete"
	ActionBudgetExceeded = "budget.exceeded"

	ActionRateLimitCreate   = "ratelimit.create"
	ActionRateLimitUpdate   = "ratelimit.update"
	ActionRateLimitDelete   = "ratelimit.delete"
	ActionRateLimitEnforced = "ratelimit.enforced"

	ActionWebhookCreate         = "webhook.create"
	ActionWebhookUpdate         = "webhook.update"
	ActionWebhookDelete         = "webhook.delete"
	ActionWebhookDeliveryFailed = "webhook.delivery.failed"

	ActionBackupRun     = "backup.run"
	ActionBackupRestore = "backup.restore"
	ActionAuditExport   = "audit.export"

	ActionMtlsCAUpdate    = "mtls.ca.update"
	ActionMtlsCertRotated = "mtls.cert.rotated"
	ActionIPGeoUpdate     = "ipgeo.update"

	ActionAutomationTokenMint   = "automation.token.mint"
	ActionAutomationTokenRevoke = "automation.token.revoke"

	ActionMCPToolCall = "mcp.tool.call"

	// MCP gateway tool sources.
	ActionMCPSourceCreate = "mcp.source.create"
	ActionMCPSourceUpdate = "mcp.source.update"
	ActionMCPSourceDelete = "mcp.source.delete"
	ActionMCPSourceSync   = "mcp.source.sync"
	ActionMCPToolApprove  = "mcp.tool.approve"
	ActionMCPToolOverride = "mcp.tool.override"
	// ActionMCPGatewayCall is one tools/call on the tool gateway (/mcp/<slug>):
	// every error and refusal, and at most one ok call per key, source and
	// minute (the gateway samples them; D30).
	ActionMCPGatewayCall = "mcp.gateway.call"

	// v0.5.0 Task 5: upstream-credential binding / unbinding.
	ActionServiceUpstreamCredentialBind   = "service.upstream_credential.bind"
	ActionServiceUpstreamCredentialUnbind = "service.upstream_credential.unbind"

	// v0.5.0 Task 7: custom domain CRUD.
	ActionServiceCustomDomainAdd    = "service.custom_domain.add"
	ActionServiceCustomDomainUpdate = "service.custom_domain.update"
	ActionServiceCustomDomainDelete = "service.custom_domain.delete"

	// v0.5.2 Task 10: emitted by the daily status-tick on every persisted
	// transition (from -> to) for a cert-bearing custom domain row. Payload:
	// {id, from, to}. NOT in aggregatedActions — transitions are rare.
	ActionServiceCustomDomainStatusChanged = "custom_domain.status_changed"

	// H10: the relay could not obtain (or renew) the ACME certificate of a
	// custom domain with cert_source auto, or refused to order one for a
	// name it does not own. Payload {hostname, error}; no actor. Aggregated
	// per host name: certmagic retries a failed order with backoff.
	ActionServiceCustomDomainCertFailed = "service.custom_domain.cert_failed"

	// v0.5.0 Task 9: retention compaction job.
	ActionRetentionCompact = "retention.compact"

	// v0.5.0 Task 9: additional high-frequency leaf actions eligible for
	// compaction (complementing ActionRedactionApplied and ActionGuardrailRefused
	// which were already present).
	ActionConnectionSessionSummary = "connection.session_summary"
	// ActionAICachePromoted is the audit action string (spec L) for a cache
	// promotion. Note the deliberate name split: the corresponding webhook event
	// is "ai.cache_promotion" (spec H.2) — webhook event names use underscores
	// within segments, whereas audit actions use only dots. Both names refer to
	// the same underlying event.
	ActionAICachePromoted = "ai.cache.promoted"
	ActionAIUpstreamError = "ai.upstream_error"

	// Browser-approved client sign-in. A row carries the hostname and the
	// source IP of the request, never its device code, user code or token.
	// Starting a request is not audited: that caller is anonymous.
	// The token minted when an approved request is collected is a
	// ActionTokenMint row of its own.
	ActionClientLoginApproved = "client.login.approved"
	ActionClientLoginDenied   = "client.login.denied"

	// A write of the global redaction settings (the rules have actions of
	// their own). The entry names the settings key, never its value.
	ActionRedactionSettingsUpdate = "redaction.settings.update"

	// A signed-in dashboard user was handed a session of the services host
	// (BURROW_SERVICES_DOMAIN) by a one-time code. The entry names the host
	// and the service slug of the return address, never the code or session.
	ActionAuthHandoff = "auth.handoff"

	// A signed-in user confirmed the account password to open the ten-minute
	// window in which secret-minting routes are allowed (no services domain).
	// Result is "ok" or "denied"; the entry never holds the password.
	ActionAuthReauth = "auth.reauth"

	// A user changed their own password (result "ok") or tried to and gave the
	// wrong current one (result "denied"). Never holds a password.
	ActionAuthPasswordChange = "auth.password_change"

	// OAuth authorization server (spec D30). A client was pre-registered or
	// deleted; a user approved a consent request or a grant was revoked (by
	// its user, an admin or its client); a refresh token or an authorization
	// code came back a second time (oauth.refresh.reuse covers both; payload
	// "via") and its grant was revoked. Never a code or token.
	ActionOAuthClientCreate = "oauth.client.create"
	ActionOAuthClientDelete = "oauth.client.delete"
	ActionOAuthGrantApprove = "oauth.grant.approve"
	ActionOAuthGrantRevoke  = "oauth.grant.revoke"
	ActionOAuthRefreshReuse = "oauth.refresh.reuse"
)

// AllActions is the closed set of every audit action Burrow defines, in a
// stable presentation order. Used by the admin UI's filter dropdown and to
// validate that new call sites never invent an off-list action string.
var AllActions = []string{
	ActionUserCreate, ActionUserUpdate, ActionUserDelete,
	ActionUserSuspend, ActionUserReactivate,
	ActionRoleCreate, ActionRoleUpdate, ActionRoleDelete,
	ActionTokenMint, ActionTokenRevoke,
	ActionSessionCreate, ActionSessionDelete, ActionSessionRevokeOthers,
	ActionServiceCreate, ActionServiceDelete,
	ActionServiceAccessModeUpdate, ActionServiceSlugChange, ActionServiceGatewayOnlyUpdate, ActionServiceAccessPolicyUpdate,
	ActionServiceTCPPortReclaimed, ActionServiceTCPAccessUpdate, ActionServiceIPGrantAdd, ActionServiceIPGrantRevoke,
	ActionServiceMaxConnectionsUpdate, ActionServiceTCPSettingsUpdate, ActionServiceHTTPSettingsUpdate, ActionServiceTCPHostnameUpdate, ActionServiceTCPHostnameOnlyUpdate, ActionServiceConnectionClose, ActionTCPForwardOpen,
	ActionServiceTCPLimitsUpdate, ActionTCPBanAdd, ActionTCPBanRemove,
	ActionServiceAPIKeyCreate, ActionServiceAPIKeyRevoke,
	ActionServiceAIConfigUpdate,
	ActionAIProviderCreate, ActionAIProviderUpdate, ActionAIProviderDelete,
	ActionAIProviderModelsSync, ActionAIProviderModelAdd, ActionAIProviderModelRemove,
	ActionAIModelCreate, ActionAIModelUpdate, ActionAIModelDelete,
	ActionAIGatewayKeyCreate, ActionAIGatewayKeyRevoke, ActionAIGatewayKeyUpdate,
	ActionCacheClear, ActionCacheSettingsUpdate,
	ActionRedactionRuleCreate, ActionRedactionRuleUpdate, ActionRedactionRuleDelete,
	ActionRedactionApplied,
	ActionGuardrailSettingsUpdate, ActionGuardrailRefused, ActionGuardrailFlagged, ActionGuardrailJudgeFailed,
	ActionBudgetCreate, ActionBudgetUpdate, ActionBudgetDelete, ActionBudgetExceeded,
	ActionRateLimitCreate, ActionRateLimitUpdate, ActionRateLimitDelete, ActionRateLimitEnforced,
	ActionWebhookCreate, ActionWebhookUpdate, ActionWebhookDelete, ActionWebhookDeliveryFailed,
	ActionBackupRun, ActionBackupRestore, ActionAuditExport,
	ActionMtlsCAUpdate, ActionMtlsCertRotated, ActionIPGeoUpdate,
	ActionAutomationTokenMint, ActionAutomationTokenRevoke,
	ActionMCPToolCall,
	ActionMCPSourceCreate, ActionMCPSourceUpdate, ActionMCPSourceDelete,
	ActionMCPSourceSync, ActionMCPToolApprove, ActionMCPToolOverride, ActionMCPGatewayCall,
	ActionServiceUpstreamCredentialBind, ActionServiceUpstreamCredentialUnbind,
	ActionServiceCustomDomainAdd, ActionServiceCustomDomainUpdate, ActionServiceCustomDomainDelete,
	ActionServiceCustomDomainStatusChanged, ActionServiceCustomDomainCertFailed,
	ActionRetentionCompact,
	ActionConnectionSessionSummary, ActionAICachePromoted, ActionAIUpstreamError,
	ActionClientLoginApproved, ActionClientLoginDenied,
	ActionRedactionSettingsUpdate,
	ActionAuthHandoff, ActionAuthReauth, ActionAuthPasswordChange,
	ActionOAuthClientCreate, ActionOAuthClientDelete,
	ActionOAuthGrantApprove, ActionOAuthGrantRevoke, ActionOAuthRefreshReuse,
}

// aggregatedActions is the set of actions that are sample-rated at 1/hour
// per (subject_id, action), or per Event.AggregationKey, by Logger.Append. A high-traffic redaction or
// guardrail storm therefore cannot saturate the chain.
var aggregatedActions = map[string]bool{
	ActionRedactionApplied:     true,
	ActionGuardrailRefused:     true,
	ActionGuardrailFlagged:     true,
	ActionGuardrailJudgeFailed: true,
	// One row per limit per hour: the caller keys it by scope, subject and
	// dimension (Event.AggregationKey).
	ActionRateLimitEnforced: true,
	// One row per host name per hour (Event.AggregationKey).
	ActionServiceCustomDomainCertFailed: true,
}

// IsAggregated reports whether the given action is sample-rated.
func IsAggregated(action string) bool { return aggregatedActions[action] }
