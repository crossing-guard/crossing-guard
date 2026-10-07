package daemon

import (
	"testing"

	"crossing-guard/engine"
	"crossing-guard/harvest"
)

// The audit's computed memory-access=write fact had no source after 2026-07-20 and
// was retired (audit-memory-write-fact plan). A memory write is now visible only
// through a real detector: the starter's memory.write emits memory=write for an
// upsertMemory call, and the audit adds no detector-less fact beside it.
func TestAuditAddsNoDetectorlessMemoryFact(t *testing.T) {
	starter, err := engine.DefaultDetectors()
	if err != nil {
		t.Fatal(err)
	}
	priorDets := auditDetectors
	t.Cleanup(func() { _ = initAudit(priorDets) })
	if err := initAudit(starter); err != nil {
		t.Fatal(err)
	}
	if _, ok := auditDetectorKinds["builtin:index"]; ok {
		t.Fatal("the retired builtin:index kind is still registered")
	}
	detail := &SessionDetail{SessionDetail: harvest.SessionDetail{Events: []harvest.CanonicalEvent{
		{Seq: 1, Kind: "tool_call", Name: "upsertMemory", Text: `{"title":"lesson"}`},
	}}}
	sawLive := false
	for _, tag := range sessionTags(detail) {
		if tag.Key == "memory-access" || tag.Key == engine.SessionStatePrefix+"memory-access" {
			t.Fatalf("retired fact emitted: %+v", tag)
		}
		if tag.Key == "memory" && tag.Value == "write" && tag.Detector == "memory.write" {
			sawLive = true
		}
	}
	if !sawLive {
		t.Fatal("the memory.write detector must still produce memory=write for an upsertMemory call")
	}
}
