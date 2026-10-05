package daemon

// placeAdoptions is what the orchestration hosts and the roster need to know about
// team adoptions (team rest-of-release plan §4.1 decision 5). The adoption owner (the
// team linker's adoption code) implements it; the hosts only ask. A place records an
// adoption key when it is turned on for a revision whose selection an adoption wrote;
// a place with no key is never touched by anything here.
type placeAdoptions interface {
	// KeyFor returns the adoption key a new place on this profile revision records, or
	// "" when the revision's current selection was not written by an adoption (the
	// lead's own agent, a member's imported or duplicated agent).
	KeyFor(profileID, sourceDigest, bundleDigest string) string
	// HoldReason returns why a place with adoptionKey pinned to these digests must start
	// no run — its adoption expired, or the adoption no longer lists these digests — or
	// "" when it may run. A place with an empty key is never held.
	HoldReason(adoptionKey, profileID, sourceDigest, bundleDigest string) string
	// MoveAllowed reports whether a place with adoptionKey may be moved to these
	// digests: only to revisions its adoption lists.
	MoveAllowed(adoptionKey, profileID, sourceDigest, bundleDigest string) bool
	// Origin returns where a profile's current selection came from, when an adoption
	// wrote it.
	Origin(profileID string) (placeAdoptionOrigin, bool)
}

// placeAdoptionOrigin is the adoption facts an agent's page shows, one fact per row.
// ReadOnly is true while the current selection is an adoption's (OD-21: Edit offers
// Duplicate); Released is true after Un-adopt ("no longer shared by <organization>").
type placeAdoptionOrigin struct {
	OrganizationID   string `json:"organization_id"`
	OrganizationName string `json:"organization_name,omitempty"`
	Scope            string `json:"scope"`
	BundleID         string `json:"bundle_id,omitempty"`
	Revision         int64  `json:"revision,omitempty"`
	ExpiresAt        string `json:"expires_at,omitempty"`
	ReadOnly         bool   `json:"read_only"`
	Released         bool   `json:"released"`
}

// Hold reasons a place can carry. They are data the surfaces show, not branches.
const (
	placeHoldExpired       = "adoption_expired"
	placeHoldVersionGone   = "version_no_longer_shared"
	placeHoldAdoptionEnded = "adoption_ended"
)

// noPlaceAdoptions is the answer when no adoption owner is wired (tests, an unlinked
// device with no adoption record): nothing is adopted, nothing is held.
type noPlaceAdoptions struct{}

func (noPlaceAdoptions) KeyFor(string, string, string) string             { return "" }
func (noPlaceAdoptions) HoldReason(string, string, string, string) string { return "" }
func (noPlaceAdoptions) MoveAllowed(string, string, string, string) bool  { return true }
func (noPlaceAdoptions) Origin(string) (placeAdoptionOrigin, bool) {
	return placeAdoptionOrigin{}, false
}
