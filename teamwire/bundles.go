// teamwire/bundles.go — the bundle wire types (team plan §5.16.3): the catalog the
// device pulls and the canonical signed form it verifies. The route literals mirror
// the server's wire package; each side's tests pin the same strings, so a drift is
// a failing test, not a broken fleet.

package teamwire

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// The bundle catalog routes (item 3b/3c). The SERVER defines the same strings in its
// own wire package; the client's teamwire owns the device half of the contract.
const (
	RouteBundles        = "/sync/v1/bundles"
	RouteBundleSigned   = "/sync/v1/bundles/{id}" // the ORIGINAL signed document (item 3c, design (b))
	RouteBundleDocument = "/sync/v1/bundles/document/{digest}"
)

// BundleCatalog is the catalog's shape as the server serves it (§5.16.4): available
// bundles, no bodies, each document named by its digest — content-addressing means
// fetching by digest is fetching the signed bytes.
type BundleCatalog struct {
	Bundles []BundleEntry `json:"bundles"`
}

type BundleEntry struct {
	ID            string          `json:"id"`
	Scope         string          `json:"scope"`
	Revision      int64           `json:"revision"`
	ExpiresAt     string          `json:"expires_at"`
	FailureMode   string          `json:"failure_mode"`
	Documents     []BundleDoc     `json:"documents"`
	ContentPolicy json.RawMessage `json:"content_policy,omitempty"`
	// OrgPublicKey is the ACTIVE organization key's public bytes (base64), served
	// beside every bundle that key signs — what a first-contact device pins
	// (item 3c seam, design (b)'s pin source).
	OrgPublicKey string `json:"org_public_key,omitempty"`
}

type BundleDoc struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Digest    string `json:"digest"`
	MediaType string `json:"media_type"`
}

// BundleError is the server's refusal envelope on the catalog routes.
type BundleRefusal struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// SignedCanonical is the normative signed byte-range (§5.16.1, as amended by 3b's
// postwork): canonical JSON of the bundle with signature.value REMOVED — object keys
// sorted recursively, array order preserved, scalars byte-exact. algorithm and
// key_id stay inside the range on purpose: the signature binds which key the author
// claims to sign with, so a key-swap (right bytes over the wrong key_id) is itself a
// bad signature, never a new trust decision. Both signers sign the bytes they
// verified; synchronization-protocol.md carries the test vectors.
func SignedCanonical(raw []byte) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	sig, ok := doc["signature"]
	if !ok {
		return nil, errors.New("no signature")
	}
	var sigDoc map[string]json.RawMessage
	if err := json.Unmarshal(sig, &sigDoc); err != nil {
		return nil, err
	}
	delete(sigDoc, "value")
	sigBytes, err := json.Marshal(sigDoc)
	if err != nil {
		return nil, err
	}
	doc["signature"] = sigBytes
	return canonicalBundle(nil, doc)
}

// canonicalBundle is SignedCanonical's walker: keys sorted, arrays in order,
// scalars byte-exact.
func canonicalBundle(buf []byte, v any) ([]byte, error) {
	switch t := v.(type) {
	case map[string]json.RawMessage:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		for i := 1; i < len(keys); i++ {
			for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
				keys[j], keys[j-1] = keys[j-1], keys[j]
			}
		}
		buf = append(buf, '{')
		for i, k := range keys {
			if i > 0 {
				buf = append(buf, ',')
			}
			kb, _ := json.Marshal(k)
			buf = append(buf, kb...)
			buf = append(buf, ':')
			var err error
			if buf, err = canonicalBundle(buf, t[k]); err != nil {
				return nil, err
			}
		}
		return append(buf, '}'), nil
	case json.RawMessage:
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(t, &nested); err == nil {
			return canonicalBundle(buf, nested)
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(t, &arr); err == nil {
			buf = append(buf, '[')
			for i, e := range arr {
				if i > 0 {
					buf = append(buf, ',')
				}
				var err error
				if buf, err = canonicalBundle(buf, e); err != nil {
					return nil, err
				}
			}
			return append(buf, ']'), nil
		}
		return append(buf, t...), nil
	default:
		return json.Marshal(v)
	}
}

// Bundle wire versions. 1.1 adds the caps below; a 1.0 document already published is
// still accepted, and the same caps apply to it (team rest-of-release plan §4.1
// decision 7, OD-7).
const (
	BundleSchemaVersion       = "1.1"
	BundleSchemaVersionLegacy = "1.0"
)

