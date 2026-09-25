package rulebook

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"crossing-guard/ruledoc"
)

const selectionFormatVersion = 1

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// SelectionInfo is the durable user-layer choice. It identifies configuration;
// it does not claim that a runtime has demonstrated enforcement.
// RuleChange is the portable mechanical diff row; see ruledoc.
type RuleChange = ruledoc.RuleChange

type SelectionInfo struct {
	Digest     string `json:"digest"`
	Source     string `json:"source"`
	SourceRef  string `json:"source_ref,omitempty"`
	Selector   string `json:"selector"`
	SelectedAt string `json:"selected_at"`
}

type selectionRecord struct {
	FormatVersion int    `json:"format_version"`
	ArtifactType  string `json:"artifact_type"`
	OriginLayer   string `json:"origin_layer"`
	SelectionInfo
}

// SelectionPreview is the fact set a CLI or GUI must show before acknowledgement.
type SelectionPreview struct {
	Source               string       `json:"source"`
	SourcePath           string       `json:"source_path,omitempty"`
	ExpectedActiveDigest string       `json:"expected_active_digest"`
	ExpectedStateToken   string       `json:"expected_state_token"`
	ActivePath           string       `json:"active_path"`
	ActiveOrigin         string       `json:"active_origin"`
	ProposedDigest       string       `json:"proposed_digest"`
	Added                []string     `json:"added"`
	Removed              []string     `json:"removed"`
	Changed              []RuleChange `json:"changed"`
	SemanticReach        string       `json:"semantic_reach"`
	RuleCount            int          `json:"rule_count"`
	ActionSummary        string       `json:"action_summary"`
	raw                  []byte
}

// SelectRequest binds consent to both the reviewed candidate and the active base.
type SelectRequest struct {
	Source               string `json:"source"`
	Path                 string `json:"path,omitempty"`
	Selector             string `json:"selector"`
	ExpectedActiveDigest string `json:"expected_active_digest"`
	ExpectedStateToken   string `json:"expected_state_token"`
	AcknowledgedDigest   string `json:"acknowledged_digest"`
}

type SelectionResult struct {
	Preview          SelectionPreview `json:"preview"`
	DurableSelection SelectionInfo    `json:"durable_selection"`
	Active           *LoadedDocument  `json:"active"`
}

type SaveResult struct {
	Backup   string          `json:"backup,omitempty"`
	Document *LoadedDocument `json:"document"`
}

type ConflictError struct {
	Expected string
	Actual   string
}

type InvocationMutationError struct{ Path string }

func (e *InvocationMutationError) Error() string {
	return "CG_RULES invocation override is active at " + e.Path +
		"; remove it before mutating policy through the console"
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("active rulebook changed: expected %s, now %s; reload and review", e.Expected, e.Actual)
}

func candidateFor(source, path string) ([]byte, string, error) {
	switch source {
	case "current":
		loaded, err := LoadDocument()
		if err != nil {
			return nil, "", err
		}
		return loaded.Raw, loaded.Path, nil
	case "starter":
		return append([]byte(nil), ruledoc.DefaultRules()...), "embedded starter catalog", nil
	case "safety-starter":
		return append([]byte(nil), ruledoc.SafetyStarterRules()...), "embedded safety-starter catalog", nil
	case "security-observe":
		return append([]byte(nil), ruledoc.SecurityObserveRules()...), "embedded security-observe catalog", nil
	case "none":
		return []byte("{\n  \"rules\": []\n}\n"), "explicit empty rulebook", nil
	case "legacy":
		cohort, _, err := installationCohort(false)
		if err != nil {
			return nil, "", err
		}
		loaded, err := loadUnselectedDocument(cohort)
		if err != nil {
			return nil, "", err
		}
		return loaded.Raw, loaded.Path, nil
	case "file":
		if strings.TrimSpace(path) == "" {
			return nil, "", fmt.Errorf("selection source file path is required")
		}
		raw, err := readRegularFile(path)
		return raw, path, err
	default:
		return nil, "", fmt.Errorf("unknown rulebook selection source %q", source)
	}
}

// PreviewSelection computes bounded mechanical facts over the exact bytes to acknowledge.
func PreviewSelection(source, path string) (SelectionPreview, error) {
	active, err := LoadDocument()
	if err != nil {
		return SelectionPreview{}, fmt.Errorf("load current rulebook before selection: %w", err)
	}
	raw, sourcePath, err := candidateFor(source, path)
	if err != nil {
		return SelectionPreview{}, err
	}
	candidate, err := ruledoc.Parse(raw)
	if err != nil {
		return SelectionPreview{}, &InvalidDocumentError{Err: err}
	}
	added, removed, changed := ruledoc.MechanicalDiff(active.Policy, candidate)
	return SelectionPreview{
		Source: source, SourcePath: sourcePath, ExpectedActiveDigest: active.Digest,
		ExpectedStateToken: active.StateToken, ActivePath: active.Path,
		ActiveOrigin: active.Origin, ProposedDigest: ruledoc.ContentDigest(raw), Added: added,
		Removed: removed, Changed: changed,
		SemanticReach: "not enumerable — regex and state predicates require representative real inputs",
		RuleCount:     len(candidate.Rules), ActionSummary: ruledoc.PolicySummary(candidate), raw: raw,
	}, nil
}

