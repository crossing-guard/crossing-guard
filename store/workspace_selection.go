package store

import (
	"database/sql"
	"errors"
	"fmt"
)

var (
	ErrWorkspaceSelectionVersionConflict     = errors.New("workspace selection version conflict")
	ErrWorkspaceSelectionIdempotencyConflict = errors.New("workspace selection idempotency key reused with different input")
	ErrWorkspaceSelectionNotCurrent          = errors.New("workspace selection is not current")
	ErrWorkspaceSelectionLeased              = errors.New("workspace selection has active consumers")
	ErrWorkspaceLeaseConflict                = errors.New("workspace consumer already holds another active selection lease")
)

type WorkspaceSelectionRecord struct {
	ID               string
	SubjectKind      string
	SubjectID        string
	BindingVersion   int64
	IdempotencyKey   string
	RequestDigest    string
	RepositoryID     string
	CheckoutID       string
	Root             string
	Kind             string
	WorktreeID       string
	SourceCheckoutID string
	StartingState    string
	Branch           string
	Head             string
	Status           string
	SnapshotDigest   string
	ObservedAt       int64
	CreatedAt        int64
	UpdatedAt        int64
}

type WorkspaceSelectionLease struct {
	SelectionID      string
	SelectionVersion int64
	OwnerKind        string
	OwnerID          string
	Status           string
	AcquiredAt       int64
	ReleasedAt       int64
}

const workspaceSelectionColumns = `id,subject_kind,subject_id,binding_version,
	idempotency_key,request_digest,repository_id,checkout_id,root,kind,COALESCE(worktree_id,''),
	source_checkout_id,starting_state,branch,head,status,snapshot_digest,observed_at,created_at,updated_at`

func scanWorkspaceSelection(row interface{ Scan(...any) error }) (WorkspaceSelectionRecord, error) {
	var out WorkspaceSelectionRecord
	err := row.Scan(&out.ID, &out.SubjectKind, &out.SubjectID, &out.BindingVersion,
		&out.IdempotencyKey, &out.RequestDigest, &out.RepositoryID, &out.CheckoutID,
		&out.Root, &out.Kind, &out.WorktreeID, &out.SourceCheckoutID, &out.StartingState,
		&out.Branch, &out.Head, &out.Status, &out.SnapshotDigest, &out.ObservedAt,
		&out.CreatedAt, &out.UpdatedAt)
	return out, err
}

func validateWorkspaceSelection(in WorkspaceSelectionRecord) error {
	if in.ID == "" || in.SubjectID == "" || in.IdempotencyKey == "" || in.RequestDigest == "" ||
		in.RepositoryID == "" || in.CheckoutID == "" || in.Root == "" || in.SnapshotDigest == "" ||
		in.ObservedAt <= 0 || in.CreatedAt <= 0 {
		return fmt.Errorf("workspace selection is incomplete")
	}
	if in.SubjectKind != "task" && in.SubjectKind != "session" {
		return fmt.Errorf("invalid workspace subject kind %q", in.SubjectKind)
	}
	if in.Kind != "local" && in.Kind != "worktree" {
		return fmt.Errorf("invalid workspace kind %q", in.Kind)
	}
	if (in.Kind == "local") != (in.WorktreeID == "") {
		return fmt.Errorf("workspace kind and worktree identity disagree")
	}
	return nil
}

