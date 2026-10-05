package daemon

// The start-up pass for named model routes (team rest-of-release plan §5.3, §5.6). It
// runs after the store opens and before any orchestration host is built, and it is pure
// and idempotent — the route files are the truth, and running it twice changes nothing:
//
//  1. Migration: every managed binding, chain entry and the review binding that has no
//     route yet gets the route whose fields equal its stored ones (found, or created and
//     named from the runtime's and model's display labels). The resolved copy is what
//     the binding already ran with, so behaviour does not change. A binding the pass
//     cannot write keeps its values and runs as before, marked migration_failed.
//  2. Repair: every route's current revision is applied to the bindings that reference
//     it, which finishes a route edit that crashed between the file and the store.
//  3. Missing: a binding whose route cannot be read is marked route_missing.

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"crossing-guard/internal/modelroute"
	"crossing-guard/store"
)

// migratedRouteNameSearch bounds the " (2)", " (3)", … search for a free route name.
// A termination bound, not a policy.
const migratedRouteNameSearch = 99

// modelRouteStartReport says what one start-up pass did, for the log and for tests.
type modelRouteStartReport struct {
	RoutesCreated    int
	BindingsMigrated int
	MigrationFailed  []string
	BindingsRepaired []string
	ReviewRepaired   bool
	RouteMissing     []string
}

// prepareModelRoutes is the daemon's one start-up call: it installs the layered
// rulebook as the admission policy of this data directory and runs the start-up pass on
// its own store handle. A failure is logged and never stops the daemon: bindings keep
// their stored values, which is what they ran with.
func prepareModelRoutes(dataDir, indexFile string) {
	routes, err := newModelRoutes(dataDir)
	if err != nil {
		log.Printf("model routes unavailable (%v) — places keep their stored settings", err)
		return
	}
	routes.state.setPolicy(layeredAdmissionPolicy(dataDir))
	ix, err := store.Open(indexFile)
	if err != nil {
		log.Printf("model routes start-up pass skipped (%v) — places keep their stored settings", err)
		return
	}
	defer func() { _ = ix.Close() }()
	report, err := reconcileModelRoutes(ix, routes)
	if err != nil {
		log.Printf("model routes start-up pass did not finish (%v) — it runs again at the next start", err)
	}
	if report.RoutesCreated > 0 || report.BindingsMigrated > 0 || len(report.MigrationFailed) > 0 ||
		len(report.BindingsRepaired) > 0 || report.ReviewRepaired || len(report.RouteMissing) > 0 {
		log.Printf("model routes: %d created, %d places given a route, %d could not be migrated, %d repaired, %d missing a route",
			report.RoutesCreated, report.BindingsMigrated, len(report.MigrationFailed),
			len(report.BindingsRepaired), len(report.RouteMissing))
	}
}

// reconcileModelRoutes runs the three steps. It holds the route write lock: no bind or
// route edit interleaves with it.
func reconcileModelRoutes(ix *store.Index, routes *modelRoutes) (modelRouteStartReport, error) {
	return reconcileModelRoutesWith(ix, routes, func(revision store.RouteRevision, now int64) (store.RouteRevisionApplied, error) {
		return ix.ApplyRouteRevision(revision, reviewPathIdentity, now)
	})
}

// routeRevisionApply moves the places of one route to its current revision.
type routeRevisionApply func(revision store.RouteRevision, now int64) (store.RouteRevisionApplied, error)

// reconcileModelRoutesWith is reconcileModelRoutes with the store's apply step passed
// in. One route whose places cannot be repaired does not stop the pass: every other
// route is still repaired and the places whose route is gone are still marked, and
// the failures come back joined, so the pass is reported as unfinished.
func reconcileModelRoutesWith(ix *store.Index, routes *modelRoutes, apply routeRevisionApply) (modelRouteStartReport, error) {
	routes.state.guard.Lock()
	defer routes.state.guard.Unlock()
	report := modelRouteStartReport{}
	now := time.Now().Unix()
	if err := migrateBindingsToRoutes(ix, routes, &report, now); err != nil {
		return report, err
	}
	listed, err := routes.owner.List()
	if err != nil {
		return report, err
	}
	readable := map[string]bool{}
	var failed []error
	for _, route := range listed.Routes {
		// The route file was read: its places are not missing a route, repaired or not.
		readable[route.RouteID] = true
		applied, applyErr := apply(routeRevisionFor(route), now)
		if applyErr != nil {
			failed = append(failed, fmt.Errorf("repair bindings of route %s: %w", route.RouteID, applyErr))
			continue
		}
		report.BindingsRepaired = append(report.BindingsRepaired, applied.ManagedBindingIDs...)
		report.ReviewRepaired = report.ReviewRepaired || applied.ReviewChanged
	}
	return report, errors.Join(append(failed, markMissingRoutes(ix, readable, &report, now))...)
}

func markMissingRoutes(ix *store.Index, readable map[string]bool, report *modelRouteStartReport, now int64) error {
	bindings, err := ix.ManagedBindings(false)
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		if binding.RouteID == "" || readable[binding.RouteID] {
			continue
		}
		if _, err := ix.SetManagedRouteProblem(binding.BindingID, store.RouteProblemMissing, now); err != nil {
			return err
		}
		report.RouteMissing = append(report.RouteMissing, binding.BindingID)
	}
	review, found, err := ix.ReviewBinding()
	if err != nil {
		return err
	}
	if found && review.RouteID != "" && !readable[review.RouteID] {
		if _, err := ix.SetReviewRouteProblem(store.RouteProblemMissing, now); err != nil {
			return err
		}
		report.RouteMissing = append(report.RouteMissing, review.BindingID)
	}
	return nil
}

