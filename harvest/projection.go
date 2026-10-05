package harvest

// Transcript search is a rebuildable projection over vendor-owned session sources.
// This file defines the optional runtime capability used to build that projection.
// Runtime stays deliberately unchanged: a runtime that cannot provide a bounded,
// generation-checked projection remains usable everywhere else and reports unsupported
// coverage to the upper layer.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"time"
)

const DefaultProjectionMaxSourceBytes int64 = 64 << 20

type ProjectionLimitationKind string

const (
	ProjectionSourceTooLarge   ProjectionLimitationKind = "source-too-large"
	ProjectionSourceUnreadable ProjectionLimitationKind = "source-unreadable"
	ProjectionSourceMutated    ProjectionLimitationKind = "source-mutated"
	ProjectionReadCancelled    ProjectionLimitationKind = "read-cancelled"
)

// ProjectionLimitation is safe semantic failure metadata. It carries bounded counts,
// never a vendor path or raw error string; callers may expose it as coverage state.
type ProjectionLimitation struct {
	Kind          ProjectionLimitationKind `json:"kind"`
	Runtime       string                   `json:"runtime,omitempty"`
	SessionID     string                   `json:"session_id,omitempty"`
	ObservedBytes int64                    `json:"observed_bytes,omitempty"`
	LimitBytes    int64                    `json:"limit_bytes,omitempty"`
}

// ProjectionError retains the underlying error for diagnostics while presenting a
// closed limitation to application code.
type ProjectionError struct {
	Limitation ProjectionLimitation
	err        error
}

func (e *ProjectionError) Error() string {
	return "transcript projection " + string(e.Limitation.Kind)
}

func (e *ProjectionError) Unwrap() error { return e.err }

func projectionError(kind ProjectionLimitationKind, runtime, sessionID string,
	observed, limit int64, err error) error {
	return &ProjectionError{Limitation: ProjectionLimitation{Kind: kind, Runtime: runtime,
		SessionID: sessionID, ObservedBytes: observed, LimitBytes: limit}, err: err}
}

type ProjectionReadLimits struct {
	MaxSourceBytes int64
}

func (l ProjectionReadLimits) normalized() ProjectionReadLimits {
	if l.MaxSourceBytes <= 0 {
		l.MaxSourceBytes = DefaultProjectionMaxSourceBytes
	}
	return l
}

// ProjectionSegment is one stable native source inside a canonical session. File-backed
// runtimes normally use one file per segment; logical runtimes may use a row identity in
// a shared database.
type ProjectionSegment struct {
	ID           string
	SourceRef    string
	UpdateMarker string
	Modified     time.Time
	SourceBytes  int64
	Summary      SessionSummary
}

// ProjectionSession is the canonical replacement unit. Summary is presentation metadata
// for the aggregate; Segments contains every native source that contributes documents.
type ProjectionSession struct {
	Runtime            string
	ID                 string
	Summary            SessionSummary
	Segments           []ProjectionSegment
	Modified           time.Time
	SourceBytes        int64
	Generation         string
	PresentationMarker string
}

// ProjectionEvent preserves canonical parser output plus deterministic native lineage.
// Lineage contains no source path.
type ProjectionEvent struct {
	Ordinal   int
	Lineage   string
	SegmentID string
	Event     CanonicalEvent
}

type ProjectionSnapshot struct {
	Session     ProjectionSession
	Events      []ProjectionEvent
	Generation  string
	SourceBytes int64
	Unparsed    int
	Usage       *SessionUsage
}

type ProjectionDiscovery struct {
	Sessions    []ProjectionSession
	Complete    bool
	Limitations []ProjectionLimitation
}

// TranscriptProjectionSource is additive by design. Generic application code discovers
// it by type assertion on the registered Runtime; adding a capable runtime requires no
// edit to Runtime or a central vendor switch.
type TranscriptProjectionSource interface {
	DiscoverTranscriptProjections(context.Context, ProjectionReadLimits) (ProjectionDiscovery, error)
	ReadTranscriptProjection(context.Context, ProjectionSession, ProjectionReadLimits) (ProjectionSnapshot, error)
	TranscriptProjectionGeneration(context.Context, ProjectionSession, ProjectionReadLimits) (string, error)
}

