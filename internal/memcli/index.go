package memcli

// index.go — the episodic index (ADR 0013 D1) over crossing-guard/store:
// embedded CGo-free SQLite (ncruces + registered FTS5), replacing the old
// sqlite3-CLI shell-out (code-organization-v1 M3). The file location and
// schema are unchanged, so existing indexes keep working and the file
// stays readable with the system sqlite3 tool.

import (
	"context"
	"os"

	"crossing-guard/internal/transcriptindex"
	"crossing-guard/store"
)

// indexDB resolves the index through the ONE shared resolver. The CLI has no --data
// flag, so it passes "" — env, then the product default. If the daemon is run with an
// explicit --data, the CLI will NOT follow it there; that divergence is inherent to
// one process having the flag and the other not, and the daemon logs its resolved
// path at startup so it is visible rather than silent.
func indexDB() string { return store.IndexPath("", home()) }

func indexExists() bool { _, err := os.Stat(indexDB()); return err == nil }

// refreshTranscriptIndex is the CLI recovery composition over the shared application
// use case. The daemon is the normal refresh owner; this command never recreates the
// unified store, and force mode replaces transcript projections only.
func refreshTranscriptIndex(mode transcriptindex.RefreshMode) (transcriptindex.RefreshResult, error) {
	ix, err := store.Open(indexDB())
	if err != nil {
		return transcriptindex.RefreshResult{Coverage: transcriptindex.UnavailableCoverage(err)}, err
	}
	defer ix.Close()
	indexer := transcriptindex.New(transcriptindex.NewRegisteredHarvestCatalog(),
		transcriptindex.NewStoreRepository(ix))
	return indexer.Refresh(context.Background(), transcriptindex.RefreshRequest{Mode: mode})
}

// ---------- indexed queries ----------

// The store row types keep the exact JSON contract the old -json output had.
type indexedSession = store.SessionRow
type indexedEventHit = store.EventHit

type indexedSearchResult struct {
	Coverage transcriptindex.Coverage `json:"coverage"`
	Hits     []indexedEventHit        `json:"hits"`
}

func indexListSessions(vendor, project string, limit int) ([]indexedSession, error) {
	ix, err := store.OpenRO(indexDB())
	if err != nil {
		return nil, err
	}
	defer ix.Close()
	return ix.ListSessions(vendor, project, limit)
}

func indexSearchEvents(query string, limit int) (indexedSearchResult, error) {
	ix, err := store.OpenRO(indexDB())
	if err != nil {
		return indexedSearchResult{Coverage: transcriptindex.UnavailableCoverage(err),
			Hits: []indexedEventHit{}}, err
	}
	defer ix.Close()
	indexer := transcriptindex.New(transcriptindex.NewRegisteredHarvestCatalog(),
		transcriptindex.NewStoreRepository(ix))
	plan, planErr := indexer.Plan(context.Background(), transcriptindex.RefreshRequest{
		Mode: transcriptindex.RefreshModeIncremental,
	})
	if planErr != nil {
		return indexedSearchResult{Coverage: transcriptindex.UnavailableCoverage(planErr),
			Hits: []indexedEventHit{}}, planErr
	}
	hits, searchErr := ix.SearchEvents(query, limit)
	if searchErr != nil {
		return indexedSearchResult{Coverage: transcriptindex.UnavailableCoverage(searchErr),
			Hits: []indexedEventHit{}}, searchErr
	}
	return indexedSearchResult{Coverage: plan.Coverage, Hits: hits}, nil
}
