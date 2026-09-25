package daemon

import "context"

// DeliverSessionMessage: Claude's sanctioned surface for reaching a running
// session is its hook system, which the installed governed lane already
// drives for every session regardless of who launched it. The adapter names
// the boundary carrier; the host records the pending message and the next
// carrier boundary's hook prints it as hookSpecificOutput.additionalContext
// through the installer's encoder (helper-session-attachment plan D5).
func (claudeChatDriver) DeliverSessionMessage(_ context.Context, target SessionIdentity, message string) SessionMessageReceipt {
	if (target.NativeID == "" && target.CatalogID == "") || message == "" {
		return SessionMessageReceipt{State: "unavailable", Tier: "none", Detail: "Delivery requires an exact session identity and a nonempty message."}
	}
	return SessionMessageReceipt{State: "accepted", Tier: "queued-delivery", Carrier: sessionMessageCarrierBoundary,
		Boundary: "next hook boundary (prompt submit, tool call, or tool result)"}
}
