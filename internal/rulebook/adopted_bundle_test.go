package rulebook

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"crossing-guard/engine"
	"crossing-guard/teamwire"
)

// The adopted-bundle record (team rest-of-release plan §4.1 decision 5, K-2): one per
// (organization, scope), written for a profile-only bundle too, migrated from format
// version 1, and never lost to a concurrent pin save.

func writeLayersV1(t *testing.T, storeDir string, doc map[string]any) {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, layersFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLayersV1MigratesAndKeepsItsRulesApplying(t *testing.T) {
	storeDir, checkout := t.TempDir(), t.TempDir()
	noUserLayer(t)
	raw, digest := layerTestDoc(t, storeDir, "legacy-org-rule", "legacy-bad")
	if _, err := StageLayer(storeDir, digest, raw); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	writeLayersV1(t, storeDir, map[string]any{
		"format_version": 1,
		"organization": map[string]any{"scope": "organization", "digest": digest,
			"adopted_at": "2026-09-01T00:00:00Z", "expires_at": expires.Format(time.RFC3339), "failure_mode": "fail-open"},
		"repositories":               map[string]any{},
		"pinned_org_key_id":          "k_legacy",
		"pinned_org_key_public_key":  "cHVi",
		"pinned_org_key_fingerprint": "AAAA-BBBB",
		"pinned_at":                  "2026-09-01T00:00:00Z",
	})
	doc, err := LoadLayers(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if doc.FormatVersion != LayersFormatVersion || len(doc.Adopted) != 1 {
		t.Fatalf("migrated document = %+v", doc)
	}
	got := doc.Adopted[0]
	if got.Scope != ScopeOrganization || got.RulebookDigest != digest || got.FailureMode != FailOpen ||
		!got.ExpiresAt.Equal(expires) || got.OrganizationID != "" || got.SignedDigest != "" || got.Revision != 0 {
		t.Fatalf("migrated record = %+v", got)
	}
	if doc.PinnedOrgKeyID != "k_legacy" || doc.PinnedOrgKeyFP != "AAAA-BBBB" {
		t.Fatalf("the pin must survive the migration: %+v", doc)
	}
	pol, _, err := LoadLayered(checkout, storeDir)
	if err != nil || len(pol.Rules) != 1 || pol.Rules[0].ID != "legacy-org-rule" || pol.Rules[0].Layer != engine.LayerOrganization {
		t.Fatalf("a migrated layer must keep deciding: %+v %v", pol, err)
	}
	// The first adoption under the new format replaces the organization-less record.
	if err := AdoptBundle(storeDir, AdoptedBundle{OrganizationID: "org_1", Scope: ScopeOrganization, BundleID: "bnd_1",
		Revision: 2, RulebookDigest: digest, ExpiresAt: expires, FailureMode: FailOpen, SignedDigest: "sha256:a"}); err != nil {
		t.Fatal(err)
	}
	doc, _ = LoadLayers(storeDir)
	if len(doc.Adopted) != 1 || doc.Adopted[0].OrganizationID != "org_1" || doc.PinnedOrgKeyID != "k_legacy" {
		t.Fatalf("after adoption = %+v", doc)
	}
	onDisk, _ := os.ReadFile(filepath.Join(storeDir, layersFile))
	if !strings.Contains(string(onDisk), `"format_version": 2`) {
		t.Fatalf("the file must be rewritten at version 2: %s", onDisk)
	}
}

func TestLayersFromANewerFormatAreRefused(t *testing.T) {
	storeDir := t.TempDir()
	writeLayersV1(t, storeDir, map[string]any{"format_version": LayersFormatVersion + 1})
	if _, err := LoadLayers(storeDir); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("a newer format must be refused by name, got %v", err)
	}
}

func TestProfileOnlyBundleHasARecordAndNoRules(t *testing.T) {
	storeDir, checkout := t.TempDir(), t.TempDir()
	noUserLayer(t)
	record := AdoptedBundle{OrganizationID: "org_1", OrganizationName: "Acme", Scope: ScopeOrganization, BundleID: "bnd_p",
		Revision: 1, KeyID: "k", FailureMode: FailOpen, ExpiresAt: time.Now().Add(time.Hour), SignedDigest: "sha256:p"}
	if err := AdoptBundle(storeDir, record); err != nil {
		t.Fatal(err)
	}
	doc, _ := LoadLayers(storeDir)
	got, found := doc.Bundle("org_1", ScopeOrganization)
	if !found || got.RulebookDigest != "" || got.BundleID != "bnd_p" || got.AdoptedAt.IsZero() {
		t.Fatalf("record = %+v found=%v", got, found)
	}
	pol, reasons, err := LoadLayered(checkout, storeDir)
	if err != nil || len(pol.Rules) != 0 || len(reasons) != 0 {
		t.Fatalf("a profile-only bundle adds no rule and no reason: %+v %v %v", pol.Rules, reasons, err)
	}
}

func TestRecordsAreKeyedByOrganizationAndScope(t *testing.T) {
	storeDir := t.TempDir()
	for _, record := range []AdoptedBundle{
		{OrganizationID: "org_a", Scope: ScopeOrganization, BundleID: "bnd_a", ExpiresAt: time.Now().Add(time.Hour)},
		{OrganizationID: "org_b", Scope: ScopeOrganization, BundleID: "bnd_b", ExpiresAt: time.Now().Add(time.Hour)},
		{OrganizationID: "org_b", Scope: "repository:repo_1", BundleID: "bnd_r", ExpiresAt: time.Now().Add(time.Hour)},
		{OrganizationID: "org_b", Scope: ScopeOrganization, BundleID: "bnd_b2", ExpiresAt: time.Now().Add(time.Hour)},
	} {
		if err := AdoptBundle(storeDir, record); err != nil {
			t.Fatal(err)
		}
	}
	doc, _ := LoadLayers(storeDir)
	if len(doc.Adopted) != 3 {
		t.Fatalf("three (organization, scope) pairs, got %+v", doc.Adopted)
	}
	if a, _ := doc.Bundle("org_a", ScopeOrganization); a.BundleID != "bnd_a" {
		t.Fatalf("another organization's record must be untouched: %+v", a)
	}
	if b, _ := doc.Bundle("org_b", ScopeOrganization); b.BundleID != "bnd_b2" {
		t.Fatalf("the same pair is replaced: %+v", b)
	}
	removed, err := UnadoptBundle(storeDir, "org_b", ScopeOrganization)
	if err != nil || !removed {
		t.Fatalf("unadopt = %v %v", removed, err)
	}
	if removed, _ := UnadoptBundle(storeDir, "org_b", ScopeOrganization); removed {
		t.Fatal("a second un-adopt removes nothing")
	}
	doc, _ = LoadLayers(storeDir)
	if _, found := doc.Bundle("org_a", ScopeOrganization); !found || len(doc.Adopted) != 2 {
		t.Fatalf("un-adopt removes one pair only: %+v", doc.Adopted)
	}
	if err := AdoptBundle(storeDir, AdoptedBundle{OrganizationID: "", Scope: ScopeOrganization}); err == nil {
		t.Fatal("a record with no organization is refused")
	}
	if err := AdoptBundle(storeDir, AdoptedBundle{OrganizationID: "org_a", Scope: "team"}); err == nil {
		t.Fatal("an unknown scope is refused")
	}
}

func TestRefreshTeamLayerChangesOnlyWhatARefreshMay(t *testing.T) {
	storeDir := t.TempDir()
	before := AdoptedBundle{OrganizationID: "org_1", Scope: ScopeOrganization, BundleID: "bnd_1", Revision: 4, KeyID: "k_old",
		FailureMode: FailClosed, ContentPolicy: json.RawMessage(`{"sync_content":"mandated"}`),
		ExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Second), SignedDigest: "sha256:one", RulebookDigest: "sha256:rules"}
	if err := AdoptBundle(storeDir, before); err != nil {
		t.Fatal(err)
	}
	later := before.ExpiresAt.Add(24 * time.Hour)
	if err := RefreshTeamLayer(storeDir, "org_1", ScopeOrganization, LayerRefresh{BundleID: "bnd_2", Revision: 5, ExpiresAt: later}); err != nil {
		t.Fatal(err)
	}
	doc, _ := LoadLayers(storeDir)
	got, _ := doc.Bundle("org_1", ScopeOrganization)
	if got.BundleID != "bnd_2" || got.Revision != 5 || !got.ExpiresAt.Equal(later) {
		t.Fatalf("refresh did not move id, revision and expiry: %+v", got)
	}
	if got.KeyID != "k_old" || got.SignedDigest != "sha256:one" || got.FailureMode != FailClosed ||
		!strings.Contains(string(got.ContentPolicy), `"mandated"`) || got.RulebookDigest != "sha256:rules" {
		t.Fatalf("a plain refresh changed a field it may not: %+v", got)
	}
	// The re-pin exception carries the new key id and digest together.
	if err := RefreshTeamLayer(storeDir, "org_1", ScopeOrganization, LayerRefresh{BundleID: "bnd_3", Revision: 6,
		ExpiresAt: later, KeyID: "k_new", SignedDigest: "sha256:two"}); err != nil {
		t.Fatal(err)
	}
	doc, _ = LoadLayers(storeDir)
	got, _ = doc.Bundle("org_1", ScopeOrganization)
	if got.KeyID != "k_new" || got.SignedDigest != "sha256:two" || got.FailureMode != FailClosed {
		t.Fatalf("re-pin refresh = %+v", got)
	}
	if err := RefreshTeamLayer(storeDir, "org_2", ScopeOrganization, LayerRefresh{BundleID: "x"}); !errors.Is(err, ErrNoAdoptedBundle) {
		t.Fatalf("refreshing nothing must say so, got %v", err)
	}
}

