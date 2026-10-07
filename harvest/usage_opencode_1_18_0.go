package harvest

// OpenCode usage calls (token-usage-analytics plan §3.3), measured against
// OpenCode 1.18.0. Each assistant message states its tokens once, at its one
// step-finish (3,517 of 3,517 measured), and per-message sums equal the
// session columns (80 of 80). A message still in flight states all zeros, so
// it is not yet a call; sessions are small, so a changed session is re-read
// whole rather than tracked by cursor, and the in-flight message is recorded
// on a later read (red-team S2-7).

import (
	"context"
	"encoding/json"
	"fmt"
)

const opencodeUsageReaderIdentity = "opencode-1.18.0/usage-1"

type openCodeUsageMessage struct {
	Role       string   `json:"role"`
	ModelID    string   `json:"modelID"`
	ProviderID string   `json:"providerID"`
	Cost       *float64 `json:"cost"`
	Time       struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Tokens *struct {
		Input     *int64 `json:"input"`
		Output    *int64 `json:"output"`
		Reasoning *int64 `json:"reasoning"`
		Cache     *struct {
			Read  *int64 `json:"read"`
			Write *int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
}

func (opencodeRuntime) UsageReader() string { return opencodeUsageReaderIdentity }

// UsageSources lists one source per session from the session listing the
// summary scan already uses; its update marker changes when a message lands.
func (opencodeRuntime) UsageSources(ctx context.Context) ([]UsageSourceRef, error) {
	path := opencodeDBPath()
	records, err := listOpenCodeProjectionRecords(ctx, path, "", false)
	if err != nil {
		return nil, err
	}
	refs := make([]UsageSourceRef, 0, len(records))
	for _, record := range records {
		summary := record.Record.Summary
		refs = append(refs, UsageSourceRef{Key: summary.ID, Session: summary.ID,
			ParentSession: summary.ParentID, Marker: record.Record.Ref.UpdateMarker,
			Modified: summary.Modified, locator: path})
	}
	sortUsageSources(refs)
	return refs, nil
}

// ReadUsage reads every assistant message of one session. It carries no
// cursor: each read is the whole session.
func (opencodeRuntime) ReadUsage(ctx context.Context, request UsageReadRequest) (UsageBatch, error) {
	path := request.Source.locator
	if path == "" {
		return UsageBatch{}, errUsageLocator
	}
	db, err := openOpenCodeReadOnly(path)
	if err != nil {
		return UsageBatch{}, err
	}
	defer db.Close()
	session := request.Source.Session
	var version string
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(version,'') FROM session WHERE id=?`,
		session).Scan(&version); err != nil {
		return UsageBatch{}, fmt.Errorf("read OpenCode session %s: %w", session, err)
	}
	rows, err := db.QueryContext(ctx, `SELECT id,data FROM message WHERE session_id=?
		AND json_extract(data,'$.role')='assistant' ORDER BY json_extract(data,'$.time.created'),id`, session)
	if err != nil {
		return UsageBatch{}, fmt.Errorf("query OpenCode messages: %w", err)
	}
	defer rows.Close()
	batch := UsageBatch{Complete: true, ParentSession: request.Source.ParentSession}
	for rows.Next() {
		var id string
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			return UsageBatch{}, err
		}
		batch.BytesRead += int64(len(data))
		var message openCodeUsageMessage
		if json.Unmarshal(data, &message) != nil {
			continue
		}
		if call, ok := openCodeUsageCall(id, session, request.Source.ParentSession, version, message); ok {
			batch.Calls = append(batch.Calls, call)
		}
	}
	return batch, rows.Err()
}

// openCodeUsageCall maps one assistant message. OpenCode counts reasoning
// apart from output, so the neutral output is their sum (catalog step 0).
func openCodeUsageCall(id, session, parent, version string, message openCodeUsageMessage) (UsageCall, bool) {
	if message.Tokens == nil {
		return UsageCall{}, false
	}
	completed := message.Time.Completed
	if completed == 0 {
		completed = message.Time.Created
	}
	tokens := message.Tokens
	call := UsageCall{ID: id, Session: session, ParentSession: parent,
		FirstAt: unixMilli(message.Time.Created).UTC(), At: unixMilli(completed).UTC(),
		Client: version, TokenClasses: TokenClasses{Input: tokens.Input, Reasoning: tokens.Reasoning}}
	if message.ProviderID != "" || message.ModelID != "" {
		call.Model = message.ProviderID + "/" + message.ModelID
	}
	if tokens.Output != nil {
		output := *tokens.Output + valueOf(tokens.Reasoning)
		call.Output = &output
	}
	if tokens.Cache != nil {
		call.CacheRead, call.CacheWrite = tokens.Cache.Read, tokens.Cache.Write
	}
	if message.Cost != nil {
		call.Cost = &Cost{Amount: *message.Cost, Unit: "USD", Basis: CostBasisRuntime}
	}
	return AdmitUsageCall(call)
}
