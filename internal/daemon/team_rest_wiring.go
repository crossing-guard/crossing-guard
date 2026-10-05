package daemon

import (
	"time"

	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/store"
	"crossing-guard/teamwire"
)

// Wiring between the owners the rest of the team release added (plan §4.1 decision 5,
// §6.1): the adoption owner answers the orchestration hosts, un-adopt turns places off
// through the host that caches one, and a handoff names the shared agents turned on for
// the sender's repository. Each owner stays unaware of the others; this file is the one
// place they meet.

// lazyPlaceAdoptions asks the adoption owner current at each call, so the hosts may be
// built before or after the team link starts, and a test may swap the link.
type lazyPlaceAdoptions struct{}

func (lazyPlaceAdoptions) KeyFor(profileID, sourceDigest, bundleDigest string) string {
	return currentPlaceAdoptions().KeyFor(profileID, sourceDigest, bundleDigest)
}

func (lazyPlaceAdoptions) HoldReason(adoptionKey, profileID, sourceDigest, bundleDigest string) string {
	return currentPlaceAdoptions().HoldReason(adoptionKey, profileID, sourceDigest, bundleDigest)
}

func (lazyPlaceAdoptions) MoveAllowed(adoptionKey, profileID, sourceDigest, bundleDigest string) bool {
	return currentPlaceAdoptions().MoveAllowed(adoptionKey, profileID, sourceDigest, bundleDigest)
}

func (lazyPlaceAdoptions) Origin(profileID string) (placeAdoptionOrigin, bool) {
	return currentPlaceAdoptions().Origin(profileID)
}

// hostAdoptionPlaces turns an adoption's places off in the store and then makes the
// review host re-read its binding: that host caches the enabled binding, so a store
// write alone would leave an un-adopted reviewer running until restart.
type hostAdoptionPlaces struct {
	ix     *store.Index
	review *orchestrationReviewHost
}

func (places hostAdoptionPlaces) TurnOffAdoption(adoptionKey string, now time.Time) (store.AdoptionPlacesOff, error) {
	off, err := places.ix.DisableAdoptionPlaces(adoptionKey, now.Unix())
	if places.review != nil {
		places.review.reloadBinding()
	}
	return off, err
}

// wireTeamRestOfRelease connects the owners once the hosts exist. Any of them may be
// nil (a daemon without a store, a host that failed to open); each connection is made
// only between owners that exist.
func wireTeamRestOfRelease(managed *orchestrationManagedHost, review *orchestrationReviewHost, profiles *profilefs.Owner) {
	if managed != nil {
		managed.setPlaceAdoptions(lazyPlaceAdoptions{})
	}
	if review != nil {
		review.setPlaceAdoptions(lazyPlaceAdoptions{})
	}
	if team != nil && governor != nil {
		team.mu.Lock()
		team.places = hostAdoptionPlaces{ix: governor.ix, review: review}
		team.mu.Unlock()
	}
	if governor != nil {
		handoffAgentsOf = func(detail *SessionDetail) []teamwire.HandoffAgent {
			return sharedAgentsForSession(governor.ix, profiles, detail)
		}
	}
}

// sharedAgentsForSession lists, as references, the shared agents turned on for the
// repository the sender's session worked in: the enabled places there that record an
// adoption key. It never carries a body, a route, or a place's settings.
func sharedAgentsForSession(ix *store.Index, profiles *profilefs.Owner, detail *SessionDetail) []teamwire.HandoffAgent {
	agents := []teamwire.HandoffAgent{}
	if ix == nil || detail == nil {
		return agents
	}
	// A folder has more than one spelling (a place saved as /tmp/x, a checkout git
	// reports as /private/tmp/x): the folder-identity owner compares them, as it does
	// when a place fires.
	checkout := newFolderScope(handoffCheckout(detail).root)
	bindings, err := ix.ManagedBindings(true)
	if err != nil {
		return agents
	}
	seen := map[string]bool{}
	for _, binding := range bindings {
		if binding.AdoptionKey == "" || !checkout.matchesRoot(binding.ProjectRoot) || seen[binding.ProfileID] {
			continue
		}
		seen[binding.ProfileID] = true
		agent := teamwire.HandoffAgent{ProfileID: binding.ProfileID, SourceDigest: binding.ProfileSourceDigest,
			BundleDigest: binding.ProfileBundleDigest}
		if profiles != nil {
			if revision, err := profiles.GetRevision(binding.ProfileID, binding.ProfileSourceDigest, binding.ProfileBundleDigest); err == nil {
				agent.Name = revision.Current.Name
			}
		}
		agents = append(agents, agent)
	}
	return agents
}