// Select records an explicit user-layer choice after verifying both acknowledged bytes
// and the active base the caller reviewed.
func Select(request SelectRequest) (SelectionResult, error) {
	if request.Source == "legacy" {
		return SelectionResult{}, fmt.Errorf("legacy is a rollback preview, not a selectable source")
	}
	if request.Selector != "cli" && request.Selector != "console" {
		return SelectionResult{}, fmt.Errorf("selector must be cli or console")
	}
	var result SelectionResult
	err := withMutationLock(func() error {
		preview, err := PreviewSelection(request.Source, request.Path)
		if err != nil {
			return err
		}
		if request.Selector == "console" && preview.ActiveOrigin == "environment-file" {
			return &InvocationMutationError{Path: os.Getenv("CG_RULES")}
		}
		if request.ExpectedStateToken != preview.ExpectedStateToken {
			return &ConflictError{Expected: request.ExpectedStateToken, Actual: preview.ExpectedStateToken}
		}
		if request.ExpectedActiveDigest != preview.ExpectedActiveDigest {
			return &ConflictError{Expected: request.ExpectedActiveDigest, Actual: preview.ExpectedActiveDigest}
		}
		if request.AcknowledgedDigest != preview.ProposedDigest {
			return fmt.Errorf("acknowledged digest %q does not match proposed %s", request.AcknowledgedDigest, preview.ProposedDigest)
		}
		if _, err := writeImmutableDocument(preview.raw, preview.ProposedDigest); err != nil {
			return err
		}
		info := SelectionInfo{Digest: preview.ProposedDigest, Source: request.Source,
			SourceRef: preview.SourcePath,
			Selector:  request.Selector, SelectedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		record := selectionRecord{FormatVersion: selectionFormatVersion, ArtifactType: "rulebook",
			OriginLayer: "user", SelectionInfo: info}
		if err := writeSelection(record); err != nil {
			return err
		}
		active, err := LoadDocument()
		if err != nil {
			return fmt.Errorf("selection written but active proof failed: %w", err)
		}
		result = SelectionResult{Preview: preview, DurableSelection: info, Active: active}
		return nil
	})
	return result, err
}

// Unselect atomically archives the durable selection after proving the cohort baseline
// that will become active. Invocation overrides must be removed first.
func Unselect(expectedStateToken, selector string) (*LoadedDocument, string, error) {
	if selector != "cli" && selector != "console" {
		return nil, "", fmt.Errorf("selector must be cli or console")
	}
	var active *LoadedDocument
	var archive string
	err := withMutationLock(func() error {
		if os.Getenv("CG_RULES") != "" {
			return fmt.Errorf("remove CG_RULES before rolling back the durable selection")
		}
		record, err := readSelection()
		if err != nil {
			return err
		}
		if record == nil {
			return fmt.Errorf("no durable rulebook selection to unselect")
		}
		cohort, _, err := installationCohort(true)
		if err != nil {
			return err
		}
		currentState := stampState(&LoadedDocument{Origin: "selected-user",
			Selection: "explicit-user", Digest: record.Digest, Selected: true,
			Selector: record.Selector, SelectedAt: record.SelectedAt,
			SelectedSource: record.Source, SelectedSourceRef: record.SourceRef,
			InstallCohort: string(cohort)}).StateToken
		if currentState != expectedStateToken {
			return &ConflictError{Expected: expectedStateToken, Actual: currentState}
		}
		baseline, err := loadUnselectedDocument(cohort)
		if err != nil {
			return fmt.Errorf("unselected baseline is not loadable: %w", err)
		}
		root, err := rulebookRoot()
		if err != nil {
			return err
		}
		history := filepath.Join(root, "history")
		if err := ensureOwnedDir(history); err != nil {
			return err
		}
		path := filepath.Join(root, "selection.json")
		stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
		archive = filepath.Join(history, "selection-"+stamp+"-"+selector+".json")
		if err := os.Rename(path, archive); err != nil {
			return fmt.Errorf("archive rulebook selection: %w", err)
		}
		active, err = LoadDocument()
		if err != nil {
			return fmt.Errorf("selection archived but legacy activation failed: %w", err)
		}
		if active.Digest != baseline.Digest {
			return fmt.Errorf("unselected active digest %s differs from preflight %s", active.Digest, baseline.Digest)
		}
		return nil
	})
	return active, archive, err
}

// RollbackLegacy retains the C5b command/API contract while delegating to the
// cohort-aware revocation operation. Callers should migrate their label to unselect.
func RollbackLegacy(expectedStateToken, selector string) (*LoadedDocument, string, error) {
	return Unselect(expectedStateToken, selector)
}
