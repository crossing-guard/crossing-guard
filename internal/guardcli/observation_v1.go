package guardcli

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"crossing-guard/engine"
	"crossing-guard/internal/observation"
)

const (
	maxObservationSpoolFiles = 10000
	maxObservationSpoolBytes = 512 << 20
	maxTypedProjectionBytes  = 256 << 10
)

func newObservationID() (string, error) {
	return newRecordID("obs_")
}

func newActionID() (string, error) {
	return newRecordID("act_")
}

func newRecordID(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

// buildRevision is the revision of the tree this binary was built from, set at link
// time by scripts/reload.sh (-X crossing-guard/internal/guardcli.buildRevision=...).
// Go's own stamp cannot be trusted for that: it ignores a linked worktree's .git file,
// so a worktree build is stamped with nothing, or with the enclosing checkout's commit.
var buildRevision string

// BuildVersion is this binary's version: the revision the build script passed when
// there is one; otherwise what the build recorded — the module version, the VCS
// revision when the version is unset or "(devel)", or both. The same string the hook
// stamps on every observation; the device report carries it as daemon.version.
func BuildVersion() string { return collectorVersion() }

func collectorVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return versionFrom(buildRevision, "", "")
	}
	stamped := ""
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			stamped = setting.Value
		}
	}
	return versionFrom(buildRevision, info.Main.Version, stamped)
}

// versionFrom chooses the version string. A link-time revision is the whole answer and
// is never joined with Go's stamp (the two can name different commits, and joined they
// would pass the 100 characters the device report allows).
func versionFrom(linked, module, stamped string) string {
	if linked != "" {
		return linked
	}
	if stamped == "" {
		return module
	}
	if module == "" || module == "(devel)" {
		return stamped
	}
	return module + "+" + stamped
}

func resolveDeclaredPath(in hookInput, raw string) (string, string) {
	if raw == "" {
		return "", "empty"
	}
	if strings.ContainsRune(raw, '\x00') {
		return "", "nul-byte"
	}
	p := filepath.Clean(raw)
	if filepath.IsAbs(p) {
		return p, "resolved"
	}
	if in.Cwd == "" || !filepath.IsAbs(in.Cwd) {
		return "", "cwd-unavailable"
	}
	base := filepath.Clean(in.Cwd)
	p = filepath.Clean(filepath.Join(base, p))
	rel, err := filepath.Rel(base, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "unsafe-traversal"
	}
	return p, "resolved"
}

func declaredClaim(in hookInput, kind, raw, operation, source string) observation.ResourceClaim {
	claim := observation.ResourceClaim{Kind: kind, RawIdentity: raw, Operation: operation,
		EvidenceClass: "declared", SourceField: source, Completeness: "complete"}
	switch kind {
	case "file", "path", "directory":
		claim.Identity, claim.Resolution = resolveDeclaredPath(in, raw)
		if claim.Identity == "" {
			claim.Completeness = "unresolved"
		}
	default:
		claim.Identity = claim.RawIdentity
		claim.Resolution = "resolved"
	}
	return claim
}

func structuredResourceClaims(in hookInput) []observation.ResourceClaim {
	claims := []observation.ResourceClaim{}
	add := func(c observation.ResourceClaim) {
		if c.RawIdentity == "" || len(claims) >= 512 {
			return
		}
		c.Ordinal = len(claims)
		claims = append(claims, c)
	}
	tool := engine.BareTool(in.ToolName)
	switch tool {
	case "Read":
		add(declaredClaim(in, "file", in.ToolInput.FilePath, "read", "tool_input.file_path"))
	case "Edit", "Write":
		add(declaredClaim(in, "file", in.ToolInput.FilePath, "write", "tool_input.file_path"))
	case "NotebookEdit":
		add(declaredClaim(in, "file", in.ToolInput.NotebookPath, "write", "tool_input.notebook_path"))
	case "Grep", "Glob":
		add(declaredClaim(in, "path", in.ToolInput.Path, "search", "tool_input.path"))
	case "WebFetch":
		add(declaredClaim(in, "url", in.ToolInput.URL, "connect", "tool_input.url"))
	case "Skill":
		add(declaredClaim(in, "skill", in.ToolInput.Skill, "use", "tool_input.skill"))
	case "apply_patch":
		for _, line := range strings.Split(string(in.ToolInput.Command), "\n") {
			for _, header := range []struct{ prefix, source string }{
				{"*** Add File: ", "tool_input.command.*** Add File"},
				{"*** Update File: ", "tool_input.command.*** Update File"},
				{"*** Delete File: ", "tool_input.command.*** Delete File"},
				{"*** Move to: ", "tool_input.command.*** Move to"},
			} {
				if strings.HasPrefix(line, header.prefix) {
					add(declaredClaim(in, "file", strings.TrimPrefix(line, header.prefix), "patch", header.source))
					break
				}
			}
		}
	default:
		if isShellTool(tool) {
			for _, claim := range shellResourceClaims(in) {
				add(claim)
			}
		} else if strings.HasPrefix(in.ToolName, "mcp__") {
			add(declaredClaim(in, "mcp", in.ToolName, "use", "tool_name"))
		}
	}
	return claims
}

