package daemon

// How a managed place uses a named model route (team rest-of-release plan §5.3, §5.4,
// §9): resolving the route a binding command names, admission at bind, the adoption
// key, and the checks a run start makes before any run is admitted.

import (
	"errors"
	"strconv"

	"crossing-guard/internal/modelroute"
	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/store"
)

// resolvedPlaceRoute is one route as a place stores it. route is nil for a binding or a
// chain entry that predates routes and is being restated unchanged.
type resolvedPlaceRoute struct {
	copy           store.ManagedRoute
	revisionDigest string
	problem        string
	route          *modelroute.Route
}

// label names the route for an error: its name, or the stored runtime of an entry that
// has no route yet.
func (resolved resolvedPlaceRoute) label() string {
	if resolved.route != nil {
		return resolved.route.Name
	}
	return resolved.copy.Runtime
}

// resolvePrimaryRoute resolves the route a command names for the place itself. A
// command with no route is refused — except the restatement of a stored binding the
// migration pass could not give a route, which keeps its stored copy and its problem,
// so such a place can still be re-saved, moved between versions, or turned on.
func (host *orchestrationManagedHost) resolvePrimaryRoute(command managedBindingCommand, prior *store.ManagedBinding) (resolvedPlaceRoute, error) {
	if command.RouteID == "" && command.keepUnrouted && prior != nil && prior.RouteID == "" {
		return resolvedPlaceRoute{copy: store.ManagedRoute{Runtime: prior.Runtime, Model: prior.Model,
			ThinkingEffort: prior.ThinkingEffort}, problem: prior.RouteProblem}, nil
	}
	route, err := host.routes.resolve(command.RouteID, modelroute.FamilyRuntimeModel)
	if err != nil {
		return resolvedPlaceRoute{}, err
	}
	if problem := routeEffortProblem(route.Fields); problem != "" {
		return resolvedPlaceRoute{}, errors.New("model route " + route.Name + ": " + problem)
	}
	return resolvedPlaceRoute{copy: chainEntryFor(route, ""), revisionDigest: route.RevisionDigest, route: &route}, nil
}

// resolveChainRoute resolves one fallback chain entry the same way.
func (host *orchestrationManagedHost) resolveChainRoute(entry bindingRouteEntry, index int) (resolvedPlaceRoute, error) {
	if entry.RouteID == "" && entry.kept != nil && entry.kept.RouteID == "" {
		return resolvedPlaceRoute{copy: *entry.kept}, nil
	}
	route, err := host.routes.resolve(entry.RouteID, modelroute.FamilyRuntimeModel)
	if err != nil {
		var refusal *routeRefusal
		if errors.As(err, &refusal) {
			named := *refusal
			named.Field = "routes[" + strconv.Itoa(index) + "].route_id"
			named.Message = "fallback " + strconv.Itoa(index+1) + ": " + refusal.Message
			return resolvedPlaceRoute{}, &named
		}
		return resolvedPlaceRoute{}, err
	}
	if problem := routeEffortProblem(route.Fields); problem != "" {
		return resolvedPlaceRoute{}, errors.New("fallback route " + route.Name + ": " + problem)
	}
	return resolvedPlaceRoute{copy: chainEntryFor(route, ""), revisionDigest: route.RevisionDigest, route: &route}, nil
}

// admitPlaceRoute runs route admission for one resolved route at bind. An entry with no
// route yet has no route facts and is not judged. A rulebook that cannot be read refuses
// the bind, typed: a rule that would refuse this route cannot be seen.
func (host *orchestrationManagedHost) admitPlaceRoute(resolved resolvedPlaceRoute, compiled profilefs.CompiledProfile, projectRoot string) error {
	if resolved.route == nil {
		return nil
	}
	admission, err := host.routes.admit(*resolved.route, managedRouteLocal(*resolved.route, resolved.copy.Mode),
		compiled.Requirements.Destination.Locality, projectRoot)
	if refused := admission.Refusal(); refused != nil {
		return refused
	}
	if err != nil {
		return rulebookUnreadable(err)
	}
	return nil
}

// placeAdoptionKey is the adoption key a written place records. A place that already
// carries a key keeps it, and may move to other digests only where its adoption lists
// them; a new place records the key of the revision it is turned on for, if any.
func (host *orchestrationManagedHost) placeAdoptionKey(prior *store.ManagedBinding, profileID, sourceDigest, bundleDigest string) (string, error) {
	return adoptionKeyForWrite(host.placeAdoptions(), priorAdoption(prior), profileID, sourceDigest, bundleDigest)
}