// A pin save and an adoption used to load, change and save separately, so one could
// overwrite the other. Both now go through UpdateLayers under the lock.
func TestConcurrentPinAndAdoptionLoseNothing(t *testing.T) {
	storeDir := t.TempDir()
	const rounds = 25
	var group sync.WaitGroup
	for round := 0; round < rounds; round++ {
		group.Add(2)
		scope := ScopeRepositoryPrefix + "repo_" + string(rune('a'+round))
		go func() {
			defer group.Done()
			if err := AdoptBundle(storeDir, AdoptedBundle{OrganizationID: "org_1", Scope: scope, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer group.Done()
			if err := UpdateLayers(storeDir, func(doc *LayersDocument) error {
				doc.PinnedOrgKeyID = "k_pinned"
				return nil
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	doc, err := LoadLayers(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Adopted) != rounds || doc.PinnedOrgKeyID != "k_pinned" {
		t.Fatalf("lost an update: %d adoptions, pin %q", len(doc.Adopted), doc.PinnedOrgKeyID)
	}
}

func TestUpdateLayersWritesNothingWhenTheChangeFails(t *testing.T) {
	storeDir := t.TempDir()
	if err := AdoptBundle(storeDir, AdoptedBundle{OrganizationID: "org_1", Scope: ScopeOrganization, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(storeDir, layersFile))
	failure := errors.New("refused")
	if err := UpdateLayers(storeDir, func(doc *LayersDocument) error {
		doc.Adopted = nil
		return failure
	}); !errors.Is(err, failure) {
		t.Fatalf("err = %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(storeDir, layersFile))
	if string(before) != string(after) {
		t.Fatal("a refused change must leave the file byte-identical")
	}
}

func TestStageLayerUsesTheSharedBundleCap(t *testing.T) {
	storeDir := t.TempDir()
	body := []byte(strings.Repeat("x", teamwire.BundleMaxBodyBytes+1))
	if _, err := StageLayer(storeDir, "sha256:whatever", body); err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("an over-cap body is refused by the shared cap, got %v", err)
	}
}

// Criterion 91's rule half: the block names the organization and the date, a refresh
// (a later expiry) lifts it with no other act, and Un-adopt lifts it at once.
func TestFailClosedExpiryNamesTheOrganizationAndDateAndLifts(t *testing.T) {
	storeDir, checkout := t.TempDir(), t.TempDir()
	noUserLayer(t)
	expired := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	raw, digest := layerTestDoc(t, storeDir, "org-rule", "org-bad")
	if _, err := StageLayer(storeDir, digest, raw); err != nil {
		t.Fatal(err)
	}
	if err := AdoptBundle(storeDir, AdoptedBundle{OrganizationID: "org_1", OrganizationName: "Acme", Scope: ScopeOrganization,
		BundleID: "bnd_1", Revision: 1, FailureMode: FailClosed, ExpiresAt: expired, RulebookDigest: digest, SignedDigest: "sha256:s"}); err != nil {
		t.Fatal(err)
	}
	command := []engine.Tag{{Key: "command", Value: "ls"}}
	pol, _, err := LoadLayered(checkout, storeDir)
	if err != nil {
		t.Fatal(err)
	}
	blocked := engine.Decide(command, pol)
	if blocked.Decision != "block" || !strings.Contains(blocked.Message, "Acme") ||
		!strings.Contains(blocked.Message, "1 October 2026") || !strings.Contains(blocked.Message, "block until renewed") {
		t.Fatalf("the block must name the organization and the date: %+v", blocked)
	}
	// A refresh moves the expiry; nothing else is needed.
	if err := RefreshTeamLayer(storeDir, "org_1", ScopeOrganization, LayerRefresh{BundleID: "bnd_2", Revision: 2,
		ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	pol, _, _ = LoadLayered(checkout, storeDir)
	if d := engine.Decide(command, pol); d.Decision == "block" {
		t.Fatalf("a refresh must lift the block: %+v", d)
	}
	if d := engine.Decide([]engine.Tag{{Key: "command", Value: "org-bad"}}, pol); d.Decision != "block" || d.Rule != "org-rule" {
		t.Fatalf("the refreshed layer's own rules decide again: %+v", d)
	}
	// Expired again, then Un-adopt: the block is gone at once.
	if err := RefreshTeamLayer(storeDir, "org_1", ScopeOrganization, LayerRefresh{BundleID: "bnd_2", Revision: 2, ExpiresAt: expired}); err != nil {
		t.Fatal(err)
	}
	pol, _, _ = LoadLayered(checkout, storeDir)
	if d := engine.Decide(command, pol); d.Decision != "block" {
		t.Fatalf("expired again must block: %+v", d)
	}
	if _, err := UnadoptBundle(storeDir, "org_1", ScopeOrganization); err != nil {
		t.Fatal(err)
	}
	pol, reasons, _ := LoadLayered(checkout, storeDir)
	if d := engine.Decide(command, pol); d.Decision == "block" || len(reasons) != 0 {
		t.Fatalf("un-adopt must lift the block at once: %+v %v", d, reasons)
	}
}

func TestFailOpenExpiryStopsApplyingTheRules(t *testing.T) {
	storeDir, checkout := t.TempDir(), t.TempDir()
	noUserLayer(t)
	adopt(t, storeDir, ScopeOrganization, "open-rule", "open-bad", time.Now().Add(-time.Minute), FailOpen)
	pol, reasons, _ := LoadLayered(checkout, storeDir)
	if d := engine.Decide([]engine.Tag{{Key: "command", Value: "open-bad"}}, pol); d.Decision == "block" {
		t.Fatalf("an expired fail-open bundle's rules must not apply: %+v", d)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "fail-open") {
		t.Fatalf("reasons = %v", reasons)
	}
}

// A caller that must not act on a partial rulebook (route admission at bind) is told
// when an adopted layer's staged rules did not load; the hook path still decides on what
// did load, with the reason on its decision.
func TestUnloadableAdoptedLayerIsReportedToCallersThatMustNotActOnAPartialRulebook(t *testing.T) {
	storeDir, checkout := t.TempDir(), t.TempDir()
	noUserLayer(t)
	raw, digest := layerTestDoc(t, storeDir, "org-rule", "org-bad")
	stagedPath, err := StageLayer(storeDir, digest, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := AdoptBundle(storeDir, AdoptedBundle{OrganizationID: "org_1", OrganizationName: "Acme", Scope: ScopeOrganization,
		BundleID: "bnd_1", Revision: 1, FailureMode: FailOpen, ExpiresAt: time.Now().Add(time.Hour), RulebookDigest: digest, SignedDigest: "sha256:s"}); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadLayeredFull(checkout, storeDir)
	if err != nil || loaded.UnloadableLayer() != "" {
		t.Fatalf("a layer that loads is not reported: %q %v", loaded.UnloadableLayer(), err)
	}
	if err := os.WriteFile(stagedPath, []byte(`{not a rule document`), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err = LoadLayeredFull(checkout, storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if reason := loaded.UnloadableLayer(); !strings.Contains(reason, ScopeOrganization) || !strings.Contains(reason, "unloadable") {
		t.Fatalf("an adopted layer whose staged rules do not parse must be reported: %q (reasons %v)", reason, loaded.Reasons)
	}
}
