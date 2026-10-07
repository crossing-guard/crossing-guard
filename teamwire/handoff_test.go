package teamwire

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/engine"
	"crossing-guard/schemas"
)

func TestHandoffRecordRoundTripsTheSchemaFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "schemas", "fixtures", "valid", "handoff.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record HandoffRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	record.ContentHash = HandoffWireHash(record)
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := schemas.Validate(SchemaForKind(KindHandoff), encoded); err != nil {
		t.Fatalf("the typed record must satisfy its own schema: %v", err)
	}
	again := record
	again.ContentHash = "sha256:" + strings.Repeat("0", 64)
	if HandoffWireHash(again) != record.ContentHash {
		t.Fatal("the wire hash must not depend on content_hash itself")
	}
	again.Remaining = append([]string{"one more"}, again.Remaining...)
	if HandoffWireHash(again) == record.ContentHash {
		t.Fatal("the wire hash must cover the remaining list")
	}
}

func TestReceiptIDIsDeterministicAndSeparatesEveryPart(t *testing.T) {
	base := ReceiptID("hnd_A", TransitionStarted, "dev_A", "tkt_A", "native-1")
	if !engine.IsTypedID(base) || !strings.HasPrefix(base, ReceiptIDPrefix+"_") {
		t.Fatalf("receipt id %q is not a typed id", base)
	}
	if base != ReceiptID("hnd_A", TransitionStarted, "dev_A", "tkt_A", "native-1") {
		t.Fatal("a retry must mint the same id")
	}
	for name, other := range map[string]string{
		"handoff":    ReceiptID("hnd_B", TransitionStarted, "dev_A", "tkt_A", "native-1"),
		"transition": ReceiptID("hnd_A", TransitionOpened, "dev_A", "tkt_A", "native-1"),
		"device":     ReceiptID("hnd_A", TransitionStarted, "dev_B", "tkt_A", "native-1"),
		"ticket":     ReceiptID("hnd_A", TransitionStarted, "dev_A", "tkt_B", "native-1"),
		"native id":  ReceiptID("hnd_A", TransitionStarted, "dev_A", "tkt_A", "native-2"),
	} {
		if other == base {
			t.Errorf("a different %s must mint a different receipt id", name)
		}
	}
	receipt := HandoffReceipt{SchemaVersion: HandoffReceiptSchemaVersion, ID: base, HandoffID: "hnd_01J8ZQ4M7T2V9K3NXW5R6YHBCJ",
		Transition: TransitionReceived, CreatedAt: "2026-10-04T16:05:00Z"}
	encoded, _ := json.Marshal(receipt)
	if err := schemas.Validate(SchemaForKind(KindHandoffReceipt), encoded); err != nil {
		t.Fatalf("a received receipt carries no ticket and no session: %v", err)
	}
}

func TestHandoffTerminalStatesEachHaveARejectionCode(t *testing.T) {
	for _, state := range []string{HandoffSent, HandoffReceived, HandoffStarted, HandoffOpened} {
		if HandoffTerminal(state) || HandoffTerminalCode(state) != "" {
			t.Errorf("%s is not terminal", state)
		}
	}
	for _, state := range []string{HandoffDeclined, HandoffClosed, HandoffWithdrawn, HandoffExpired} {
		if !HandoffTerminal(state) || HandoffTerminalCode(state) == "" {
			t.Errorf("%s must be terminal with a code", state)
		}
	}
}

const signedDigestBase = `{"schema_version":"1.1","id":"bnd_01J8ZQ4M7T2V9K3NXW5R6YHBCJ","organization_id":"org_1",
"scope":{"type":"organization","id":"org_1"},"revision":4,"created_at":"2026-10-01T00:00:00Z","expires_at":"2026-10-31T00:00:00Z",
"failure_mode":{"stateful_tier":"fail-open"},"content_policy":{"sync_content":"off"},
"documents":[{"kind":"profile","name":"follower","digest":"sha256:aa","media_type":"text/markdown","body":"x"}],
"signature":{"algorithm":"ed25519","key_id":"key-1","value":"c2ln"}}`

