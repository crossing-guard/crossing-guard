package harvest

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
)

// ActivityCapability states whether this machine can mechanically observe open vendor
// session files. Open means only that a live expected vendor process holds the exact
// file open; it does not claim that a model turn is generating.
type ActivityCapability struct {
	Status string `json:"status"` // available | unavailable
	Detail string `json:"detail"`
}

// ActivityObservation is request-scoped and never persisted.
type ActivityObservation struct {
	Capability ActivityCapability
	OpenPaths  map[string]bool
}

type openFileRecord struct {
	Command string
	Path    string
}

var errActivityUnsupported = errors.New("open-session observation is unavailable on this platform")
var observeOpenFilesFn = observeOpenFiles

// activityRuntime is optional so adding a vendor remains open/closed: each adapter owns
// the process names that are truthful evidence for its session files.
type activityRuntime interface {
	ActivityProcess(command string) bool
}

// ObserveOpenSessions performs one bounded platform probe over already-discovered exact
// paths. Failure is capability-level unavailable, never an inferred empty population.
func ObserveOpenSessions(ctx context.Context, sessions []SessionSummary) ActivityObservation {
	paths := make([]string, 0, len(sessions))
	byPath := make(map[string]SessionSummary, len(sessions))
	for _, s := range sessions {
		if s.Path == "" {
			continue
		}
		clean := filepath.Clean(s.Path)
		paths = append(paths, clean)
		byPath[clean] = s
	}
	records, err := observeOpenFilesFn(ctx, paths)
	if err != nil {
		detail := err.Error()
		if errors.Is(err, errActivityUnsupported) {
			detail = "Open-session observation is unavailable on this platform."
		}
		return ActivityObservation{
			Capability: ActivityCapability{Status: "unavailable", Detail: detail},
			OpenPaths:  map[string]bool{},
		}
	}
	open := map[string]bool{}
	for _, record := range records {
		clean := filepath.Clean(record.Path)
		s, ok := byPath[clean]
		if !ok {
			continue
		}
		rt := runtimeFor(s.Runtime)
		owner, ok := rt.(activityRuntime)
		if ok && owner.ActivityProcess(record.Command) {
			open[clean] = true
		}
	}
	return ActivityObservation{
		Capability: ActivityCapability{
			Status: "available",
			Detail: "Open means an expected live vendor process currently holds the exact session file open; it does not mean a model turn is generating.",
		},
		OpenPaths: open,
	}
}

func commandBase(command string) string {
	return strings.ToLower(filepath.Base(strings.TrimSpace(command)))
}

func parseOpenFileRecords(output string) []openFileRecord {
	command := ""
	records := []openFileRecord{}
	for _, line := range strings.Split(output, "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			command = ""
		case 'c':
			command = line[1:]
		case 'n':
			if command != "" {
				records = append(records, openFileRecord{Command: command, Path: line[1:]})
			}
		}
	}
	return records
}
