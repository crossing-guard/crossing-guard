package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
)

// Named model routes on bindings (schema 46, team rest-of-release plan §5.3, §5.6).
//
// A binding row holds a route REFERENCE (route_id, route_revision_digest) and the
// RESOLVED COPY of that revision in the columns every reader already reads (runtime,
// model, thinking_effort; the reviewer's endpoint and model). Two writers set the
// copy: a binding write, and the functions here, which the route owner calls when a
// route changes, at start-up to repair a crash between the route file and this store,
// and once to attach routes to bindings that predate them. The route file is the truth;
// every function here is idempotent — applying the same revision twice changes nothing
// and mints no new state token.

// ManagedRouteCopy is a runtime-model route revision as managed bindings store it.
type ManagedRouteCopy struct {
	Runtime        string
	Model          string
	ThinkingEffort *ThinkingEffort
}

// ReviewRouteCopy is an inference route revision as the review binding stores it.
type ReviewRouteCopy struct {
	Endpoint string
	Model    string
}

// RouteRevision is one route's current revision, in the shape of whichever family it
// has: exactly one of Managed and Review is set.
type RouteRevision struct {
	RouteID        string
	RevisionDigest string
	Managed        *ManagedRouteCopy
	Review         *ReviewRouteCopy
}

// ReviewPathIdentity derives a review binding's request-path kind and digest from its
// endpoint, model and its own limits. The store does not know how a request path is
// built; the reviewer host's own function is passed in, so the identity the host later
// checks is computed by the code that checks it.
type ReviewPathIdentity func(binding ReviewBinding) (kind, digest string, err error)

// RouteRevisionApplied says which bindings a revision moved.
type RouteRevisionApplied struct {
	ManagedBindingIDs []string
	ReviewChanged     bool
}

// Changed reports whether any binding moved.
func (applied RouteRevisionApplied) Changed() bool {
	return len(applied.ManagedBindingIDs) > 0 || applied.ReviewChanged
}

// ErrRouteRevisionShape reports a revision that names no family or both.
var ErrRouteRevisionShape = errors.New("a route revision carries exactly one family's fields")

// ApplyRouteRevision moves every binding and chain entry that references the route to
// this revision, in ONE transaction: the digest and the resolved copy on each managed
// binding and chain entry; on the review binding the digest, the resolved copy, and its
// request-path kind and digest recomputed from the new endpoint and model and that
// binding's own limits. Every binding it changes gets a new state token, so a sheet
// holding the old one is refused by the ordinary state check. A binding already at this
// revision is left untouched, token included. A route_missing mark is cleared on a
// binding whose route is applied: the route is there.
func (ix *Index) ApplyRouteRevision(revision RouteRevision, identity ReviewPathIdentity, now int64) (RouteRevisionApplied, error) {
	if revision.RouteID == "" || revision.RevisionDigest == "" || (revision.Managed == nil) == (revision.Review == nil) {
		return RouteRevisionApplied{}, ErrRouteRevisionShape
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return RouteRevisionApplied{}, err
	}
	defer func() { _ = tx.Rollback() }()
	applied := RouteRevisionApplied{ManagedBindingIDs: []string{}}
	if revision.Managed != nil {
		applied.ManagedBindingIDs, err = applyManagedRouteRevisionTx(tx, revision, now)
	} else {
		applied.ReviewChanged, err = applyReviewRouteRevisionTx(tx, revision, identity, now)
	}
	if err != nil {
		return RouteRevisionApplied{}, err
	}
	if err := tx.Commit(); err != nil {
		return RouteRevisionApplied{}, err
	}
	return applied, nil
}

