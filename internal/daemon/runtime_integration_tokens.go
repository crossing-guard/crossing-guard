package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

const (
	runtimeIntegrationPreviewTTL = 5 * time.Minute
	runtimeIntegrationPreviewMax = 64
)

type runtimeIntegrationPreviewEntry struct {
	Runtime   string
	Operation string
	Digest    string
	ExpiresAt time.Time
	IssuedAt  time.Time
}

type runtimeIntegrationWatchEntry struct {
	Runtime            string
	DisplayName        string
	Surface            string
	SurfaceLabel       string
	AfterEventID       int64
	ScanEventID        int64
	ConnectionRevision string
	ExpiresAt          time.Time
	IssuedAt           time.Time
	VisibleConfirmed   bool
	ObservedEventID    int64
	ObservedSessionID  string
	ObservedTool       string
	ObservedDecision   string
	DeniedEventID      int64
	DeniedSessionID    string
	DeniedTool         string
	DeniedDecision     string
}

type runtimeIntegrationWatchObservation struct {
	EventID   int64
	SessionID string
	Tool      string
	Decision  string
	Denied    bool
}

// runtimeIntegrationStateStore owns short-lived, in-memory authority for both
// preview commits and verification watches. Tokens are opaque, bounded, and never
// persisted or exposed through provider configuration.
type runtimeIntegrationStateStore struct {
	mu      sync.Mutex
	entries map[string]runtimeIntegrationPreviewEntry
	watches map[string]runtimeIntegrationWatchEntry
	now     func() time.Time
	random  func([]byte) (int, error)
}

func newRuntimeIntegrationPreviewStore() *runtimeIntegrationStateStore {
	return &runtimeIntegrationStateStore{
		entries: map[string]runtimeIntegrationPreviewEntry{},
		watches: map[string]runtimeIntegrationWatchEntry{},
		now:     time.Now,
		random:  rand.Read,
	}
}

var runtimeIntegrationPreviews = newRuntimeIntegrationPreviewStore()

func (store *runtimeIntegrationStateStore) issue(runtimeName, operation, digest string) (string, time.Time, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	now := store.now()
	store.purgeExpired(now)
	if len(store.entries) >= runtimeIntegrationPreviewMax {
		oldestToken := ""
		var oldest time.Time
		for token, entry := range store.entries {
			if oldestToken == "" || entry.IssuedAt.Before(oldest) {
				oldestToken, oldest = token, entry.IssuedAt
			}
		}
		delete(store.entries, oldestToken)
	}
	token, err := store.randomToken()
	if err != nil {
		return "", time.Time{}, err
	}
	expiresAt := now.Add(runtimeIntegrationPreviewTTL)
	store.entries[token] = runtimeIntegrationPreviewEntry{
		Runtime: runtimeName, Operation: operation, Digest: digest,
		ExpiresAt: expiresAt, IssuedAt: now,
	}
	return token, expiresAt, nil
}

func (store *runtimeIntegrationStateStore) issueWatch(watch runtimeIntegrationWatchEntry) (string, time.Time, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	now := store.now()
	store.purgeExpired(now)
	if len(store.watches) >= runtimeIntegrationPreviewMax {
		oldestToken := ""
		var oldest time.Time
		for token, entry := range store.watches {
			if oldestToken == "" || entry.IssuedAt.Before(oldest) {
				oldestToken, oldest = token, entry.IssuedAt
			}
		}
		delete(store.watches, oldestToken)
	}
	token, err := store.randomToken()
	if err != nil {
		return "", time.Time{}, err
	}
	watch.IssuedAt = now
	watch.ExpiresAt = now.Add(runtimeIntegrationPreviewTTL)
	store.watches[token] = watch
	return token, watch.ExpiresAt, nil
}

func (store *runtimeIntegrationStateStore) consume(token, runtimeName, operation string) (runtimeIntegrationPreviewEntry, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.purgeExpired(store.now())
	entry, ok := store.entries[token]
	if !ok {
		return runtimeIntegrationPreviewEntry{}, errors.New("preview token is expired, already used, or unknown; preview again")
	}
	delete(store.entries, token)
	if entry.Runtime != runtimeName || entry.Operation != operation {
		return runtimeIntegrationPreviewEntry{}, errors.New("preview token does not match this runtime and operation; preview again")
	}
	return entry, nil
}

func (store *runtimeIntegrationStateStore) watch(token, runtimeName string) (runtimeIntegrationWatchEntry, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.purgeExpired(store.now())
	watch, ok := store.watches[token]
	if !ok || watch.Runtime != runtimeName {
		return runtimeIntegrationWatchEntry{}, errors.New("watch is expired or unknown; start a new watch")
	}
	return watch, nil
}

func (store *runtimeIntegrationStateStore) confirmVisible(token, runtimeName string) (runtimeIntegrationWatchEntry, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.purgeExpired(store.now())
	watch, ok := store.watches[token]
	if !ok || watch.Runtime != runtimeName {
		return runtimeIntegrationWatchEntry{}, errors.New("watch is expired or unknown; start a new watch")
	}
	watch.VisibleConfirmed = true
	store.watches[token] = watch
	return watch, nil
}

func (store *runtimeIntegrationStateStore) recordWatchEvents(token, runtimeName string, expectedScan int64,
	events []runtimeIntegrationWatchObservation) (runtimeIntegrationWatchEntry, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.purgeExpired(store.now())
	watch, ok := store.watches[token]
	if !ok || watch.Runtime != runtimeName {
		return runtimeIntegrationWatchEntry{}, errors.New("watch is expired or unknown; start a new watch")
	}
	if watch.ScanEventID != expectedScan {
		return watch, nil
	}
	for _, event := range events {
		if event.EventID <= watch.ScanEventID {
			continue
		}
		watch.ScanEventID = event.EventID
		watch.ObservedEventID = event.EventID
		watch.ObservedSessionID = event.SessionID
		watch.ObservedTool = event.Tool
		watch.ObservedDecision = event.Decision
		if event.Denied {
			watch.DeniedEventID = event.EventID
			watch.DeniedSessionID = event.SessionID
			watch.DeniedTool = event.Tool
			watch.DeniedDecision = event.Decision
		}
	}
	store.watches[token] = watch
	return watch, nil
}

func (store *runtimeIntegrationStateStore) purgeExpired(now time.Time) {
	for token, entry := range store.entries {
		if !now.Before(entry.ExpiresAt) {
			delete(store.entries, token)
		}
	}
	for token, watch := range store.watches {
		if !now.Before(watch.ExpiresAt) {
			delete(store.watches, token)
		}
	}
}

func (store *runtimeIntegrationStateStore) randomToken() (string, error) {
	for attempt := 0; attempt < 4; attempt++ {
		bytes := make([]byte, 32)
		if _, err := store.random(bytes); err != nil {
			return "", err
		}
		token := hex.EncodeToString(bytes)
		if _, previewExists := store.entries[token]; previewExists {
			continue
		}
		if _, watchExists := store.watches[token]; watchExists {
			continue
		}
		return token, nil
	}
	return "", errors.New("could not allocate a unique token")
}