// The refresh-without-prompt rule rests on this digest: equal digests mean two
// verified bundles differ ONLY in id, revision, expires_at, created_at and the
// signature value. Every other signed field must move it.
func TestBundleSignedDigestIgnoresExactlyTheFiveExemptFields(t *testing.T) {
	digest := func(mutate func(map[string]any)) string {
		t.Helper()
		var doc map[string]any
		if err := json.Unmarshal([]byte(signedDigestBase), &doc); err != nil {
			t.Fatal(err)
		}
		mutate(doc)
		raw, _ := json.Marshal(doc)
		got, err := BundleSignedDigest(raw)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	base := digest(func(map[string]any) {})
	exempt := map[string]func(map[string]any){
		"id":              func(d map[string]any) { d["id"] = "bnd_01J8ZQ4M7T2V9K3NXW5R6YHBCK" },
		"revision":        func(d map[string]any) { d["revision"] = 5 },
		"expires_at":      func(d map[string]any) { d["expires_at"] = "2026-11-30T00:00:00Z" },
		"created_at":      func(d map[string]any) { d["created_at"] = "2026-10-02T00:00:00Z" },
		"signature.value": func(d map[string]any) { d["signature"].(map[string]any)["value"] = "b3RoZXI=" },
	}
	for name, mutate := range exempt {
		if digest(mutate) != base {
			t.Errorf("%s is exempt and must not change the signed digest", name)
		}
	}
	covered := map[string]func(map[string]any){
		"organization_id": func(d map[string]any) { d["organization_id"] = "org_2" },
		"scope":           func(d map[string]any) { d["scope"].(map[string]any)["id"] = "repo" },
		"schema_version":  func(d map[string]any) { d["schema_version"] = "1.0" },
		"failure_mode":    func(d map[string]any) { d["failure_mode"].(map[string]any)["stateful_tier"] = "fail-closed" },
		"content_policy":  func(d map[string]any) { d["content_policy"].(map[string]any)["sync_content"] = "mandated" },
		"content_policy removed": func(d map[string]any) {
			delete(d, "content_policy")
		},
		"key_id":          func(d map[string]any) { d["signature"].(map[string]any)["key_id"] = "key-2" },
		"document digest": func(d map[string]any) { d["documents"].([]any)[0].(map[string]any)["digest"] = "sha256:bb" },
		"document body":   func(d map[string]any) { d["documents"].([]any)[0].(map[string]any)["body"] = "y" },
		"document added": func(d map[string]any) {
			d["documents"] = append(d["documents"].([]any), map[string]any{"kind": "detectors", "name": "d", "digest": "sha256:cc", "media_type": "application/json", "body": "{}"})
		},
		"a field a later schema adds": func(d map[string]any) { d["later_field"] = true },
	}
	for name, mutate := range covered {
		if digest(mutate) == base {
			t.Errorf("%s is a signed field outside the exempt five and must change the signed digest", name)
		}
	}
}

func TestBundleSignedDigestIgnoringKeyDropsOnlyTheKeyID(t *testing.T) {
	rekeyed := strings.Replace(signedDigestBase, `"key_id":"key-1"`, `"key_id":"key-2"`, 1)
	a, errA := BundleSignedDigestIgnoringKey([]byte(signedDigestBase))
	b, errB := BundleSignedDigestIgnoringKey([]byte(rekeyed))
	if errA != nil || errB != nil || a != b {
		t.Fatalf("a re-signed bundle differs only in key id: %q %q %v %v", a, b, errA, errB)
	}
	changed := strings.Replace(rekeyed, `"fail-open"`, `"fail-closed"`, 1)
	if c, _ := BundleSignedDigestIgnoringKey([]byte(changed)); c == a {
		t.Fatal("a re-signed bundle that also changes failure_mode must not compare equal")
	}
}

func TestCheckBundleCaps(t *testing.T) {
	document := func(kind string, size int) SignedBundleDoc {
		return SignedBundleDoc{Kind: kind, Name: "n", Body: strings.Repeat("x", size)}
	}
	eight := make([]SignedBundleDoc, 0, 9)
	for range BundleMaxDocuments {
		eight = append(eight, document(BundleKindProfile, 1))
	}
	if code, _ := CheckBundleCaps(SignedBundle{Documents: eight}); code != "" {
		t.Fatalf("eight documents fit: %s", code)
	}
	if code, _ := CheckBundleCaps(SignedBundle{Documents: append(eight, document(BundleKindProfile, 1))}); code != CodeBundleTooManyDocuments {
		t.Fatalf("a ninth document: %q", code)
	}
	if code, index := CheckBundleCaps(SignedBundle{Documents: []SignedBundleDoc{document(BundleKindRulebook, 1), document(BundleKindRulebook, 1)}}); code != CodeBundleTooManyRulebooks || index != 1 {
		t.Fatalf("a second rulebook: %q at %d", code, index)
	}
	if code, _ := CheckBundleCaps(SignedBundle{Documents: []SignedBundleDoc{document(BundleKindProfile, BundleMaxBodyBytes)}}); code != "" {
		t.Fatalf("a body at the cap fits: %s", code)
	}
	if code, index := CheckBundleCaps(SignedBundle{Documents: []SignedBundleDoc{document(BundleKindProfile, BundleMaxBodyBytes+1)}}); code != CodeBundleBodyTooLarge || index != 0 {
		t.Fatalf("an over-size body: %q at %d", code, index)
	}
}
