package taskinput

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

type inputRecord struct {
	Input
	PreparedName string `json:"prepared_name"`
}

type scopeRecord struct {
	ID        string        `json:"id"`
	State     ScopeState    `json:"state"`
	Inputs    []inputRecord `json:"inputs"`
	CreatedAt int64         `json:"created_at"`
	ExpiresAt int64         `json:"expires_at"`
	TaskID    string        `json:"task_id,omitempty"`
	Problem   string        `json:"problem,omitempty"`
}

type Service struct {
	mu         sync.Mutex
	config     Config
	fs         fileSystem
	now        func() time.Time
	admissions chan struct{}
}

func NewService(root string, config Config, now func() time.Time) (*Service, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	fs, err := openFileSystem(root)
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	service := &Service{config: config, fs: fs, now: now,
		admissions: make(chan struct{}, config.Limits.MaxConcurrentAdmissions)}
	if err := service.Recover(); err != nil {
		return nil, err
	}
	return service, nil
}

func (service *Service) Stage(ctx context.Context, scopeID string, source Source, name string, body io.Reader) (Scope, Input, error) {
	if !source.Valid() {
		return Scope{}, Input{}, inputError("invalid_source", "attachment source must be picker, paste, or drop")
	}
	displayName, err := validateName(name, service.config.Limits)
	if err != nil {
		return Scope{}, Input{}, err
	}
	select {
	case service.admissions <- struct{}{}:
		defer func() { <-service.admissions }()
	case <-ctx.Done():
		return Scope{}, Input{}, ctx.Err()
	}

	service.mu.Lock()
	defer service.mu.Unlock()
	created := false
	var record scopeRecord
	if scopeID == "" {
		scopeID = opaqueID("scope_")
		now := service.now()
		record = scopeRecord{ID: scopeID, State: ScopeOpen, CreatedAt: now.UnixMilli(),
			ExpiresAt: now.Add(time.Duration(service.config.Limits.UnclaimedExpirySeconds) * time.Second).UnixMilli()}
		if err := service.fs.createScopeDirs(scopeID); err != nil {
			return Scope{}, Input{}, err
		}
		if err := service.fs.writeManifest(record); err != nil {
			_ = os.RemoveAll(service.fs.scopeDir(scopeID))
			return Scope{}, Input{}, err
		}
		created = true
	} else {
		if !validOpaqueID(scopeID, "scope_") {
			return Scope{}, Input{}, inputError("invalid_scope", "task input scope is invalid")
		}
		record, err = service.fs.readManifest(scopeID)
		if err != nil {
			return Scope{}, Input{}, inputError("scope_not_found", "task input scope was not found")
		}
	}
	cleanupCreated := func() {
		if created {
			_ = os.RemoveAll(service.fs.scopeDir(scopeID))
		}
	}
	if record.State != ScopeOpen {
		cleanupCreated()
		return Scope{}, Input{}, inputError("scope_closed", "task input scope is no longer open")
	}
	if record.ExpiresAt <= service.now().UnixMilli() {
		cleanupCreated()
		return Scope{}, Input{}, inputError("scope_expired", "task input scope has expired")
	}
	if len(record.Inputs) >= service.config.Limits.MaxItemsPerScope {
		cleanupCreated()
		return Scope{}, Input{}, inputError("scope_full", "task input scope reached its item limit")
	}

	incoming, err := service.fs.tempFile()
	if err != nil {
		cleanupCreated()
		return Scope{}, Input{}, err
	}
	incomingPath := incoming.Name()
	defer func() { _ = os.Remove(incomingPath) }()
	sourceBytes, err := copyBounded(incoming, body, service.config.Limits.MaxSourceBytesPerItem)
	closeErr := incoming.Close()
	if err != nil {
		cleanupCreated()
		return Scope{}, Input{}, err
	}
	if closeErr != nil {
		cleanupCreated()
		return Scope{}, Input{}, closeErr
	}
	scopeSource, scopePrepared := scopeTotals(record)
	if scopeSource+sourceBytes > service.config.Limits.MaxSourceBytesPerScope {
		cleanupCreated()
		return Scope{}, Input{}, inputError("scope_bytes", "task input scope exceeds its source-byte limit")
	}
	global, err := service.globalSourceBytes()
	if err != nil {
		cleanupCreated()
		return Scope{}, Input{}, err
	}
	if global+sourceBytes > service.config.Limits.MaxGlobalSourceBytes {
		cleanupCreated()
		return Scope{}, Input{}, inputError("global_bytes", "task input staging capacity is full")
	}

	admitted, err := admitFile(service.config, incomingPath, displayName, service.fs.tempFile)
	if err != nil {
		cleanupCreated()
		return Scope{}, Input{}, err
	}
	defer func() { _ = os.Remove(admitted.preparedPath) }()
	if scopePrepared+admitted.preparedBytes > service.config.Limits.MaxNormalizedBytesPerScope {
		cleanupCreated()
		return Scope{}, Input{}, inputError("scope_prepared_bytes", "task input scope exceeds its prepared-byte limit")
	}
	if admitted.kind == KindText {
		textBytes := int64(0)
		for _, item := range record.Inputs {
			if item.Kind == KindText {
				textBytes += item.PreparedBytes
			}
		}
		if textBytes+admitted.preparedBytes > service.config.Limits.MaxTextBytesPerScope {
			cleanupCreated()
			return Scope{}, Input{}, inputError("scope_text_bytes", "task input scope exceeds its text-byte limit")
		}
	}
	inputID := opaqueID("input_")
	original := service.fs.originalPath(scopeID, inputID)
	prepared := service.fs.preparedPath(scopeID, inputID, admitted.extension)
	if err := os.Rename(incomingPath, original); err != nil {
		cleanupCreated()
		return Scope{}, Input{}, err
	}
	if err := os.Rename(admitted.preparedPath, prepared); err != nil {
		_ = os.Remove(original)
		cleanupCreated()
		return Scope{}, Input{}, err
	}
	input := Input{ID: inputID, Name: displayName, Kind: admitted.kind, MediaType: admitted.mediaType,
		SourceBytes: sourceBytes, PreparedBytes: admitted.preparedBytes, PreviewAvailable: true,
		ExpiresAt: record.ExpiresAt}
	record.Inputs = append(record.Inputs, inputRecord{Input: input, PreparedName: filepath.Base(prepared)})
	if err := service.fs.writeManifest(record); err != nil {
		_ = os.Remove(original)
		_ = os.Remove(prepared)
		cleanupCreated()
		return Scope{}, Input{}, err
	}
	return publicScope(record), input, nil
}