func applyManagedRouteRevisionTx(tx *sql.Tx, revision RouteRevision, now int64) ([]string, error) {
	bindings, err := managedBindingsTx(tx)
	if err != nil {
		return nil, err
	}
	changed := []string{}
	for _, binding := range bindings {
		moved := false
		if binding.RouteID == revision.RouteID && (binding.RouteRevisionDigest != revision.RevisionDigest ||
			binding.RouteProblem != "" || !managedCopyEquals(binding.Runtime, binding.Model, binding.ThinkingEffort, *revision.Managed)) {
			binding.Runtime, binding.Model = revision.Managed.Runtime, revision.Managed.Model
			binding.ThinkingEffort = copyEffort(revision.Managed.ThinkingEffort)
			binding.RouteRevisionDigest, binding.RouteProblem = revision.RevisionDigest, ""
			moved = true
		}
		for index, entry := range binding.Routes {
			if entry.RouteID != revision.RouteID || managedCopyEquals(entry.Runtime, entry.Model, entry.ThinkingEffort, *revision.Managed) {
				continue
			}
			entry.Runtime, entry.Model = revision.Managed.Runtime, revision.Managed.Model
			entry.ThinkingEffort = copyEffort(revision.Managed.ThinkingEffort)
			binding.Routes[index] = entry
			moved = true
		}
		if !moved {
			continue
		}
		if err := writeManagedRouteFieldsTx(tx, binding, now); err != nil {
			return nil, err
		}
		changed = append(changed, binding.BindingID)
	}
	return changed, nil
}

func applyReviewRouteRevisionTx(tx *sql.Tx, revision RouteRevision, identity ReviewPathIdentity, now int64) (bool, error) {
	binding, err := scanReviewBinding(tx.QueryRow(`SELECT `+reviewBindingColumns+
		` FROM orchestration_review_binding WHERE binding_id=?`, ReviewBindingID))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if binding.RouteID != revision.RouteID {
		return false, nil
	}
	prior := binding
	binding.Endpoint, binding.Model = revision.Review.Endpoint, revision.Review.Model
	binding.RouteRevisionDigest, binding.RouteProblem = revision.RevisionDigest, ""
	if err := setReviewPathIdentity(&binding, identity); err != nil {
		return false, err
	}
	if binding == prior {
		return false, nil
	}
	return true, writeReviewRouteFieldsTx(tx, binding, prior.StateToken, now)
}

// setReviewPathIdentity recomputes the request-path identity from the binding's
// endpoint, model and its own limits. A binding is never written with an identity the
// reviewer host would then refuse.
func setReviewPathIdentity(binding *ReviewBinding, identity ReviewPathIdentity) error {
	if identity == nil {
		return errors.New("a review route revision needs the request-path identity function")
	}
	kind, digest, err := identity(*binding)
	if err != nil {
		return fmt.Errorf("review request path for the route's endpoint and model: %w", err)
	}
	binding.RequestPathKind, binding.RequestPathDigest = kind, digest
	return nil
}

// AttachManagedRoutes gives a binding that predates routes its references (plan §5.6):
// primary, when set, is the route for the binding's own runtime, model and effort;
// chain maps a fallback entry's index to its route. It is a compare-and-swap: the
// binding must still carry expectedToken, each target must still have no route, and
// each route's fields must equal the stored ones — so the resolved copy does not
// change and the place runs exactly as before. Anything else is
// ErrManagedBindingConflict and nothing is written.
func (ix *Index) AttachManagedRoutes(bindingID, expectedToken string, primary *RouteRevision, chain map[int]RouteRevision, now int64) (ManagedBinding, error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return ManagedBinding{}, err
	}
	defer func() { _ = tx.Rollback() }()
	binding, err := scanManagedBinding(tx.QueryRow(`SELECT `+managedBindingColumns+
		` FROM orchestration_managed_binding WHERE binding_id=?`, bindingID))
	if errors.Is(err, sql.ErrNoRows) {
		return ManagedBinding{}, ErrManagedBindingConflict
	}
	if err != nil {
		return ManagedBinding{}, err
	}
	if binding.StateToken != expectedToken {
		return ManagedBinding{}, ErrManagedBindingConflict
	}
	if primary != nil {
		if binding.RouteID != "" || primary.Managed == nil ||
			!managedCopyEquals(binding.Runtime, binding.Model, binding.ThinkingEffort, *primary.Managed) {
			return ManagedBinding{}, ErrManagedBindingConflict
		}
		binding.RouteID, binding.RouteRevisionDigest, binding.RouteProblem = primary.RouteID, primary.RevisionDigest, ""
	}
	for index, revision := range chain {
		if index < 0 || index >= len(binding.Routes) || revision.Managed == nil {
			return ManagedBinding{}, ErrManagedBindingConflict
		}
		entry := binding.Routes[index]
		if entry.RouteID != "" || !managedCopyEquals(entry.Runtime, entry.Model, entry.ThinkingEffort, *revision.Managed) {
			return ManagedBinding{}, ErrManagedBindingConflict
		}
		binding.Routes[index].RouteID = revision.RouteID
	}
	if err := writeManagedRouteFieldsTx(tx, binding, now); err != nil {
		return ManagedBinding{}, err
	}
	if err := tx.Commit(); err != nil {
		return ManagedBinding{}, err
	}
	binding.UpdatedAt = now
	binding.StateToken = ManagedBindingStateToken(binding)
	return binding, nil
}

