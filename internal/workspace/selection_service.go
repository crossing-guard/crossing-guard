package workspace

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"crossing-guard/internal/changeenv"
	"crossing-guard/store"
)

type CheckoutInspector interface {
	InspectCheckout(string) (changeenv.CheckoutFacts, error)
}

type SelectionRepository interface {
	BindWorkspaceSelection(store.WorkspaceSelectionRecord, int64) (store.WorkspaceSelectionRecord, bool, error)
	WorkspaceSelection(string) (store.WorkspaceSelectionRecord, bool, error)
	CurrentWorkspaceSelection(string, string) (store.WorkspaceSelectionRecord, bool, error)
	AcquireWorkspaceSelectionLease(string, int64, string, string, int64) (store.WorkspaceSelectionLease, bool, error)
	ReleaseWorkspaceSelectionLease(string, string, string, int64) (bool, error)
}

type changeenvCheckoutInspector struct{}

func (changeenvCheckoutInspector) InspectCheckout(path string) (changeenv.CheckoutFacts, error) {
	return changeenv.InspectCheckout(path)
}

type SelectionService struct {
	config     Config
	repository SelectionRepository
	inspector  CheckoutInspector
	now        func() time.Time
	newID      func() (string, error)
}

func NewSelectionService(config Config, repository SelectionRepository) *SelectionService {
	return &SelectionService{config: config, repository: repository, inspector: changeenvCheckoutInspector{},
		now: time.Now, newID: newSelectionID}
}

// List resolves configured local roots plus daemon-supplied subject roots. Subject
// roots never broaden authority: the resolved checkout must still be inside an
// explicit allowed root. Invalid/stale roots are omitted rather than represented as
// selectable authority.
func (s *SelectionService) List(subject Subject, subjectRoots []string) (CandidateSet, error) {
	if err := validateSubject(subject); err != nil {
		return CandidateSet{}, err
	}
	type rootSource struct{ root, source string }
	roots := make([]rootSource, 0, len(s.config.AllowedRoots)+len(subjectRoots))
	for _, root := range s.config.AllowedRoots {
		roots = append(roots, rootSource{root, "configured"})
	}
	for _, root := range subjectRoots {
		roots = append(roots, rootSource{root, subject.Kind})
	}
	byCheckout := map[string]Candidate{}
	for _, item := range roots {
		facts, err := s.inspector.InspectCheckout(item.root)
		if err != nil || !s.localRootAllowed(facts.Repository.Root) {
			continue
		}
		candidate := candidateFromFacts(facts, item.source)
		if old, exists := byCheckout[candidate.CheckoutID]; !exists || old.Source == "configured" {
			byCheckout[candidate.CheckoutID] = candidate
		}
	}
	out := CandidateSet{Candidates: make([]Candidate, 0, len(byCheckout))}
	for _, candidate := range byCheckout {
		out.Candidates = append(out.Candidates, candidate)
	}
	sort.Slice(out.Candidates, func(i, j int) bool { return out.Candidates[i].Root < out.Candidates[j].Root })
	current, found, err := s.repository.CurrentWorkspaceSelection(subject.Kind, subject.ID)
	if err != nil {
		return CandidateSet{}, err
	}
	if found {
		selection := selectionFromRecord(current)
		out.Current = &selection
	}
	return out, nil
}

func (s *SelectionService) Bind(request BindRequest, subjectRoots []string) (Selection, bool, error) {
	subject := Subject{Kind: request.SubjectKind, ID: request.SubjectID}
	if err := validateSubject(subject); err != nil {
		return Selection{}, false, err
	}
	if request.CandidateID == "" || strings.TrimSpace(request.IdempotencyKey) == "" {
		return Selection{}, false, fmt.Errorf("candidate_id and idempotency_key are required")
	}
	set, err := s.List(subject, subjectRoots)
	if err != nil {
		return Selection{}, false, err
	}
	var selected *Candidate
	for i := range set.Candidates {
		if set.Candidates[i].ID == request.CandidateID {
			selected = &set.Candidates[i]
			break
		}
	}
	if selected == nil {
		return Selection{}, false, fmt.Errorf("workspace candidate is unavailable or stale")
	}
	id, err := s.newID()
	if err != nil {
		return Selection{}, false, err
	}
	now := s.now().UnixMilli()
	requestDigest := hashStrings("workspace-bind-v1", request.SubjectKind, request.SubjectID, request.CandidateID)
	record, created, err := s.repository.BindWorkspaceSelection(store.WorkspaceSelectionRecord{
		ID: id, SubjectKind: request.SubjectKind, SubjectID: request.SubjectID,
		IdempotencyKey: request.IdempotencyKey, RequestDigest: requestDigest,
		RepositoryID: selected.RepositoryID, CheckoutID: selected.CheckoutID, Root: selected.Root,
		Kind: selected.Kind, WorktreeID: selected.WorktreeID, SourceCheckoutID: selected.SourceCheckoutID,
		StartingState: selected.StartingState, Branch: selected.Branch, Head: selected.Head,
		SnapshotDigest: selected.SnapshotDigest, ObservedAt: selected.ObservedAt,
		CreatedAt: now, UpdatedAt: now,
	}, request.ExpectedVersion)
	if err != nil {
		return Selection{}, false, err
	}
	return selectionFromRecord(record), created, nil
}

