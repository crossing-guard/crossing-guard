package guardcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"crossing-guard/internal/observation"
)

func buildSessionEntryEnvelope(in hookInput) (observation.SessionEntryEnvelope, error) {
	id, err := newRecordID("ent_")
	if err != nil {
		return observation.SessionEntryEnvelope{}, err
	}
	now := time.Now().Unix()
	// The ticket is copied as it is, when present and of a ticket's size: its
	// meaning is the daemon's. A session the person started in a terminal has
	// none, and claims nothing.
	ticket := os.Getenv(observation.HandoffTicketEnv)
	if len(ticket) > observation.MaxHandoffTicketBytes {
		ticket = ""
	}
	return observation.SessionEntryEnvelope{
		HandoffTicket: ticket,
		Schema:        observation.SessionEntrySchemaV1, ObservationID: id,
		CollectorID: observation.CollectorSessionEntry, CollectorVersion: collectorVersion(),
		Runtime: in.Runtime, SessionID: in.SessionID, HookEventName: in.HookEventName,
		EntryKind: normalizeSessionEntry(in.Runtime, in.Source), NativeSource: in.Source,
		TranscriptPath: in.TranscriptPath, Cwd: in.Cwd, ObservedAt: now, QueuedAt: now,
		DeliveryAttempts: 1, DeliveryMode: "direct",
	}, nil
}

func spoolSessionEntry(entry observation.SessionEntryEnvelope) (string, error) {
	body, err := json.Marshal(entry)
	if err != nil {
		return "", err
	}
	return spoolRecord(entry.ObservationID, body)
}

func sendSessionEntry(addr, token string, entry observation.SessionEntryEnvelope) (observation.SessionEntryReceipt, error) {
	var receipt observation.SessionEntryReceipt
	body, err := json.Marshal(entry)
	if err != nil {
		return receipt, err
	}
	request, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/govern/session-entry/v1", bytes.NewReader(body))
	if err != nil {
		return receipt, err
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("X-CG-Token", token)
	}
	response, err := (&http.Client{Timeout: observation.HookDeliveryBudget}).Do(request)
	if err != nil {
		return receipt, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return receipt, fmt.Errorf("session entry rejected (%s): %s", response.Status, strings.TrimSpace(string(message)))
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&receipt); err != nil {
		return receipt, err
	}
	if receipt.Schema != observation.SessionEntrySchemaV1 || receipt.ObservationID != entry.ObservationID {
		return receipt, fmt.Errorf("session entry acknowledgement did not confirm observation identity")
	}
	switch receipt.Checkpoint.Status {
	case "complete", "failed", "unavailable":
		return receipt, nil
	default:
		return receipt, fmt.Errorf("session entry checkpoint remained %s", receipt.Checkpoint.Status)
	}
}

func emitSessionEntry(in hookInput) {
	if os.Getenv("CG_OBSERVE") == "0" {
		return
	}
	entry, err := buildSessionEntryEnvelope(in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[crossing-guard] session entry envelope failed:", err)
		return
	}
	spoolPath, err := spoolSessionEntry(entry)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[crossing-guard] session entry spool failed:", err)
		return
	}
	addr, token, ok := daemonEndpoint()
	if !ok {
		return
	}
	if _, err := sendSessionEntry(addr, token, entry); err != nil {
		fmt.Fprintln(os.Stderr, "[crossing-guard] session entry pending:", err)
		return
	}
	if err := os.Remove(spoolPath); err == nil {
		_ = syncObservationSpoolDir(filepath.Dir(spoolPath))
	}
}