// AttachReviewRoute gives the review binding its route reference under the same
// compare-and-swap rule as AttachManagedRoutes, recomputing the request-path identity
// through the same function a route edit uses (plan §5.6 step 3).
func (ix *Index) AttachReviewRoute(expectedToken string, revision RouteRevision, identity ReviewPathIdentity, now int64) (ReviewBinding, error) {
	if revision.Review == nil {
		return ReviewBinding{}, ErrRouteRevisionShape
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return ReviewBinding{}, err
	}
	defer func() { _ = tx.Rollback() }()
	binding, err := scanReviewBinding(tx.QueryRow(`SELECT `+reviewBindingColumns+
		` FROM orchestration_review_binding WHERE binding_id=?`, ReviewBindingID))
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewBinding{}, ErrReviewBindingConflict
	}
	if err != nil {
		return ReviewBinding{}, err
	}
	if binding.StateToken != expectedToken || binding.RouteID != "" ||
		binding.Endpoint != revision.Review.Endpoint || binding.Model != revision.Review.Model {
		return ReviewBinding{}, ErrReviewBindingConflict
	}
	prior := binding.StateToken
	binding.RouteID, binding.RouteRevisionDigest, binding.RouteProblem = revision.RouteID, revision.RevisionDigest, ""
	if err := setReviewPathIdentity(&binding, identity); err != nil {
		return ReviewBinding{}, err
	}
	if err := writeReviewRouteFieldsTx(tx, binding, prior, now); err != nil {
		return ReviewBinding{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReviewBinding{}, err
	}
	binding.UpdatedAt = now
	binding.StateToken = ReviewBindingStateToken(binding)
	return binding, nil
}

