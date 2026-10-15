package mcpserv

// curatedTool is an operation offered as a tool of its own. Description
// replaces the operation's summary and first paragraph when set.
type curatedTool struct {
	ID          string
	Description string
}

// curatedTools are the operations an agent needs most often, in the order
// tools/list shows them. Every other operation is reached through the
// generic tools. New() refuses an id the registry does not have.
var curatedTools = []curatedTool{
	// Read
	{ID: "meGet", Description: "Who this token acts for: the user, the role and the permissions it holds."},
	{ID: "servicesList"},
	{ID: "serviceGet"},
	{ID: "knownClientsList", Description: "Every machine that ever connected with a client token: online, offline or never connected, with what it serves."},
	{ID: "providersList"},
	{ID: "providerGet"},
	{ID: "providerMetricsGet"},
	{ID: "aiModelsList"},
	{ID: "gatewayKeysList"},
	{ID: "aiRequestsList", Description: "Recent requests through the AI gateway across all providers, newest first. Filter with provider, model or gateway_key."},
	{ID: "inspectorRequestGet"},
	{ID: "costSummaryGet"},
	{ID: "budgetsList"},
	{ID: "auditEventsList"},
	{ID: "connectionLogsList"},
	// Write
	{ID: "serviceCreate"},
	{ID: "serviceAccessModeUpdate"},
	{ID: "providerCreate"},
	{ID: "providerUpdate"},
	{ID: "providerModelsSync"},
	{ID: "aiModelCreate"},
	{ID: "aiModelUpdate"},
	{ID: "budgetCreate"},
	// Delete
	{ID: "providerDelete"},
	{ID: "aiModelDelete"},
	{ID: "gatewayKeyDelete"},
}