type projectionGroup struct {
	segments []ProjectionSegment
}

func discoverFileTranscriptProjections(ctx context.Context, rt Runtime, jobs []fileJob,
	limits ProjectionReadLimits) (ProjectionDiscovery, error) {
	limits = limits.normalized()
	out := ProjectionDiscovery{Sessions: []ProjectionSession{}, Limitations: []ProjectionLimitation{}, Complete: true}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].path < jobs[j].path })
	groups := map[string]*projectionGroup{}
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return out, projectionError(ProjectionReadCancelled, rt.Name(), "", 0, limits.MaxSourceBytes, err)
		}
		if job.size > limits.MaxSourceBytes {
			out.Complete = false
			out.Limitations = append(out.Limitations, ProjectionLimitation{Kind: ProjectionSourceTooLarge,
				Runtime: rt.Name(), ObservedBytes: job.size, LimitBytes: limits.MaxSourceBytes})
			continue
		}
		summary, ok := summaryForJob(rt, job)
		if !ok {
			if job.size == 0 {
				continue
			}
			out.Complete = false
			out.Limitations = append(out.Limitations, ProjectionLimitation{Kind: ProjectionSourceUnreadable,
				Runtime: rt.Name()})
			continue
		}
		decorateSummary(&summary)
		canonicalID := rt.CanonicalID(summary)
		if canonicalID == "" {
			out.Complete = false
			out.Limitations = append(out.Limitations, ProjectionLimitation{Kind: ProjectionSourceUnreadable,
				Runtime: rt.Name()})
			continue
		}
		segmentID := summary.SourceSegment
		if segmentID == "" {
			segmentID = summary.ID
		}
		marker := fileProjectionMarker(job.mod, job.size)
		segment := ProjectionSegment{ID: segmentID, SourceRef: job.path, UpdateMarker: marker,
			Modified: job.mod, SourceBytes: job.size, Summary: summary}
		group := groups[canonicalID]
		if group == nil {
			group = &projectionGroup{}
			groups[canonicalID] = group
		}
		group.segments = append(group.segments, segment)
	}

	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		session := aggregateProjectionSession(rt, id, groups[id].segments)
		if session.SourceBytes > limits.MaxSourceBytes {
			out.Complete = false
			out.Limitations = append(out.Limitations, ProjectionLimitation{Kind: ProjectionSourceTooLarge,
				Runtime: rt.Name(), SessionID: id, ObservedBytes: session.SourceBytes,
				LimitBytes: limits.MaxSourceBytes})
		}
		out.Sessions = append(out.Sessions, session)
	}
	sort.Slice(out.Sessions, func(i, j int) bool {
		if out.Sessions[i].Modified.Equal(out.Sessions[j].Modified) {
			return out.Sessions[i].ID < out.Sessions[j].ID
		}
		return out.Sessions[i].Modified.After(out.Sessions[j].Modified)
	})
	return out, nil
}

func aggregateProjectionSession(rt Runtime, id string, segments []ProjectionSegment) ProjectionSession {
	sort.Slice(segments, func(i, j int) bool {
		if segments[i].Modified.Equal(segments[j].Modified) {
			if segments[i].ID == segments[j].ID {
				return segments[i].SourceRef < segments[j].SourceRef
			}
			return segments[i].ID < segments[j].ID
		}
		return segments[i].Modified.Before(segments[j].Modified)
	})
	session := ProjectionSession{Runtime: rt.Name(), ID: id, Segments: segments}
	if len(segments) == 0 {
		return session
	}
	session.Summary = segments[len(segments)-1].Summary
	session.Modified = segments[len(segments)-1].Modified
	session.PresentationMarker = rt.ThreadTitle(session.Summary)
	if session.PresentationMarker != "" {
		session.Summary.Title = session.PresentationMarker
		markTitleSource(&session.Summary)
	}
	session.Summary.Lines, session.Summary.UserTurns, session.Summary.Turns = 0, 0, 0
	for _, segment := range segments {
		session.SourceBytes += segment.SourceBytes
		session.Summary.Lines += segment.Summary.Lines
		session.Summary.UserTurns += segment.Summary.UserTurns
		session.Summary.Turns += segment.Summary.Turns
	}
	session.Generation = projectionGeneration(session)
	return session
}

