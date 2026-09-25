package guardcli

// `crossing-guard entities` — the Phase 2 inspection surface: what resources has the system
// observed, what does it believe about each, and WHY.
//
// The "why" is the point. A label on a file is only actionable if you can see what
// produced it, so every fact prints its detector and the basis that detector used.
// Basis is DERIVED from the detector's kind, not stored: source-kind detectors are
// owner-declared path/tool maps (strong), pattern and content detectors are the
// scanning floor (weak). Storing it would have meant overloading `provenance`, which
// already carries the TRUST axis (observed vs asserted) — the same conflation that
// had to be undone once when event.provenance became event.origin.
//
// Read-only, and works with the daemon down: it opens the index read-only rather than
// asking the API, so "what does this thing think about my files" is answerable even
// when the governor is not running.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"crossing-guard/engine"
	"crossing-guard/store"
)

func cmdEntities(args []string) {
	fs := flag.NewFlagSet("entities", flag.ExitOnError)
	kind := fs.String("kind", "", "filter by entity kind (file|url|mcp)")
	limit := fs.Int("limit", 50, "max entities to list")
	asJSON := fs.Bool("json", false, "emit JSON")
	id := fs.String("id", "", "inspect ONE entity (accepts a bare path or url)")
	_ = fs.Parse(args)

	path := store.IndexPath("", homeDir())
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(os.Stderr, "no governance store at %s — nothing has been observed yet\n", path)
		os.Exit(1)
	}
	ix, err := store.OpenRO(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open index: %v\n", err)
		os.Exit(1)
	}
	defer ix.Close()

	basis := detectorBasis()

	if *id != "" {
		ent, err := ix.LookupEntity(canonicalEntityID(*id))
		if err != nil {
			fmt.Fprintf(os.Stderr, "lookup: %v\n", err)
			os.Exit(1)
		}
		if ent == nil {
			// Absence is an answer, and a different one from "no labels".
			fmt.Printf("no record of %s — never observed by a governed session\n", canonicalEntityID(*id))
			return
		}
		st, err := ix.EntityState(ent.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "state: %v\n", err)
			os.Exit(1)
		}
		one := store.EntityWithState{Entity: *ent, State: st}
		if *asJSON {
			emitJSON([]store.EntityWithState{one}, basis)
			return
		}
		printEntity(one, basis, true)
		return
	}

	list, err := ix.ListEntities(*kind, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list: %v\n", err)
		os.Exit(1)
	}
	if *asJSON {
		emitJSON(list, basis)
		return
	}
	if len(list) == 0 {
		fmt.Println("no entities observed yet (the governor records them as sessions touch files, urls and mcp tools)")
		return
	}
	for _, e := range list {
		printEntity(e, basis, false)
	}
	fmt.Printf("\n%d entit%s · store %s\n", len(list), plural(len(list), "y", "ies"), path)
}

func printEntity(e store.EntityWithState, basis map[string]string, verbose bool) {
	fmt.Printf("\n%s\n", e.Entity.ID)
	fmt.Printf("  seen   %s → %s\n", ts(e.Entity.FirstSeen), ts(e.Entity.LastSeen))
	if len(e.State) == 0 {
		// INV-21: say why it is empty rather than printing nothing.
		fmt.Printf("  facts  none — observed, but no resource-scoped detector matched it\n")
		return
	}
	for _, s := range e.State {
		b := basis[s.Detector]
		if b == "" {
			b = "unknown detector (removed or renamed since this fact was folded)"
		}
		fmt.Printf("  fact   %s=%s\n", s.Key, s.Value)
		fmt.Printf("         via %s [%s] · trust=%s\n", s.Detector, b, s.Provenance)
		if verbose && s.Evidence != "" {
			fmt.Printf("         evidence: %s\n", s.Evidence)
		}
	}
}

// detectorBasis maps detector id → how strongly its label is grounded. Derived from
// the shipped library + overlay so an operator's own detectors are classified too.
func detectorBasis() map[string]string {
	out := map[string]string{}
	loaded, err := activeDetectorDocument()
	if err != nil {
		return out
	}
	for _, d := range loaded.Detectors {
		switch d.Kind {
		case "source":
			out[d.ID] = "source-map: declared tool/path rule"
		case "destination":
			out[d.ID] = "destination map: host classification"
		case "pattern":
			out[d.ID] = "scan floor: regex over text"
		case "content":
			out[d.ID] = "scan floor: keyword in text"
		default:
			out[d.ID] = d.Kind
		}
	}
	return out
}

type entityJSON struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Identity  string `json:"identity"`
	FirstSeen int64  `json:"first_seen"`
	LastSeen  int64  `json:"last_seen"`
	Facts     []struct {
		Key      string `json:"key"`
		Value    string `json:"value"`
		Detector string `json:"detector"`
		Basis    string `json:"basis"`
		Trust    string `json:"trust"`
		Evidence string `json:"evidence,omitempty"`
	} `json:"facts"`
}

func emitJSON(list []store.EntityWithState, basis map[string]string) {
	out := make([]entityJSON, 0, len(list))
	for _, e := range list {
		j := entityJSON{ID: e.Entity.ID, Kind: e.Entity.Kind, Identity: e.Entity.Identity,
			FirstSeen: e.Entity.FirstSeen, LastSeen: e.Entity.LastSeen}
		j.Facts = j.Facts[:0]
		for _, s := range e.State {
			j.Facts = append(j.Facts, struct {
				Key      string `json:"key"`
				Value    string `json:"value"`
				Detector string `json:"detector"`
				Basis    string `json:"basis"`
				Trust    string `json:"trust"`
				Evidence string `json:"evidence,omitempty"`
			}{s.Key, s.Value, s.Detector, basis[s.Detector], s.Provenance, s.Evidence})
		}
		sort.Slice(j.Facts, func(a, b int) bool { return j.Facts[a].Key < j.Facts[b].Key })
		out = append(out, j)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

func ts(sec int64) string {
	if sec == 0 {
		return "—"
	}
	return time.Unix(sec, 0).Format("2006-01-02 15:04")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func homeDir() string { h, _ := os.UserHomeDir(); return h }

// canonicalEntityID mirrors the daemon's: a caller should not have to know the id
// scheme to ask about a file they can see.
//
// Relative paths are resolved against the CWD first. Entities are stored under
// ABSOLUTE paths (the hook reports what the runtime gave it), so `crossing-guard entities --id
// docs/x.md` from the repo root otherwise reported "never observed" for a file the
// store knew about — a false negative that reads as an authoritative answer.
func canonicalEntityID(s string) string {
	for _, p := range []string{"file:", "url:", "mcp:", "session:", "memory:", "db:"} {
		if len(s) >= len(p) && s[:len(p)] == p {
			return s
		}
	}
	if len(s) > 8 && (s[:7] == "http://" || s[:8] == "https://") {
		return engine.EntityID("url", s)
	}
	if abs, err := filepath.Abs(s); err == nil {
		s = abs
	}
	return engine.EntityID("file", s)
}
