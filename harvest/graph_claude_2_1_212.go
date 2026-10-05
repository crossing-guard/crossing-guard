package harvest

// Claude observed lineage and edges. Child transcripts live at
// <slug>/<parentSessionId>/subagents/agent-<agentId>.jsonl beside an optional
// agent-<agentId>.meta.json ({agentType, description, toolUseId, spawnDepth,
// requestShape, requestNonInteractive, and parentAgentId on a nested agent).
// The main collect scan deliberately keeps skipping these directories: child
// transcripts stay out of the top-level catalog; lineage only ENUMERATES them.
// (Folding them in would let the search-index projection silently attribute
// child output to the parent — graph design review §2.5.)

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

const claudeSubagentsDirName = "subagents"

type claudeSubagentMeta struct {
	AgentType     string `json:"agentType"`
	Description   string `json:"description"`
	SpawnDepth    int    `json:"spawnDepth"`
	ParentAgentID string `json:"parentAgentId"`
}

// readClaudeSubagentMeta parses one subagent's sidecar. It is the one reader
// of that file: lineage labels children with it, and the usage listing takes
// each source's role from it. ok is false when the file is absent or malformed.
func readClaudeSubagentMeta(path string) (claudeSubagentMeta, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return claudeSubagentMeta{}, false
	}
	var meta claudeSubagentMeta
	if json.Unmarshal(raw, &meta) != nil {
		return claudeSubagentMeta{}, false
	}
	return meta, true
}

// claudeSubagentFile is one child transcript under a session's directory.
type claudeSubagentFile struct {
	id, path, metaPath string
}

// claudeSubagentFiles lists a session directory's child transcripts from one
// directory listing; nothing is parsed. Lineage and usage both list with it.
func claudeSubagentFiles(sessionDir string) []claudeSubagentFile {
	dir := filepath.Join(sessionDir, claudeSubagentsDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []claudeSubagentFile
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".jsonl")
		files = append(files, claudeSubagentFile{id: id, path: filepath.Join(dir, name),
			metaPath: filepath.Join(dir, "agent-"+id+".meta.json")})
	}
	return files
}

// claudeSubagentChildren enumerates a session's native children from one
// directory listing — no child transcript is parsed.
func claudeSubagentChildren(s SessionSummary) []LineageChild {
	var children []LineageChild
	for _, file := range claudeSubagentFiles(filepath.Join(filepath.Dir(s.Path), s.ID)) {
		child := LineageChild{ID: file.id, Kind: "native-subagent"}
		// The sidecar carries vendor-published labels; absent one, the child
		// stays unlabeled — nothing is inferred from the prompt or transcript.
		if meta, ok := readClaudeSubagentMeta(file.metaPath); ok {
			child.Role = meta.AgentType
			child.Depth = meta.SpawnDepth
			child.Description = truncate(meta.Description, titleMaxLen)
			// A nested agent names the agent that launched it; the
			// session itself never stated that relationship.
			child.Via = meta.ParentAgentID
		}
		children = append(children, child)
	}
	return children
}

func (claudeRuntime) Lineage(s SessionSummary) (LineageFacts, bool) {
	facts := LineageFacts{Provenance: EdgeProvenanceObserved}
	// Child side: the path layout itself is the vendor's lineage statement.
	// (Child records carry the PARENT's sessionId, so the inner id must never
	// be trusted for identity — the same hazard the codex split fixes.)
	if dir := filepath.Dir(s.Path); filepath.Base(dir) == claudeSubagentsDirName {
		facts.Parent = EdgeEndpoint{Runtime: s.Runtime, ID: filepath.Base(filepath.Dir(dir))}
		facts.Kind = "native-subagent"
	}
	facts.Children = claudeSubagentChildren(s)
	if facts.Parent.ID == "" && len(facts.Children) == 0 {
		return LineageFacts{}, false
	}
	return facts, true
}

func (claudeRuntime) TurnAnchor(_ SessionSummary, e CanonicalEvent) (string, bool) {
	return e.TurnAnchor, e.TurnAnchor != ""
}

// Edges derives observed edges from one parent transcript: spawned from
// toolUseResult{status:"async_launched", agentId} records, messaged from
// SendMessage tool_use records. Lazy, per-session.
func (runtime claudeRuntime) Edges(s SessionSummary) ([]Edge, error) {
	f, err := os.Open(s.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	self := EdgeEndpoint{Runtime: s.Runtime, ID: runtime.CanonicalID(s)}
	// Known child ids resolve SendMessage recipients; built once, one listing.
	childIDs := map[string]bool{}
	for _, child := range claudeSubagentChildren(s) {
		childIDs[child.ID] = true
	}
	collector := newEdgeCollector()
	sc := newLineScanner(f)
	for sc.Scan() {
		var obj map[string]any
		if json.Unmarshal(sc.Bytes(), &obj) != nil {
			continue
		}
		obs := EdgeObservation{Ts: anyString(obj["timestamp"])}
		if anchor, ok := obj["uuid"].(string); ok {
			obs.Anchor = anchor
		}
		if result, ok := obj["toolUseResult"].(map[string]any); ok {
			if anyString(result["status"]) == "async_launched" {
				if agentID := anyString(result["agentId"]); agentID != "" {
					collector.observe(self, EdgeEndpoint{Runtime: s.Runtime, ID: agentID}, EdgeKindSpawned, obs)
				}
			}
		}
		msg, _ := obj["message"].(map[string]any)
		content, _ := msg["content"].([]any)
		for _, item := range content {
			m, ok := item.(map[string]any)
			if !ok || m["type"] != "tool_use" || anyString(m["name"]) != "SendMessage" {
				continue
			}
			input, _ := m["input"].(map[string]any)
			to := anyString(input["to"])
			if to == "" {
				continue
			}
			sendObs := obs
			sendObs.Note = anyString(input["summary"])
			target := EdgeEndpoint{Runtime: s.Runtime}
			if childIDs[to] {
				target.ID = to
			} else {
				// `to` may name a peer session rather than an agent id; an
				// unresolvable recipient keeps the edge, marked unresolved,
				// rather than dropping an observed message.
				target.Raw, target.Unresolved = to, true
			}
			collector.observe(self, target, EdgeKindMessaged, sendObs)
		}
	}
	return collector.edges, sc.Err()
}
