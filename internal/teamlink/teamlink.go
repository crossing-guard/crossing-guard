// Package teamlink is the client side of a device's link to the team server (team plan
// §5.15, build order 2c): the typed owner of team.json and the device key, both kept
// beside the store because the device id and the linked flag live IN the store; the
// signed HTTPS client that speaks crossing-guard/teamwire; the device-authorization
// enrollment; and the report builder.
//
// Nothing here is reachable from the hook path: linking, polling, pushing, and reporting
// run on the daemon's own cadence (invariant 5). The daemon owns every write; the CLI
// and the console only ask it.
package teamlink

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"crossing-guard/internal/filelock"
	"crossing-guard/teamwire"
)

//go:embed team.default.json
var defaultDocument []byte

// FormatVersion is team.json's format.
const FormatVersion = 1

// File names beside the store.
const (
	DocumentFile = "team.json"
	KeyFile      = "team-device.key"
	ReportFile   = "team-report-state.json"
	lockFile     = "team.lock"
)

// Document is team.json: the link's facts and the tunables that govern it. The tunables
// carry embedded defaults (invariant 12) and are read when the daemon starts or links;
// the drift check then freezes the WHOLE file, tunables included, because a hand-edited
// cadence is exactly how a report is silenced. So: set tunables before linking, in a
// team.json holding only them; after a link, a hand edit is drift (reporting stops until
// unlink and link); an unlink removes the file, tunables and all, so a fresh link starts
// from the embedded defaults. The facts are written by the daemon and never by hand.
type Document struct {
	FormatVersion     int          `json:"format_version"`
	Server            string       `json:"server,omitempty"`
	Organization      Organization `json:"organization,omitzero"`
	Device            DeviceFacts  `json:"device,omitzero"`
	LinkedAt          string       `json:"linked_at,omitempty"`
	ApprovedBy        string       `json:"approved_by,omitempty"`
	ReportInterval    Duration     `json:"report_interval"`
	PullInterval      Duration     `json:"pull_interval"` // the bundle-catalog cadence (§5.4: 60 s; RT3-17)
	PollInterval      Duration     `json:"poll_interval"`
	RequestTimeout    Duration     `json:"request_timeout"`
	EnrollmentTimeout Duration     `json:"enrollment_timeout"`
	// The outbox drain (item 4 decision 1): its cadence, its batch, how often a kind the
	// server refused as unsupported is re-probed, and the backlog size the console flags.
	PushInterval    Duration `json:"push_interval"`
	PushBatch       int      `json:"push_batch"`
	PushMaxAttempts int      `json:"push_max_attempts"` // a row the server keeps failing to store is dead-lettered after this many
	PushMaxBytes    int64    `json:"push_max_bytes"`    // one push's size; the server's devices.push_max_bytes
	ParkedKindRetry Duration `json:"parked_kind_retry"`
	OutboxHighWater int      `json:"outbox_high_water"`
	// ContentSessionCap bounds the opted-in content one session sends, in bytes
	// (item 4 decision 10); a chunk that would pass it is refused by name.
	ContentSessionCap int64 `json:"content_session_cap"`
	// PullApplyBatch is how many pulled memory rows land per local transaction (team
	// item 5 decision 5; never a whole page in one, R2-L3). The pull shares pull_interval.
	PullApplyBatch int `json:"pull_apply_batch"`
	// MemoryPullPages bounds the pages one pull tick lands (a backlog drains over
	// several ticks). IdentityUpgradeRoots bounds the checkout roots the guarded
	// repository-identity upgrade resolves, IdentityResolveTimeout each resolution.
	// DeletionsVerifyMax bounds the deletions whose reach one console read asks the
	// server about; ConflictsPageMax one conflict-copy listing.
	MemoryPullPages        int      `json:"memory_pull_pages"`
	IdentityUpgradeRoots   int      `json:"identity_upgrade_roots"`
	IdentityResolveTimeout Duration `json:"identity_resolve_timeout"`
	DeletionsVerifyMax     int      `json:"deletions_verify_max"`
	ConflictsPageMax       int      `json:"conflicts_page_max"`
	// MemoryPullStartDelay is how long after the link's jobs start the first memory pull
	// runs. NotShareableWindow is how long a row refused on this device as not_shareable
	// stays in the console's count (the one-off backlog a device carried into the
	// feature ages out of the line).
	MemoryPullStartDelay Duration `json:"memory_pull_start_delay"`
	NotShareableWindow   Duration `json:"not_shareable_window"`
	Sync                 Sync     `json:"sync"`
	// Handoff bounds the handoff pull; MembersRefresh is how often the member directory
	// is re-read; Bundle holds the defaults `bundle build` uses for a scope's first
	// bundle (team rest-of-release plan §8.4, OD-8, OD-23).
	Handoff        HandoffSync    `json:"handoff"`
	MembersRefresh Duration       `json:"members_refresh"`
	Bundle         BundleDefaults `json:"bundle"`
}

