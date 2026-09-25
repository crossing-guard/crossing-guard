package harvest

// Observed session-graph model (orchestration-agents-redesign-plan §4,
// cross-session-graph-design-review §3). Harvest emits OBSERVED edges only —
// a vendor record states the relationship. Caused edges (we launched it) live
// in the orchestration store; asserted edges (we inferred it) belong to a
// named algorithm elsewhere. No code path may promote one class into another.

// Edge kinds are data constants, not an enum: a new kind is a new constant
// plus the adapter that observes it, never a switch edit in generic code.
const (
	EdgeKindSpawned       = "spawned"
	EdgeKindMessaged      = "messaged"
	EdgeKindWaitedOn      = "waited_on"
	EdgeKindReadContextOf = "read_context_of"
)

// EdgeProvenanceObserved is the only provenance this package produces.
const EdgeProvenanceObserved = "observed"

// EdgeEndpoint names one session as a (runtime, native session id) pair.
type EdgeEndpoint struct {
	Runtime string `json:"runtime"`
	// ID is the canonical session id. Empty when the vendor record names no
	// counterparty at all (a codex wait with zero receivers, a list_threads
	// read over unenumerated threads) — an honest "no target", never a guess.
	ID string `json:"id,omitempty"`
	// Raw carries the vendor's own token when it could not be resolved to a
	// session id (a claude SendMessage `to` naming a peer by nickname).
	Raw string `json:"raw,omitempty"`
	// Unresolved marks an endpoint the vendor named but we could not resolve.
	Unresolved bool `json:"unresolved,omitempty"`
}

// EdgeObservation is one anchored sighting of an edge. A vendor may report the
// same relationship many times as a lifecycle (codex SubAgentActivity kinds
// started/interacted/repeating): that is ONE edge with several observations,
// never duplicate edges.
type EdgeObservation struct {
	// Anchor is opaque and vendor-composed; generic code compares anchors only
	// for equality. Empty when the vendor supplied no turn identity.
	Anchor string `json:"anchor,omitempty"`
	Ts     string `json:"ts,omitempty"`
	// Note is a bounded string the vendor itself published for this sighting
	// (a lifecycle word, an agents_states blob) — carried, never interpreted.
	Note string `json:"note,omitempty"`
}

// Edge is one observed relationship between two sessions.
type Edge struct {
	From       EdgeEndpoint `json:"from"`
	To         EdgeEndpoint `json:"to"`
	Kind       string       `json:"kind"`
	Provenance string       `json:"provenance"`
	// Anchor is the first observation's anchor — where in the source session
	// the relationship was first stated. Opaque, equality-only.
	Anchor       string            `json:"anchor,omitempty"`
	Observations []EdgeObservation `json:"observations,omitempty"`
}

// LineageFacts is one session's observed native lineage, reported by the
// SessionLineage capability. Every string the vendor did not publish stays
// empty — no role, nickname, or purpose is ever synthesized.
type LineageFacts struct {
	// Parent is zero-valued for a top-level session.
	Parent EdgeEndpoint `json:"parent,omitempty"`
	// Children are the native children this adapter can enumerate cheaply.
	// An adapter whose store only records the child side (codex session_meta)
	// reports none here; callers invert parent links across the scan instead.
	Children []LineageChild `json:"children,omitempty"`
	// Kind is vendor-neutral: "native-subagent" | "native-thread-spawn" | "".
	Kind       string `json:"kind,omitempty"`
	Provenance string `json:"provenance,omitempty"` // always "observed" here
	Depth      int    `json:"depth,omitempty"`      // the runtime's own reported depth, else 0
	Role       string `json:"role,omitempty"`       // vendor-published role label
	Nickname   string `json:"nickname,omitempty"`   // vendor-published nickname
}

// LineageChild identifies one native child session.
type LineageChild struct {
	ID       string `json:"id"`
	Kind     string `json:"kind,omitempty"`
	Depth    int    `json:"depth,omitempty"`
	Role     string `json:"role,omitempty"`
	Nickname string `json:"nickname,omitempty"`
}

// edgeCollector merges repeated sightings of one relationship into one edge
// with accumulated observations, preserving first-seen order.
type edgeCollector struct {
	edges []Edge
	index map[string]int
}

func newEdgeCollector() *edgeCollector {
	return &edgeCollector{index: map[string]int{}}
}

func (c *edgeCollector) observe(from, to EdgeEndpoint, kind string, obs EdgeObservation) {
	key := kind + "\x00" + to.Runtime + "\x00" + to.ID + "\x00" + to.Raw
	if i, ok := c.index[key]; ok {
		c.edges[i].Observations = append(c.edges[i].Observations, obs)
		return
	}
	c.index[key] = len(c.edges)
	c.edges = append(c.edges, Edge{
		From: from, To: to, Kind: kind, Provenance: EdgeProvenanceObserved,
		Anchor: obs.Anchor, Observations: []EdgeObservation{obs},
	})
}
