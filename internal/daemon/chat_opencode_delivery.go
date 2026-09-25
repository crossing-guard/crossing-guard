package daemon

import "context"

// DeliverSessionMessage: a plainly launched OpenCode session has no network
// listener, so the only sanctioned path into it is the installed plugin
// running inside that process with the SDK client bound to its own instance.
// The adapter names the boundary carrier; the plugin drains the pending record
// at its next tool call or tool result (session.idle maps to turn.ended, which
// is never a carrier kind) and appends it as a context-only message
// (helper-session-attachment plan D5).
func (openCodeChatDriver) DeliverSessionMessage(_ context.Context, target SessionIdentity, message string) SessionMessageReceipt {
	if (target.NativeID == "" && target.CatalogID == "") || message == "" {
		return SessionMessageReceipt{State: "unavailable", Tier: "none", Detail: "Delivery requires an exact session identity and a nonempty message."}
	}
	return SessionMessageReceipt{State: "accepted", Tier: "queued-delivery", Carrier: sessionMessageCarrierBoundary,
		Boundary: "next tool call or tool result (plugin append)"}
}
