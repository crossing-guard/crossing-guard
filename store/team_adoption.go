package store

// Places that belong to a team adoption (team rest-of-release plan §4.1 decision 5).
// A place records an adoption key — "<organization id>\x1f<scope>" — when it is turned
// on for a revision whose selection an adoption wrote. Un-adopt turns those places
// off; nothing here touches a place with no key.

import (
	"database/sql"
	"errors"
)

// AdoptionKeySeparator joins the organization id and the scope in an adoption key.
const AdoptionKeySeparator = "\x1f"

// AdoptionKey is the key a place records for one (organization, scope).
func AdoptionKey(organizationID, scope string) string {
	return organizationID + AdoptionKeySeparator + scope
}

// AdoptionPlacesOff is what turning an adoption's places off changed: the managed
// places switched off and whether the review place was.
type AdoptionPlacesOff struct {
	Managed []string `json:"managed"`
	Review  bool     `json:"review"`
}

// Count is how many places were turned off.
func (off AdoptionPlacesOff) Count() int {
	count := len(off.Managed)
	if off.Review {
		count++
	}
	return count
}

// DisableAdoptionPlaces turns off every enabled place that records adoptionKey,
// through the binding owners' own switch-off writers, so each row keeps a valid state
// token. A place with another key, or none, is not read. An empty key turns nothing
// off: it is the mark of a place no adoption governs.
func (ix *Index) DisableAdoptionPlaces(adoptionKey string, now int64) (AdoptionPlacesOff, error) {
	off := AdoptionPlacesOff{Managed: []string{}}
	if adoptionKey == "" {
		return off, nil
	}
	changes, err := ix.adoptionManagedChanges(adoptionKey)
	if err != nil {
		return off, err
	}
	if len(changes) > 0 {
		writes, err := ix.PutManagedBindings(changes, now)
		if err != nil {
			return off, err
		}
		for _, write := range writes {
			off.Managed = append(off.Managed, write.Saved.BindingID)
		}
	}
	var reviewToken string
	err = ix.db.QueryRow(`SELECT state_token FROM orchestration_review_binding
		WHERE binding_id=? AND adoption_key=? AND state='enabled'`, ReviewBindingID, adoptionKey).Scan(&reviewToken)
	if errors.Is(err, sql.ErrNoRows) {
		return off, nil
	}
	if err != nil {
		return off, err
	}
	if _, err := ix.DisableReviewBinding(reviewToken, now); err != nil {
		return off, err
	}
	off.Review = true
	return off, nil
}

func (ix *Index) adoptionManagedChanges(adoptionKey string) ([]ManagedBindingChange, error) {
	rows, err := ix.db.Query(`SELECT binding_id,state_token FROM orchestration_managed_binding
		WHERE adoption_key=? AND state='enabled' ORDER BY created_at,binding_id`, adoptionKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	changes := []ManagedBindingChange{}
	for rows.Next() {
		var change ManagedBindingChange
		if err := rows.Scan(&change.BindingID, &change.Expected); err != nil {
			return nil, err
		}
		change.Disable = true
		changes = append(changes, change)
	}
	return changes, rows.Err()
}
