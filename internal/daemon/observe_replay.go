package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"crossing-guard/internal/observation"
	"crossing-guard/store"
)

const replayBatchLimit = 64

// replayMaxDeliveryAttempts bounds how often one spooled record may fail delivery
// before it is quarantined (bytes preserved, collection issue recorded). Without a
// bound, a record the ingest path persistently rejects for a non-validation reason
// retried every cycle forever and drowned the log.
const replayMaxDeliveryAttempts = 100

// giveUpUndeliverable quarantines a decoded record whose delivery-attempt count
// exceeded the bound. Returns (handled, error): handled=false means the record is
// still within its budget and replay should proceed.
func giveUpUndeliverable(g *Governor, path, entryName string, body []byte, occurredAt int64,
	sessionID, runtime, observationID string, attempts int) (bool, error) {
	if attempts < replayMaxDeliveryAttempts {
		return false, nil
	}
	// Recorded under the existing "malformed" issue kind: the store's issue-kind
	// vocabulary is CHECK-constrained, and widening it is a schema migration this
	// bounded fix does not take on. The quarantine log line and the cause digest
	// carry the exhausted-budget specifics.
	err := rejectReplayRecord(g, path, entryName, body, occurredAt, sessionID, runtime,
		observationID, "malformed", fmt.Errorf("gave up after %d delivery attempts", attempts))
	if err == nil {
		log.Printf("observation replay quarantined %s after %d delivery attempts — bytes preserved in quarantine",
			entryName, attempts)
	}
	return true, err
}

var quarantineObservationRename = os.Rename

type quarantineSyncDirectory interface {
	Sync() error
	Close() error
}

var quarantineObservationOpenDir = func(path string) (quarantineSyncDirectory, error) {
	return os.Open(path)
}

func replayIssue(g *Governor, entryName string, body []byte, occurredAt int64,
	sessionID, runtime, observationID, kind string, cause error) error {
	bodyDigest := observation.DigestBytes(body)
	detail := bodyDigest
	if cause != nil {
		detail = observation.DigestBytes([]byte(cause.Error()))
	}
	issue := store.CollectionIssue{
		IssueID: observationIssueID(kind, entryName+"\x00"+bodyDigest), SessionID: sessionID,
		Runtime: runtime, ObservationID: observationID, SourceRef: entryName,
		CollectorID: "daemon-observation-replay", Kind: kind, AffectedCount: 1,
		FirstSeen: occurredAt, LastSeen: occurredAt, DetailDigest: detail,
	}
	g.writeMu.Lock()
	err := g.ix.EnsureCollectionIssue(issue)
	g.writeMu.Unlock()
	return err
}

func rejectReplayRecord(g *Governor, path, entryName string, body []byte, occurredAt int64,
	sessionID, runtime, observationID, kind string, cause error) error {
	bodyDigest := observation.DigestBytes(body)
	if err := replayIssue(g, entryName, body, occurredAt, sessionID, runtime,
		observationID, kind, cause); err != nil {
		return fmt.Errorf("record rejected replay %s: %w", entryName, err)
	}
	if err := quarantineObservation(path, bodyDigest); err != nil {
		return fmt.Errorf("quarantine rejected replay %s: %w", entryName, err)
	}
	return nil
}

