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

	ActionServiceCreate             = "service.create"
	ActionServiceDelete             = "service.delete"
	ActionServiceAccessModeUpdate   = "service.access_mode.update"
	ActionServiceSlugChange         = "service.slug_changed"
	ActionServiceAccessPolicyUpdate = "service.access_policy.update"
	ActionServiceAPIKeyCreate       = "service.api_key.create"
	ActionServiceAPIKeyRevoke       = "service.api_key.revoke"
	ActionServiceAIConfigUpdate     = "service.ai_config.update"
	ActionAIProviderCreate          = "ai_provider.create"
	ActionAIProviderUpdate          = "ai_provider.update"
	ActionAIProviderDelete          = "ai_provider.delete"

	ActionCacheClear          = "cache.clear"
	ActionCacheSettingsUpdate = "cache.settings.update"

	ActionRedactionRuleCreate = "redaction.rule.create"
	ActionRedactionRuleUpdate = "redaction.rule.update"
	ActionRedactionRuleDelete = "redaction.rule.delete"
	// ActionRedactionApplied is aggregated 1/hour per (subject_id, action) —
	// see Logger.Append's sample-rate gate.
	ActionRedactionApplied = "redaction.applied"

	ActionGuardrailSettingsUpdate = "guardrail.settings.update"
	// ActionGuardrailRefused is aggregated 1/hour per (subject_id, action).
	ActionGuardrailRefused = "guardrail.refused"

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
	ActionServiceAccessModeUpdate, ActionServiceSlugChange, ActionServiceAccessPolicyUpdate,
	ActionServiceAPIKeyCreate, ActionServiceAPIKeyRevoke,
	ActionServiceAIConfigUpdate,
	ActionAIProviderCreate, ActionAIProviderUpdate, ActionAIProviderDelete,
	ActionCacheClear, ActionCacheSettingsUpdate,
	ActionRedactionRuleCreate, ActionRedactionRuleUpdate, ActionRedactionRuleDelete,
	ActionRedactionApplied,
	ActionGuardrailSettingsUpdate, ActionGuardrailRefused,
	ActionBudgetCreate, ActionBudgetUpdate, ActionBudgetDelete, ActionBudgetExceeded,
	ActionRateLimitCreate, ActionRateLimitUpdate, ActionRateLimitDelete, ActionRateLimitEnforced,
	ActionWebhookCreate, ActionWebhookUpdate, ActionWebhookDelete, ActionWebhookDeliveryFailed,
	ActionBackupRun, ActionBackupRestore, ActionAuditExport,
	ActionMtlsCAUpdate, ActionMtlsCertRotated, ActionIPGeoUpdate,
	ActionAutomationTokenMint, ActionAutomationTokenRevoke,
	ActionMCPToolCall,
	ActionServiceUpstreamCredentialBind, ActionServiceUpstreamCredentialUnbind,
	ActionServiceCustomDomainAdd, ActionServiceCustomDomainUpdate, ActionServiceCustomDomainDelete,
	ActionServiceCustomDomainStatusChanged,
	ActionRetentionCompact,
	ActionConnectionSessionSummary, ActionAICachePromoted, ActionAIUpstreamError,
}

// aggregatedActions is the set of actions that are sample-rated at 1/hour
// per (subject_id, action) by Logger.Append. A high-traffic redaction or
// guardrail storm therefore cannot saturate the chain.
var aggregatedActions = map[string]bool{
	ActionRedactionApplied: true,
	ActionGuardrailRefused: true,
}

// IsAggregated reports whether the given action is sample-rated.
func IsAggregated(action string) bool { return aggregatedActions[action] }
