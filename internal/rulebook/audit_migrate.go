package rulebook

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"strings"

	"crossing-guard/engine"
	"crossing-guard/ruledoc"
)

type legacyAuditRule struct {
	ID       string           `json:"id"`
	Severity string           `json:"severity"`
	If       engine.Predicate `json:"if"`
	Then     string           `json:"then"`
	Message  string           `json:"message"`
}

// AuditImportResult records the one-time legacy audit migration. ArchiveWarning is
// post-save only: imported rules are active and Audit remains usable, while startup
// can say that the source file still needs archival.
type AuditImportResult struct {
	Imported       int
	Skipped        int
	Archive        string
	Backup         string
	ArchiveWarning string
}

// ImportLegacyAudit folds the former report-only audit document into the active
// rulebook, then archives the source. It never strengthens behavior: every imported
// rule is observe-only, and bare tags become session-scoped because the old evaluator
// always ran over a whole session's accumulated tags.
func ImportLegacyAudit(path string) (AuditImportResult, error) {
	var result AuditImportResult
	legacyRaw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if os.Getenv("CG_RULES") != "" {
		return result, fmt.Errorf("refusing legacy audit migration while CG_RULES invocation override is active")
	}
	var legacy struct {
		Rules []legacyAuditRule `json:"rules"`
	}
	if err := json.Unmarshal(legacyRaw, &legacy); err != nil {
		return result, fmt.Errorf("parse legacy audit rules: %w", err)
	}

	loaded, err := LoadDocument()
	if err != nil {
		return result, fmt.Errorf("read active rules: %w", err)
	}
	activeRaw := loaded.Raw
	var document map[string]json.RawMessage
	if err := json.Unmarshal(activeRaw, &document); err != nil {
		return result, fmt.Errorf("parse active rule document: %w", err)
	}
	var rawRules []json.RawMessage
	if value, ok := document["rules"]; ok {
		if err := json.Unmarshal(value, &rawRules); err != nil {
			return result, fmt.Errorf("parse active rules array: %w", err)
		}
	}
	active := make([]engine.Rule, len(rawRules))
	for index, raw := range rawRules {
		if err := json.Unmarshal(raw, &active[index]); err != nil {
			return result, fmt.Errorf("parse active rule %d: %w", index, err)
		}
	}

	for _, old := range legacy.Rules {
		converted := engine.Rule{ID: old.ID, Action: "observe", Severity: old.Severity,
			If: prefixSessionTags(old.If), Message: old.Message}
		id, duplicate := importedRuleID(active, converted, old.ID)
		if duplicate {
			result.Skipped++
			continue
		}
		converted.ID = id
		encoded, err := json.Marshal(converted)
		if err != nil {
			return result, err
		}
		rawRules = append(rawRules, encoded)
		active = append(active, converted)
		result.Imported++
	}
	if result.Imported > 0 {
		encodedRules, err := json.Marshal(rawRules)
		if err != nil {
			return result, err
		}
		document["rules"] = encodedRules
		candidate, err := json.MarshalIndent(document, "", "  ")
		if err != nil {
			return result, err
		}
		candidate = append(candidate, '\n')
		result.Backup, err = Save(candidate)
		if err != nil {
			return result, fmt.Errorf("save migrated audit rules: %w", err)
		}
		proved, err := LoadDocument()
		if err != nil || proved.Digest != ruledoc.ContentDigest(candidate) {
			if err == nil {
				err = fmt.Errorf("active digest %s, want %s", proved.Digest, ruledoc.ContentDigest(candidate))
			}
			return result, fmt.Errorf("prove migrated audit rules active before archival: %w", err)
		}
	}

	archive := path + ".migrated"
	if _, err := os.Stat(archive); err == nil {
		result.ArchiveWarning = "archive already exists at " + archive
		return result, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		result.ArchiveWarning = err.Error()
		return result, nil
	}
	if err := os.Rename(path, archive); err != nil {
		result.ArchiveWarning = err.Error()
		return result, nil
	}
	result.Archive = archive
	return result, nil
}

func prefixSessionTags(predicate engine.Predicate) engine.Predicate {
	for index := range predicate.All {
		predicate.All[index] = prefixSessionTags(predicate.All[index])
	}
	for index := range predicate.Any {
		predicate.Any[index] = prefixSessionTags(predicate.Any[index])
	}
	if predicate.Not != nil {
		prefixed := prefixSessionTags(*predicate.Not)
		predicate.Not = &prefixed
	}
	if predicate.Tag != "" && !strings.HasPrefix(predicate.Tag, "session:") &&
		!strings.HasPrefix(predicate.Tag, "target:") {
		predicate.Tag = "session:" + predicate.Tag
	}
	return predicate
}

func importedRuleID(active []engine.Rule, candidate engine.Rule, originalID string) (string, bool) {
	ids := make(map[string]bool, len(active))
	for _, existing := range active {
		ids[existing.ID] = true
		if importedIdentity(existing.ID, originalID) && sameRuleExceptID(existing, candidate) {
			return existing.ID, true
		}
	}
	if !ids[originalID] {
		return originalID, false
	}
	base := "legacy-audit-" + originalID
	for suffix := 1; ; suffix++ {
		id := base
		if suffix > 1 {
			id = fmt.Sprintf("%s-%d", base, suffix)
		}
		if !ids[id] {
			return id, false
		}
	}
}

func importedIdentity(activeID, originalID string) bool {
	base := "legacy-audit-" + originalID
	return activeID == originalID || activeID == base || strings.HasPrefix(activeID, base+"-")
}

func sameRuleExceptID(left, right engine.Rule) bool {
	left.ID, right.ID = "", ""
	return reflect.DeepEqual(left, right)
}
