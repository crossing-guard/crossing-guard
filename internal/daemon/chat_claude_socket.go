package daemon

// The vendor's inbox socket transport (session-message-layer plan §5.2,
// decided by owner D1/D5): one direct post of one attributed message to one
// resolved inbox. Vendor-named file: the wire frame, the auth-line decision,
// and the platform gate live here and nowhere else. The post is
// fire-and-forget — the peer does not acknowledge — so a socket receipt says
// the transport accepted the message and nothing more: the vendor's inbound
// controls may hold, drop, or expire it afterwards, and no store row is
// written (the run's delivery receipt is the whole of the evidence).

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"runtime"
	"strings"
	"time"
)

// claudeSocketTransport is the transport setting this file serves; the
// adapter's delivery dispatch (chat_claude_delivery.go) selects on it.
const claudeSocketTransport = "socket"

// claudeSocketMaxMessageBytes bounds one post. The vendor's own inbox refuses
// a serialized line over about a million characters by closing the
// connection, which after a local Write success is indistinguishable from
// acceptance — the honest move is to refuse an oversized message here, before
// dialing, where the outcome is a visible terminal receipt rather than a
// silent vendor drop. The installed helper profiles cap their own output well
// below this; the bound guards the adapter seam, not the caller.
const claudeSocketMaxMessageBytes = 64 << 10

// deliverSessionMessageViaSocket posts one attributed message to one resolved
// inbox. One process, one post: dial, write one JSON line, close. The auth
// line is deliberately omitted (owner D1): the exported token attests
// own-child, and the daemon is a verified peer either way — holding a token
// the target session never exported to us would add a credential surface for
// no attribution gain.
func (claudeChatDriver) deliverSessionMessageViaSocket(
	ctx context.Context, inbox sessionInbox, message string) SessionMessageReceipt {
	if inbox.SocketPath == "" || message == "" {
		return SessionMessageReceipt{State: "unavailable", Tier: "none",
			Detail: "Delivery requires a resolved inbox and a nonempty message."}
	}
	if len(message) > claudeSocketMaxMessageBytes {
		return SessionMessageReceipt{State: "unavailable", Tier: "none",
			Detail: fmt.Sprintf("The message is over the %d-byte inbox bound and was not sent.", claudeSocketMaxMessageBytes)}
	}
	var dialer net.Dialer
	// The caller's context owns the budget (the host already bounds the call
	// at AdapterTimeout). A caller without a deadline still gets a bounded
	// dial: a unix connect is local and instant, and a pathological one must
	// surface as `unknown`, not hang the host.
	dialer.Deadline, _ = ctx.Deadline()
	if dialer.Deadline.IsZero() {
		dialer.Deadline = time.Now().Add(30 * time.Second)
	}
	conn, err := dialer.DialContext(ctx, "unix", inbox.SocketPath)
	if err != nil {
		if ctx.Err() != nil {
			return SessionMessageReceipt{State: "unknown", Tier: "none", Carrier: sessionMessageCarrierSocket,
				Detail: "Connect did not complete before the delivery budget; do not retry automatically."}
		}
		if isConnRefused(err) {
			return SessionMessageReceipt{State: "unavailable", Tier: "none",
				Detail: "No live inbox for this session: " + err.Error()}
		}
		return SessionMessageReceipt{State: "unavailable", Tier: "none",
			Detail: "The session's inbox could not be opened: " + err.Error()}
	}
	// The receipt is decided by the Write below; closing a unix stream
	// reports no delivery outcome, so its error carries nothing to record.
	defer func() { _ = conn.Close() }()
	frame, err := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": message,
		},
	})
	if err != nil {
		// The frame is built from one string; encoding cannot fail, and a
		// failure would be a programming error worth surfacing honestly.
		return SessionMessageReceipt{State: "unknown", Tier: "none", Carrier: sessionMessageCarrierSocket,
			Detail: "The message frame could not be encoded: " + err.Error()}
	}
	if _, err := conn.Write(append(frame, '\n')); err != nil {
		// After connect, a failed write leaves what the peer may or may not
		// have read genuinely ambiguous; the outcome is terminal either way.
		return SessionMessageReceipt{State: "unknown", Tier: "none", Carrier: sessionMessageCarrierSocket,
			Detail: "The inbox accepted the connection but not the message; do not retry automatically."}
	}
	return SessionMessageReceipt{State: "accepted", Tier: "socket-post", Carrier: sessionMessageCarrierSocket,
		Boundary: "the session's inbox (peer message; vendor controls apply)",
		Detail: "Transport accepted; the vendor's inbound controls may hold, drop, or expire " +
			"this message (a bypass-mode target holds by default); consumption is not confirmed."}
}

// socketTransportUnsupported is the receipt for platforms whose inbox transport
// this build does not serve (the vendor's Windows inbox is an authenticated
// named pipe with a different wire shape).
func socketTransportUnsupported() SessionMessageReceipt {
	return SessionMessageReceipt{State: "unavailable", Tier: "none",
		Detail: fmt.Sprintf("The %s inbox transport is not served on this platform.", runtime.GOOS)}
}

func isConnRefused(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "connection refused")
}
