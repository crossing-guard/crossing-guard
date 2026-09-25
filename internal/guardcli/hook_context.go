package guardcli

// hookContextEncoding is the shared half of a runtime's HookContextEncoder:
// the set of raw events at which that runtime documents injected context, and
// its byte cap. Each installer builds one from its own hook table; the
// envelope shape below is the one two registered runtimes share, and an
// installer whose runtime speaks another shape implements the port itself.

import (
	"encoding/json"
	"strings"
)

const hookContextTruncationMarker = "\n[truncated by the carrier's byte cap]"

type hookContextEncoding struct {
	events   map[string]bool
	capBytes int
}

func (encoding hookContextEncoding) encode(rawEvent, context string) ([]byte, bool) {
	if !encoding.events[rawEvent] {
		return nil, false
	}
	return hookSpecificOutputContext(rawEvent, context, encoding.capBytes), true
}

// hookSpecificOutputContext formats injected context, truncating on a rune
// boundary so the whole output — marker included — stays within capBytes.
func hookSpecificOutputContext(rawEvent, context string, capBytes int) []byte {
	if capBytes > 0 && len(context) > capBytes {
		cut := capBytes - len(hookContextTruncationMarker)
		if cut < 0 {
			cut = 0
		}
		for cut > 0 && cut < len(context) && (context[cut]&0xC0) == 0x80 {
			cut--
		}
		context = strings.ToValidUTF8(context[:cut], "") + hookContextTruncationMarker
	}
	return mustJSON(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":     rawEvent,
		"additionalContext": context,
	}})
}

func hookSpecificOutputDeny(rawEvent, reason string) []byte {
	return mustJSON(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            rawEvent,
		"permissionDecision":       "deny",
		"permissionDecisionReason": reason,
	}})
}

// mustJSON encodes a value the caller built from strings and maps of strings;
// encoding cannot fail for those inputs, and a failure would be a programming
// error worth stopping on rather than a silent empty envelope.
func mustJSON(value any) []byte {
	b, err := json.Marshal(value)
	if err != nil {
		panic("hook envelope could not be encoded: " + err.Error())
	}
	return b
}
