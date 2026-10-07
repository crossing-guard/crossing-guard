// Package teamwire is what a device and the team server must agree on byte for byte: how
// a device request is signed, the shape of a push, the device report, and how a report is
// read as a standing. It is portable on purpose — standard library and engine only, pinned
// by a test — because the server imports it rather than re-implementing it. Two
// implementations of a canonical string is how signatures stop verifying; two readings of
// one report is how a fleet page and a local console come to disagree (team plan §5.15).
package teamwire

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"crossing-guard/engine"
)

// SignatureScheme is the first line of every canonical string. A future scheme is a new
// literal, never a reinterpretation of this one.
const SignatureScheme = "crossing-guard-device-v1"

// KeyAlgorithm names the only device key algorithm. It is recorded beside every enrolled
// key so a second algorithm later is a new value, not a second signing scheme.
const KeyAlgorithm = "ed25519"

// Request headers. The body digest is declared so the server can verify the signature
// BEFORE it reads the body, then compare while streaming: a forged request never costs a
// body hash.
const (
	HeaderDevice     = "Crossing-Guard-Device"
	HeaderTimestamp  = "Crossing-Guard-Timestamp"
	HeaderNonce      = "Crossing-Guard-Nonce"
	HeaderBodyDigest = "Crossing-Guard-Content-SHA256"
	HeaderSignature  = "Crossing-Guard-Signature"
)

// Errors a verifier distinguishes. What a server DISCLOSES about them is its decision
// (an unknown device must read exactly like a bad signature); that they are distinct
// here is what lets a client say "this device's clock is off" instead of going silent.
var (
	ErrMalformed            = errors.New("malformed signed request")
	ErrBadSignature         = errors.New("signature does not verify")
	ErrTimestampOutOfWindow = errors.New("timestamp outside the accepted window")
)

// maxTimestampSeconds is the timestamp grammar's ceiling: twelve digits, the year 33658.
const maxTimestampSeconds = 999_999_999_999

var (
	methodShape = regexp.MustCompile(`^[A-Z]{3,7}$`)
	routeShape  = regexp.MustCompile(`^/[A-Za-z0-9/_{}.-]{0,199}$`)
	nonceShape  = regexp.MustCompile(`^[A-Za-z0-9_-]{22,43}$`)
	digestShape = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// SignedRequest is everything a signature covers.
//
// Route is the server's route-table PATTERN ("/sync/v1/push"), not the bytes that arrived:
// a proxy may re-encode a path, merge slashes, or strip a prefix, and none of that may
// decide whether a signature verifies. Wire routes take no query string, so there is no
// query to canonicalize. Origin and DeviceID bind a signature to one server and one
// device: a signature captured against staging is not valid against production restored
// from the same database.
type SignedRequest struct {
	Origin     string // CanonicalOrigin of the server's public URL
	DeviceID   string
	Method     string
	Route      string
	Timestamp  int64 // Unix seconds
	Nonce      string
	BodySHA256 string // lowercase hex; BodyDigest(nil) for an empty body
}

// Canonical renders the exact bytes that are signed, refusing anything outside the grammar
// — a field containing a newline would let one request's fields read as another's.
func (r SignedRequest) Canonical() ([]byte, error) {
	origin, err := CanonicalOrigin(r.Origin)
	if err != nil || origin != r.Origin {
		return nil, fmt.Errorf("%w: origin is not canonical", ErrMalformed)
	}
	if !strings.HasPrefix(r.DeviceID, engine.DeviceIDPrefix+"_") || !engine.IsTypedID(r.DeviceID) {
		return nil, fmt.Errorf("%w: device id", ErrMalformed)
	}
	if !methodShape.MatchString(r.Method) {
		return nil, fmt.Errorf("%w: method", ErrMalformed)
	}
	if !routeShape.MatchString(r.Route) {
		return nil, fmt.Errorf("%w: route", ErrMalformed)
	}
	if r.Timestamp <= 0 || r.Timestamp > maxTimestampSeconds {
		return nil, fmt.Errorf("%w: timestamp", ErrMalformed)
	}
	if !nonceShape.MatchString(r.Nonce) {
		return nil, fmt.Errorf("%w: nonce", ErrMalformed)
	}
	if !digestShape.MatchString(r.BodySHA256) {
		return nil, fmt.Errorf("%w: body digest", ErrMalformed)
	}
	return []byte(strings.Join([]string{SignatureScheme, r.Origin, r.DeviceID, r.Method, r.Route,
		strconv.FormatInt(r.Timestamp, 10), r.Nonce, r.BodySHA256}, "\n")), nil
}

// Sign returns the base64url (unpadded) Ed25519 signature over the canonical string.
func Sign(key ed25519.PrivateKey, r SignedRequest) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("%w: private key size", ErrMalformed)
	}
	canonical, err := r.Canonical()
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, canonical)), nil
}