// Document kinds a bundle may carry.
const (
	BundleKindRulebook  = "rulebook"
	BundleKindProfile   = "profile"
	BundleKindDetectors = "detectors"
)

// Bundle caps. They are part of the wire contract — the schema states the first two —
// and are checked in code by the server at publish and by the device before any parser
// runs, because a count of one kind and a size in bytes are outside the schema subset.
const (
	BundleMaxDocuments = 8
	BundleMaxRulebooks = 1
	BundleMaxBodyBytes = 262_144
	BundleIDPrefix     = "bnd"
)

// Cap refusal codes, shared by the publish route and the device's backstop.
const (
	CodeBundleTooManyDocuments = "bundle_too_many_documents"
	CodeBundleTooManyRulebooks = "bundle_too_many_rulebooks"
	CodeBundleBodyTooLarge     = "bundle_body_too_large"
	CodeBundleDocumentInvalid  = "bundle_document_invalid"
	CodeBundleKeyRetired       = "bundle_key_retired"
)

// SignedBundle is the signed document decoded: every value a device records, shows or
// compares is read from these bytes after the signature verified, never from the
// unsigned catalog entry (§4.1 decision 5).
type SignedBundle struct {
	SchemaVersion  string              `json:"schema_version"`
	ID             string              `json:"id"`
	OrganizationID string              `json:"organization_id"`
	Scope          BundleScope         `json:"scope"`
	Revision       int64               `json:"revision"`
	CreatedAt      string              `json:"created_at"`
	ExpiresAt      string              `json:"expires_at"`
	FailureMode    BundleFailureMode   `json:"failure_mode"`
	Documents      []SignedBundleDoc   `json:"documents"`
	ContentPolicy  json.RawMessage     `json:"content_policy,omitempty"`
	Signature      *BundleSignatureDoc `json:"signature,omitempty"`
}

// BundleScope is a bundle's scope: the organization, or one repository identity.
type BundleScope struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// BundleFailureMode is what the stateful tier does when the bundle has expired.
type BundleFailureMode struct {
	StatefulTier string `json:"stateful_tier"`
}

// SignedBundleDoc is one document with its body.
type SignedBundleDoc struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Digest    string `json:"digest"`
	MediaType string `json:"media_type"`
	Body      string `json:"body"`
}

// BundleSignatureDoc is the signature block. An unsigned bundle (what `bundle build`
// writes) has none.
type BundleSignatureDoc struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	Value     string `json:"value,omitempty"`
}

// CheckBundleCaps returns the first cap a bundle breaks as (code, document index), or
// ("", -1). It reads sizes only; no parser has run.
func CheckBundleCaps(bundle SignedBundle) (string, int) {
	if len(bundle.Documents) > BundleMaxDocuments {
		return CodeBundleTooManyDocuments, BundleMaxDocuments
	}
	rulebooks := 0
	for index, document := range bundle.Documents {
		if len(document.Body) > BundleMaxBodyBytes {
			return CodeBundleBodyTooLarge, index
		}
		if document.Kind == BundleKindRulebook {
			rulebooks++
			if rulebooks > BundleMaxRulebooks {
				return CodeBundleTooManyRulebooks, index
			}
		}
	}
	return "", -1
}

// signedDigestExempt are the signed fields a refresh may change without asking: a new
// revision of the same content has a new id, revision number, expiry and creation
// time, and therefore a new signature value. Everything else — the organization, the
// scope, schema_version, failure_mode, content_policy, the signing key id, the document
// list with every digest and body, and any field a later schema adds — is inside the
// digest (§4.1 decision 5, "Refresh without a prompt — exact").
var signedDigestExempt = []string{"id", "revision", "expires_at", "created_at"}

