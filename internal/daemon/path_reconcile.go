package daemon

import (
	"path/filepath"
	"strings"
	"time"

	"crossing-guard/store"
)

const pathReconciliationAlgorithm = "checkpoint-path-v1"

func repositoryRelativeIdentity(root, raw string) (string, bool) {
	if raw == "" || strings.ContainsRune(raw, '\x00') {
		return "", false
	}
	path := filepath.Clean(raw)
	if filepath.IsAbs(path) {
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", false
		}
		path = rel
	}
	path = filepath.ToSlash(path)
	if path == "." || path == ".." || strings.HasPrefix(path, "../") {
		return "", false
	}
	return path, true
}

func reconcileCheckpointPaths(ix *store.Index, current store.SessionCheckpoint) error {
	if current.Status != "complete" || current.ChangeRecordID == 0 || current.CheckoutID == "" {
		return nil
	}
	checkpoints, err := ix.CompletedCheckpointsForSessionCheckout(current.SessionID, current.CheckoutID)
	if err != nil {
		return err
	}
	var predecessor store.SessionCheckpoint
	for _, candidate := range checkpoints {
		if candidate.ID < current.ID {
			predecessor = candidate
		}
	}
	currentRecord, err := ix.ChangeRecordByID(current.ChangeRecordID)
	if err != nil {
		return err
	}
	var predecessorRecord store.ChangeRecord
	if predecessor.ChangeRecordID != 0 {
		predecessorRecord, err = ix.ChangeRecordByID(predecessor.ChangeRecordID)
		if err != nil {
			return err
		}
	}
	currentPayloads, err := ix.CheckpointPayloads(current.ID)
	if err != nil {
		return err
	}
	previousPayloads := []store.CheckpointPayload{}
	if predecessor.ID != 0 {
		previousPayloads, err = ix.CheckpointPayloads(predecessor.ID)
		if err != nil {
			return err
		}
	}
	currentDigest, previousDigest := map[string]string{}, map[string]string{}
	for _, payload := range currentPayloads {
		if payload.ContentCompleteness == "complete" {
			currentDigest[payload.Path] = payload.ContentDigest
		}
	}
	for _, payload := range previousPayloads {
		if payload.ContentCompleteness == "complete" {
			previousDigest[payload.Path] = payload.ContentDigest
		}
	}
	effects, err := ix.LinkedResultEffectsForSession(current.SessionID)
	if err != nil {
		return err
	}
	effectsByPath := map[string][]store.LinkedResultEffect{}
	for _, effect := range effects {
		if predecessor.CaptureEndedAt != 0 && effect.CompletedAt != 0 && effect.CompletedAt < predecessor.CaptureEndedAt {
			continue
		}
		if current.CaptureEndedAt != 0 && effect.CompletedAt > current.CaptureEndedAt {
			continue
		}
		if path, ok := repositoryRelativeIdentity(current.CheckoutRoot, effect.RawIdentity); ok {
			effectsByPath[path] = append(effectsByPath[path], effect)
		}
	}
	itemsByPath := map[string]store.ChangeItem{}
	for _, item := range currentRecord.Items {
		prior, found := itemsByPath[item.Path]
		if !found || prior.Layer != "worktree" {
			itemsByPath[item.Path] = item
		}
	}
	matchedEffects := map[string]bool{}
	for path, item := range itemsByPath {
		fact := store.PathReconciliation{SessionID: current.SessionID, CheckoutID: current.CheckoutID,
			PredecessorCheckpointID: predecessor.ID, CurrentCheckpointID: current.ID, Path: path,
			OldPath: item.OldPath, Algorithm: pathReconciliationAlgorithm, ReconciledAt: time.Now().Unix(),
			GitEffectDigest: currentDigest[path]}
		if current.Kind == "attachment" || (predecessor.ID != 0 && previousDigest[path] != "" && previousDigest[path] == currentDigest[path]) {
			fact.Classification = "inherited"
			if err := ix.AppendPathReconciliation(fact); err != nil {
				return err
			}
			continue
		}
		pathEffects := effectsByPath[path]
		if len(pathEffects) > 0 {
			matchedEffects[path] = true
			best := pathEffects[len(pathEffects)-1]
			fact.EventID, fact.ResultID = best.EventID, best.ResultID
			fact.RuntimeEffectDigest = best.ContentDigest
			if fact.RuntimeEffectDigest == "" {
				fact.RuntimeEffectDigest = best.DiffDigest
			}
			switch {
			case best.ContentMeasured && best.ContentDigest != "" && currentDigest[path] != "" && best.ContentDigest == currentDigest[path]:
				fact.Classification = "native-effect-git-matched"
			default:
				fact.Classification = "action-reported"
			}
		} else if predecessor.ID == 0 {
			fact.Classification = "session-associated"
		} else if predecessorRecord.BaseRevision != currentRecord.BaseRevision || predecessorRecord.HeadRevision != currentRecord.HeadRevision {
			fact.Classification = "session-associated"
		} else {
			fact.Classification = "unattributed"
		}
		if err := ix.AppendPathReconciliation(fact); err != nil {
			return err
		}
	}
	for path, pathEffects := range effectsByPath {
		if matchedEffects[path] || len(pathEffects) == 0 {
			continue
		}
		best := pathEffects[len(pathEffects)-1]
		classification := "action-reported"
		if predecessor.ID != 0 && previousDigest[path] == currentDigest[path] {
			classification = "no-net-change-at-checkpoint"
		}
		fact := store.PathReconciliation{SessionID: current.SessionID, CheckoutID: current.CheckoutID,
			PredecessorCheckpointID: predecessor.ID, CurrentCheckpointID: current.ID,
			EventID: best.EventID, ResultID: best.ResultID, Path: path, Classification: classification,
			RuntimeEffectDigest: best.ContentDigest, GitEffectDigest: currentDigest[path],
			Algorithm: pathReconciliationAlgorithm, ReconciledAt: time.Now().Unix()}
		if fact.RuntimeEffectDigest == "" {
			fact.RuntimeEffectDigest = best.DiffDigest
		}
		if err := ix.AppendPathReconciliation(fact); err != nil {
			return err
		}
	}
	return nil
}
