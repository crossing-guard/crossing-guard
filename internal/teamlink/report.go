package teamlink

import (
	"encoding/json"
	"time"

	"crossing-guard/teamwire"
)

// RuntimeFacts is what the daemon observed about one runtime.
type RuntimeFacts struct {
	Name     string
	Attached bool
	LastLive *time.Time // newest live event from this runtime
	Canary   *time.Time // newest live deny by the proof rule from this runtime
}

// ReportFacts is everything a device report is built from. The daemon gathers them;
// this builder only shapes them, so it is testable without a store.
type ReportFacts struct {
	DeviceID         string
	ReportedAt       time.Time
	DaemonVersion    string
	StoreSchema      int
	Runtimes         []RuntimeFacts
	RulebookDigest   string // "" when no document is active
	RulebookLayer    string // "user" until organization bundles exist
	CanaryRuleActive bool
}

// BuildReport shapes the facts as schemas/device-report.schema.json 1.0. Nothing free-
// text goes in: names are runtime identifiers, versions are bounded strings, the rest
// are digests, booleans, and times.
func BuildReport(f ReportFacts) teamwire.DeviceReport {
	rt := make([]teamwire.RuntimeReport, 0, len(f.Runtimes))
	for _, r := range f.Runtimes {
		rt = append(rt, teamwire.RuntimeReport{Name: r.Name, Attached: r.Attached,
			Firing: teamwire.Observed{ObservedAt: rfc(r.LastLive)},
			Canary: teamwire.CanaryReport{ObservedAt: rfc(r.Canary), RuleActive: f.CanaryRuleActive}})
	}
	report := teamwire.DeviceReport{SchemaVersion: "1.0", DeviceID: f.DeviceID, ReportedAt: f.ReportedAt.UTC().Format(time.RFC3339),
		Daemon: teamwire.ReportDaemon{Version: f.DaemonVersion, Running: true}, Store: teamwire.ReportStore{SchemaVersion: f.StoreSchema},
		Runtimes: rt, Rulebook: teamwire.ReportRulebook{Layers: []teamwire.ReportLayer{}}}
	if f.RulebookDigest != "" {
		layer := f.RulebookLayer
		if layer == "" {
			layer = "user"
		}
		report.Rulebook.Layers = append(report.Rulebook.Layers, teamwire.ReportLayer{Origin: layer, Digest: f.RulebookDigest, Adopted: true})
	}
	return report
}

// Record wraps a report as one push record with a fresh id and its content hash.
func Record(id string, report teamwire.DeviceReport) (teamwire.PushRecord, error) {
	body, err := json.Marshal(report)
	if err != nil {
		return teamwire.PushRecord{}, err
	}
	return teamwire.PushRecord{Kind: teamwire.KindDeviceReport, ID: id, ContentHash: teamwire.ContentHash(body), Body: body}, nil
}

func rfc(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}
