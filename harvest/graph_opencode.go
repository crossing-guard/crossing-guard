package harvest

// OpenCode observed lineage: session.parent_id has exactly one writer
// (listOpenCodeProjectionRecords → SessionSummary.ParentID) and these
// capability methods are its first readers.

func (opencodeRuntime) Lineage(s SessionSummary) (LineageFacts, bool) {
	if s.ParentID == "" {
		return LineageFacts{}, false
	}
	return LineageFacts{
		Parent:     EdgeEndpoint{Runtime: s.Runtime, ID: s.ParentID},
		Kind:       "native-subagent",
		Provenance: EdgeProvenanceObserved,
	}, true
}

// Edges reports the spawn the session row itself states. OpenCode publishes
// no turn identity for it, so the edge is honestly unanchored.
func (opencodeRuntime) Edges(s SessionSummary) ([]Edge, error) {
	if s.ParentID == "" {
		return nil, nil
	}
	return []Edge{{
		From:       EdgeEndpoint{Runtime: s.Runtime, ID: s.ParentID},
		To:         EdgeEndpoint{Runtime: s.Runtime, ID: s.ID},
		Kind:       EdgeKindSpawned,
		Provenance: EdgeProvenanceObserved,
	}}, nil
}