func projectionGeneration(session ProjectionSession) string {
	h := sha256.New()
	write := func(value string) {
		_, _ = io.WriteString(h, strconv.Itoa(len(value)))
		_, _ = io.WriteString(h, ":")
		_, _ = io.WriteString(h, value)
	}
	write(session.Runtime)
	write(session.ID)
	write(session.Summary.ID)
	write(session.Summary.ResumeID)
	write(session.Summary.Title)
	write(session.Summary.TitleSource)
	write(session.PresentationMarker)
	write(session.Summary.Cwd)
	write(session.Summary.Project)
	for _, segment := range session.Segments {
		write(segment.ID)
		write(segment.SourceRef)
		write(segment.UpdateMarker)
		write(strconv.FormatInt(segment.SourceBytes, 10))
	}
	return "sha256-v1:" + hex.EncodeToString(h.Sum(nil))
}

func fileProjectionMarker(modified time.Time, size int64) string {
	return strconv.FormatInt(size, 10) + ":" + strconv.FormatInt(modified.UnixNano(), 10)
}

func refreshFileProjectionSession(ctx context.Context, rt Runtime,
	session ProjectionSession, limits ProjectionReadLimits) (ProjectionSession, error) {
	limits = limits.normalized()
	refreshed := session
	refreshed.Segments = append([]ProjectionSegment(nil), session.Segments...)
	refreshed.SourceBytes = 0
	for index := range refreshed.Segments {
		if err := ctx.Err(); err != nil {
			return ProjectionSession{}, projectionError(ProjectionReadCancelled, session.Runtime,
				session.ID, refreshed.SourceBytes, limits.MaxSourceBytes, err)
		}
		info, err := os.Stat(refreshed.Segments[index].SourceRef)
		if err != nil || !info.Mode().IsRegular() {
			return ProjectionSession{}, projectionError(ProjectionSourceUnreadable, session.Runtime,
				session.ID, refreshed.SourceBytes, limits.MaxSourceBytes, err)
		}
		refreshed.Segments[index].Modified = info.ModTime()
		refreshed.Segments[index].SourceBytes = info.Size()
		refreshed.Segments[index].UpdateMarker = fileProjectionMarker(info.ModTime(), info.Size())
		refreshed.SourceBytes += info.Size()
		if refreshed.SourceBytes > limits.MaxSourceBytes {
			return ProjectionSession{}, projectionError(ProjectionSourceTooLarge, session.Runtime,
				session.ID, refreshed.SourceBytes, limits.MaxSourceBytes, nil)
		}
	}
	refreshed.PresentationMarker = rt.ThreadTitle(refreshed.Summary)
	refreshed.Generation = projectionGeneration(refreshed)
	return refreshed, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
	err error
}

func (r *contextReader) Read(body []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		r.err = err
		return 0, err
	}
	n, err := r.r.Read(body)
	if err != nil && !errors.Is(err, io.EOF) {
		r.err = err
	}
	return n, err
}

type projectionNormalizer func(string, io.Reader) ([]CanonicalEvent, int, *SessionUsage, error)