// Verify checks a signature. It says nothing about freshness or replay; see Freshness.
func Verify(key ed25519.PublicKey, r SignedRequest, signature string) error {
	if len(key) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: public key size", ErrMalformed)
	}
	canonical, err := r.Canonical()
	if err != nil {
		return err
	}
	raw, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return fmt.Errorf("%w: signature encoding", ErrMalformed)
	}
	if !ed25519.Verify(key, canonical, raw) {
		return ErrBadSignature
	}
	return nil
}

// Freshness is the replay rule, kept beside the signature it protects because getting it
// subtly wrong is easy. A request stamped ts is acceptable while
//
//	now - Window <= ts <= now + ForwardSkew
//
// so it stays acceptable until ts + Window. A nonce must therefore be remembered until
// NonceExpiry(ts) — NOT until "Window after it was received": a request stamped at the
// far future edge would outlive a nonce pruned on receipt time, and replay would succeed.
// Both durations are used at whole-second resolution (timestamps are seconds); Check and
// NonceExpiry truncate identically, so a sub-second setting cannot open a gap.
type Freshness struct {
	Window      time.Duration
	ForwardSkew time.Duration
}

// Check reports whether ts is acceptable at now.
func (f Freshness) Check(ts, now int64) error {
	if ts < now-int64(f.Window/time.Second) || ts > now+int64(f.ForwardSkew/time.Second) {
		return ErrTimestampOutOfWindow
	}
	return nil
}

// NonceExpiry is the first second at which a request stamped ts can no longer pass Check,
// and so the earliest moment its nonce may be forgotten.
func (f Freshness) NonceExpiry(ts int64) int64 { return ts + int64(f.Window/time.Second) + 1 }

// NewNonce returns 128 random bits, base64url — 22 characters.
func NewNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// BodyDigest is the lowercase hex sha256 of a request body; nil and empty are the same.
func BodyDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// CanonicalOrigin reduces a server's public URL to scheme://host[:port], lowercased, with
// a default port removed and any path ignored (a deployment may sit under a prefix).
// Everything a signature, a cookie, or an Origin check compares against comes from this
// one function, never from a Host or forwarded header.
func CanonicalOrigin(publicURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(publicURL))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", fmt.Errorf("%w: scheme must be http or https", ErrMalformed)
	}
	if u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: origin must be a bare scheme and host", ErrMalformed)
	}
	host := strings.ToLower(u.Hostname())
	for _, c := range host {
		if c > 0x7f {
			// A browser sends Origin in punycode; an operator types Unicode. Two spellings of
			// one host must not be a silent mismatch, so only the ASCII form is accepted.
			return "", fmt.Errorf("%w: host must be ASCII (use the punycode form)", ErrMalformed)
		}
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host, nil
}

// KeyThumbprint identifies a public key: "sha256:" + hex. It is the server's lookup handle.
func KeyThumbprint(key ed25519.PublicKey) string {
	sum := sha256.Sum256(key)
	return "sha256:" + hex.EncodeToString(sum[:])
}

var crockford = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// KeyFingerprint is the thumbprint's first 100 bits for human comparison — what the
// terminal prints and the approval page shows: five groups of four, no I, L, O, or U.
func KeyFingerprint(key ed25519.PublicKey) string {
	sum := sha256.Sum256(key)
	text := crockford.EncodeToString(sum[:13])[:20]
	groups := make([]string, 0, 5)
	for i := 0; i < len(text); i += 4 {
		groups = append(groups, text[i:i+4])
	}
	return strings.Join(groups, "-")
}
