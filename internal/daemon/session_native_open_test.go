package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"crossing-guard/harvest"
)

// The desktop-app link rides the session summary onto every wire shape that
// embeds it, and a search hit copies it; a session without one keeps its old
// bytes (native-session-open-links plan §2.1, §2.2).
func TestNativeOpenRidesTheWireAndIsOmittedWhenAbsent(t *testing.T) {
	link := harvest.NativeOpenLink{URL: "app://thread/1", App: "App"}
	linked := SessionSummary{Runtime: "r", ID: "one", NativeOpen: link}
	for name, value := range map[string]any{
		"rail row":       railSession{SessionSummary: linked},
		"session detail": SessionDetail{SessionDetail: harvest.SessionDetail{SessionSummary: linked}},
		"search hit":     enrichSearchHitIdentity(SearchHit{Runtime: "r"}, linked),
	} {
		raw, err := json.Marshal(value)
		if err != nil || !strings.Contains(string(raw), `"native_open":{"url":"app://thread/1","app":"App"}`) {
			t.Fatalf("%s must carry native_open: %s (%v)", name, raw, err)
		}
	}
	for name, value := range map[string]any{
		"rail row":   railSession{SessionSummary: SessionSummary{Runtime: "r", ID: "one"}},
		"search hit": enrichSearchHitIdentity(SearchHit{Runtime: "r"}, SessionSummary{Runtime: "r", ID: "one"}),
	} {
		raw, err := json.Marshal(value)
		if err != nil || strings.Contains(string(raw), "native_open") {
			t.Fatalf("%s without a link must omit native_open: %s (%v)", name, raw, err)
		}
	}
}

// The off switch is a boolean so that a daemon.json which says false wins over
// the default; a file that omits it keeps the default.
func TestNativeOpenLinksDefaultsOnAndDecodesOff(t *testing.T) {
	dataDir := t.TempDir()
	if !defaultConsoleConfig().NativeOpenLinks {
		t.Fatal("native_open_links must default to true")
	}
	kept, _, err := parseConsoleConfig([]byte(`{}`), dataDir)
	if err != nil || !kept.NativeOpenLinks {
		t.Fatalf("a file that omits native_open_links keeps the default: %v %v", kept.NativeOpenLinks, err)
	}
	off, _, err := parseConsoleConfig([]byte(`{"native_open_links": false}`), dataDir)
	if err != nil || off.NativeOpenLinks {
		t.Fatalf("native_open_links: false must switch the control off: %v %v", off.NativeOpenLinks, err)
	}
}

// A search hit is built from the index row, which cannot say whether a session
// is a subagent. Its link therefore comes only from the scanned catalog row
// with the same runtime and catalog id, and a hit the catalog does not hold,
// or holds without a link, gets none (found on the installed daemon,
// 2026-10-04: every search hit lacked the link).
func TestSearchHitsTakeTheirLinkFromTheCatalogRow(t *testing.T) {
	link := harvest.NativeOpenLink{URL: "app://thread/parent", App: "App"}
	catalog := []SessionSummary{
		{Runtime: "r", ID: "parent", ResumeID: "thread-1", NativeOpen: link},
		// A child resumes through its parent's thread and has no link of its own.
		{Runtime: "r", ID: "child", ResumeID: "thread-1"},
		{Runtime: "other", ID: "parent"},
	}
	hits := []SearchHit{
		{Runtime: "r", ID: "parent", ResumeID: "thread-1"},
		{Runtime: "r", ID: "child", ResumeID: "thread-1"},
		{Runtime: "other", ID: "parent"},
		{Runtime: "r", ID: "not-in-catalog", ResumeID: "thread-1"},
	}
	linkSearchHits(hits, catalog)
	if hits[0].NativeOpen != link {
		t.Fatalf("the parent hit must carry its catalog row's link: %+v", hits[0].NativeOpen)
	}
	for _, hit := range hits[1:] {
		if hit.NativeOpen != (harvest.NativeOpenLink{}) {
			t.Fatalf("hit %s/%s must carry no link: %+v", hit.Runtime, hit.ID, hit.NativeOpen)
		}
	}
	linkSearchHits(hits, nil) // no completed scan: nothing changes, nothing panics
	coalescer := &scanCoalescer{}
	if got := coalescer.lastCompleted(); len(got) != 0 {
		t.Fatalf("before the first scan there is no catalog: %v", got)
	}
	coalescer.result = catalog
	if got := coalescer.lastCompleted(); len(got) != len(catalog) {
		t.Fatalf("lastCompleted must return the completed scan without scanning: %d", len(got))
	}
}
