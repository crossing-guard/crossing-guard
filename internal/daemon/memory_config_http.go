package daemon

// memory_config_http.go — GET /api/memory/config: the memory settings in one
// declared place (config-ownership plan Fix A / RT-C2/C6): the propose
// consent + per-session bound (the memory owner's, read live) and the memory
// search budgets (still the console/daemon config's recall section — the
// boundary the plan kept). The importer's throttle stays in doctor, not here.

import (
	"net/http"
)

type memoryConfigResponse struct {
	Propose memoryProposeConfigPayload `json:"propose"`
	Search  memorySearchBudgets        `json:"search"`
	Store   memoryStoreLocation        `json:"store"`
}

// memoryStoreLocation names the index this daemon actually reads — resolved
// once at start from --data or $CG_INDEX (setIndexPath), never re-derived by a
// caller. The CLI's memory reads compare it with their own store and refuse a
// mismatch (memory-reads-through-daemon plan §4.2, postwork PW-1).
type memoryStoreLocation struct {
	IndexPath string `json:"index_path"`
}

type memoryProposeConfigPayload struct {
	Enabled       bool `json:"enabled"`
	PerSessionMax int  `json:"per_session_max"`
}

type memorySearchBudgets struct {
	MemoryBodyMaxBytes   int `json:"memory_body_max_bytes"`
	MemoryExcerptBytes   int `json:"memory_excerpt_bytes"`
	MemorySearchLimitMax int `json:"memory_search_limit_max"`
}

func handleMemoryConfig(w http.ResponseWriter, _ *http.Request) {
	config, _ := memoryConfig()
	limits, _ := consoleConfig()
	writeJSON(w, memoryConfigResponse{
		Propose: memoryProposeConfigPayload{
			Enabled:       config.Propose.Enabled,
			PerSessionMax: config.Propose.PerSessionMax,
		},
		Search: memorySearchBudgets{
			MemoryBodyMaxBytes:   limits.Recall.MemoryBodyMaxBytes,
			MemoryExcerptBytes:   limits.Recall.MemoryExcerptBytes,
			MemorySearchLimitMax: limits.Recall.MemorySearchLimitMax,
		},
		Store: memoryStoreLocation{IndexPath: indexPath()},
	})
}
