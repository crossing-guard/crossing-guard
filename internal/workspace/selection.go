package workspace

type Subject struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type DirtyFacts struct {
	Dirty      bool `json:"dirty"`
	Staged     int  `json:"staged"`
	Unstaged   int  `json:"unstaged"`
	Untracked  int  `json:"untracked"`
	Conflicted int  `json:"conflicted"`
}

type Candidate struct {
	ID               string     `json:"id"`
	RepositoryID     string     `json:"repository_id"`
	CheckoutID       string     `json:"checkout_id"`
	Root             string     `json:"root"`
	Kind             string     `json:"kind"`
	WorktreeID       string     `json:"worktree_id,omitempty"`
	SourceCheckoutID string     `json:"source_checkout_id,omitempty"`
	StartingState    string     `json:"starting_state,omitempty"`
	Branch           string     `json:"branch,omitempty"`
	Head             string     `json:"head"`
	Detached         bool       `json:"detached"`
	Dirty            DirtyFacts `json:"dirty_facts"`
	SnapshotDigest   string     `json:"snapshot_digest"`
	Source           string     `json:"source"`
	ObservedAt       int64      `json:"observed_at"`
	Freshness        string     `json:"freshness"`
}

type Selection struct {
	ID        string    `json:"id"`
	Subject   Subject   `json:"subject"`
	Version   int64     `json:"version"`
	Candidate Candidate `json:"candidate"`
	Status    string    `json:"status"`
	CreatedAt int64     `json:"created_at"`
	UpdatedAt int64     `json:"updated_at"`
}

type CandidateSet struct {
	Candidates []Candidate `json:"candidates"`
	Current    *Selection  `json:"current,omitempty"`
}

type BindRequest struct {
	SubjectKind     string `json:"subject_kind"`
	SubjectID       string `json:"subject_id"`
	CandidateID     string `json:"candidate_id"`
	ExpectedVersion int64  `json:"expected_version"`
	IdempotencyKey  string `json:"idempotency_key"`
}
