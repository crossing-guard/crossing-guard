// Package rulebook owns the active enforcement rule document: where it lives,
// how legacy documents migrate, how predicates compile, and which default ships.
// Evaluation remains the engine's (engine.Judge); this package deliberately has no verdict API.
package rulebook

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"crossing-guard/engine"
	"crossing-guard/internal/installprofile"
	"crossing-guard/ruledoc"
)

// CanaryMarker is the string the shipped proof rule fires on. No genuine command
// should contain it; demo and verification use it to prove the real hook path.
const CanaryMarker = ruledoc.CanaryMarker

// CanaryRuleID is the shipped proof rule's id; see ruledoc.CanaryRuleID.
const CanaryRuleID = ruledoc.CanaryRuleID

// DemoRuntime is the demo verb's runtime name; see ruledoc.DemoRuntime.
const DemoRuntime = ruledoc.DemoRuntime

// LoadedDocument is the validated rulebook document that is actually active.
// Path is the canonical write target even when the embedded fallback is active.
// Origin and Selection are observed loader facts, not claims of user intent.
type LoadedDocument struct {
	Policy                 *engine.Policy `json:"-"`
	Raw                    []byte         `json:"-"`
	Path                   string         `json:"path"`
	Origin                 string         `json:"origin"`
	Selection              string         `json:"selection"`
	Digest                 string         `json:"digest"`
	StateToken             string         `json:"state_token"`
	Active                 bool           `json:"active"`
	AvailableStarterDigest string         `json:"available_starter_digest"`
	Selected               bool           `json:"selected"`
	Selector               string         `json:"selector,omitempty"`
	SelectedAt             string         `json:"selected_at,omitempty"`
	SelectedSource         string         `json:"selected_source,omitempty"`
	SelectedSourceRef      string         `json:"selected_source_ref,omitempty"`
	Compatibility          string         `json:"compatibility,omitempty"`
	Displaced              *SelectionInfo `json:"displaced_selection,omitempty"`
	DisplacedError         string         `json:"displaced_selection_error,omitempty"`
	InstallCohort          string         `json:"install_cohort"`
	InstallProfileCreated  bool           `json:"install_profile_created,omitempty"`
}

type resolution struct {
	path   string
	origin string
}

// LegacyCandidatePath resolves only the pre-selection compatibility path. It must not
// be used as active truth; consumers use LoadDocument for that.
func LegacyCandidatePath() string {
	if executable, err := os.Executable(); err == nil {
		path := filepath.Join(filepath.Dir(executable), "rules.json")
		if fileExists(path) {
			return path
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".crossing-guard", "policy", "rules.json")
	}
	return "rules.json"
}

func legacyResolution() resolution {
	path := LegacyCandidatePath()
	origin := "relative-file"
	if filepath.IsAbs(path) {
		origin = "user-file"
		if executable, err := os.Executable(); err == nil && path == filepath.Join(filepath.Dir(executable), "rules.json") {
			origin = "executable-file"
		}
	}
	return resolution{path: path, origin: origin}
}

// Load reads the user document, or the embedded default only when the file is absent.
// A present malformed document is always a loud error.
func Load() (*engine.Policy, error) {
	doc, err := LoadDocument()
	if err != nil {
		return nil, err
	}
	return doc.Policy, nil
}

func installRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".crossing-guard"), nil
}

func installationCohort(persist bool) (installprofile.Cohort, bool, error) {
	root, err := installRoot()
	if err != nil {
		return "", false, err
	}
	if persist {
		result, err := installprofile.Ensure(root)
		return result.Profile.Cohort, result.Created, err
	}
	profile, err := installprofile.Preview(root)
	return profile.Cohort, false, err
}

// LoadDocumentPreview resolves the same effective rulebook without creating the
// install profile. It exists only for the init --dry-run no-write contract.
func LoadDocumentPreview() (*LoadedDocument, error) {
	cohort, _, err := installationCohort(false)
	if err != nil {
		return nil, err
	}
	return loadDocument(cohort)
}

// LoadDocument returns policy content and provenance from the same successful load.
// Active is set only after migration, decoding, and predicate compilation succeed.
func LoadDocument() (*LoadedDocument, error) {
	cohort, created, err := installationCohort(true)
	if err != nil {
		return nil, err
	}
	doc, err := loadDocument(cohort)
	if doc != nil {
		doc.InstallProfileCreated = created
	}
	return doc, err
}

