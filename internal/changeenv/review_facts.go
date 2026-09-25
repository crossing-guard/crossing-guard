package changeenv

import (
	"encoding/json"
	"sort"

	"crossing-guard/engine"
	"crossing-guard/store"
)

// StatementFacts exposes the rebuildable storage-sanitized transcript projection without
// promoting conversational text into approval, declaration, or governance evidence.
type StatementFacts struct {
	State      string                   `json:"state"`
	Reason     string                   `json:"reason,omitempty"`
	Source     string                   `json:"source"`
	Page       Page                     `json:"page"`
	Statements []store.SessionStatement `json:"statements"`
}

// BuildStatementFacts returns bounded display-only attributed statement facts.
func BuildStatementFacts(ix *store.Index, runtime, sessionID string, offset, limit int) (StatementFacts, error) {
	out := StatementFacts{State: "unavailable", Source: "rebuildable-storage-sanitized-index",
		Statements: []store.SessionStatement{}}
	if runtime == "" {
		out.Reason = "runtime is required to select one composite transcript projection"
		return out, nil
	}
	page, err := ix.SessionStatements(runtime, sessionID, offset, limit)
	if err != nil {
		return out, err
	}
	out.State = "rebuildable"
	out.Statements = page.Statements
	out.Page = Page{Total: page.Total, Returned: len(page.Statements), Offset: page.Offset,
		Limit: page.Limit, Exact: page.Offset+len(page.Statements) >= page.Total}
	if !out.Page.Exact {
		next := page.Offset + len(page.Statements)
		out.Page.NextOffset = &next
	}
	return out, nil
}

// ObservedCheck is a frozen test/build action plus current result-link metadata. The
// action's input remains behind the existing action-detail endpoint.
type ObservedCheck struct {
	EventID     int64                   `json:"event_id"`
	ObservedAt  int64                   `json:"observed_at"`
	Runtime     string                  `json:"runtime,omitempty"`
	Tool        string                  `json:"tool"`
	Verb        string                  `json:"verb"`
	Decision    string                  `json:"decision,omitempty"`
	Kinds       []string                `json:"kinds"`
	ResultState string                  `json:"result_state"`
	Results     []store.EventResultLink `json:"results"`
}

// ObservedCheckFacts is a bounded observed check-action population with result-link
// completeness kept separate from process verification.
type ObservedCheckFacts struct {
	State               string          `json:"state"`
	Reason              string          `json:"reason,omitempty"`
	Page                Page            `json:"page"`
	Checks              []ObservedCheck `json:"checks"`
	ResultLinksReturned int             `json:"result_links_returned"`
	ResultLinksTotal    int             `json:"result_links_total"`
	ResultLinksComplete bool            `json:"result_links_complete"`
}

func observedCheckKinds(tags string) []string {
	var facts []engine.Tag
	if json.Unmarshal([]byte(tags), &facts) != nil {
		return []string{}
	}
	seen := map[string]bool{}
	for _, fact := range facts {
		if fact.Value == "run" && (fact.Key == "test" || fact.Key == "build") {
			seen[fact.Key] = true
		}
	}
	out := make([]string, 0, len(seen))
	for kind := range seen {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

func checkResultState(event store.EventRecord, links []store.EventResultLink, linksComplete bool,
	activity []store.SessionActivityObservation) string {
	switch event.Decision {
	case "deny":
		return "not_executed"
	case "ask":
		return "held"
	}
	selected, indirect, ambiguous := []store.EventResultLink{}, false, false
	for _, link := range links {
		if link.Selected {
			selected = append(selected, link)
			if link.JoinClass != "exact" {
				indirect = true
			}
		} else if link.JoinClass == "ambiguous" {
			ambiguous = true
		}
	}
	if len(selected) > 0 {
		bodyMissing := true
		for _, link := range selected {
			if link.RawBytes == 0 || link.RetainedBytes > 0 {
				bodyMissing = false
				break
			}
		}
		if bodyMissing {
			return "terminal_without_retained_body"
		}
		if indirect {
			return "indirect"
		}
		return "exact"
	}
	if ambiguous {
		return "ambiguous"
	}
	if !linksComplete {
		return "unknown_due_to_bound"
	}
	for _, fact := range activity {
		if fact.State == "open" && fact.ObservedAt <= event.TS && event.TS <= fact.ValidUntil {
			return "pending"
		}
	}
	return "missing"
}

// BuildObservedCheckFacts joins frozen test/build action tags to current deterministic
// result-reconciliation metadata without rerunning detectors.
func BuildObservedCheckFacts(ix *store.Index, sessionID string, offset, limit int) (ObservedCheckFacts, error) {
	out := ObservedCheckFacts{State: "observed", Checks: []ObservedCheck{}}
	page, err := ix.TaggedEventsForSession(sessionID, []store.EventTagValue{
		{Key: "test", Value: "run"}, {Key: "build", Value: "run"},
	}, offset, limit)
	if err != nil {
		return out, err
	}
	eventIDs := make([]int64, 0, len(page.Events))
	for _, event := range page.Events {
		eventIDs = append(eventIDs, event.ID)
	}
	links, linkTotal, err := ix.ResultLinksForEvents(eventIDs)
	if err != nil {
		return out, err
	}
	out.ResultLinksReturned, out.ResultLinksTotal = len(links), linkTotal
	out.ResultLinksComplete = len(links) == linkTotal
	byEvent := map[int64][]store.EventResultLink{}
	for _, link := range links {
		byEvent[link.EventID] = append(byEvent[link.EventID], link)
	}
	activityByRuntime := map[string][]store.SessionActivityObservation{}
	for _, event := range page.Events {
		if _, loaded := activityByRuntime[event.Runtime]; !loaded && event.Runtime != "" {
			activityByRuntime[event.Runtime], err = ix.SessionActivityObservations(event.Runtime, sessionID, 10)
			if err != nil {
				return out, err
			}
		}
		rowLinks := byEvent[event.ID]
		out.Checks = append(out.Checks, ObservedCheck{EventID: event.ID, ObservedAt: event.TS,
			Runtime: event.Runtime, Tool: event.Tool, Verb: event.Verb, Decision: event.Decision,
			Kinds: observedCheckKinds(event.Tags), Results: rowLinks,
			ResultState: checkResultState(event, rowLinks, out.ResultLinksComplete,
				activityByRuntime[event.Runtime])})
	}
	out.Page = Page{Total: page.Total, Returned: len(page.Events), Offset: page.Offset,
		Limit: page.Limit, Exact: page.Offset+len(page.Events) >= page.Total}
	if !out.Page.Exact {
		next := page.Offset + len(page.Events)
		out.Page.NextOffset = &next
	}
	if page.Total == 0 {
		out.Reason = "no frozen test/build action tags were captured"
	}
	return out, nil
}
