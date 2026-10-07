package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Probe behavior is independent of the native providers and needs no model.
type skillsProbeFixture struct {
	futureSkillsFixture
	calls *int
}

func (p skillsProbeFixture) Probe() *ProbeInfo {
	*p.calls++
	return &ProbeInfo{OK: true, At: "test", Names: []string{"visible-skill"}}
}

func TestSkillsProbeNeutralRejectionAndSupportedRecovery(t *testing.T) {
	original := skillsProviders
	skillsProviders = map[string]SkillsProvider{}
	probeCache.Lock()
	previous := probeCache.m
	probeCache.m = map[string]*ProbeInfo{}
	probeCache.Unlock()
	t.Cleanup(func() {
		skillsProviders = original
		probeCache.Lock()
		probeCache.m = previous
		probeCache.Unlock()
	})
	calls := 0
	registerSkillsProvider(skillsProbeFixture{futureSkillsFixture{name: "available", probe: true}, &calls})
	registerSkillsProvider(skillsProbeFixture{futureSkillsFixture{name: "unsupported"}, &calls})
	for _, runtime := range []string{"unknown", "unsupported"} {
		w := httptest.NewRecorder()
		handleSkillsProbe(w, httptest.NewRequest(http.MethodPost, "/api/skills/probe", strings.NewReader(`{"runtime":"`+runtime+`"}`)))
		if w.Code != http.StatusBadRequest || w.Body.String() != "probe not available for this runtime\n" {
			t.Fatalf("rejection = %d %q", w.Code, w.Body.String())
		}
	}
	if calls != 0 {
		t.Fatalf("rejected probe invoked provider: %d", calls)
	}
	w := httptest.NewRecorder()
	handleSkillsProbe(w, httptest.NewRequest(http.MethodPost, "/api/skills/probe", strings.NewReader(`{"runtime":"available"}`)))
	var report SkillsReport
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil || w.Code != http.StatusOK || calls != 1 {
		t.Fatalf("supported recovery = %d %s calls=%d err=%v", w.Code, w.Body.String(), calls, err)
	}
	if pi := report.Probes["available"]; pi == nil || !pi.OK || pi.At != "test" || len(pi.Extra) != 1 || pi.Extra[0] != "visible-skill" {
		t.Fatalf("cache/report lost result: %+v", report.Probes)
	}
}
