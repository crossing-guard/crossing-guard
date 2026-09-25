// Package transcriptindex owns the application policy for turning canonical runtime
// transcripts into a rebuildable search projection. Runtime discovery and parsing stay
// behind Catalog; relational persistence stays behind Repository.
package transcriptindex

import (
	"context"
	"errors"
	"time"
)

type RefreshMode string

const (
	RefreshModeIncremental RefreshMode = "incremental"
	RefreshModeForce       RefreshMode = "force"
)

const (
	DefaultMaxSourceBytes int64 = 64 << 20
	DefaultMaxDocuments         = 25_000
	DefaultMaxTextBytes   int64 = 16 << 20
)

// Limits are fixed admission ceilings for one canonical session. They are a single
// aggregate budget across all resumed/native source segments.
type Limits struct {
	MaxSourceBytes int64
	MaxDocuments   int
	MaxTextBytes   int64
}

func DefaultLimits() Limits {
	return Limits{
		MaxSourceBytes: DefaultMaxSourceBytes,
		MaxDocuments:   DefaultMaxDocuments,
		MaxTextBytes:   DefaultMaxTextBytes,
	}
}

func (l Limits) normalized() Limits {
	defaults := DefaultLimits()
	if l.MaxSourceBytes <= 0 {
		l.MaxSourceBytes = defaults.MaxSourceBytes
	}
	if l.MaxDocuments <= 0 {
		l.MaxDocuments = defaults.MaxDocuments
	}
	if l.MaxTextBytes <= 0 {
		l.MaxTextBytes = defaults.MaxTextBytes
	}
	return l
}

type LimitationKind string

const (
	LimitationUnsupportedAdapter    LimitationKind = "unsupported-adapter"
	LimitationDiscoveryIncomplete   LimitationKind = "discovery-incomplete"
	LimitationSourceTooLarge        LimitationKind = "source-too-large"
	LimitationSourceUnreadable      LimitationKind = "source-unreadable"
	LimitationSourceMutated         LimitationKind = "source-mutated"
	LimitationReadCancelled         LimitationKind = "read-cancelled"
	LimitationDocumentLimit         LimitationKind = "document-limit"
	LimitationTextLimit             LimitationKind = "text-limit"
	LimitationRepositoryBusy        LimitationKind = "repository-busy"
	LimitationRepositoryUnavailable LimitationKind = "repository-unavailable"
	LimitationInvalidProjection     LimitationKind = "invalid-projection"
)

// Limitation is safe to expose to local clients. It intentionally excludes source
// paths, transcript text, and raw error strings.
type Limitation struct {
	Kind          LimitationKind `json:"kind"`
	Runtime       string         `json:"runtime,omitempty"`
	SessionID     string         `json:"session_id,omitempty"`
	ObservedBytes int64          `json:"observed_bytes,omitempty"`
	LimitBytes    int64          `json:"limit_bytes,omitempty"`
	ObservedCount int            `json:"observed_count,omitempty"`
	LimitCount    int            `json:"limit_count,omitempty"`
}

// LimitedError keeps diagnostics available through errors.Unwrap while giving callers
// one closed, non-sensitive coverage vocabulary.
type LimitedError struct {
	Limitation Limitation
	err        error
}

func NewLimitedError(limitation Limitation, err error) error {
	return &LimitedError{Limitation: limitation, err: err}
}

func (e *LimitedError) Error() string { return "transcript index " + string(e.Limitation.Kind) }
func (e *LimitedError) Unwrap() error { return e.err }

func LimitationFromError(err error) (Limitation, bool) {
	var limited *LimitedError
	if !errors.As(err, &limited) {
		return Limitation{}, false
	}
	return limited.Limitation, true
}

type SessionKey struct {
	Runtime   string `json:"runtime"`
	SessionID string `json:"session_id"`
}

type SourceSegment struct {
	ID           string
	SourceRef    string
	UpdateMarker string
	Modified     time.Time
	SourceBytes  int64
}

// SourceSession is catalog metadata for one canonical session. SourceRef values are
// opaque to this package and never enter a search document or coverage limitation.
type SourceSession struct {
	Key         SessionKey
	CatalogID   string
	ResumeID    string
	Title       string
	TitleSource string
	Path        string
	CWD         string
	Project     string
	Modified    time.Time
	Turns       int
	SourceBytes int64
	Generation  string
	Segments    []SourceSegment
	// catalogToken is an opaque round-trip value owned by the concrete Catalog.
	// Generic refresh policy cannot inspect it. The harvest adapter uses it to retain
	// runtime-owned generation inputs without leaking vendor fields into this port.
	catalogToken any
}