// BundleSignedDigest is the digest of a signed bundle's canonical form with id,
// revision, expires_at, created_at and signature.value removed. Two verified bundles
// with equal digests differ only in those five fields.
func BundleSignedDigest(raw []byte) (string, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	for _, field := range signedDigestExempt {
		delete(doc, field)
	}
	stripped, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	canonical, err := SignedCanonical(stripped)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// BundleSignedDigestIgnoringKey is BundleSignedDigest with the signing key id removed
// too. It exists for one comparison only: after a chained re-pin, a bundle whose only
// difference is its key id refreshes with no prompt (§4.1 decision 5, the one
// exception). Callers must have checked the re-pin first.
func BundleSignedDigestIgnoringKey(raw []byte) (string, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	var signature map[string]json.RawMessage
	if err := json.Unmarshal(doc["signature"], &signature); err != nil {
		return "", errors.New("no signature")
	}
	delete(signature, "key_id")
	withoutKey, err := json.Marshal(signature)
	if err != nil {
		return "", err
	}
	doc["signature"] = withoutKey
	stripped, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	return BundleSignedDigest(stripped)
}

// Media types `bundle build` writes for the documents it adds.
const (
	BundleMediaTypeRulebook = "application/json"
	BundleMediaTypeProfile  = "text/markdown"
)

// BundleSignatureAlgorithm is the one signature algorithm a bundle carries.
const BundleSignatureAlgorithm = "ed25519"

// OrgKeyIDPrefix starts every key id `org-key init` prints.
const OrgKeyIDPrefix = "orgkey_"

// OrgKeyID is the key id `org-key init` prints for an organization key: the prefix
// and the key's fingerprint in lower case without its dashes. The server takes a key
// id as given at registration; deriving it from the public key means the same file
// always prints the same id, and a bundle's key id says which key signed it.
func OrgKeyID(key ed25519.PublicKey) string {
	return OrgKeyIDPrefix + strings.ToLower(strings.ReplaceAll(KeyFingerprint(key), "-", ""))
}

// BundleDocumentDigest is the digest a bundle document carries for its body.
func BundleDocumentDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// unsignedSuffix and signedSuffix name a built bundle file and its signed sibling.
const (
	unsignedSuffix = ".json"
	signedSuffix   = ".signed.json"
)

// SignedBundlePath is where `bundle sign` writes the signed document for an unsigned
// bundle file: beside it, "…-r<revision>.signed.json".
func SignedBundlePath(unsignedPath string) string {
	return strings.TrimSuffix(unsignedPath, unsignedSuffix) + signedSuffix
}

// SignBundle signs an unsigned bundle document: it sets the signature's algorithm and
// key id, signs SignedCanonical of the result — the exact bytes a device verifies —
// and returns the document with the signature value in place. A document that already
// carries a signature value is refused rather than re-signed.
func SignBundle(unsigned []byte, key ed25519.PrivateKey, keyID string) ([]byte, error) {
	if keyID == "" {
		return nil, errors.New("a signature names its key id")
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(unsigned, &doc); err != nil {
		return nil, err
	}
	if existing, ok := doc["signature"]; ok {
		var signature BundleSignatureDoc
		if err := json.Unmarshal(existing, &signature); err != nil || signature.Value != "" {
			return nil, errors.New("the bundle already carries a signature")
		}
	}
	signature, err := json.Marshal(BundleSignatureDoc{Algorithm: BundleSignatureAlgorithm, KeyID: keyID})
	if err != nil {
		return nil, err
	}
	doc["signature"] = signature
	withFacts, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	canonical, err := SignedCanonical(withFacts)
	if err != nil {
		return nil, err
	}
	value := base64.StdEncoding.EncodeToString(ed25519.Sign(key, canonical))
	signature, err = json.Marshal(BundleSignatureDoc{Algorithm: BundleSignatureAlgorithm, KeyID: keyID, Value: value})
	if err != nil {
		return nil, err
	}
	doc["signature"] = signature
	return json.MarshalIndent(doc, "", "  ")
}

// VerifyBundleSignature checks a signed bundle document against one public key: the
// check a device runs on the bytes it pulled.
func VerifyBundleSignature(signed []byte, key ed25519.PublicKey) error {
	var doc SignedBundle
	if err := json.Unmarshal(signed, &doc); err != nil {
		return err
	}
	if doc.Signature == nil || doc.Signature.Algorithm != BundleSignatureAlgorithm || doc.Signature.KeyID == "" || doc.Signature.Value == "" {
		return errors.New("missing signature facts")
	}
	value, err := base64.StdEncoding.DecodeString(doc.Signature.Value)
	if err != nil {
		return errors.New("signature is not base64")
	}
	canonical, err := SignedCanonical(signed)
	if err != nil {
		return err
	}
	if !ed25519.Verify(key, canonical, value) {
		return errors.New("the signature does not verify")
	}
	return nil
}