// SetManagedRouteProblem records (or, with "", clears) a binding's route problem. A
// binding already carrying that problem is left untouched, token included, so the
// start-up pass that calls this is idempotent.
func (ix *Index) SetManagedRouteProblem(bindingID, routeProblem string, now int64) (bool, error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	binding, err := scanManagedBinding(tx.QueryRow(`SELECT `+managedBindingColumns+
		` FROM orchestration_managed_binding WHERE binding_id=?`, bindingID))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if binding.RouteProblem == routeProblem {
		return false, nil
	}
	binding.RouteProblem = routeProblem
	if err := writeManagedRouteFieldsTx(tx, binding, now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// SetReviewRouteProblem is SetManagedRouteProblem for the review binding.
func (ix *Index) SetReviewRouteProblem(routeProblem string, now int64) (bool, error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	binding, err := scanReviewBinding(tx.QueryRow(`SELECT `+reviewBindingColumns+
		` FROM orchestration_review_binding WHERE binding_id=?`, ReviewBindingID))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if binding.RouteProblem == routeProblem {
		return false, nil
	}
	prior := binding.StateToken
	binding.RouteProblem = routeProblem
	if err := writeReviewRouteFieldsTx(tx, binding, prior, now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// RouteReference is one place, or one fallback chain entry of a place, that references
// a route.
type RouteReference struct {
	RouteID   string `json:"route_id"`
	BindingID string `json:"place_id"`
	// Lane is "managed" or "review".
	Lane        string `json:"lane"`
	ProfileID   string `json:"profile_id"`
	ProjectRoot string `json:"project_root,omitempty"`
	Repository  string `json:"repository,omitempty"`
	State       string `json:"state"`
	// Fallback is true for a chain entry; Position is its 1-based place in the chain.
	Fallback bool `json:"fallback"`
	Position int  `json:"position,omitempty"`
}

// RouteReferences lists every reference any binding holds to any route: each managed
// binding's own route, each of its chain entries, and the review binding's. A binding
// with no route yet contributes nothing.
func (ix *Index) RouteReferences() ([]RouteReference, error) {
	bindings, err := ix.ManagedBindings(false)
	if err != nil {
		return nil, err
	}
	out := []RouteReference{}
	for _, binding := range bindings {
		base := RouteReference{BindingID: binding.BindingID, Lane: "managed", ProfileID: binding.ProfileID,
			ProjectRoot: binding.ProjectRoot, Repository: filepath.Base(binding.ProjectRoot), State: binding.State}
		if binding.RouteID != "" {
			reference := base
			reference.RouteID = binding.RouteID
			out = append(out, reference)
		}
		for index, entry := range binding.Routes {
			if entry.RouteID == "" {
				continue
			}
			reference := base
			reference.RouteID, reference.Fallback, reference.Position = entry.RouteID, true, index+1
			out = append(out, reference)
		}
	}
	review, found, err := ix.ReviewBinding()
	if err != nil {
		return nil, err
	}
	if found && review.RouteID != "" {
		out = append(out, RouteReference{RouteID: review.RouteID, BindingID: review.BindingID, Lane: "review",
			ProfileID: review.ProfileID, State: review.State})
	}
	return out, nil
}

func managedBindingsTx(tx *sql.Tx) ([]ManagedBinding, error) {
	rows, err := tx.Query(`SELECT ` + managedBindingColumns + ` FROM orchestration_managed_binding ORDER BY created_at,binding_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ManagedBinding{}
	for rows.Next() {
		binding, err := scanManagedBinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, binding)
	}
	return out, rows.Err()
}

// writeManagedRouteFieldsTx writes exactly the columns the route owner may set — the
// reference, the resolved copy, the chain and the problem — with a new state token.
// The compare on the prior token keeps a concurrent binding write from being lost.
func writeManagedRouteFieldsTx(tx *sql.Tx, binding ManagedBinding, now int64) error {
	prior := binding.StateToken
	routes := binding.Routes
	if routes == nil {
		routes = []ManagedRoute{}
	}
	routesJSON, err := json.Marshal(routes)
	if err != nil {
		return err
	}
	effort, err := json.Marshal(binding.ThinkingEffort)
	if err != nil {
		return err
	}
	binding.UpdatedAt = now
	token := ManagedBindingStateToken(binding)
	result, err := tx.Exec(`UPDATE orchestration_managed_binding SET runtime=?,model=?,thinking_effort=?,routes_json=?,
		route_id=?,route_revision_digest=?,route_problem=?,state_token=?,updated_at=? WHERE binding_id=? AND state_token=?`,
		binding.Runtime, binding.Model, string(effort), string(routesJSON), binding.RouteID,
		binding.RouteRevisionDigest, binding.RouteProblem, token, now, binding.BindingID, prior)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return ErrManagedBindingConflict
	}
	return nil
}

func writeReviewRouteFieldsTx(tx *sql.Tx, binding ReviewBinding, priorToken string, now int64) error {
	binding.UpdatedAt = now
	token := ReviewBindingStateToken(binding)
	result, err := tx.Exec(`UPDATE orchestration_review_binding SET endpoint=?,model=?,request_path_kind=?,
		request_path_digest=?,route_id=?,route_revision_digest=?,route_problem=?,state_token=?,updated_at=?
		WHERE binding_id=? AND state_token=?`, binding.Endpoint, binding.Model, binding.RequestPathKind,
		binding.RequestPathDigest, binding.RouteID, binding.RouteRevisionDigest, binding.RouteProblem,
		token, now, binding.BindingID, priorToken)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return ErrReviewBindingConflict
	}
	return nil
}

func managedCopyEquals(runtime, model string, effort *ThinkingEffort, route ManagedRouteCopy) bool {
	if runtime != route.Runtime || model != route.Model || (effort == nil) != (route.ThinkingEffort == nil) {
		return false
	}
	return effort == nil || *effort == *route.ThinkingEffort
}

func copyEffort(effort *ThinkingEffort) *ThinkingEffort {
	if effort == nil {
		return nil
	}
	clone := *effort
	return &clone
}