// HandoffSync bounds one handoff pull tick, in pages.
type HandoffSync struct {
	PullPages int `json:"pull_pages"`
}

// BundleDefaults are what `bundle build` uses when neither a flag nor the scope's
// published revision says otherwise.
type BundleDefaults struct {
	DefaultExpiry      Duration `json:"default_expiry"`
	DefaultFailureMode string   `json:"default_failure_mode"`
}

// Bundle failure modes.
const (
	FailOpen   = "fail-open"
	FailClosed = "fail-closed"
)

// MaxPullApplyBatch bounds pull_apply_batch: one local write transaction holds the store's
// one writer, so a large batch would stall the hook path's writes.
const MaxPullApplyBatch = 100

// MaxPushBatch bounds push_batch: a batch is one signed request, and the server caps a
// request's bytes (its devices.push_max_bytes), so an unbounded batch is a refused one.
const MaxPushBatch = 500

// Organization is the team the device is linked to, as the server named it.
type Organization struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// DeviceFacts is what the device told the server about itself.
type DeviceFacts struct {
	Name        string `json:"name,omitempty"`
	Platform    string `json:"platform,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// Sync is the content policy (D-12): off until a developer opts a session in.
type Sync struct {
	Content string `json:"content"`
}

// Content policies. In this release sync.content is recorded but not consulted: what
// leaves is decided by per-session consent (POST /api/team/content) and the mandate of
// an ADOPTED bundle of any scope (organization or repository; OD-27), both shown in GET /api/team's content object. The key
// is kept for the document's compatibility; a device-level veto is a later decision.
const (
	ContentOff      = "off"
	ContentConsent  = "consent"
	ContentMandated = "mandated-by-bundle"
)

// Duration decodes "300s"-style strings.
type Duration struct{ time.Duration }

// UnmarshalJSON accepts a duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"300s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// MarshalJSON renders the duration string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.Duration.String()) }

// Linked reports whether the document records a link.
func (d Document) Linked() bool { return d.Server != "" && d.LinkedAt != "" }

// Default is the embedded document alone.
func Default() (Document, error) {
	var d Document
	if err := decodeStrict(defaultDocument, &d); err != nil {
		return Document{}, fmt.Errorf("embedded team.default.json: %w", err)
	}
	return d, nil
}

func decodeStrict(raw []byte, into *Document) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the document")
	}
	return nil
}

// Validate checks the tunables and the link facts.
func (d Document) Validate() error {
	if d.FormatVersion != FormatVersion {
		return fmt.Errorf("format_version must be %d", FormatVersion)
	}
	if d.ReportInterval.Duration <= 0 || d.PullInterval.Duration <= 0 || d.PollInterval.Duration <= 0 || d.RequestTimeout.Duration <= 0 || d.EnrollmentTimeout.Duration <= 0 ||
		d.PushInterval.Duration <= 0 || d.ParkedKindRetry.Duration <= 0 {
		return errors.New("durations must be positive")
	}
	if d.PushBatch < 1 || d.PushBatch > MaxPushBatch {
		return fmt.Errorf("push_batch must be between 1 and %d", MaxPushBatch)
	}
	if d.PushMaxBytes < MinPushBytes {
		return errors.New("push_max_bytes must be at least 65536 (a record larger than it is refused by name as over_push_limit)")
	}
	if d.PushMaxAttempts < 1 {
		return errors.New("push_max_attempts must be positive")
	}
	if d.ContentSessionCap < 1 {
		return errors.New("content_session_cap must be positive")
	}
	if d.OutboxHighWater < 1 {
		return errors.New("outbox_high_water must be positive")
	}
	if d.PullApplyBatch < 1 || d.PullApplyBatch > MaxPullApplyBatch {
		return fmt.Errorf("pull_apply_batch must be between 1 and %d", MaxPullApplyBatch)
	}
	if d.MemoryPullPages < 1 || d.IdentityUpgradeRoots < 1 || d.IdentityResolveTimeout.Duration <= 0 || d.DeletionsVerifyMax < 1 || d.ConflictsPageMax < 1 {
		return errors.New("memory_pull_pages, identity_upgrade_roots, identity_resolve_timeout, deletions_verify_max and conflicts_page_max must be positive")
	}
	if d.MemoryPullStartDelay.Duration <= 0 || d.NotShareableWindow.Duration <= 0 {
		return errors.New("memory_pull_start_delay and not_shareable_window must be positive")
	}
	if d.Sync.Content != ContentOff && d.Sync.Content != ContentConsent && d.Sync.Content != ContentMandated {
		return fmt.Errorf("sync.content must be %s, %s, or %s", ContentOff, ContentConsent, ContentMandated)
	}
	if d.Handoff.PullPages < 1 || d.MembersRefresh.Duration <= 0 {
		return errors.New("handoff.pull_pages and members_refresh must be positive")
	}
	if d.Bundle.DefaultExpiry.Duration <= 0 {
		return errors.New("bundle.default_expiry must be positive")
	}
	if d.Bundle.DefaultFailureMode != FailOpen && d.Bundle.DefaultFailureMode != FailClosed {
		return fmt.Errorf("bundle.default_failure_mode must be %s or %s", FailOpen, FailClosed)
	}
	if d.Server != "" {
		if _, err := teamwire.CanonicalOrigin(d.Server); err != nil {
			return fmt.Errorf("server: %w", err)
		}
	}
	return nil
}

// Paths are the files beside the store.
type Paths struct {
	Document, Key, Report, lock string
}

// PathsIn returns the files for a store directory.
func PathsIn(storeDir string) Paths {
	return Paths{Document: filepath.Join(storeDir, DocumentFile), Key: filepath.Join(storeDir, KeyFile),
		Report: filepath.Join(storeDir, ReportFile), lock: filepath.Join(storeDir, lockFile)}
}

// Load reads team.json over the embedded default. found is false when no file exists
// (the defaults alone are returned, valid, unlinked).
func Load(storeDir string) (Document, bool, error) {
	d, err := Default()
	if err != nil {
		return Document{}, false, err
	}
	raw, err := os.ReadFile(PathsIn(storeDir).Document)
	if err != nil {
		if os.IsNotExist(err) {
			return d, false, nil
		}
		return Document{}, false, err
	}
	if err := decodeStrict(raw, &d); err != nil {
		return Document{}, true, fmt.Errorf("%s: %w", DocumentFile, err)
	}
	if err := d.Validate(); err != nil {
		return Document{}, true, fmt.Errorf("%s: %w", DocumentFile, err)
	}
	return d, true, nil
}

// Save writes team.json atomically under the mutation lock (0600: it names the server
// and the organization, which is nobody else's business on a shared machine).
func Save(storeDir string, d Document) error {
	if err := d.Validate(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	p := PathsIn(storeDir)
	return filelock.With(p.lock, 0o600, func() error { return writeAtomic(p.Document, append(raw, '\n')) })
}

// Remove deletes team.json, the key, and the report state: the unlink. Tunables in
// team.json go with it — a relink starts from the embedded defaults, so a file that
// drifted cannot carry its cadence into a fresh link. Missing files are not errors.
func Remove(storeDir string) error {
	p := PathsIn(storeDir)
	return filelock.With(p.lock, 0o600, func() error {
		for _, f := range []string{p.Document, p.Key, p.Report} {
			if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	})
}

// --- the key ------------------------------------------------------------------------

// NewKey generates a device key.
func NewKey() (ed25519.PrivateKey, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	return key, err
}

// SaveKey writes the key's seed, base64, 0600, atomically.
func SaveKey(storeDir string, key ed25519.PrivateKey) error {
	p := PathsIn(storeDir)
	return filelock.With(p.lock, 0o600, func() error {
		return writeAtomic(p.Key, []byte(base64.StdEncoding.EncodeToString(key.Seed())+"\n"))
	})
}

// LoadKey reads the key. found is false when no key exists.
func LoadKey(storeDir string) (ed25519.PrivateKey, bool, error) {
	raw, err := os.ReadFile(PathsIn(storeDir).Key)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, true, fmt.Errorf("%s is not a device key", KeyFile)
	}
	return ed25519.NewKeyFromSeed(seed), true, nil
}

// writeAtomic is the selection store's sequence: temp file, 0600, write, sync, rename.
func writeAtomic(path string, raw []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// Digests fingerprints the link files as they are on disk, so the daemon can notice a
// change it did not make (D-16: a hostile link is detected, not pretended away).
type Digests struct {
	Document string `json:"document"`
	Key      string `json:"key"`
}

// DigestsIn reads the digests; a missing file digests to "".
func DigestsIn(storeDir string) Digests {
	p := PathsIn(storeDir)
	return Digests{Document: fileDigest(p.Document), Key: fileDigest(p.Key)}
}

func fileDigest(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// --- the report state -----------------------------------------------------------------

// Report outcomes as the daemon records them: the four per-record statuses of the wire,
// plus "error" (nothing reached the server, or its answer named no record) and
// "revoked" (the server refused the key).
const (
	OutcomeError   = "error"
	OutcomeRevoked = "revoked"
)

// ReportState is what the daemon persists beside the store about its reporting: when it
// last reported and how, the exact document it sent, and the link files' digests as it
// wrote them — the baseline every drift check compares against. It lives in ReportFile,
// which Remove deletes with the link.
type ReportState struct {
	LastAt   time.Time       `json:"last_at"`
	Outcome  string          `json:"outcome"`
	Error    string          `json:"error,omitempty"`
	RecordID string          `json:"record_id,omitempty"`
	Document json.RawMessage `json:"document,omitempty"`
	Digests  Digests         `json:"digests"`
	Push     PushState       `json:"push,omitzero"`
	Pull     PullState       `json:"pull,omitzero"`
}

// PullState is the memory pull job's persisted record (team item 5): when it last pulled
// and how, the cursor it acknowledged, and what landing did, counted.
type PullState struct {
	LastAt    time.Time `json:"last_at,omitzero"`
	Outcome   string    `json:"outcome,omitempty"`
	Error     string    `json:"error,omitempty"`
	Cursor    int64     `json:"cursor,omitempty"`
	Landed    int64     `json:"landed,omitempty"`
	Held      int64     `json:"held,omitempty"`
	Ignored   int64     `json:"ignored,omitempty"`
	Conflicts int64     `json:"conflicts,omitempty"`
	Discarded int64     `json:"discarded,omitempty"`
	Shadowed  int64     `json:"shadowed,omitempty"`
	Aliased   int64     `json:"aliased,omitempty"`
	Deleted   int64     `json:"deleted,omitempty"`
	// Unlandable counts pulled rows this device's own rules refused; each was skipped.
	Unlandable int64 `json:"unlandable,omitempty"`
	// UnlandableBuild is the build that skipped them: a different build starts the pull
	// over, since it may be able to land what that one could not.
	UnlandableBuild string `json:"unlandable_build,omitempty"`
}

// PushState is the outbox drain's persisted record (item 4 decision 1): when it last
// pushed and how, and the terminal refusals it acknowledged — counted per kind and code,
// because an acknowledged refusal is otherwise invisible.
type PushState struct {
	LastAt     time.Time                 `json:"last_at,omitzero"`
	Outcome    string                    `json:"outcome,omitempty"`
	Error      string                    `json:"error,omitempty"`
	Accepted   int64                     `json:"accepted,omitempty"`
	DeadLetter map[string]map[string]int `json:"dead_letter,omitempty"` // kind → code → count
	Conflicts  map[string]int            `json:"conflicts,omitempty"`   // kind → count
	// ContentBytes is the opted-in content each session has sent (wire session id →
	// bytes the server accepted), the per-session cap's running total.
	ContentBytes map[string]int64 `json:"content_bytes,omitempty"`
	// Refused counts what this DEVICE refused to send (consent revoked, over a cap,
	// not encodable) — kept apart from DeadLetter, which is what the server refused.
	Refused map[string]map[string]int `json:"refused,omitempty"`
}

// MinPushBytes is the smallest batch the drain will fit to after a 413.
const MinPushBytes = 64 * 1024

// LoadReportState reads the persisted state; a missing file is an error the caller
// reads as "no baseline".
func LoadReportState(storeDir string) (ReportState, error) {
	raw, err := os.ReadFile(PathsIn(storeDir).Report)
	if err != nil {
		return ReportState{}, err
	}
	var s ReportState
	if err := json.Unmarshal(raw, &s); err != nil {
		return ReportState{}, fmt.Errorf("%s: %w", ReportFile, err)
	}
	return s, nil
}

// SaveReportState writes the state the way the document and the key are written.
func SaveReportState(storeDir string, s ReportState) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	p := PathsIn(storeDir)
	return filelock.With(p.lock, 0o600, func() error { return writeAtomic(p.Report, append(raw, '\n')) })
}

// --- the client ---------------------------------------------------------------------

// ErrServer is a refusal the server sent, with its envelope.
type ErrServer struct {
	Status int
	Body   teamwire.ErrorBody
}

func (e *ErrServer) Error() string {
	if e.Body.Error.Code != "" {
		return fmt.Sprintf("%s (%d): %s", e.Body.Error.Code, e.Status, e.Body.Error.Message)
	}
	return fmt.Sprintf("server answered %d", e.Status)
}

// Code returns the server's error code, or "" when the failure was not the server's.
func Code(err error) string {
	var se *ErrServer
	if errors.As(err, &se) {
		return se.Body.Error.Code
	}
	return ""
}

// ErrServerURL is a server address the device refuses to sign to before it ever
// connects: malformed, or plaintext beyond loopback. A client-side fact, not a
// link-state fact — the daemon's link route answers it with 400, not 409.
type ErrServerURL struct{ Err error }

func (e *ErrServerURL) Error() string { return e.Err.Error() }
func (e *ErrServerURL) Unwrap() error { return e.Err }

// ErrEnrollmentStart is a failure to start an enrollment against a server the device
// was willing to sign to: unreachable, or it refused. The daemon's link route answers
// it with 502, carrying the server's code in the message.
type ErrEnrollmentStart struct{ Err error }

func (e *ErrEnrollmentStart) Error() string { return "enrollment start refused: " + e.Err.Error() }
func (e *ErrEnrollmentStart) Unwrap() error { return e.Err }

// Client signs requests to one server as one device. Its transport follows no
// redirects, reads no proxy from the environment, and times out: a device never
// wanders (the audio-upload backend set that precedent).
type Client struct {
	origin   string
	deviceID string
	key      ed25519.PrivateKey
	http     *http.Client
	now      func() time.Time
}

// NewClient validates the server origin and builds the client. An http:// origin is
// accepted only on loopback: the device refuses to sign to a plaintext network address.
func NewClient(server, deviceID string, key ed25519.PrivateKey, timeout time.Duration, now func() time.Time) (*Client, error) {
	origin, err := teamwire.CanonicalOrigin(server)
	if err != nil {
		return nil, err
	}
	u, _ := url.Parse(origin)
	if u.Scheme == "http" && !loopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("%s is http:// beyond loopback: a device signs only to https, or to a loopback development server", origin)
	}
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("device key is not an Ed25519 private key")
	}
	if now == nil {
		now = time.Now
	}
	transport := &http.Transport{Proxy: nil, TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout}
	return &Client{origin: origin, deviceID: deviceID, key: key, now: now,
		http: &http.Client{Timeout: timeout, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Origin is the canonical server origin.
func (c *Client) Origin() string { return c.origin }

// PublicKey is the device's verification key.
func (c *Client) PublicKey() ed25519.PublicKey { return c.key.Public().(ed25519.PublicKey) }

// Fingerprint is what the terminal prints and the approval page shows.
func (c *Client) Fingerprint() string { return teamwire.KeyFingerprint(c.PublicKey()) }

// do sends one signed request and decodes a JSON answer into out (nil = ignore body).
// method names the HTTP verb the signature covers — GET for the two catalog routes
// (§5.16.3, RT3-07), POST everywhere else.
func (c *Client) do(method, route string, body []byte, out any) error {
	nonce, err := teamwire.NewNonce()
	if err != nil {
		return err
	}
	if method == "" {
		method = http.MethodPost
	}
	sr := teamwire.SignedRequest{Origin: c.origin, DeviceID: c.deviceID, Method: method, Route: route,
		Timestamp: c.now().Unix(), Nonce: nonce, BodySHA256: teamwire.BodyDigest(body)}
	sig, err := teamwire.Sign(c.key, sr)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(method, c.origin+route, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(teamwire.HeaderDevice, c.deviceID)
	req.Header.Set(teamwire.HeaderTimestamp, fmt.Sprint(sr.Timestamp))
	req.Header.Set(teamwire.HeaderNonce, sr.Nonce)
	req.Header.Set(teamwire.HeaderBodyDigest, sr.BodySHA256)
	req.Header.Set(teamwire.HeaderSignature, sig)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusMethodNotAllowed {
		// The J2 journey's 405: WHICH request reached the server is the diagnosis.
		return &ErrServer{Status: resp.StatusCode, Body: teamwire.ErrorBody{Error: teamwire.Error{
			Code:    "method_not_allowed",
			Message: fmt.Sprintf("method %s to %s was not allowed — check the route and method the signature covered", method, route)}}}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		se := &ErrServer{Status: resp.StatusCode}
		_ = json.Unmarshal(raw, &se.Body) // a non-JSON refusal keeps its status and an empty envelope
		return se
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// EnrollStart is the server's answer to an enrollment start.
type EnrollStart struct {
	UserCode            string `json:"user_code"`
	VerificationURL     string `json:"verification_url"`
	PollIntervalSeconds int64  `json:"poll_interval_seconds"`
	ExpiresAt           string `json:"expires_at"`
}

// StartEnrollment begins the device-authorization flow, signed with the new key.
func (c *Client) StartEnrollment(name, platform, clientVersion string) (EnrollStart, error) {
	body, err := json.Marshal(map[string]any{"device_id": c.deviceID, "key_alg": teamwire.KeyAlgorithm,
		"public_key": base64.StdEncoding.EncodeToString(c.PublicKey()), "name": name, "platform": platform, "client_version": clientVersion})
	if err != nil {
		return EnrollStart{}, err
	}
	var out EnrollStart
	err = c.do(http.MethodPost, teamwire.RouteEnrollStart, body, &out)
	return out, err
}

// EnrollPoll is the server's answer to a poll.
type EnrollPoll struct {
	Status string `json:"status"`
	Device *struct {
		ID         string `json:"id"`
		UserID     string `json:"user_id"`
		Name       string `json:"name"`
		ApprovedBy string `json:"approved_by"`
	} `json:"device,omitempty"`
	Organization *Organization `json:"organization,omitempty"`
}

// PollEnrollment asks whether the enrollment was decided.
func (c *Client) PollEnrollment() (EnrollPoll, error) {
	var out EnrollPoll
	err := c.do(http.MethodPost, teamwire.RouteEnrollPoll, nil, &out)
	return out, err
}

// Push sends one batch and returns the per-record results.
func (c *Client) Push(records []teamwire.PushRecord) (teamwire.PushResponse, error) {
	body, err := json.Marshal(teamwire.PushRequest{SchemaVersion: teamwire.WireVersion, Records: records})
	if err != nil {
		return teamwire.PushResponse{}, err
	}
	var out teamwire.PushResponse
	err = c.do(http.MethodPost, teamwire.RoutePush, body, &out)
	return out, err
}

// Pull asks for the memory rows after cursor (team item 5 decision 4).
func (c *Client) Pull(cursor int64, kinds []string) (teamwire.PullResponse, error) {
	body, err := json.Marshal(teamwire.PullRequest{SchemaVersion: teamwire.WireVersion, Cursor: cursor, Kinds: kinds})
	if err != nil {
		return teamwire.PullResponse{}, err
	}
	var out teamwire.PullResponse
	err = c.do(http.MethodPost, teamwire.RoutePull, body, &out)
	return out, err
}

// Ack records that every row up to cursor was applied (decision 16's one progress owner).
func (c *Client) Ack(cursor int64) (teamwire.AckResponse, error) {
	body, err := json.Marshal(teamwire.AckRequest{SchemaVersion: teamwire.WireVersion, Cursor: cursor})
	if err != nil {
		return teamwire.AckResponse{}, err
	}
	var out teamwire.AckResponse
	err = c.do(http.MethodPost, teamwire.RouteAck, body, &out)
	return out, err
}

// VerifyDeletion asks how far one deletion has reached (decision 16).
func (c *Client) VerifyDeletion(recordID string) (teamwire.VerifyResponse, error) {
	body, err := json.Marshal(teamwire.VerifyRequest{SchemaVersion: teamwire.WireVersion, RecordID: recordID})
	if err != nil {
		return teamwire.VerifyResponse{}, err
	}
	var out teamwire.VerifyResponse
	err = c.do(http.MethodPost, teamwire.RouteDeletionsVerify, body, &out)
	return out, err
}

// RevokeSelf tells the server this device is unlinking. The server deletes nothing.
func (c *Client) RevokeSelf() error {
	return c.do(http.MethodPost, teamwire.RouteDevicesRevoke, nil, nil)
}

// --- the bundle catalog (item 3c) -----------------------------------------------------

// Catalog pulls the signed catalog: available bundles, no bodies (§5.16.4). The
// signature covers the route pattern literal.
func (c *Client) Catalog() (teamwire.BundleCatalog, error) {
	var out teamwire.BundleCatalog
	err := c.do(http.MethodGet, teamwire.RouteBundles, nil, &out)
	return out, err
}

// SignedBundle fetches one bundle's ORIGINAL signed document bytes by id — the
// attestation the device verifies directly with its pinned org key (item 3c,
// design (b)). The signature covers the route pattern literal; the URL carries
// the concrete id.
func (c *Client) SignedBundle(id string) ([]byte, error) {
	nonce, err := teamwire.NewNonce()
	if err != nil {
		return nil, err
	}
	sr := teamwire.SignedRequest{Origin: c.origin, DeviceID: c.deviceID, Method: http.MethodGet,
		Route: teamwire.RouteBundleSigned, Timestamp: c.now().Unix(), Nonce: nonce, BodySHA256: teamwire.BodyDigest(nil)}
	sig, err := teamwire.Sign(c.key, sr)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, c.origin+"/sync/v1/bundles/"+id, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(teamwire.HeaderDevice, c.deviceID)
	req.Header.Set(teamwire.HeaderTimestamp, fmt.Sprint(sr.Timestamp))
	req.Header.Set(teamwire.HeaderNonce, sr.Nonce)
	req.Header.Set(teamwire.HeaderBodyDigest, sr.BodySHA256)
	req.Header.Set(teamwire.HeaderSignature, sig)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		se := &ErrServer{Status: resp.StatusCode}
		_ = json.Unmarshal(raw, &se.Body)
		return nil, se
	}
	return raw, nil
}

// BundleDocument fetches one document's bytes by digest. The signature covers the
// route-table PATTERN (the dispatcher verifies the pattern, not the URL — §5.15.2);
// the request addresses the concrete digest, and the bytes come back raw so they
// hash to exactly what the manifest committed.
func (c *Client) BundleDocument(digest string) ([]byte, error) {
	nonce, err := teamwire.NewNonce()
	if err != nil {
		return nil, err
	}
	sr := teamwire.SignedRequest{Origin: c.origin, DeviceID: c.deviceID, Method: http.MethodGet,
		Route: teamwire.RouteBundleDocument, Timestamp: c.now().Unix(), Nonce: nonce, BodySHA256: teamwire.BodyDigest(nil)}
	sig, err := teamwire.Sign(c.key, sr)
	if err != nil {
		return nil, err
	}
	url := c.origin + "/sync/v1/bundles/document/" + digest
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(teamwire.HeaderDevice, c.deviceID)
	req.Header.Set(teamwire.HeaderTimestamp, fmt.Sprint(sr.Timestamp))
	req.Header.Set(teamwire.HeaderNonce, sr.Nonce)
	req.Header.Set(teamwire.HeaderBodyDigest, sr.BodySHA256)
	req.Header.Set(teamwire.HeaderSignature, sig)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<21))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		se := &ErrServer{Status: resp.StatusCode}
		_ = json.Unmarshal(raw, &se.Body)
		return nil, se
	}
	return raw, nil
}

// loopbackHost is the server's own definition (config.IsLoopbackHost): a loopback
// ADDRESS, or exactly "localhost". Never a name that merely starts like one —
// "127.evil.example" resolves wherever its owner says.
func loopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}
