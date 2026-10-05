package teamwire

import "time"

// DeviceReport is schemas/device-report.schema.json 1.0. It carries no free text, no user,
// no host, and no path: every string in it is an identifier, a version, a digest, or a
// time. That is a design property, not an accident — an admin console renders it.
type DeviceReport struct {
	SchemaVersion string          `json:"schema_version"`
	DeviceID      string          `json:"device_id"`
	ReportedAt    string          `json:"reported_at"`
	Daemon        ReportDaemon    `json:"daemon"`
	Store         ReportStore     `json:"store"`
	Runtimes      []RuntimeReport `json:"runtimes"`
	Rulebook      ReportRulebook  `json:"rulebook"`
}

// ReportDaemon is the reporting process.
type ReportDaemon struct {
	Version string `json:"version"`
	Running bool   `json:"running"`
}

// ReportStore is the local store.
type ReportStore struct {
	SchemaVersion int `json:"schema_version"`
}

// ReportRulebook lists adopted layers; only "user" exists until organization bundles do.
type ReportRulebook struct {
	Layers []ReportLayer `json:"layers"`
}

// ReportLayer is one adopted rulebook layer.
type ReportLayer struct {
	Origin  string `json:"origin"`
	Digest  string `json:"digest"`
	Adopted bool   `json:"adopted"`
}

// RuntimeReport is one agent runtime's three rungs. Each is an observation with a time,
// never a verdict; Standing turns them into the word a person reads.
type RuntimeReport struct {
	Name     string       `json:"name"`
	Attached bool         `json:"attached"`
	Firing   Observed     `json:"firing"`
	Canary   CanaryReport `json:"canary"`
}

// Observed is when something was last seen, or null if it never was.
type Observed struct {
	ObservedAt *string `json:"observed_at"`
}

// CanaryReport is the third rung. RuleActive says whether the device's active rulebook
// would deny the proof command by the proof rule at all: without it, "never observed"
// would be blamed on the runtime when the cause is the rulebook.
type CanaryReport struct {
	ObservedAt *string `json:"observed_at"`
	RuleActive bool    `json:"rule_active"`
}

// Standing is a runtime's position on the ladder. Every value is REPORTED BY THE DEVICE:
// a process running as the same user can forge a report, so a fleet page is an honest
// aggregate of self-attestation and must label it so (team plan §5.15, U16).
type Standing string

const (
	// StandingNotAttached: the hook is not in the runtime's configuration.
	StandingNotAttached Standing = "not-attached"
	// StandingNeverFired: configured, but no live event has ever arrived — registration
	// is not coverage.
	StandingNeverFired Standing = "attached-never-fired"
	// StandingNoCanaryRule: the hook fires, but the active rulebook has no proof rule to
	// observe. Not the runtime's fault, and not the same as never having been tested.
	StandingNoCanaryRule Standing = "firing-no-canary-rule"
	// StandingCanaryNever: the hook fires and a proof rule exists, but no block has ever
	// been observed reaching this runtime.
	StandingCanaryNever Standing = "firing-canary-never"
	// StandingCanaryStale: a block was observed, longer ago than the reader accepts.
	StandingCanaryStale Standing = "canary-stale"
	// StandingEnforced: a block by the proof rule was observed within the window.
	StandingEnforced Standing = "enforced"
)

// RuntimeStanding reads one runtime's rungs. It is the ONLY reader: the local console and
// the fleet page both call it, so they cannot disagree about the same report. canaryFresh
// is the reader's window — the server's configuration on the fleet page; a window of zero
// or less means every canary is stale, so a misconfigured reader can never label a
// years-old proof "enforced". An unparseable time reads as never observed; a report is
// never trusted further than it can be parsed.
func RuntimeStanding(r RuntimeReport, now time.Time, canaryFresh time.Duration) Standing {
	if !r.Attached {
		return StandingNotAttached
	}
	if _, ok := observedTime(r.Firing.ObservedAt); !ok {
		return StandingNeverFired
	}
	if !r.Canary.RuleActive {
		return StandingNoCanaryRule
	}
	at, ok := observedTime(r.Canary.ObservedAt)
	if !ok {
		return StandingCanaryNever
	}
	if canaryFresh <= 0 || now.Sub(at) > canaryFresh {
		return StandingCanaryStale
	}
	return StandingEnforced
}

func observedTime(s *string) (time.Time, bool) {
	if s == nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, *s)
	return t, err == nil
}