func replayObservationSpoolOnce(ctx context.Context, dataDir string, g *Governor) error {
	dir := filepath.Join(dataDir, "observation-spool")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var firstErr error
	processed := 0
	for _, entry := range entries {
		if processed >= replayBatchLimit || ctx.Err() != nil {
			break
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") ||
			!(strings.HasPrefix(entry.Name(), "obs_") || strings.HasPrefix(entry.Name(), "res_") ||
				strings.HasPrefix(entry.Name(), "cls_") || strings.HasPrefix(entry.Name(), "ent_") ||
				strings.HasPrefix(entry.Name(), "trn_")) {
			continue
		}
		processed++
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > observation.MaxEnvelopeBytes {
			if firstErr == nil {
				firstErr = fmt.Errorf("unsafe observation spool file %s", entry.Name())
			}
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		b, readErr := io.ReadAll(io.LimitReader(f, observation.MaxEnvelopeBytes+1))
		_ = f.Close()
		if readErr != nil || len(b) > observation.MaxEnvelopeBytes {
			if firstErr == nil {
				firstErr = fmt.Errorf("read observation spool %s: %v", entry.Name(), readErr)
			}
			continue
		}
		if strings.HasPrefix(entry.Name(), "trn_") {
			var envelope observation.SessionTurnEnvelope
			if err := json.Unmarshal(b, &envelope); err != nil {
				rejectErr := rejectReplayRecord(g, path, entry.Name(), b, info.ModTime().Unix(),
					"", "", "", "malformed", err)
				if firstErr == nil {
					if rejectErr != nil {
						firstErr = rejectErr
					} else {
						firstErr = fmt.Errorf("decode session turn spool %s: %w", entry.Name(), err)
					}
				}
				continue
			}
			if err := validateSessionTurnEnvelope(envelope); err != nil {
				rejectErr := rejectReplayRecord(g, path, entry.Name(), b, info.ModTime().Unix(),
					envelope.SessionID, envelope.Runtime, envelope.ObservationID, "malformed", err)
				if firstErr == nil {
					if rejectErr != nil {
						firstErr = rejectErr
					} else {
						firstErr = fmt.Errorf("validate session turn spool %s: %w", entry.Name(), err)
					}
				}
				continue
			}
			if handled, giveUpErr := giveUpUndeliverable(g, path, entry.Name(), b, info.ModTime().Unix(),
				envelope.SessionID, envelope.Runtime, envelope.ObservationID, envelope.DeliveryAttempts); handled {
				if giveUpErr != nil && firstErr == nil {
					firstErr = giveUpErr
				}
				continue
			}
			envelope.DeliveryMode = "replay"
			envelope.DeliveryAttempts++
			if err := rewriteReplayRecord(path, envelope); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			receipt, err := ingestSessionTurnV1(g, envelope)
			if err != nil {
				var validation *observationValidationError
				if errors.As(err, &validation) || errors.Is(err, store.ErrSessionTurnCollision) {
					kind := "malformed"
					if errors.Is(err, store.ErrSessionTurnCollision) {
						kind = "collision"
					}
					rejectErr := rejectReplayRecord(g, path, entry.Name(), b, info.ModTime().Unix(),
						envelope.SessionID, envelope.Runtime, envelope.ObservationID, kind, err)
					if rejectErr != nil && firstErr == nil {
						firstErr = rejectErr
					}
				}
				if firstErr == nil {
					firstErr = fmt.Errorf("replay session turn %s: %w", entry.Name(), err)
				}
				continue
			}
			if receipt.Schema != observation.SessionTurnSchemaV1 ||
				receipt.ObservationID != envelope.ObservationID {
				if firstErr == nil {
					firstErr = fmt.Errorf("session turn replay acknowledgement mismatch for %s", entry.Name())
				}
				continue
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) && firstErr == nil {
				firstErr = err
			}
			continue
		}
		if strings.HasPrefix(entry.Name(), "ent_") {
			var envelope observation.SessionEntryEnvelope
			if err := json.Unmarshal(b, &envelope); err != nil {
				rejectErr := rejectReplayRecord(g, path, entry.Name(), b, info.ModTime().Unix(),
					"", "", "", "malformed", err)
				if firstErr == nil {
					if rejectErr != nil {
						firstErr = rejectErr
					} else {
						firstErr = fmt.Errorf("decode session entry spool %s: %w", entry.Name(), err)
					}
				}
				continue
			}
			if err := validateSessionEntryEnvelope(envelope); err != nil {
				rejectErr := rejectReplayRecord(g, path, entry.Name(), b, info.ModTime().Unix(),
					envelope.SessionID, envelope.Runtime, envelope.ObservationID, "malformed", err)
				if firstErr == nil {
					if rejectErr != nil {
						firstErr = rejectErr
					} else {
						firstErr = fmt.Errorf("validate session entry spool %s: %w", entry.Name(), err)
					}
				}
				continue
			}
			if handled, giveUpErr := giveUpUndeliverable(g, path, entry.Name(), b, info.ModTime().Unix(),
				envelope.SessionID, envelope.Runtime, envelope.ObservationID, envelope.DeliveryAttempts); handled {
				if giveUpErr != nil && firstErr == nil {
					firstErr = giveUpErr
				}
				continue
			}
			envelope.DeliveryMode = "replay"
			envelope.DeliveryAttempts++
			if err := rewriteReplayRecord(path, envelope); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			receipt, err := ingestSessionEntryV1(ctx, g, envelope)
			if err != nil {
				var validation *observationValidationError
				if errors.As(err, &validation) || errors.Is(err, store.ErrSessionActivityCollision) {
					kind := "malformed"
					if errors.Is(err, store.ErrSessionActivityCollision) {
						kind = "collision"
					}
					rejectErr := rejectReplayRecord(g, path, entry.Name(), b, info.ModTime().Unix(),
						envelope.SessionID, envelope.Runtime, envelope.ObservationID, kind, err)
					if rejectErr != nil && firstErr == nil {
						firstErr = rejectErr
					}
				}
				if firstErr == nil {
					firstErr = fmt.Errorf("replay session entry %s: %w", entry.Name(), err)
				}
				continue
			}
			if receipt.Schema != observation.SessionEntrySchemaV1 ||
				receipt.ObservationID != envelope.ObservationID {
				if firstErr == nil {
					firstErr = fmt.Errorf("session entry replay acknowledgement mismatch for %s", entry.Name())
				}
				continue
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) && firstErr == nil {
				firstErr = err
			}
			continue
		}
		if strings.HasPrefix(entry.Name(), "res_") {
			var envelope observation.ResultEnvelope
			if err := json.Unmarshal(b, &envelope); err != nil {
				rejectErr := rejectReplayRecord(g, path, entry.Name(), b, info.ModTime().Unix(),
					"", "", "", "malformed", err)
				if firstErr == nil {
					if rejectErr != nil {
						firstErr = rejectErr
					} else {
						firstErr = fmt.Errorf("decode result spool %s: %w", entry.Name(), err)
					}
				}
				continue
			}
			if err := validateResultEnvelope(envelope); err != nil {
				rejectErr := rejectReplayRecord(g, path, entry.Name(), b, info.ModTime().Unix(),
					envelope.SessionID, envelope.Runtime, envelope.ObservationID, "malformed", err)
				if firstErr == nil {
					if rejectErr != nil {
						firstErr = rejectErr
					} else {
						firstErr = fmt.Errorf("validate result spool %s: %w", entry.Name(), err)
					}
				}
				continue
			}
			if handled, giveUpErr := giveUpUndeliverable(g, path, entry.Name(), b, info.ModTime().Unix(),
				envelope.SessionID, envelope.Runtime, envelope.ObservationID, envelope.DeliveryAttempts); handled {
				if giveUpErr != nil && firstErr == nil {
					firstErr = giveUpErr
				}
				continue
			}
			envelope.DeliveryMode = "replay"
			envelope.DeliveryAttempts++
			if err := rewriteReplayRecord(path, envelope); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			receipt, err := ingestResultV1(g, envelope)
			if err != nil || receipt.ResultID == 0 {
				var validation *observationValidationError
				if errors.As(err, &validation) || errors.Is(err, store.ErrResultObservationCollision) {
					kind := "malformed"
					if errors.Is(err, store.ErrResultObservationCollision) {
						kind = "result-collision"
					}
					rejectErr := rejectReplayRecord(g, path, entry.Name(), b, info.ModTime().Unix(),
						envelope.SessionID, envelope.Runtime, envelope.ObservationID, kind, err)
					if rejectErr != nil && firstErr == nil {
						firstErr = rejectErr
					}
				}
				if firstErr == nil {
					firstErr = fmt.Errorf("replay result %s: %w", entry.Name(), err)
				}
				continue
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) && firstErr == nil {
				firstErr = err
			}
			continue
		}
		if strings.HasPrefix(entry.Name(), "cls_") {
			var envelope observation.ClosureEnvelope
			if err := json.Unmarshal(b, &envelope); err != nil {
				rejectErr := rejectReplayRecord(g, path, entry.Name(), b, info.ModTime().Unix(),
					"", "", "", "malformed", err)
				if firstErr == nil {
					if rejectErr != nil {
						firstErr = rejectErr
					} else {
						firstErr = fmt.Errorf("decode closure spool %s: %w", entry.Name(), err)
					}
				}
				continue
			}
			if err := validateClosureEnvelope(envelope); err != nil {
				rejectErr := rejectReplayRecord(g, path, entry.Name(), b, info.ModTime().Unix(),
					envelope.SessionID, envelope.Runtime, envelope.ObservationID, "malformed", err)
				if firstErr == nil {
					if rejectErr != nil {
						firstErr = rejectErr
					} else {
						firstErr = fmt.Errorf("validate closure spool %s: %w", entry.Name(), err)
					}
				}
				continue
			}
			if handled, giveUpErr := giveUpUndeliverable(g, path, entry.Name(), b, info.ModTime().Unix(),
				envelope.SessionID, envelope.Runtime, envelope.ObservationID, envelope.DeliveryAttempts); handled {
				if giveUpErr != nil && firstErr == nil {
					firstErr = giveUpErr
				}
				continue
			}
			envelope.DeliveryMode = "replay"
			envelope.DeliveryAttempts++
			if err := rewriteReplayRecord(path, envelope); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			receipt, err := ingestClosureV1(g, envelope)
			if err != nil || receipt.ObservationID == "" {
				if firstErr == nil {
					firstErr = fmt.Errorf("replay closure %s: %w", entry.Name(), err)
				}
				continue
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) && firstErr == nil {
				firstErr = err
			}
			continue
		}
		var envelope observation.Envelope
		if err := json.Unmarshal(b, &envelope); err != nil {
			rejectErr := rejectReplayRecord(g, path, entry.Name(), b, info.ModTime().Unix(),
				"", "", "", "malformed", err)
			if firstErr == nil {
				if rejectErr != nil {
					firstErr = rejectErr
				} else {
					firstErr = fmt.Errorf("decode observation spool %s: %w", entry.Name(), err)
				}
			}
			continue
		}
		if err := validateObservationV1(envelope); err != nil {
			rejectErr := rejectReplayRecord(g, path, entry.Name(), b, info.ModTime().Unix(),
				envelope.SessionID, envelope.Runtime, envelope.ObservationID, "malformed", err)
			if firstErr == nil {
				if rejectErr != nil {
					firstErr = rejectErr
				} else {
					firstErr = fmt.Errorf("validate observation spool %s: %w", entry.Name(), err)
				}
			}
			continue
		}
		if handled, giveUpErr := giveUpUndeliverable(g, path, entry.Name(), b, info.ModTime().Unix(),
			envelope.SessionID, envelope.Runtime, envelope.ObservationID, envelope.DeliveryAttempts); handled {
			if giveUpErr != nil && firstErr == nil {
				firstErr = giveUpErr
			}
			continue
		}
		envelope.DeliveryMode = "replay"
		envelope.DeliveryAttempts++
		if err := rewriteReplayEnvelope(path, envelope); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("update replay attempt %s: %w", entry.Name(), err)
			}
			continue
		}
		receipt, err := ingestObservationV1(ctx, g, envelope)
		if err != nil {
			var validation *observationValidationError
			if errors.As(err, &validation) || errors.Is(err, ErrObservationCollision) {
				kind := "malformed"
				if errors.Is(err, ErrObservationCollision) {
					kind = "collision"
				}
				rejectErr := rejectReplayRecord(g, path, entry.Name(), b, info.ModTime().Unix(),
					envelope.SessionID, envelope.Runtime, envelope.ObservationID, kind, err)
				if rejectErr != nil && firstErr == nil {
					firstErr = rejectErr
				}
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("replay observation %s: %w", entry.Name(), err)
			}
			continue
		}
		if receipt.Schema != observation.SchemaV1 || receipt.ObservationID != envelope.ObservationID || receipt.EventID == 0 {
			if firstErr == nil {
				firstErr = fmt.Errorf("replay acknowledgement mismatch for %s", entry.Name())
			}
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func rewriteReplayEnvelope(path string, envelope observation.Envelope) error {
	return rewriteReplayRecord(path, envelope)
}

func rewriteReplayRecord(path string, envelope any) error {
	b, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	return rewriteReplayBytes(path, b)
}

func rewriteReplayBytes(path string, b []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".replay-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func quarantineObservationPath(path, bodyDigest string) string {
	identity := strings.TrimPrefix(bodyDigest, "sha256-v1:")
	return path + "." + identity + ".rejected"
}

func quarantineObservation(path, bodyDigest string) error {
	rejected := quarantineObservationPath(path, bodyDigest)
	dir, err := quarantineObservationOpenDir(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	if err := quarantineObservationRename(path, rejected); err != nil {
		return err
	}
	if syncErr := dir.Sync(); syncErr != nil {
		if rollbackErr := quarantineObservationRename(rejected, path); rollbackErr != nil {
			body, readErr := os.ReadFile(rejected)
			if readErr == nil {
				if len(body) > observation.MaxEnvelopeBytes {
					readErr = fmt.Errorf("rejected replay exceeds %d bytes", observation.MaxEnvelopeBytes)
				} else {
					readErr = rewriteReplayBytes(path, body)
				}
			}
			if readErr == nil {
				readErr = dir.Sync()
			}
			if readErr != nil {
				return errors.Join(syncErr, fmt.Errorf("restore rejected replay: %w", rollbackErr),
					fmt.Errorf("recreate pending replay: %w", readErr))
			}
			return errors.Join(syncErr, fmt.Errorf("restore rejected replay: %w", rollbackErr))
		}
		if rollbackSyncErr := dir.Sync(); rollbackSyncErr != nil {
			return errors.Join(syncErr, fmt.Errorf("sync restored replay: %w", rollbackSyncErr))
		}
		return syncErr
	}
	return nil
}

func startObservationReplay(dataDir string, g *Governor) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	err := replayObservationSpoolOnce(ctx, dataDir, g)
	cancel()
	if err != nil {
		log.Printf("observation replay pending: %v", err)
	}
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			err := replayObservationSpoolOnce(ctx, dataDir, g)
			cancel()
			if err != nil {
				log.Printf("observation replay pending: %v", err)
			}
		}
	}()
}
