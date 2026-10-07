package teamwire

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
)

func unsignedTestBundle(t *testing.T) []byte {
	t.Helper()
	body := `{"rules":[]}`
	raw, err := json.Marshal(SignedBundle{SchemaVersion: BundleSchemaVersion, ID: "bnd_01TESTSIGN0000000000000000", OrganizationID: "org_1",
		Scope: BundleScope{Type: "organization", ID: "org_1"}, Revision: 3, CreatedAt: "2026-10-04T12:00:00Z",
		ExpiresAt: "2026-11-03T12:00:00Z", FailureMode: BundleFailureMode{StatefulTier: "fail-open"},
		Documents: []SignedBundleDoc{{Kind: BundleKindRulebook, Name: "rulebook.json", Digest: BundleDocumentDigest([]byte(body)),
			MediaType: BundleMediaTypeRulebook, Body: body}}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// SignBundle signs the bytes a device verifies: the result verifies under the key,
// fails under another key, and fails after any signed byte changes.
func TestSignBundleVerifiesAndATamperedByteIsRefused(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyID := OrgKeyID(public)
	if !strings.HasPrefix(keyID, OrgKeyIDPrefix) || keyID != OrgKeyID(public) || strings.ContainsAny(keyID, "- ") {
		t.Fatalf("key id = %q", keyID)
	}
	signed, err := SignBundle(unsignedTestBundle(t), private, keyID)
	if err != nil {
		t.Fatal(err)
	}
	var doc SignedBundle
	if err := json.Unmarshal(signed, &doc); err != nil || doc.Signature == nil || doc.Signature.KeyID != keyID ||
		doc.Signature.Algorithm != BundleSignatureAlgorithm || doc.Signature.Value == "" || doc.Revision != 3 {
		t.Fatalf("signed = %+v %v", doc, err)
	}
	if err := VerifyBundleSignature(signed, public); err != nil {
		t.Fatalf("the signed bundle must verify: %v", err)
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := VerifyBundleSignature(signed, other); err == nil {
		t.Fatal("another key must not verify")
	}
	for _, change := range [][2]string{{`"revision": 3`, `"revision": 4`}, {`fail-open`, `fail-closed`}, {keyID, keyID + "x"}} {
		tampered := []byte(strings.Replace(string(signed), change[0], change[1], 1))
		if string(tampered) == string(signed) {
			t.Fatalf("the tamper %v changed nothing", change)
		}
		if err := VerifyBundleSignature(tampered, public); err == nil {
			t.Fatalf("a tampered %s must be refused", change[0])
		}
	}
	if _, err := SignBundle(signed, private, keyID); err == nil {
		t.Fatal("a document that already carries a signature is not signed again")
	}
	if _, err := SignBundle(unsignedTestBundle(t), private, ""); err == nil {
		t.Fatal("a signature names its key id")
	}
	if err := VerifyBundleSignature(unsignedTestBundle(t), public); err == nil {
		t.Fatal("an unsigned document does not verify")
	}
}

func TestSignedBundlePathSitsBesideTheUnsignedFile(t *testing.T) {
	if got := SignedBundlePath("/data/bundles/organization-r4.json"); got != "/data/bundles/organization-r4.signed.json" {
		t.Fatalf("path = %s", got)
	}
}
