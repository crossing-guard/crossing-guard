package daemon

// What the Agents roster reads from the team link (team rest-of-release plan §4.1
// decision 4, §14 Q5): which offered team agents use an id a member's own agent already
// has. The offer's own page says it too; the roster says it on the member's agent.

// rosterCollision is the organization whose offered agent uses a member's own agent's
// id. That offered agent will not be adopted and the member's agent is never replaced.
type rosterCollision struct {
	OrganizationName string `json:"organization_name,omitempty"`
}

// teamIDCollisions reads the current offer for documents the adoption will skip because
// a member's own agent uses the id, keyed by that profile id. It reads the verified
// offer already in memory: no pull, no file. With no team link it is empty.
func teamIDCollisions() map[string]rosterCollision {
	out := map[string]rosterCollision{}
	if team == nil {
		return out
	}
	team.mu.Lock()
	defer team.mu.Unlock()
	for _, offer := range team.available {
		for _, document := range offer.Documents {
			if document.State == offerDocumentIDInUse && document.ProfileID != "" {
				out[document.ProfileID] = rosterCollision{OrganizationName: team.doc.Organization.Name}
			}
		}
	}
	return out
}