func readFileTranscriptProjection(ctx context.Context, rt Runtime, session ProjectionSession,
	limits ProjectionReadLimits, normalize projectionNormalizer) (ProjectionSnapshot, error) {
	limits = limits.normalized()
	current, err := refreshFileProjectionSession(ctx, rt, session, limits)
	if err != nil {
		return ProjectionSnapshot{}, err
	}
	if current.Generation != session.Generation {
		return ProjectionSnapshot{}, projectionError(ProjectionSourceMutated, session.Runtime,
			session.ID, current.SourceBytes, limits.MaxSourceBytes, nil)
	}
	snapshot := ProjectionSnapshot{Session: session, Events: []ProjectionEvent{},
		Generation: current.Generation, SourceBytes: current.SourceBytes}
	for _, segment := range current.Segments {
		file, openErr := os.Open(segment.SourceRef)
		if openErr != nil {
			return ProjectionSnapshot{}, projectionError(ProjectionSourceUnreadable, session.Runtime,
				session.ID, snapshot.SourceBytes, limits.MaxSourceBytes, openErr)
		}
		reader := &contextReader{ctx: ctx, r: io.NewSectionReader(file, 0, segment.SourceBytes)}
		events, unparsed, usage, normalizeErr := normalize(segment.SourceRef, reader)
		closeErr := file.Close()
		if normalizeErr == nil {
			normalizeErr = reader.err
		}
		if normalizeErr != nil {
			kind := ProjectionSourceUnreadable
			if errors.Is(normalizeErr, context.Canceled) || errors.Is(normalizeErr, context.DeadlineExceeded) {
				kind = ProjectionReadCancelled
			}
			return ProjectionSnapshot{}, projectionError(kind, session.Runtime, session.ID,
				snapshot.SourceBytes, limits.MaxSourceBytes, normalizeErr)
		}
		if closeErr != nil {
			return ProjectionSnapshot{}, projectionError(ProjectionSourceUnreadable, session.Runtime,
				session.ID, snapshot.SourceBytes, limits.MaxSourceBytes, closeErr)
		}
		snapshot.Unparsed += unparsed
		snapshot.Usage = mergeProjectionUsage(snapshot.Usage, usage)
		for _, event := range events {
			snapshot.Events = append(snapshot.Events, ProjectionEvent{Ordinal: len(snapshot.Events),
				Lineage:   projectionEventLineage(session.Runtime, session.ID, segment.ID, event.Seq),
				SegmentID: segment.ID, Event: event})
		}
	}
	after, err := refreshFileProjectionSession(ctx, rt, session, limits)
	if err != nil {
		return ProjectionSnapshot{}, err
	}
	if after.Generation != snapshot.Generation {
		return ProjectionSnapshot{}, projectionError(ProjectionSourceMutated, session.Runtime,
			session.ID, after.SourceBytes, limits.MaxSourceBytes, nil)
	}
	return snapshot, nil
}

func projectionEventLineage(runtime, sessionID, segmentID string, sequence int) string {
	value := runtime + "\x00" + sessionID + "\x00" + segmentID + "\x00" + strconv.Itoa(sequence)
	digest := sha256.Sum256([]byte(value))
	return "sha256-v1:" + hex.EncodeToString(digest[:])
}

func mergeProjectionUsage(total, next *SessionUsage) *SessionUsage {
	if next == nil {
		return total
	}
	if total == nil {
		total = &SessionUsage{}
	}
	addUsageCounts(total, next)
	mixed := false
	addSessionCost(total, next.Cost, &mixed)
	if next.Context > 0 {
		total.Context = next.Context
	}
	if next.ContextWindow > 0 {
		total.ContextWindow = next.ContextWindow
	}
	if next.Model != "" {
		total.Model = next.Model
	}
	finishUsage(total)
	return total
}

func projectionLimitation(err error) (ProjectionLimitation, bool) {
	var projectionErr *ProjectionError
	if !errors.As(err, &projectionErr) {
		return ProjectionLimitation{}, false
	}
	return projectionErr.Limitation, true
}

func validateProjectionSession(runtime string, session ProjectionSession) error {
	if session.Runtime != runtime || session.ID == "" || len(session.Segments) == 0 {
		return fmt.Errorf("invalid transcript projection identity")
	}
	return nil
}
