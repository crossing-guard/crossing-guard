package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const approvalPresenceLease = 15 * time.Second

type approvalPresence struct {
	visible bool
	focused bool
	seenAt  time.Time
	// attached is true while the client holds an open multiplexed stream: the
	// connection is its lease, so it never expires by time alone.
	attached bool
}

// expired reports whether a lease that is not connection-held has gone stale.
func (p approvalPresence) expired(now time.Time) bool {
	return !p.attached && now.Sub(p.seenAt) > approvalPresenceLease
}

// approvalAttentionRouter owns only short-lived browser-attention leases and
// desktop-notification routing. It cannot create, decide, hide, or extend an
// approval; every approval is published to the canonical stream first.
type approvalAttentionRouter struct {
	mu       sync.Mutex
	clients  map[string]approvalPresence
	now      func() time.Time
	notifier func(string)
}

func newApprovalAttentionRouter(notifier func(string)) *approvalAttentionRouter {
	return &approvalAttentionRouter{
		clients:  make(map[string]approvalPresence),
		now:      time.Now,
		notifier: notifier,
	}
}

var approvalAttention = newApprovalAttentionRouter(desktopApprovalNotification)

func (r *approvalAttentionRouter) Update(clientID string, visible, focused bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for id, lease := range r.clients {
		if lease.expired(now) {
			delete(r.clients, id)
		}
	}
	current := r.clients[clientID]
	r.clients[clientID] = approvalPresence{visible: visible, focused: focused, seenAt: now, attached: current.attached}
}

// Attach marks a client as holding an open stream; its lease lasts as long as
// the connection. Visibility and focus still arrive through Update.
func (r *approvalAttentionRouter) Attach(clientID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.clients[clientID]
	current.attached, current.seenAt = true, r.now()
	r.clients[clientID] = current
}

// Detach releases the connection lease; the last reported state then ages out
// on the ordinary fallback lease.
func (r *approvalAttentionRouter) Detach(clientID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.clients[clientID]
	if !ok {
		return
	}
	current.attached, current.seenAt = false, r.now()
	r.clients[clientID] = current
}

func (r *approvalAttentionRouter) hasActiveViewer() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	active := false
	for id, lease := range r.clients {
		if lease.expired(now) {
			delete(r.clients, id)
			continue
		}
		active = active || (lease.visible && lease.focused)
	}
	return active
}

// Notify emits an OS notification only when no browser is currently visible
// and focused. Its return value exists for focused tests, not approval logic.
func (r *approvalAttentionRouter) Notify(message string) bool {
	if r.hasActiveViewer() {
		return false
	}
	if r.notifier != nil {
		r.notifier(message)
	}
	return true
}

func handleApprovalPresence(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ClientID string `json:"client_id"`
		Visible  bool   `json:"visible"`
		Focused  bool   `json:"focused"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		http.Error(w, "invalid presence update", http.StatusBadRequest)
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "presence update must contain exactly one JSON object", http.StatusBadRequest)
		return
	}
	in.ClientID = strings.TrimSpace(in.ClientID)
	if !validPresenceClientID(in.ClientID) {
		http.Error(w, "invalid client_id", http.StatusBadRequest)
		return
	}
	approvalAttention.Update(in.ClientID, in.Visible, in.Focused)
	w.WriteHeader(http.StatusNoContent)
}

// validPresenceClientID is the one rule for a browser client id, shared by the
// presence POST and the multiplexed stream's attach.
func validPresenceClientID(id string) bool {
	return len(id) >= 8 && len(id) <= 128 && !strings.ContainsAny(id, " \t\r\n/\\")
}

// desktopApprovalNotification raises a non-blocking best-effort macOS
// notification. It is an attention fallback, never an approval dialog.
func desktopApprovalNotification(message string) {
	if os.Getenv("CG_NOTIFY") == "off" {
		return
	}
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(message)
	script := fmt.Sprintf("display notification \"%s\" with title \"Crossing Guard\"", escaped)
	cmd := exec.Command("osascript", "-e", script)
	if cmd.Start() == nil {
		go func() { _ = cmd.Wait() }()
	}
}