// adoptedPlace is the adoption-relevant part of a stored place, of either lane.
type adoptedPlace struct {
	key, profileID, sourceDigest, bundleDigest string
}

func priorAdoption(prior *store.ManagedBinding) *adoptedPlace {
	if prior == nil {
		return nil
	}
	return &adoptedPlace{key: prior.AdoptionKey, profileID: prior.ProfileID,
		sourceDigest: prior.ProfileSourceDigest, bundleDigest: prior.ProfileBundleDigest}
}

// adoptionKeyForWrite is the one rule both lanes write a place's adoption key by.
func adoptionKeyForWrite(adoptions placeAdoptions, prior *adoptedPlace, profileID, sourceDigest, bundleDigest string) (string, error) {
	if prior == nil || prior.key == "" || prior.profileID != profileID {
		return adoptions.KeyFor(profileID, sourceDigest, bundleDigest), nil
	}
	moved := prior.sourceDigest != sourceDigest || prior.bundleDigest != bundleDigest
	if moved && !adoptions.MoveAllowed(prior.key, profileID, sourceDigest, bundleDigest) {
		return "", &routeRefusal{Code: routeRefusalMove, Field: "revision",
			Message: "This place runs a shared agent and can move only to a version its team still shares."}
	}
	return prior.key, nil
}

// runStartRefusal is what stops a managed run before it is admitted: the place is held
// by its adoption, its route cannot be read, or admission refuses the route. observed
// names route rules that fired without refusing. Nothing here falls back to another
// model: a route that is missing refuses the run.
func (host *orchestrationManagedHost) runStartRefusal(binding store.ManagedBinding, compiled profilefs.CompiledProfile) (*routeRefusal, []string) {
	return host.attemptStartRefusal(binding, compiled, binding.RouteID, binding.Mode)
}

// attemptStartRefusal is runStartRefusal for one attempt on one entry of a place's
// chain: the first attempt runs on the place's own route, and a parked run's next
// attempt on whichever entry the outage ladder reached. Every attempt answers the
// same two questions (is the place held, is this route admitted), so a run parked
// before a bundle expired or a rule was written does not start past either.
func (host *orchestrationManagedHost) attemptStartRefusal(binding store.ManagedBinding, compiled profilefs.CompiledProfile, routeID, mode string) (*routeRefusal, []string) {
	if reason := host.placeAdoptions().HoldReason(binding.AdoptionKey, binding.ProfileID,
		binding.ProfileSourceDigest, binding.ProfileBundleDigest); reason != "" {
		return &routeRefusal{Code: reason, Message: "This place is held (" + reason + "): its shared agent is not current on this device. It starts no run until the team's bundle is refreshed; the place stays on."}, nil
	}
	if routeID == "" {
		// A place the migration could not give a route runs as before (plan §5.6 step 4).
		return nil, nil
	}
	route, err := host.routes.resolve(routeID, modelroute.FamilyRuntimeModel)
	if err != nil {
		var refusal *routeRefusal
		if errors.As(err, &refusal) {
			return refusal, nil
		}
		return &routeRefusal{Code: routeRefusalMissing, Message: "route_missing: the model route of this place could not be read: " + err.Error()}, nil
	}
	// A rulebook that cannot be read does not stop a run that was bound while it could
	// be: admit logs the read error and answers over the rules that were read.
	admission, _ := host.routes.admit(route, managedRouteLocal(route, mode),
		compiled.Requirements.Destination.Locality, binding.ProjectRoot)
	if !admission.Allowed {
		return &routeRefusal{Code: modelroute.AdmissionRefusedCode, Message: admission.Refusal().Error()}, nil
	}
	return nil, admission.Fired
}

// chainRouteMissing reports whether a fallback chain entry's route can no longer be
// read. An entry with no route yet is not missing: it runs as it was stored.
func (host *orchestrationManagedHost) chainRouteMissing(entry store.ManagedRoute) bool {
	if entry.RouteID == "" {
		return false
	}
	_, err := host.routes.resolve(entry.RouteID, modelroute.FamilyRuntimeModel)
	return err != nil
}