func buildObservationEnvelope(in hookInput, decision, reason string) (observation.Envelope, error) {
	id, err := newObservationID()
	if err != nil {
		return observation.Envelope{}, err
	}
	now := time.Now().Unix()
	raw := append([]byte(nil), in.RawToolInput...)
	completeness := "unavailable"
	digest := ""
	rawBytes := len(raw)
	if len(raw) > 0 && string(raw) != "null" {
		digest = observation.DigestBytes(raw)
		completeness = "complete"
		if len(raw) > observation.MaxRetainedInput {
			raw = nil
			completeness = "metadata-only"
		}
	} else {
		raw = nil
	}
	content := in.ToolInput.Content
	if content == "" {
		content = in.ToolInput.NewString
	}
	if completeness == "metadata-only" {
		content = ""
	}
	command := string(in.ToolInput.Command)
	if len(content) > maxTypedProjectionBytes {
		content = ""
	}
	if len(command) > maxTypedProjectionBytes || completeness == "metadata-only" {
		command = ""
	}
	nativeID, nativeKind := in.ToolUseID, "tool_use_id"
	if nativeID == "" {
		nativeID = in.CallID
		if nativeID != "" {
			nativeKind = "call_id"
		} else {
			nativeKind = ""
		}
	}
	claims := structuredResourceClaims(in)
	files := []string{}
	for _, claim := range claims {
		if claim.Kind == "file" && claim.Identity != "" {
			files = append(files, claim.Identity)
		}
	}
	return observation.Envelope{Schema: observation.SchemaV1, ObservationID: id, ActionID: in.ActionID,
		CollectorID: observation.CollectorPreTool, CollectorVersion: collectorVersion(),
		NativeCallID: nativeID, NativeCallKind: nativeKind, TranscriptPath: in.TranscriptPath,
		SessionID: in.SessionID, Runtime: in.Runtime, Tool: in.ToolName,
		Command: command, Content: content, FilePath: in.ToolInput.FilePath,
		FilePaths: files, Cwd: in.Cwd, URL: in.ToolInput.URL, Skill: in.ToolInput.Skill,
		Decision: decision, Reason: reason, Rule: in.Rule, Layer: in.Layer,
		LayerReasons: in.LayerReasons, TS: now, ToolInput: raw, ToolInputBytes: rawBytes,
		ToolInputDigest: digest, ToolInputCompleteness: completeness, ResourceClaims: claims,
		QueuedAt: now, DeliveryAttempts: 1, DeliveryMode: "direct", Carrier: in.Carrier}, nil
}

func observationSpoolDir() string { return filepath.Join(dataDir(), "observation-spool") }

func spoolObservation(e observation.Envelope) (string, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	return spoolRecord(e.ObservationID, b)
}

func spoolRecord(id string, b []byte) (string, error) {
	dir := observationSpoolDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	if len(b) > observation.MaxEnvelopeBytes {
		return "", fmt.Errorf("observation envelope is %d bytes; maximum is %d", len(b), observation.MaxEnvelopeBytes)
	}
	if err := checkObservationSpoolCapacity(dir, int64(len(b)), maxObservationSpoolFiles, maxObservationSpoolBytes); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".pending-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := tmp.Write(b); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	path := filepath.Join(dir, id+".json")
	// Link is the no-replace atomic publish primitive here: a cryptographic-ID
	// collision must not overwrite a previously queued observation.
	if err := os.Link(tmpName, path); err != nil {
		return "", err
	}
	if err := os.Remove(tmpName); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if err := syncObservationSpoolDir(dir); err != nil {
		// Keep the published file: even when directory fsync is unavailable, deleting
		// the only recovery copy would make the failure strictly worse.
		return "", fmt.Errorf("sync observation spool directory: %w", err)
	}
	ok = true
	return path, nil
}

func syncObservationSpoolDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func checkObservationSpoolCapacity(dir string, addBytes int64, maxFiles int, maxBytes int64) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	files := 0
	var bytesUsed int64
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		files++
		bytesUsed += info.Size()
	}
	if files >= maxFiles || bytesUsed+addBytes > maxBytes {
		return fmt.Errorf("observation spool capacity reached: files=%d/%d bytes=%d/%d", files, maxFiles, bytesUsed, maxBytes)
	}
	return nil
}

func pendingObservations() ([]string, error) {
	entries, err := os.ReadDir(observationSpoolDir())
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	paths := []string{}
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), "obs_") && strings.HasSuffix(entry.Name(), ".json") {
			paths = append(paths, filepath.Join(observationSpoolDir(), entry.Name()))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func readSpooledObservation(path string) (observation.Envelope, error) {
	var e observation.Envelope
	info, err := os.Lstat(path)
	if err != nil {
		return e, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > observation.MaxEnvelopeBytes {
		return e, fmt.Errorf("unsafe observation spool file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return e, err
	}
	return e, nil
}

func sendObservation(addr, token string, e observation.Envelope) (observation.Receipt, error) {
	var receipt observation.Receipt
	body, err := json.Marshal(e)
	if err != nil {
		return receipt, err
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/govern/observe/v1", bytes.NewReader(body))
	if err != nil {
		return receipt, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-CG-Token", token)
	}
	req.Header.Set(observation.HookDeadlineHeader, observation.HookDeadlineValue(time.Now()))
	resp, err := (&http.Client{Timeout: observation.HookDeliveryBudget}).Do(req)
	if err != nil {
		return receipt, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return receipt, fmt.Errorf("observe rejected (%s): %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&receipt); err != nil {
		return receipt, err
	}
	if receipt.Schema != observation.SchemaV1 || receipt.ObservationID != e.ObservationID || receipt.EventID == 0 {
		return receipt, fmt.Errorf("observe acknowledgement did not confirm observation identity")
	}
	return receipt, nil
}