func (service *Service) List(scopeID string) (Scope, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	record, err := service.openScope(scopeID)
	if err != nil {
		return Scope{}, err
	}
	return publicScope(record), nil
}

func (service *Service) Preview(scopeID, inputID string) (*os.File, Input, error) {
	service.mu.Lock()
	record, err := service.openScope(scopeID)
	if err != nil {
		service.mu.Unlock()
		return nil, Input{}, err
	}
	if record.State != ScopeOpen {
		service.mu.Unlock()
		return nil, Input{}, inputError("scope_closed", "task input scope is no longer open")
	}
	index := slices.IndexFunc(record.Inputs, func(input inputRecord) bool { return input.ID == inputID })
	if index < 0 {
		service.mu.Unlock()
		return nil, Input{}, inputError("input_not_found", "task input was not found")
	}
	input := record.Inputs[index]
	path := filepath.Join(service.fs.scopeDir(scopeID), "prepared", input.PreparedName)
	service.mu.Unlock()
	file, err := os.Open(path)
	if err != nil {
		return nil, Input{}, err
	}
	return file, input.Input, nil
}

func (service *Service) Remove(scopeID, inputID string) (Scope, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	record, err := service.openScope(scopeID)
	if err != nil {
		return Scope{}, err
	}
	if record.State != ScopeOpen {
		return Scope{}, inputError("scope_closed", "claimed task inputs cannot be removed")
	}
	index := slices.IndexFunc(record.Inputs, func(input inputRecord) bool { return input.ID == inputID })
	if index < 0 {
		return publicScope(record), nil
	}
	input := record.Inputs[index]
	if err := os.Remove(service.fs.originalPath(scopeID, input.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Scope{}, err
	}
	if err := os.Remove(filepath.Join(service.fs.scopeDir(scopeID), "prepared", input.PreparedName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Scope{}, err
	}
	record.Inputs = append(record.Inputs[:index], record.Inputs[index+1:]...)
	if err := service.fs.writeManifest(record); err != nil {
		return Scope{}, err
	}
	return publicScope(record), nil
}

func (service *Service) Preflight(scopeID string, inputIDs []string) ([]ResolvedInput, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.resolveOpen(scopeID, inputIDs)
}

func (service *Service) Claim(scopeID string, inputIDs []string, taskID string) (Claim, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	resolved, err := service.resolveOpen(scopeID, inputIDs)
	if err != nil {
		return Claim{}, err
	}
	record, err := service.fs.readManifest(scopeID)
	if err != nil {
		return Claim{}, err
	}
	record.State, record.TaskID = ScopeClaimed, taskID
	if err := service.fs.writeManifest(record); err != nil {
		return Claim{}, err
	}
	return Claim{ScopeID: scopeID, TaskID: taskID, Inputs: resolved}, nil
}

func (service *Service) ConsumeTask(taskID string) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	entries, err := os.ReadDir(filepath.Join(service.fs.root, "scopes"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		record, err := service.fs.readManifest(entry.Name())
		if err != nil || record.TaskID != taskID {
			continue
		}
		if err := os.RemoveAll(service.fs.scopeDir(record.ID)); err != nil {
			record.State, record.Problem = ScopeRecoveryRequired, err.Error()
			_ = service.fs.writeManifest(record)
			return err
		}
	}
	return nil
}

func (service *Service) Recover() error {
	service.mu.Lock()
	defer service.mu.Unlock()
	entries, err := os.ReadDir(filepath.Join(service.fs.root, "scopes"))
	if err != nil {
		return err
	}
	now := service.now().UnixMilli()
	for _, entry := range entries {
		if !entry.IsDir() || !validOpaqueID(entry.Name(), "scope_") {
			continue
		}
		record, err := service.fs.readManifest(entry.Name())
		if err != nil {
			return err
		}
		if record.State == ScopeClaimed || (record.State == ScopeOpen && record.ExpiresAt <= now) {
			if err := os.RemoveAll(service.fs.scopeDir(record.ID)); err != nil {
				record.State, record.Problem = ScopeRecoveryRequired, err.Error()
				if writeErr := service.fs.writeManifest(record); writeErr != nil {
					return writeErr
				}
			}
		}
	}
	return nil
}

func (service *Service) openScope(scopeID string) (scopeRecord, error) {
	if !validOpaqueID(scopeID, "scope_") {
		return scopeRecord{}, inputError("invalid_scope", "task input scope is invalid")
	}
	record, err := service.fs.readManifest(scopeID)
	if err != nil {
		return scopeRecord{}, inputError("scope_not_found", "task input scope was not found")
	}
	if record.State == ScopeOpen && record.ExpiresAt <= service.now().UnixMilli() {
		return scopeRecord{}, inputError("scope_expired", "task input scope has expired")
	}
	return record, nil
}

func (service *Service) resolveOpen(scopeID string, inputIDs []string) ([]ResolvedInput, error) {
	if len(inputIDs) == 0 {
		return nil, inputError("inputs_required", "at least one task input is required")
	}
	record, err := service.openScope(scopeID)
	if err != nil {
		return nil, err
	}
	if record.State != ScopeOpen {
		return nil, inputError("scope_closed", "task input scope is no longer open")
	}
	if len(inputIDs) != len(record.Inputs) {
		return nil, inputError("scope_mismatch", "task input IDs must include the complete staged scope")
	}
	seen := map[string]struct{}{}
	resolved := make([]ResolvedInput, 0, len(inputIDs))
	for _, inputID := range inputIDs {
		if _, duplicate := seen[inputID]; duplicate {
			return nil, inputError("duplicate_input", "task input IDs must be unique")
		}
		seen[inputID] = struct{}{}
		index := slices.IndexFunc(record.Inputs, func(input inputRecord) bool { return input.ID == inputID })
		if index < 0 {
			return nil, inputError("input_not_found", "task input %q was not found in this scope", inputID)
		}
		input := record.Inputs[index]
		path := filepath.Join(service.fs.scopeDir(scopeID), "prepared", input.PreparedName)
		if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
			return nil, inputError("input_unavailable", "prepared task input is unavailable")
		}
		resolved = append(resolved, ResolvedInput{Input: input.Input, Path: path})
	}
	return resolved, nil
}

func (service *Service) globalSourceBytes() (int64, error) {
	entries, err := os.ReadDir(filepath.Join(service.fs.root, "scopes"))
	if err != nil {
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		record, err := service.fs.readManifest(entry.Name())
		if err != nil {
			return 0, err
		}
		for _, input := range record.Inputs {
			total += input.SourceBytes
		}
	}
	return total, nil
}

func scopeTotals(record scopeRecord) (int64, int64) {
	var source, prepared int64
	for _, input := range record.Inputs {
		source += input.SourceBytes
		prepared += input.PreparedBytes
	}
	return source, prepared
}

func publicScope(record scopeRecord) Scope {
	inputs := make([]Input, len(record.Inputs))
	for index := range record.Inputs {
		inputs[index] = record.Inputs[index].Input
	}
	return Scope{ID: record.ID, State: record.State, Inputs: inputs, ExpiresAt: record.ExpiresAt}
}

func opaqueID(prefix string) string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		panic(fmt.Sprintf("generate task input ID: %v", err))
	}
	return prefix + hex.EncodeToString(raw)
}

func validOpaqueID(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+32 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil
}