// migrateBindingsToRoutes is step 1. A binding it cannot give a route is marked and
// left running; only a store read failure stops the pass.
func migrateBindingsToRoutes(ix *store.Index, routes *modelRoutes, report *modelRouteStartReport, now int64) error {
	bindings, err := ix.ManagedBindings(false)
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		if err := migrateManagedBinding(ix, routes, binding, report, now); err != nil {
			report.MigrationFailed = append(report.MigrationFailed, binding.BindingID)
			log.Printf("model routes: place %s keeps its stored settings (migration failed: %v)", binding.BindingID, err)
			if _, markErr := ix.SetManagedRouteProblem(binding.BindingID, store.RouteProblemMigrationFailed, now); markErr != nil {
				log.Printf("model routes: place %s could not be marked migration_failed: %v", binding.BindingID, markErr)
			}
		}
	}
	review, found, err := ix.ReviewBinding()
	if err != nil {
		return err
	}
	if !found || review.RouteID != "" {
		return nil
	}
	if err := migrateReviewBinding(ix, routes, review, report, now); err != nil {
		report.MigrationFailed = append(report.MigrationFailed, review.BindingID)
		log.Printf("model routes: the reviewer keeps its stored settings (migration failed: %v)", err)
		if _, markErr := ix.SetReviewRouteProblem(store.RouteProblemMigrationFailed, now); markErr != nil {
			log.Printf("model routes: the reviewer could not be marked migration_failed: %v", markErr)
		}
	}
	return nil
}

func migrateManagedBinding(ix *store.Index, routes *modelRoutes, binding store.ManagedBinding, report *modelRouteStartReport, now int64) error {
	var primary *store.RouteRevision
	if binding.RouteID == "" {
		revision, err := migratedRoute(routes, modelroute.FamilyRuntimeModel, modelroute.Fields{Runtime: binding.Runtime,
			Model: binding.Model, ThinkingEffort: effortForRoute(binding.ThinkingEffort)}, report)
		if err != nil {
			return err
		}
		primary = &revision
	}
	chain := map[int]store.RouteRevision{}
	for index, entry := range binding.Routes {
		if entry.RouteID != "" {
			continue
		}
		revision, err := migratedRoute(routes, modelroute.FamilyRuntimeModel, modelroute.Fields{Runtime: entry.Runtime,
			Model: entry.Model, ThinkingEffort: effortForRoute(entry.ThinkingEffort)}, report)
		if err != nil {
			return err
		}
		chain[index] = revision
	}
	if primary == nil && len(chain) == 0 {
		return nil
	}
	if _, err := ix.AttachManagedRoutes(binding.BindingID, binding.StateToken, primary, chain, now); err != nil {
		return err
	}
	report.BindingsMigrated++
	return nil
}

func migrateReviewBinding(ix *store.Index, routes *modelRoutes, review store.ReviewBinding, report *modelRouteStartReport, now int64) error {
	revision, err := migratedRoute(routes, modelroute.FamilyInference,
		modelroute.Fields{Endpoint: review.Endpoint, Model: review.Model}, report)
	if err != nil {
		return err
	}
	// The route owner stores the canonical endpoint; a binding stored it canonical too
	// (the reviewer host writes path.Endpoint). If they differ the compare-and-swap
	// refuses, and the reviewer keeps running as it was.
	if _, err := ix.AttachReviewRoute(review.StateToken, revision, reviewPathIdentity, now); err != nil {
		return err
	}
	report.BindingsMigrated++
	return nil
}

// migratedRoute finds the route whose fields equal these, or creates it. Finding before
// creating is what makes a crashed pass safe to run again: the route it wrote is found.
func migratedRoute(routes *modelRoutes, family string, fields modelroute.Fields, report *modelRouteStartReport) (store.RouteRevision, error) {
	if found, ok, err := routes.owner.FindByFields(family, fields); err != nil {
		return store.RouteRevision{}, err
	} else if ok {
		return routeRevisionFor(found), nil
	}
	base := migratedRouteName(family, fields)
	for attempt := 1; attempt <= migratedRouteNameSearch; attempt++ {
		name := base
		if attempt > 1 {
			name += " (" + strconv.Itoa(attempt) + ")"
		}
		draft := modelroute.Draft{Name: name, Family: family, Fields: fields}
		preview, err := routes.owner.Preview(draft)
		if modelroute.IsCode(err, modelroute.CodeNameTaken) {
			continue
		}
		if err != nil {
			return store.RouteRevision{}, err
		}
		if !modelroute.SameFields(preview.Draft.Fields, fields) {
			// The owner would canonicalise a field (an endpoint spelling): the stored
			// value is not what the route would resolve to, so attaching it would
			// change what the place runs with. Nothing is created.
			return store.RouteRevision{}, errors.New("the stored settings are not in the form a model route stores")
		}
		created, err := routes.owner.Select(modelroute.SelectCommand{Draft: draft, ExpectedPreviewDigest: preview.PreviewDigest,
			ExpectedStateToken: preview.StateToken, Confirmed: true, Migrated: true})
		if err != nil {
			return store.RouteRevision{}, err
		}
		report.RoutesCreated++
		return routeRevisionFor(created.Route), nil
	}
	return store.RouteRevision{}, errors.New("no free route name within " + strconv.Itoa(migratedRouteNameSearch) + " suffixes")
}

// migratedRouteName names a migrated route from the runtime's and model's own display
// labels (plan §5.6 step 3). An inference route has no runtime; it is named for where
// it runs and its model.
func migratedRouteName(family string, fields modelroute.Fields) string {
	if family == modelroute.FamilyInference {
		return "This machine · " + fields.Model
	}
	return chatRuntimeLabel(fields.Runtime) + " · " + chatModelLabel(fields.Runtime, fields.Model)
}