// BindWorkspaceSelection atomically compares the subject binding version and advances
// its immutable selection revision. An idempotent retry is resolved before the CAS,
// so it remains a retry even if the subject has subsequently moved to a newer revision.
func (ix *Index) BindWorkspaceSelection(in WorkspaceSelectionRecord, expectedVersion int64) (WorkspaceSelectionRecord, bool, error) {
	if err := validateWorkspaceSelection(in); err != nil {
		return WorkspaceSelectionRecord{}, false, err
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return WorkspaceSelectionRecord{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := scanWorkspaceSelection(tx.QueryRow(`SELECT `+workspaceSelectionColumns+`
		FROM workspace_selection WHERE subject_kind=? AND subject_id=? AND idempotency_key=?`,
		in.SubjectKind, in.SubjectID, in.IdempotencyKey))
	if err == nil {
		if existing.RequestDigest != in.RequestDigest {
			return WorkspaceSelectionRecord{}, false, ErrWorkspaceSelectionIdempotencyConflict
		}
		return existing, false, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return WorkspaceSelectionRecord{}, false, err
	}

	var currentVersion int64
	var previousID string
	err = tx.QueryRow(`SELECT version,selection_id FROM workspace_binding
		WHERE subject_kind=? AND subject_id=?`, in.SubjectKind, in.SubjectID).
		Scan(&currentVersion, &previousID)
	if errors.Is(err, sql.ErrNoRows) {
		currentVersion, previousID, err = 0, "", nil
	}
	if err != nil {
		return WorkspaceSelectionRecord{}, false, err
	}
	if currentVersion != expectedVersion {
		return WorkspaceSelectionRecord{}, false, ErrWorkspaceSelectionVersionConflict
	}
	if previousID != "" {
		var leased int
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM workspace_selection_lease
			WHERE selection_id=? AND status='active')`, previousID).Scan(&leased); err != nil {
			return WorkspaceSelectionRecord{}, false, err
		}
		if leased == 1 {
			return WorkspaceSelectionRecord{}, false, ErrWorkspaceSelectionLeased
		}
	}

	in.BindingVersion = currentVersion + 1
	in.Status = "active"
	if in.UpdatedAt == 0 {
		in.UpdatedAt = in.CreatedAt
	}
	var worktreeID any
	if in.WorktreeID != "" {
		worktreeID = in.WorktreeID
	}
	_, err = tx.Exec(`INSERT INTO workspace_selection(
		id,subject_kind,subject_id,binding_version,idempotency_key,request_digest,
		repository_id,checkout_id,root,kind,worktree_id,source_checkout_id,starting_state,
		branch,head,status,snapshot_digest,observed_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, in.ID, in.SubjectKind, in.SubjectID,
		in.BindingVersion, in.IdempotencyKey, in.RequestDigest, in.RepositoryID, in.CheckoutID,
		in.Root, in.Kind, worktreeID, in.SourceCheckoutID, in.StartingState, in.Branch, in.Head,
		in.Status, in.SnapshotDigest, in.ObservedAt, in.CreatedAt, in.UpdatedAt)
	if err != nil {
		return WorkspaceSelectionRecord{}, false, fmt.Errorf("insert workspace selection: %w", err)
	}
	if previousID != "" {
		if _, err := tx.Exec(`UPDATE workspace_selection SET status='superseded',updated_at=?
			WHERE id=? AND status='active'`, in.UpdatedAt, previousID); err != nil {
			return WorkspaceSelectionRecord{}, false, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO workspace_binding(subject_kind,subject_id,version,selection_id,updated_at)
		VALUES(?,?,?,?,?) ON CONFLICT(subject_kind,subject_id) DO UPDATE SET
		version=excluded.version,selection_id=excluded.selection_id,updated_at=excluded.updated_at`,
		in.SubjectKind, in.SubjectID, in.BindingVersion, in.ID, in.UpdatedAt); err != nil {
		return WorkspaceSelectionRecord{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return WorkspaceSelectionRecord{}, false, err
	}
	return in, true, nil
}

func (ix *Index) WorkspaceSelection(id string) (WorkspaceSelectionRecord, bool, error) {
	out, err := scanWorkspaceSelection(ix.db.QueryRow(`SELECT `+workspaceSelectionColumns+`
		FROM workspace_selection WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return WorkspaceSelectionRecord{}, false, nil
	}
	return out, err == nil, err
}

func (ix *Index) CurrentWorkspaceSelection(subjectKind, subjectID string) (WorkspaceSelectionRecord, bool, error) {
	out, err := scanWorkspaceSelection(ix.db.QueryRow(`SELECT `+workspaceSelectionColumns+`
		FROM workspace_selection WHERE id=(SELECT selection_id FROM workspace_binding
		WHERE subject_kind=? AND subject_id=?)`, subjectKind, subjectID))
	if errors.Is(err, sql.ErrNoRows) {
		return WorkspaceSelectionRecord{}, false, nil
	}
	return out, err == nil, err
}

// AcquireWorkspaceSelectionLease proves that the immutable selection is still the
// current revision before admitting a task, terminal, or mutation consumer.
func (ix *Index) AcquireWorkspaceSelectionLease(selectionID string, selectionVersion int64, ownerKind, ownerID string, at int64) (WorkspaceSelectionLease, bool, error) {
	if selectionID == "" || selectionVersion <= 0 || ownerID == "" ||
		(ownerKind != "task" && ownerKind != "terminal" && ownerKind != "mutation") || at <= 0 {
		return WorkspaceSelectionLease{}, false, fmt.Errorf("workspace lease is incomplete")
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return WorkspaceSelectionLease{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var current int
	if err := tx.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM workspace_selection s JOIN workspace_binding b
		ON b.selection_id=s.id AND b.version=s.binding_version
		WHERE s.id=? AND s.binding_version=? AND s.status='active')`, selectionID, selectionVersion).Scan(&current); err != nil {
		return WorkspaceSelectionLease{}, false, err
	}
	if current != 1 {
		return WorkspaceSelectionLease{}, false, ErrWorkspaceSelectionNotCurrent
	}
	result, err := tx.Exec(`INSERT OR IGNORE INTO workspace_selection_lease(
		selection_id,selection_version,owner_kind,owner_id,status,acquired_at,released_at)
		VALUES(?,?,?,?, 'active',?,0)`, selectionID, selectionVersion, ownerKind, ownerID, at)
	if err != nil {
		return WorkspaceSelectionLease{}, false, classifyWorkspaceLeaseError(err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return WorkspaceSelectionLease{}, false, err
	}
	var out WorkspaceSelectionLease
	err = tx.QueryRow(`SELECT selection_id,selection_version,owner_kind,owner_id,status,acquired_at,released_at
		FROM workspace_selection_lease WHERE selection_id=? AND owner_kind=? AND owner_id=?`,
		selectionID, ownerKind, ownerID).Scan(&out.SelectionID, &out.SelectionVersion,
		&out.OwnerKind, &out.OwnerID, &out.Status, &out.AcquiredAt, &out.ReleasedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkspaceSelectionLease{}, false, ErrWorkspaceLeaseConflict
	}
	if err != nil {
		return WorkspaceSelectionLease{}, false, err
	}
	if out.Status != "active" {
		return WorkspaceSelectionLease{}, false, ErrWorkspaceLeaseConflict
	}
	if err := tx.Commit(); err != nil {
		return WorkspaceSelectionLease{}, false, err
	}
	return out, rows == 1, nil
}

func classifyWorkspaceLeaseError(err error) error {
	if err == nil {
		return nil
	}
	// The partial unique owner index is the only uniqueness failure possible after
	// the same-selection primary key has been handled by INSERT OR IGNORE.
	if errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrWorkspaceLeaseConflict, err)
}

func (ix *Index) ReleaseWorkspaceSelectionLease(selectionID, ownerKind, ownerID string, at int64) (bool, error) {
	result, err := ix.db.Exec(`UPDATE workspace_selection_lease SET status='released',released_at=?
		WHERE selection_id=? AND owner_kind=? AND owner_id=? AND status='active'`,
		at, selectionID, ownerKind, ownerID)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (ix *Index) ActiveWorkspaceSelectionLeases(selectionID string) ([]WorkspaceSelectionLease, error) {
	rows, err := ix.db.Query(`SELECT selection_id,selection_version,owner_kind,owner_id,status,acquired_at,released_at
		FROM workspace_selection_lease WHERE selection_id=? AND status='active'
		ORDER BY owner_kind,owner_id`, selectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]WorkspaceSelectionLease, 0)
	for rows.Next() {
		var lease WorkspaceSelectionLease
		if err := rows.Scan(&lease.SelectionID, &lease.SelectionVersion, &lease.OwnerKind,
			&lease.OwnerID, &lease.Status, &lease.AcquiredAt, &lease.ReleasedAt); err != nil {
			return nil, err
		}
		out = append(out, lease)
	}
	return out, rows.Err()
}

// WorkspaceSubjectRoots returns only persisted daemon facts for candidate discovery.
// A session id that maps to more than one distinct checkout root is ambiguous and
// returns no authority; callers can still list explicitly configured roots.
// WorkspaceSubjectRoots keeps the existing refusal: several distinct folders
// yield no root. WorkspaceSubjectRootsDetailed says which of "none" and
// "several" happened, so a reader can be told the difference.
func (ix *Index) WorkspaceSubjectRoots(subjectKind, subjectID string) ([]string, error) {
	roots, ambiguous, err := ix.WorkspaceSubjectRootsDetailed(subjectKind, subjectID)
	if err != nil || ambiguous {
		return nil, err
	}
	return roots, nil
}

// WorkspaceSubjectRootsDetailed lists the distinct folders a subject recorded
// (at most two are read: one is a root, two is ambiguity).
func (ix *Index) WorkspaceSubjectRootsDetailed(subjectKind, subjectID string) ([]string, bool, error) {
	roots, err := ix.workspaceSubjectRootRows(subjectKind, subjectID)
	if err != nil {
		return nil, false, err
	}
	return roots, len(roots) > 1, nil
}

func (ix *Index) workspaceSubjectRootRows(subjectKind, subjectID string) ([]string, error) {
	if subjectKind == "task" {
		record, found, err := ix.RuntimeTask(subjectID)
		if err != nil || !found || record.WorkingDirectory == "" {
			return nil, err
		}
		return []string{record.WorkingDirectory}, nil
	}
	if subjectKind != "session" {
		return nil, fmt.Errorf("invalid workspace subject kind %q", subjectKind)
	}
	rows, err := ix.db.Query(`SELECT DISTINCT cwd FROM sessions
		WHERE (id=? OR catalog_id=?) AND cwd!='' ORDER BY cwd LIMIT 2`, subjectID, subjectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	roots := []string{}
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return nil, err
		}
		roots = append(roots, root)
	}
	return roots, rows.Err()
}

// ReleaseTerminalTaskWorkspaceLeases reconciles the durable cross-table truth after
// restart. Unknown is terminal in the runtime-task state machine; no PID is used to
// reclaim execution authority.
func (ix *Index) ReleaseTerminalTaskWorkspaceLeases(at int64) (int64, error) {
	result, err := ix.db.Exec(`UPDATE workspace_selection_lease SET status='released',released_at=?
		WHERE owner_kind='task' AND status='active' AND EXISTS(
		SELECT 1 FROM runtime_task t WHERE t.id=workspace_selection_lease.owner_id
		AND t.lifecycle IN ('completed','interrupted','failed','unknown'))`, at)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