// Revalidate resolves the daemon-owned root again and proves repository/checkout
// identity before a read or consumer lease. Snapshot changes are returned as fresh
// facts; identity changes fail closed.
func (s *SelectionService) Revalidate(id string, expectedVersion int64) (Candidate, error) {
	record, found, err := s.repository.WorkspaceSelection(id)
	if err != nil {
		return Candidate{}, err
	}
	if !found || record.Status != "active" || record.BindingVersion != expectedVersion {
		return Candidate{}, store.ErrWorkspaceSelectionNotCurrent
	}
	if record.Kind == "local" && !s.localRootAllowed(record.Root) {
		return Candidate{}, fmt.Errorf("selected checkout is outside configured roots")
	}
	if record.Kind == "worktree" && !pathWithin(record.Root, s.config.Worktrees.Root) {
		return Candidate{}, fmt.Errorf("selected worktree is outside the managed root")
	}
	facts, err := s.inspector.InspectCheckout(record.Root)
	if err != nil {
		return Candidate{}, err
	}
	if facts.Repository.ID != record.RepositoryID || facts.Repository.CheckoutID != record.CheckoutID || facts.Repository.Root != record.Root {
		return Candidate{}, fmt.Errorf("selected checkout identity changed")
	}
	candidate := candidateFromFacts(facts, "selection")
	candidate.Kind, candidate.WorktreeID = record.Kind, record.WorktreeID
	candidate.SourceCheckoutID, candidate.StartingState = record.SourceCheckoutID, record.StartingState
	return candidate, nil
}

// AcquireForConsumer revalidates identity before taking the durable lease. The
// returned root is therefore the only CWD a task, terminal, or mutation may use.
func (s *SelectionService) AcquireForConsumer(selectionID, ownerKind, ownerID string) (Candidate, int64, error) {
	record, found, err := s.repository.WorkspaceSelection(selectionID)
	if err != nil {
		return Candidate{}, 0, err
	}
	if !found {
		return Candidate{}, 0, store.ErrWorkspaceSelectionNotCurrent
	}
	candidate, err := s.Revalidate(selectionID, record.BindingVersion)
	if err != nil {
		return Candidate{}, 0, err
	}
	if _, _, err := s.repository.AcquireWorkspaceSelectionLease(selectionID, record.BindingVersion,
		ownerKind, ownerID, s.now().UnixMilli()); err != nil {
		return Candidate{}, 0, err
	}
	return candidate, record.BindingVersion, nil
}

func (s *SelectionService) ReleaseConsumer(selectionID, ownerKind, ownerID string) error {
	if selectionID == "" {
		return nil
	}
	_, err := s.repository.ReleaseWorkspaceSelectionLease(selectionID, ownerKind, ownerID, s.now().UnixMilli())
	return err
}

func (s *SelectionService) localRootAllowed(root string) bool {
	for _, allowed := range s.config.AllowedRoots {
		if pathWithin(root, allowed) {
			return true
		}
	}
	return false
}

func candidateFromFacts(facts changeenv.CheckoutFacts, source string) Candidate {
	return Candidate{ID: hashStrings("workspace-candidate-v1", facts.Repository.ID, facts.Repository.CheckoutID),
		RepositoryID: facts.Repository.ID, CheckoutID: facts.Repository.CheckoutID, Root: facts.Repository.Root,
		Kind: "local", Branch: facts.Branch, Head: facts.Head, Detached: facts.Detached,
		Dirty: DirtyFacts{Dirty: facts.Dirty, Staged: facts.Staged, Unstaged: facts.Unstaged,
			Untracked: facts.Untracked, Conflicted: facts.Conflicted}, SnapshotDigest: facts.SnapshotDigest,
		Source: source, ObservedAt: facts.ObservedAt, Freshness: "live"}
}

func selectionFromRecord(record store.WorkspaceSelectionRecord) Selection {
	return Selection{ID: record.ID, Subject: Subject{Kind: record.SubjectKind, ID: record.SubjectID},
		Version: record.BindingVersion, Status: record.Status, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
		Candidate: Candidate{ID: hashStrings("workspace-candidate-v1", record.RepositoryID, record.CheckoutID),
			RepositoryID: record.RepositoryID, CheckoutID: record.CheckoutID, Root: record.Root,
			Kind: record.Kind, WorktreeID: record.WorktreeID, SourceCheckoutID: record.SourceCheckoutID,
			StartingState: record.StartingState, Branch: record.Branch, Head: record.Head,
			SnapshotDigest: record.SnapshotDigest, Source: "selection", ObservedAt: record.ObservedAt,
			Freshness: "stored"}}
}

func validateSubject(subject Subject) error {
	if (subject.Kind != "task" && subject.Kind != "session") || strings.TrimSpace(subject.ID) == "" {
		return fmt.Errorf("workspace subject must be a task or session with an id")
	}
	return nil
}

func hashStrings(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return "sha256-v1:" + hex.EncodeToString(h.Sum(nil))
}

func newSelectionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "workspace_selection_" + hex.EncodeToString(raw[:]), nil
}
