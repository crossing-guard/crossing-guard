package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"runtime"
)

// claudeDeliveryBoundary names where a pending message lands: the session's
// own conversation, at its next carrier hook. A sub-agent's hooks name the
// parent session but print into the sub-agent's separate conversation, so the
// hook client never offers them as carriers (subagent-carrier-boundary plan).
// Which hook kinds carry is configuration (delivery.carrier_kinds), so the text
// names none of them.
const claudeDeliveryBoundary = "next hook boundary in the main conversation; sub-agent tool calls do not carry"

// DeliverSessionMessage: two transports, selected once per process by the
// delivery options (session-message-layer plan §5.3):
//
//   - hook (default): the sanctioned boundary carrier the installed governed
//     lane already drives for every session regardless of who launched it. The
//     adapter names the boundary carrier; the host records the pending message
//     and the next carrier boundary's hook prints it as
//     hookSpecificOutput.additionalContext through the installer's encoder
//     (helper-session-attachment plan D5).
//   - socket (opt-in): one direct post to the session's own inbox
//     (chat_claude_socket.go). Receipt-only evidence; the vendor's inbound
//     controls govern the message after transport acceptance.
//
// A failed socket resolution is an outcome, never a silent fallback to hook.
var claudeDeliveryTransport = claudeHookTransport

// claudeHookTransport is the default transport setting: the boundary carrier.
const claudeHookTransport = "hook"

func claudeDeliveryCapability() SessionMessageCapability {
	if claudeDeliveryTransport == claudeSocketTransport {
		return SessionMessageCapability{Supported: true,
			Boundary: "the session's inbox (peer message; vendor controls apply)",
			Detail:   "Posted directly to the session's inbox as a peer message; the vendor's inbound controls may hold, drop, or expire it (a bypass-mode target holds by default). Receipt-only: consumption is not confirmed. Configured transport: socket."}
	}
	return SessionMessageCapability{Supported: true, Boundary: claudeDeliveryBoundary,
		Detail: "Delivered as hook-provided context by the installed Crossing Guard hook at the session's next boundary; works for terminal and console sessions alike. Requires the hook lane; an opted-out session lets the message expire. Configured transport: hook."}
}

// ConfigureDelivery is the adapter's half of delivery.runtime_options. The
// transport is resolved once per process, like every orchestration tunable;
// changing it requires the daemon restart the reload flow already performs.
func (claudeChatDriver) ConfigureDelivery(raw json.RawMessage) error {
	var options struct {
		Transport string `json:"transport"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&options); err != nil {
		return err
	}
	switch options.Transport {
	case "", claudeHookTransport:
		claudeDeliveryTransport = claudeHookTransport
	case claudeSocketTransport:
		claudeDeliveryTransport = claudeSocketTransport
	default:
		return fmt.Errorf("transport must be hook or socket, not %q", options.Transport)
	}
	return nil
}

func (claudeChatDriver) DeliverSessionMessage(ctx context.Context, target SessionIdentity, message string) SessionMessageReceipt {
	if (target.NativeID == "" && target.CatalogID == "") || message == "" {
		return SessionMessageReceipt{State: "unavailable", Tier: "none", Detail: "Delivery requires an exact session identity and a nonempty message."}
	}
	if claudeDeliveryTransport == claudeSocketTransport {
		if runtime.GOOS == "windows" {
			return socketTransportUnsupported()
		}
		inbox, usable, reason := resolveSessionInbox(target)
		if !usable {
			return SessionMessageReceipt{State: "unavailable", Tier: "none", Detail: reason}
		}
		return claudeChatDriver{}.deliverSessionMessageViaSocket(ctx, inbox, message)
	}
	return SessionMessageReceipt{State: "accepted", Tier: "queued-delivery", Carrier: sessionMessageCarrierBoundary,
		Boundary: claudeDeliveryBoundary}
}
