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

// Turn-boundary collection. The installer bakes `--observe <kind>` into the
// vendor's hook command, where <kind> is OUR vocabulary (observation.
// SessionTurnKinds). This file therefore never inspects the provider's event
// name to decide anything; it only records it as inspectable provenance.

func buildSessionTurnEnvelope(in hookInput, kind string) (observation.SessionTurnEnvelope, error) {
	if !observation.SessionTurnKinds[kind] {
		return observation.SessionTurnEnvelope{}, fmt.Errorf("unknown turn kind %q", kind)
	}
	id, err := newRecordID("trn_")
	if err != nil {
		return observation.SessionTurnEnvelope{}, err
	}
	native := in.HookEventName
	// The sub-type is provenance: one that would push the value past the
	// daemon's bound is left out, never truncated, so it cannot cost the turn.
	if in.NotificationType != "" &&
		len(native)+1+len(in.NotificationType) <= observation.MaxNativeSourceBytes {
		native += ":" + in.NotificationType
	}
	now := time.Now().Unix()
	return observation.SessionTurnEnvelope{
		Schema: observation.SessionTurnSchemaV1, ObservationID: id,
		CollectorID: observation.CollectorSessionTurn, CollectorVersion: collectorVersion(),
		Runtime: in.Runtime, SessionID: in.SessionID, Kind: kind, NativeSource: native,
		TranscriptPath: in.TranscriptPath, Cwd: in.Cwd, ObservedAt: now, QueuedAt: now,
		DeliveryAttempts: 1, DeliveryMode: "direct", Carrier: in.Carrier,
	}, nil
}

// HookTurnEnvelope is the session-turn envelope the installed hook for runtime posts
// for one raw payload at kind: the payload decoded by cmdHook's own reader, so the
// envelope's Carrier is the hook's answer and not a caller's. The daemon's tests drive
// recorded vendor payloads through it to the handlers.
func HookTurnEnvelope(runtime string, payload io.Reader, kind string) (observation.SessionTurnEnvelope, error) {
	in, err := readHookInput(payload, runtime, "")
	if err != nil {
		return observation.SessionTurnEnvelope{}, err
	}
	return buildSessionTurnEnvelope(in, kind)
}

func spoolSessionTurn(turn observation.SessionTurnEnvelope) (string, error) {
	body, err := json.Marshal(turn)
	if err != nil {
		return "", err
	}
	return spoolRecord(turn.ObservationID, body)
}

func sendSessionTurn(addr, token string, turn observation.SessionTurnEnvelope) (observation.SessionTurnReceipt, error) {
	var receipt observation.SessionTurnReceipt
	body, err := json.Marshal(turn)
	if err != nil {
		return receipt, err
	}
	request, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/govern/session-turn/v1", bytes.NewReader(body))
	if err != nil {
		return receipt, err
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("X-CG-Token", token)
	}
	request.Header.Set(observation.HookDeadlineHeader, observation.HookDeadlineValue(time.Now()))
	response, err := (&http.Client{Timeout: observation.HookDeliveryBudget}).Do(request)
	if err != nil {
		return receipt, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return receipt, fmt.Errorf("session turn rejected (%s): %s", response.Status, strings.TrimSpace(string(message)))
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&receipt); err != nil {
		return receipt, err
	}
	if receipt.Schema != observation.SessionTurnSchemaV1 || receipt.ObservationID != turn.ObservationID {
		return receipt, fmt.Errorf("session turn acknowledgement did not confirm observation identity")
	}
	return receipt, nil
}

// emitSessionTurn spools first, then delivers; a daemon that is down loses
// nothing — the spool replays on its next start.
func emitSessionTurn(in hookInput, kind string) []observation.Delivery {
	if os.Getenv("CG_OBSERVE") == "0" {
		return nil
	}
	turn, err := buildSessionTurnEnvelope(in, kind)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[crossing-guard] session turn envelope failed:", err)
		return nil
	}
	spoolPath, err := spoolSessionTurn(turn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[crossing-guard] session turn spool failed:", err)
		return nil
	}
	addr, token, ok := daemonEndpoint()
	if !ok {
		return nil
	}
	receipt, err := sendSessionTurn(addr, token, turn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[crossing-guard] session turn pending:", err)
		return nil
	}
	if err := os.Remove(spoolPath); err == nil {
		_ = syncObservationSpoolDir(filepath.Dir(spoolPath))
	}
	return receipt.Deliveries
}