func loadDocument(cohort installprofile.Cohort) (*LoadedDocument, error) {
	starterDigest := ruledoc.ContentDigest(ruledoc.SafetyStarterRules())
	if path := os.Getenv("CG_RULES"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, err
			}
			record, recordErr := readSelection()
			if recordErr != nil {
				return nil, recordErr
			}
			if record != nil {
				return nil, fmt.Errorf("CG_RULES invocation file is missing at %s; refusing to hide durable selection %s behind fallback", path, record.Digest)
			}
			raw, origin, selection := unselectedBaseline(cohort)
			policy, parseErr := ruledoc.Parse(raw)
			if parseErr != nil {
				return nil, parseErr
			}
			return stampState(&LoadedDocument{Policy: policy, Raw: append([]byte(nil), raw...), Path: path,
				Origin: origin, Selection: selection, Digest: ruledoc.ContentDigest(raw),
				Active: true, AvailableStarterDigest: starterDigest, Selected: false,
				Selector: "environment", Compatibility: "missing-invocation-" + selection,
				InstallCohort: string(cohort)}), nil
		}
		policy, err := ruledoc.Parse(raw)
		if err != nil {
			return nil, err
		}
		doc := &LoadedDocument{Policy: policy, Raw: append([]byte(nil), raw...), Path: path,
			Origin: "environment-file", Selection: "invocation-path", Digest: ruledoc.ContentDigest(raw),
			Active: true, AvailableStarterDigest: starterDigest, Selected: true, Selector: "environment",
			InstallCohort: string(cohort)}
		if record, recordErr := readSelection(); recordErr != nil {
			doc.DisplacedError = recordErr.Error()
		} else if record != nil {
			info := record.SelectionInfo
			doc.Displaced = &info
		}
		return stampState(doc), nil
	}
	record, err := readSelection()
	if err != nil {
		return nil, err
	}
	if record != nil {
		raw, policy, path, err := readSelected(record)
		if err != nil {
			return nil, err
		}
		return stampState(&LoadedDocument{Policy: policy, Raw: append([]byte(nil), raw...), Path: path,
			Origin: "selected-user", Selection: "explicit-user", Digest: record.Digest,
			Active: true, AvailableStarterDigest: starterDigest, Selected: true,
			Selector: record.Selector, SelectedAt: record.SelectedAt,
			SelectedSource: record.Source, SelectedSourceRef: record.SourceRef,
			InstallCohort: string(cohort)}), nil
	}
	return loadUnselectedDocument(cohort)
}

func unselectedBaseline(cohort installprofile.Cohort) ([]byte, string, string) {
	if cohort == installprofile.MechanismFirst {
		return []byte("{\n  \"rules\": []\n}\n"), "mechanism-floor", "unselected-baseline"
	}
	return ruledoc.DefaultRules(), "embedded-catalog", "legacy-implicit"
}

func loadUnselectedDocument(cohort installprofile.Cohort) (*LoadedDocument, error) {
	resolved := legacyResolution()
	if cohort == installprofile.MechanismFirst {
		raw, origin, selection := unselectedBaseline(cohort)
		policy, err := ruledoc.Parse(raw)
		if err != nil {
			return nil, err
		}
		return stampState(&LoadedDocument{Policy: policy, Raw: append([]byte(nil), raw...),
			Path: resolved.path, Origin: origin, Selection: selection, Digest: ruledoc.ContentDigest(raw),
			Active: true, AvailableStarterDigest: ruledoc.ContentDigest(ruledoc.SafetyStarterRules()), Selected: false,
			Compatibility: selection, InstallCohort: string(cohort)}), nil
	}
	raw, err := os.ReadFile(resolved.path)
	origin := resolved.origin
	selection := "legacy-path-precedence"
	if err != nil {
		if os.IsNotExist(err) {
			raw = ruledoc.DefaultRules()
			origin = "embedded-catalog"
			selection = "legacy-implicit"
		} else {
			return nil, err
		}
	}
	policy, err := ruledoc.Parse(raw)
	if err != nil {
		return nil, err
	}
	return stampState(&LoadedDocument{
		Policy: policy, Raw: append([]byte(nil), raw...), Path: resolved.path,
		Origin: origin, Selection: selection, Digest: ruledoc.ContentDigest(raw), Active: true,
		AvailableStarterDigest: ruledoc.ContentDigest(ruledoc.SafetyStarterRules()), Selected: false,
		Compatibility: selection, InstallCohort: string(cohort),
	}), nil
}

func stampState(doc *LoadedDocument) *LoadedDocument {
	displaced := ""
	if doc.Displaced != nil {
		displaced = doc.Displaced.Digest + "|" + doc.Displaced.Source + "|" +
			doc.Displaced.SourceRef + "|" + doc.Displaced.SelectedAt
	}
	doc.StateToken = ruledoc.ContentDigest([]byte(strings.Join([]string{doc.Origin, doc.Selection,
		doc.Digest, doc.Selector, doc.SelectedAt, doc.SelectedSource,
		doc.SelectedSourceRef, displaced, doc.DisplacedError, doc.InstallCohort}, "|")))
	return doc
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
