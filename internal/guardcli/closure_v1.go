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

func buildClosureEnvelope(in hookInput) (observation.ClosureEnvelope, error) {
	id, err := newRecordID("cls_")
	if err != nil {
		return observation.ClosureEnvelope{}, err
	}
	now := time.Now().Unix()
	return observation.ClosureEnvelope{Schema: observation.ClosureSchemaV1, ObservationID: id,
		CollectorID: observation.CollectorClosure, CollectorVersion: collectorVersion(),
		Runtime: in.Runtime, SessionID: in.SessionID, HookEventName: in.HookEventName,
		TranscriptPath: in.TranscriptPath, Cwd: in.Cwd, ObservedAt: now, QueuedAt: now,
		DeliveryAttempts: 1, DeliveryMode: "direct"}, nil
}

func spoolClosure(e observation.ClosureEnvelope) (string, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	return spoolRecord(e.ObservationID, b)
}

func sendClosure(addr, token string, e observation.ClosureEnvelope) (observation.ClosureReceipt, error) {
	var receipt observation.ClosureReceipt
	body, err := json.Marshal(e)
	if err != nil {
		return receipt, err
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/govern/closure/v1", bytes.NewReader(body))
	if err != nil {
		return receipt, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-CG-Token", token)
	}
	resp, err := (&http.Client{Timeout: observation.HookDeliveryBudget}).Do(req)
	if err != nil {
		return receipt, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return receipt, fmt.Errorf("closure rejected (%s): %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&receipt); err != nil {
		return receipt, err
	}
	if receipt.Schema != observation.ClosureSchemaV1 || receipt.ObservationID != e.ObservationID {
		return receipt, fmt.Errorf("closure acknowledgement did not confirm observation identity")
	}
	return receipt, nil
}

func emitClosure(in hookInput) {
	if os.Getenv("CG_OBSERVE") == "0" {
		return
	}
	envelope, err := buildClosureEnvelope(in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[crossing-guard] closure envelope failed:", err)
		return
	}
	spoolPath, err := spoolClosure(envelope)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[crossing-guard] closure spool failed:", err)
		return
	}
	addr, token, ok := daemonEndpoint()
	if !ok {
		return
	}
	if _, err := sendClosure(addr, token, envelope); err != nil {
		fmt.Fprintln(os.Stderr, "[crossing-guard] closure pending:", err)
		return
	}
	if err := os.Remove(spoolPath); err == nil {
		_ = syncObservationSpoolDir(filepath.Dir(spoolPath))
	}
}
