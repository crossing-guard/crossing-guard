package harvest

// codexRecord is the structural view shared by Codex metadata, transcript, and
// lifecycle projections. It deliberately assigns no canonical meaning: each
// consumer still owns what a vendor record means for its own projection.
type codexRecord struct {
	Envelope string
	Kind     string
	Body     map[string]any
	Flat     bool
}

// decodeCodexRecord accepts both observed Codex JSONL generations:
//
//   - flat:  {"type":"event_msg","payload":{"type":"token_count",...}}
//   - nested:{"payload":{"type":"event_msg","payload":{"type":"user_message",...}}}
//
// A present top-level discriminator is authoritative. In the nested generation,
// payload.type is the envelope and payload.payload is its body.
func decodeCodexRecord(object map[string]any) (codexRecord, bool) {
	payload, ok := object["payload"].(map[string]any)
	if !ok || payload == nil {
		return codexRecord{}, false
	}
	if envelope := anyString(object["type"]); envelope != "" {
		return codexRecord{
			Envelope: envelope,
			Kind:     anyString(payload["type"]),
			Body:     payload,
			Flat:     true,
		}, true
	}

	envelope := anyString(payload["type"])
	if envelope == "" {
		return codexRecord{}, false
	}
	body := payload
	if nested, nestedOK := payload["payload"].(map[string]any); nestedOK && nested != nil {
		body = nested
	}
	return codexRecord{
		Envelope: envelope,
		Kind:     anyString(body["type"]),
		Body:     body,
	}, true
}