type SourceEvent struct {
	Ordinal   int
	Lineage   string
	SegmentID string
	Timestamp string
	Kind      string
	Name      string
	Text      string
}

type SourceSnapshot struct {
	Session     SourceSession
	Events      []SourceEvent
	Generation  string
	SourceBytes int64
}

type Discovery struct {
	Sessions    []SourceSession
	Complete    bool
	AsOf        time.Time
	Limitations []Limitation
}

// Catalog is the consumer-owned port for optional runtime projection capabilities.
type Catalog interface {
	Discover(context.Context, Limits) (Discovery, error)
	Read(context.Context, SourceSession, Limits) (SourceSnapshot, error)
	Generation(context.Context, SourceSession, Limits) (string, error)
}

type SearchDocument struct {
	Order     int
	Timestamp string
	Kind      string
	Text      string
	Lineage   string
}

type SessionMetadata struct {
	Key       SessionKey
	CatalogID string
	ResumeID  string
	Path      string
	CWD       string
	Project   string
	Title     string
	Modified  time.Time
	Turns     int
}

type ProjectionReplacement struct {
	Session SessionMetadata
	// ExpectedGeneration is the saved marker observed while planning. It lets the
	// repository distinguish a deliberate force replacement of an already-current
	// generation from the same generation committed concurrently after planning.
	ExpectedGeneration string
	ExpectedIndexedAt  time.Time
	Generation         string
	IndexedAt          time.Time
	SourceCount        int
	SourceBytes        int64
	TextBytes          int64
	Documents          []SearchDocument
}

type ProjectionState struct {
	Key             SessionKey
	Generation      string
	IndexedAt       time.Time
	SourceCount     int
	DocumentCount   int
	LastAttemptedAt time.Time
	Limitation      *Limitation
}

type ReplaceResult struct {
	State    ProjectionState
	Replaced bool
}

// Repository is the consumer-owned atomic persistence port. Implementations replace a
// canonical session and its projection marker in one transaction, with the marker last.
type Repository interface {
	ProjectionStates(context.Context) (map[SessionKey]ProjectionState, error)
	RecordTranscriptProjectionLimitation(context.Context, SessionKey, ProjectionState,
		time.Time, Limitation) (ProjectionState, error)
	ReplaceTranscriptProjection(context.Context, ProjectionReplacement) (ReplaceResult, error)
	RemoveTranscriptOrphans(context.Context, []SessionKey) (int, error)
}

type RefreshRequest struct {
	Mode        RefreshMode
	MaxSessions int
}

type RefreshStats struct {
	Discovered int `json:"discovered"`
	Planned    int `json:"planned"`
	Updated    int `json:"updated"`
	Skipped    int `json:"skipped"`
	Failed     int `json:"failed"`
	Remaining  int `json:"remaining"`
	Orphaned   int `json:"orphaned"`
}

type CoverageState string

const (
	CoverageCurrent     CoverageState = "current"
	CoverageCatchingUp  CoverageState = "catching-up"
	CoverageStale       CoverageState = "stale"
	CoverageIncomplete  CoverageState = "incomplete"
	CoverageUnavailable CoverageState = "unavailable"
)

type SessionCoverage struct {
	Key               SessionKey    `json:"key"`
	State             CoverageState `json:"state"`
	SourceGeneration  string        `json:"source_generation,omitempty"`
	IndexedGeneration string        `json:"indexed_generation,omitempty"`
	IndexedAt         time.Time     `json:"indexed_at,omitempty"`
	Modified          time.Time     `json:"modified,omitempty"`
	Limitations       []Limitation  `json:"limitations,omitempty"`
}

type Coverage struct {
	State                   CoverageState     `json:"state"`
	DiscoveredSessions      int               `json:"discovered_sessions"`
	IndexedSessions         int               `json:"indexed_sessions"`
	LastSuccess             time.Time         `json:"last_success,omitempty"`
	NewestUnindexedModified time.Time         `json:"newest_unindexed_modified,omitempty"`
	CoverageAsOf            time.Time         `json:"coverage_as_of"`
	Limitations             []Limitation      `json:"limitations,omitempty"`
	Sessions                []SessionCoverage `json:"sessions,omitempty"`
}

type RefreshPlan struct {
	Discovery Discovery
	Sessions  []SourceSession
	States    map[SessionKey]ProjectionState
	Coverage  Coverage
	Stats     RefreshStats
}

type RefreshResult struct {
	Coverage Coverage
	Stats    RefreshStats
}

// UnavailableCoverage is the shared error-to-coverage boundary for entrypoints that
// cannot open or query a repository. Presentation layers must not invent a parallel
// state or limitation vocabulary.
func UnavailableCoverage(err error) Coverage {
	return unavailableCoverage(time.Now, err)
}
