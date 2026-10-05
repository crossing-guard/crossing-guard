// Package daemon is the ONE daemon (ADR 0018) — the console and its API,
// folded from the consoleprobe experiment at M4 slice C:
//   - harvests Claude Code (~/.claude/projects) and Codex (~/.codex/sessions)
//     sessions and serves the normalized cross-agent session browser
//   - serves the memory store (direct package import — no subprocess)
//   - the policy surface over the in-process guardcli evaluator
//   - the live tamper-evident ledger (shared engine)
//   - fallback chat by spawning the official `claude` / `codex` binaries
//     (vendor binaries are the adapter boundary — the one allowed spawn)
//
// Run `crossing-guard serve` from a plain terminal for chat (claude OAuth
// cannot refresh inside a host-managed session); everything else works
// anywhere.
package daemon

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"crossing-guard/internal/detectorselection"
	"crossing-guard/internal/guardcli"
	"crossing-guard/internal/installprofile"
	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/internal/platform"
	"crossing-guard/internal/rulebook"
	"crossing-guard/internal/workspace"
)

//go:embed static
var staticFS embed.FS

// readFileString reads a small file, returning "" when absent — used for the
// token/address rendezvous files where absence is normal, not an error.
func readFileString(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// Main runs the daemon; args is everything after the `serve` verb.
func Main(args []string) {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := flags.String("addr", defaultAddr, "listen address (loopback only)")
	dataDir := flags.String("data", defaultDataDir(), "directory for memory/policy records")
	authToken := flags.String("token", "", "API bearer token (random if empty; printed at startup)")
	ledgerPath := flags.String("ledger", "", "live-ledger file (default: <data>/ledger.jsonl)")
	detectorsPath := flags.String("detectors", "", "engine detectors.json (default: $CG_DETECTORS, then <data>/detectors.json)")
	policyPath := flags.String("policy", "", "engine policy.json (default: $CG_POLICY, then <data>/policy-engine.json)")
	noHookInstall := flags.Bool("no-hook-install", false, "do not ensure/repair vendor hooks on startup (default: ensure them)")
	flags.Parse(args)
	if err := dropInheritedHandoffTicket(); err != nil {
		log.Printf("handoff: the daemon's own environment could not be cleared of a ticket: %v", err)
	}

	// Fix the index location ONCE, from the flag the operator actually passed.
	// flags.Visit reports only explicitly-set flags, which is the distinction that
	// matters: a DEFAULTED --data must not silently override $CG_INDEX, but an
	// explicit one must win — and must take the database with it, which is exactly
	// what it failed to do before (D1).
	explicitData := ""
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "data" {
			explicitData = *dataDir
		}
	})
	log.Printf("index: %s", setIndexPath(explicitData))

	install, err := installprofile.Ensure(*dataDir)
	if err != nil {
		log.Fatalf("installation profile: %v", err)
	}
	if install.Created {
		log.Printf("installation profile created: cohort=%s basis=%s path=%s",
			install.Profile.Cohort, install.Profile.Basis, install.Path)
	}
	store := NewStore(*dataDir)
	initTaskInputs(*dataDir)
	defer closeTaskInputs()
	if taskInputs == nil {
		log.Printf("task inputs unavailable (%s) — text-only tasks remain available", taskInputsProblem)
	} else {
		log.Printf("task input staging ready: %s", filepath.Join(*dataDir, "task-inputs"))
	}
	profileOwner, err := profilefs.New(*dataDir)
	if err != nil {
		log.Fatalf("orchestration profile configuration: %v", err)
	}
	transcriptCoordinator := newDaemonTranscriptIndexCoordinator(indexPath())

	// Live ledger (P3) — folded into the one daemon via the shared engine package.
	// Non-fatal: absent config leaves harvest/memory/policy fully working.
	// Config resolution (dependency rule 9: no experiment-sibling defaults):
	// flag > env > data dir.
	lp := *ledgerPath
	if lp == "" {
		lp = filepath.Join(*dataDir, "ledger.jsonl")
	}
	dp, pp := *detectorsPath, *policyPath
	if dp == "" {
		dp = os.Getenv("CG_DETECTORS")
	}
	if pp == "" {
		if pp = os.Getenv("CG_POLICY"); pp == "" {
			pp = filepath.Join(*dataDir, "policy-engine.json")
		}
	}
	daemonPolicyPath = pp
	// An absent policy-engine.json is the NORMAL state of a healthy install, not a
	// fault: enforcement reads policy/rules.json (the hook and the stateful tier both
	// go through rulebook.Load), so nothing about being governed depends
	// on this file. Logging it as "not configured" with the raw open error made a
	// working install read as broken — `doctor` says what is actually unavailable,
	// and this line now agrees with it.
	ledgerDetectors, ledgerDetectorErr := detectorselection.ResolveDetectors(*dataDir, detectorselection.DetectorSurfaceLedger, dp)
	governorDetectors, governorDetectorErr := detectorselection.ResolveDetectors(*dataDir, detectorselection.DetectorSurfaceGovernor, dp)
	configurePolicyDetectorRuntime(*dataDir, dp, ledgerDetectors, governorDetectors)
	if ledgerDetectorErr != nil {
		log.Printf("detector assembly unavailable (ledger/audit): %v", ledgerDetectorErr)
	}
	if governorDetectorErr != nil {
		log.Printf("detector assembly unavailable (governor): %v", governorDetectorErr)
	}
	ledgerInitErr := ledgerDetectorErr
	if ledgerInitErr == nil {
		ledgerInitErr = initLedger(lp, ledgerDetectors.Detectors, pp)
	}
	if ledgerInitErr != nil {
		log.Printf("engine ledger unavailable (%v) — the dev Ledger tab is off. "+
			"Enforcement and capture are unaffected.", ledgerInitErr)
	} else {
		log.Printf("live ledger ready: %s (detectors=%s origin=%s)", lp, ledgerDetectors.Digest, ledgerDetectors.Origin)
	}

	// Audit over real sessions (P4 @ harvest) uses the ONE active rulebook. A former
	// audit-rules.json is migration input only: import as observe rules before serving
	// requests, then archive it so two config paths cannot start drifting again.
	legacyAuditPath := filepath.Join(*dataDir, "audit-rules.json")
	imported, migrationErr := rulebook.ImportLegacyAudit(legacyAuditPath)
	auditMigrationErr = migrationErr
	if migrationErr != nil {
		log.Printf("audit migration REQUIRED from %s: %v — Audit endpoints stay unavailable until resolved",
			legacyAuditPath, migrationErr)
	} else if imported.Imported > 0 || imported.Skipped > 0 {
		activePath := "(active rulebook unavailable)"
		if loaded, loadErr := rulebook.LoadDocument(); loadErr == nil {
			activePath = loaded.Path
		}
		log.Printf("audit rules migrated into %s: imported=%d already-present=%d archive=%s",
			activePath, imported.Imported, imported.Skipped, imported.Archive)
		if imported.ArchiveWarning != "" {
			log.Printf("audit migration archive warning: %s — imported rules are active; archival will retry",
				imported.ArchiveWarning)
		}
	}
	if ledgerDetectorErr != nil {
		log.Printf("audit unavailable (detectors): %v", ledgerDetectorErr)
	} else if err := initAudit(ledgerDetectors.Detectors); err != nil {
		log.Printf("audit unavailable (detectors): %v", err)
	} else if migrationErr == nil {
		if loaded, loadErr := rulebook.LoadDocument(); loadErr == nil {
			log.Printf("audit detectors ready; requests will validate active stateful rules from %s", loaded.Path)
		} else {
			log.Printf("audit detectors ready; active rulebook unavailable: %v", loadErr)
		}
	}

	// Live governor (plan Phase 1b): observe → classify → fold into the one index.
	// Non-fatal: without it the daemon serves degraded — observe returns 503 and
	// /api/govern/health names the reason (serve-startup-unopenable-store plan §2).
	// A busy store is retried once before the listener exists; exiting instead
	// would crash-loop the service for as long as the lock is held.
	governorErr := governorDetectorErr
	if governorErr == nil {
		governorErr = initGovernor(*dataDir, governorDetectors.Detectors)
		if governorStartupBusy(governorErr) {
			log.Printf("governor: the store is busy (%v) — retrying once", governorErr)
			if governorErr = initGovernor(*dataDir, governorDetectors.Detectors); governorStartupBusy(governorErr) {
				governorErr = fmt.Errorf("the store was busy at start-up (%w) — another process held its "+
					"write lock; restart the daemon to retry", governorErr)
			}
		}
	}
	if governorErr != nil {
		setGovernorUnavailable(governorErr)
		log.Printf("governor not configured (%v) — serving degraded: live observe disabled", governorErr)
	} else {
		log.Printf("governor ready: observe → %s", indexPath())
	}
	workspaceHost, workspaceErr := newWorkspaceHost(*dataDir, indexPath())
	if workspaceErr != nil {
		log.Printf("workspace selection unavailable (%v) — existing console and chat remain available", workspaceErr)
		workspaceHost = nil
	} else {
		defer workspaceHost.close()
		log.Printf("workspace selection ready: configured roots from %s", filepath.Join(*dataDir, "workspace.json"))
	}
	var taskWorkspaceService taskWorkspace
	if workspaceHost != nil {
		taskWorkspaceService = workspaceHost.service
	}
	if workspaceHost != nil {
		initSpeech(*dataDir, workspaceHost.service)
	} else {
		initSpeech(*dataDir, nil)
	}
	defer closeSpeech()
	if err := initRuntimeTaskService(indexPath(), taskWorkspaceService); err != nil {
		runtimeTasksProblem = err.Error()
		log.Printf("runtime tasks unavailable (%v) — owned chat is disabled", err)
	} else {
		defer closeRuntimeTaskService()
		log.Printf("runtime tasks ready: daemon-owned turns → %s", indexPath())
		// The handoff owner follows the tasks an Open launched: their first session
		// frame and their end (team rest-of-release plan §6.3).
		handoffOpens.start()
	}
	initSessionActivityService()
	defer closeSessionActivityService()
	log.Printf("native session activity ready: qualified file-open observation every %s", sessionActivityConfig().SamplerInterval())
	// Named model routes (team rest-of-release plan §5.6): after the store is open and
	// before any orchestration host is built.
	prepareModelRoutes(*dataDir, indexPath())
	if governor != nil {
		reviewHost, err = newOrchestrationReviewHost(governor.ix, profileOwner)
		if err != nil {
			log.Printf("report-only reviews unavailable (%v) — monitoring and governance are unaffected", err)
			reviewHost = nil
		} else {
			defer reviewHost.close()
			log.Printf("report-only review host ready: optional committed-action consumer")
		}
	}
	managedHost, err = openOrchestrationManagedHost(indexPath(), profileOwner, runtimeTasks)
	if err != nil {
		log.Printf("managed orchestration unavailable (%v) — lower layers are unaffected", err)
		managedHost = nil
		setOrchestrationManagedHostService(nil)
	} else {
		setOrchestrationManagedHostService(managedHost)
		defer func() {
			setOrchestrationManagedHostService(nil)
			managedHost.close()
		}()
		log.Printf("managed orchestration host ready: optional durable task-event consumer")
	}
	wireTeamRestOfRelease(managedHost, reviewHost, profileOwner)
	// Construction is Governor-independent, but activation waits until the existing
	// SQLite owners have attempted their opens so a first v22 migration cannot race a
	// primary owner. The worker itself remains non-blocking for HTTP startup.
	transcriptCoordinator.Start()
	defer transcriptCoordinator.Close()
	// The usage recorder reads every runtime's usage sources into the store on its
	// own handle (token-usage-analytics plan §3.4); it starts with the other
	// store owners for the same reason.
	daemonUsageRecorder = newUsageRecorderCoordinator(indexPath())
	daemonUsageRecorder.Start()
	defer daemonUsageRecorder.Close()

	// Publish our listen address so hooks find us on ANY port without an env var
	// (a hardcoded default port would silently break every non-default daemon).
	defer publishDaemonAddr(*dataDir, *addr)()

	// Self-healing install: make every vendor's hook match THIS binary. "Run
	// crossing-guard install" was an unowned human step — the same failure mode that left the
	// session index hours stale — so the daemon owns it. A vendor step we cannot
	// automate (Codex trust) is printed loudly instead of assumed.
	//
	// The daemon-owned memory import shares this ownership switch: a scratch
	// daemon never imports into the installed memory store (daemon-memory-
	// import plan D2).
	enableDaemonMemoryImport(!*noHookInstall)
	memoryConfig()
	if !*noHookInstall {
		// The daemon is a hard requirement for live capture, so make it survive
		// logout/reboot rather than depending on someone re-running `crossing-guard serve`.
		switch svc := EnsureService(*dataDir, *addr); svc.Action {
		case "current":
			log.Printf("service: current — %s", svc.Path)
		case "installed":
			log.Printf("service: INSTALLED -> %s (%s)", svc.Path, svc.Detail)
		case "updated":
			log.Printf("service: UPDATED -> %s — %s", svc.Path, svc.Detail)
		case "foreign":
			log.Printf("service: NOT TOUCHED — %s", svc.Detail)
		case "unsupported":
			log.Printf("service: NOT MANAGED — %s", svc.Detail)
		default:
			log.Printf("service: ERROR %s", svc.Detail)
		}
		for _, st := range guardcli.EnsureHooks() {
			switch st.Action {
			case "current":
				log.Printf("hooks[%s]: current — %s", st.Vendor, st.Path)
			case "installed":
				log.Printf("hooks[%s]: INSTALLED/REPAIRED -> %s", st.Vendor, st.Path)
			case "absent":
				log.Printf("hooks[%s]: %s", st.Vendor, st.Detail)
			case "unconsented":
				// Not an error and not a silent skip: a runtime we could govern and
				// deliberately did not. Said out loud so an ungoverned agent is a
				// visible state rather than an assumption.
				log.Printf("hooks[%s]: NOT GOVERNED — %s (%s)", st.Vendor, st.Detail, st.Path)
			default:
				log.Printf("hooks[%s]: ERROR %s", st.Vendor, st.Detail)
			}
			if st.Manual != "" {
				log.Printf("hooks[%s]: ACTION REQUIRED — %s", st.Vendor, st.Manual)
			}
		}
		// Recall tools: repaired only where their own yes is recorded.
		for _, st := range guardcli.EnsureRecall() {
			switch st.Action {
			case "current":
				log.Printf("recall[%s]: current — %s", st.Vendor, st.Path)
			case "installed":
				log.Printf("recall[%s]: REGISTERED/REPAIRED -> %s", st.Vendor, st.Path)
			default:
				log.Printf("recall[%s]: %s %s (%s)", st.Vendor, strings.ToUpper(st.Action), st.Detail, st.Path)
			}
		}
		// The platform capability record (item 8): say plainly what THIS GOOS has
		// demonstrated, so "supported" is never assumed. Item 17 gates stateful
		// enforcement on it; logging it here makes the gate's basis visible at boot.
		ps := platform.Current()
		log.Printf("platform[%s]: service=%s uninstall=%s store-acl=%s stateful-enforcement-ready=%v",
			ps.GOOS, ps.Service, ps.Uninstall, ps.StoreACL, ps.StatefulEnforcementReady())
		if ps.Note != "" {
			log.Printf("platform[%s]: %s", ps.GOOS, ps.Note)
		}
	}

	mux := http.NewServeMux()

	// --- live ledger (P3): hook feed + verify + status ---
	mux.HandleFunc("POST /api/govern/observe", handleGovernObserve)
	mux.HandleFunc("POST /api/govern/observe/v1", handleGovernObserveV1)
	mux.HandleFunc("POST /api/govern/result/v1", handleGovernResultV1)
	mux.HandleFunc("POST /api/govern/closure/v1", handleGovernClosureV1)
	mux.HandleFunc("POST /api/govern/session-entry/v1", handleGovernSessionEntryV1)
	mux.HandleFunc("POST /api/govern/session-turn/v1", handleGovernSessionTurnV1)
	mux.HandleFunc("GET /api/govern/results", handleGovernResults)
	mux.HandleFunc("POST /api/govern/decide", handleGovernDecide)
	mux.HandleFunc("GET /api/govern/health", handleGovernHealth)
	mux.HandleFunc("GET /api/chain/verify", handleChainVerify)
	// --- the team link (team plan §5.15): the daemon owns it; console and CLI ask ---
	mux.HandleFunc("GET /api/team", handleTeamStatus)
	mux.HandleFunc("POST /api/team/link", handleTeamLink)
	mux.HandleFunc("POST /api/team/unlink", handleTeamUnlink)
	mux.HandleFunc("POST /api/team/content", handleTeamContent)
	mux.HandleFunc("GET /api/team/content/sent", handleTeamContentSent)
	mux.HandleFunc("POST /api/team/memory/share", handleTeamMemoryShare)
	mux.HandleFunc("GET /api/team/memory/deletions", handleTeamMemoryDeletions)
	mux.HandleFunc("POST /api/team/memory/take-team-version", handleTeamMemoryTake)
	mux.HandleFunc("GET /api/team/layers", handleTeamLayers)
	mux.HandleFunc("POST /api/team/layers/adopt", handleTeamAdopt)
	mux.HandleFunc("POST /api/team/layers/unadopt", handleTeamUnadopt)
	registerTeamPublishingRoutes(mux)
	mux.HandleFunc("GET /api/govern/runtimes", handleGovernRuntimes)
	mux.HandleFunc("GET /api/govern/sessions", handleGovernSessions)
	mux.HandleFunc("GET /api/govern/session", handleGovernSession)
	mux.HandleFunc("GET /api/govern/entity", handleGovernEntity)
	mux.HandleFunc("POST /api/ledger/observe", handleLedgerObserve)
	mux.HandleFunc("GET /api/ledger/verify", handleLedgerVerify)
	mux.HandleFunc("GET /api/ledger/status", handleLedgerStatus)

	// --- audit over real sessions (P4 @ harvest): rules + run ---
	mux.HandleFunc("GET /api/audit/rules", handleAuditRules)
	mux.HandleFunc("POST /api/audit/run", handleAuditRun)
	mux.HandleFunc("GET /api/audit/session", handleAuditSession)

	// --- session harvest ---
	mux.HandleFunc("GET /api/sessions", handleSessions)
	mux.HandleFunc("GET /api/sessions/peers", handleSessionPeers)
	// --- agent-initiated cross-vendor sends (session-message-cross-vendor-plan §4.1):
	// one typed, documented POST route; the record is the ledger, the reply is typed.
	mux.HandleFunc("POST /api/session-message/send", handleSessionMessageSend)
	mux.HandleFunc("GET /api/session-message/invocations", handleSessionMessageInvocations)
	mux.HandleFunc("GET /api/session-message/health", handleSessionMessageHealth)
	mux.HandleFunc("GET /api/session", func(w http.ResponseWriter, r *http.Request) {
		runtime, id := r.URL.Query().Get("runtime"), r.URL.Query().Get("id")
		detail, err := LoadSession(runtime, id)
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		// The recorded usage (plan D-6); absent until the recorder has read it.
		detail.Usage, _ = recordedSessionUsage(detail.SessionSummary)
		writeJSON(w, decorateSessionAgents(detail))
	})
	registerSessionGraphRoutes(mux)
	registerSessionLiveRoutes(mux)
	// One event, untruncated. The transcript clips tool payloads to stay
	// openable; this is how the console gets the rest when a reader expands a
	// chip, so "what did the agent actually write" is answerable at all.
	mux.HandleFunc("GET /api/session/event", handleSessionEvent)
	mux.HandleFunc("GET /api/search", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, SearchAll(r.URL.Query().Get("q"), transcriptCoordinator.Coverage()))
	})
	registerUsageRoutes(mux)
	mux.HandleFunc("GET /api/files", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, SearchFiles(r.URL.Query().Get("cwd"), r.URL.Query().Get("q")))
	})

	// --- references (console-and-info-panel §5; read-only) ---
	mux.HandleFunc("GET /api/refs/index", func(w http.ResponseWriter, r *http.Request) {
		manifest, err := RefIndexManifest(r.URL.Query().Get("cwd"))
		if err != nil {
			http.Error(w, err.Error(), refRequestStatus(err))
			return
		}
		writeJSON(w, manifest)
	})
	mux.HandleFunc("GET /api/refs/resolve", func(w http.ResponseWriter, r *http.Request) {
		target, err := ResolveRef(r.URL.Query().Get("cwd"), r.URL.Query().Get("token"))
		if err != nil {
			http.Error(w, err.Error(), refRequestStatus(err))
			return
		}
		writeJSON(w, target)
	})
	mux.HandleFunc("GET /api/refs/doc", func(w http.ResponseWriter, r *http.Request) {
		doc, err := ReadRefDoc(r.URL.Query().Get("cwd"), r.URL.Query().Get("path"))
		if err != nil {
			http.Error(w, err.Error(), refRequestStatus(err))
			return
		}
		writeJSON(w, doc)
	})
	mux.HandleFunc("GET /api/codemap/descriptor", handleCodemapDescriptor)
	mux.HandleFunc("GET /api/refs/backlinks", func(w http.ResponseWriter, r *http.Request) {
		report, err := RefBacklinks(r.URL.Query().Get("cwd"), r.URL.Query().Get("target"))
		if err != nil {
			http.Error(w, err.Error(), refRequestStatus(err))
			return
		}
		writeJSON(w, report)
	})

	// --- memory ---
	mux.HandleFunc("GET /api/memory", func(w http.ResponseWriter, r *http.Request) {
		handleConsoleMemories(w, store.ListMemories)
	})
	mux.HandleFunc("POST /api/memory", func(w http.ResponseWriter, r *http.Request) {
		handleMemoryUpsert(w, r, store)
	})

	mux.HandleFunc("GET /api/memory/search", handleMemorySearch)

	// --- owner tags over memory records (plan §6; session grammar, one more
	// record kind — never agent context, never a rule input) ---
	mux.HandleFunc("POST /api/memory/tags", handleMemoryTags)
	mux.HandleFunc("GET /api/memory/tags", handleMemoryTagUses)
	mux.HandleFunc("GET /api/memory/by-tag", handleMemoryByTag)
	// The memory settings in one declared place (config-ownership plan Fix A):
	// the propose consent, read live, plus the search budgets. The MCP propose
	// tool reads this per invocation; the import throttle stays in doctor.
	mux.HandleFunc("GET /api/memory/config", handleMemoryConfig)

	// --- memory proposal door for agent sessions (plan §5.2 / RT-6) ---
	// Consent is checked HERE (recall.propose_enabled), the rate is bounded
	// per session, and the store enforces pending — an agent can never write
	// an active record through this route.
	mux.HandleFunc("POST /api/memory/propose", handleMemoryPropose)

	// --- full record (dossier body) — lazy-fetched on expand ---
	mux.HandleFunc("GET /api/memory/record", handleMemoryRecord)
	// --- one status's records in the /record shape (the CLI's list and
	// SessionStart index read; memory-reads-through-daemon plan §4.2) ---
	mux.HandleFunc("GET /api/memory/records", handleMemoryRecords)
	// --- conflict copies: local versions a team revision or deletion displaced ---
	mux.HandleFunc("GET /api/memory/conflicts", handleMemoryConflicts)

	// --- memory proposal inbox (engine pending/ queue; R7) ---
	mux.HandleFunc("GET /api/memory/pending", func(w http.ResponseWriter, r *http.Request) {
		handleConsoleMemories(w, ListPendingMemories)
	})
	mux.HandleFunc("POST /api/memory/promote", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
			http.Error(w, "id required", http.StatusBadRequest)
			return
		}
		out, err := PromoteMemory(req.ID)
		if err != nil {
			http.Error(w, out+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"result": out})
	})
	mux.HandleFunc("POST /api/memory/reject", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     string `json:"id"`
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
			http.Error(w, "id required", http.StatusBadRequest)
			return
		}
		out, err := RejectMemory(req.ID, req.Reason)
		if err != nil {
			http.Error(w, out+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"result": out})
	})

	// --- notes (pinned to sessions/events) ---
	mux.HandleFunc("GET /api/notes", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, store.ListNotes())
	})
	mux.HandleFunc("POST /api/notes", func(w http.ResponseWriter, r *http.Request) {
		var n Note
		if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		saved, err := store.AddNote(n)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, saved)
	})

	// --- policy v1a: live rules + check + decisions over the cp engine ---
	mux.HandleFunc("GET /api/policy/rules", handlePolicyRulesGet)
	mux.HandleFunc("PUT /api/policy/rules", handlePolicyRulesPut)
	mux.HandleFunc("POST /api/policy/rules/selection/preview", handlePolicySelectionPreview)
	mux.HandleFunc("POST /api/policy/rules/selection", handlePolicySelection)
	mux.HandleFunc("POST /api/policy/rules/selection/rollback", handlePolicySelectionRollback)
	mux.HandleFunc("POST /api/policy/rules/selection/unselect", handlePolicySelectionRollback)
	mux.HandleFunc("GET /api/policy/detectors", handlePolicyDetectorsGet)
	mux.HandleFunc("POST /api/policy/detectors/selection/preview", handlePolicyDetectorSelectionPreview)
	mux.HandleFunc("POST /api/policy/detectors/selection", handlePolicyDetectorSelection)
	mux.HandleFunc("POST /api/policy/detectors/selection/unselect", handlePolicyDetectorUnselect)
	mux.HandleFunc("POST /api/policy/check", handlePolicyCheck)
	mux.HandleFunc("GET /api/policy/decisions", handlePolicyDecisions)
	mux.HandleFunc("GET /api/policy/coverage", handlePolicyCoverage)

	// --- approvals inbox (gui-design §10.1): the hook's held call + the human act ---
	mux.HandleFunc("POST /api/approvals/request", handleApprovalRequest)
	mux.HandleFunc("POST /api/approvals/decision", handleApprovalDecision)
	mux.HandleFunc("POST /api/approvals/grant/revoke", handleApprovalGrantRevoke)
	mux.HandleFunc("GET /api/approvals", handleApprovalsList)
	mux.HandleFunc("GET /api/approvals/stream", handleApprovalsStream)
	mux.HandleFunc("POST /api/approvals/presence", handleApprovalPresence)

	mux.HandleFunc("GET /api/policy", handleRetiredPolicyGrid)
	mux.HandleFunc("PUT /api/policy", handleRetiredPolicyGrid)

	// --- skills surface (inventory + coverage matrix, read-only v0) ---
	mux.HandleFunc("GET /api/skills", handleSkills)
	mux.HandleFunc("POST /api/skills/probe", handleSkillsProbe)

	// --- handoff composer (the product verb) ---
	mux.HandleFunc("GET /api/handoff/generate", handleHandoffGenerate)
	registerTeamHandoffRoutes(mux)
	registerFolderChooseRoutes(mux)

	// --- fallback chat (SSE over POST) ---
	mux.HandleFunc("GET /api/chat/auth", handleVendorAuthStatus)
	mux.HandleFunc("POST /api/chat/auth/start", handleVendorAuthStart)
	mux.HandleFunc("GET /api/chat/capabilities", handleChatCapabilities)
	mux.HandleFunc("GET /api/chat/models", handleChatModels)
	mux.HandleFunc("GET /api/session-turn-settings", handleSessionEffortGet)
	mux.HandleFunc("PUT /api/session-turn-settings", handleSessionEffortPut)
	mux.HandleFunc("POST /api/chat/effort-preview", handleEffortPreview)
	mux.HandleFunc("POST /api/chat/models/refresh", handleChatModelsRefresh)
	registerEventStreamRoutes(mux)
	registerConsoleConfigRoutes(mux)
	registerSessionOrganizationRoutes(mux)
	registerWorkspaceRoutes(mux, workspaceHost)
	// The git scopes read recorded session folders from the store; without the
	// governor's handle they answer store-unavailable instead of dereferencing nil.
	var reviewService *workspace.ReviewService
	if governor != nil {
		consoleSettings, _ := consoleConfig()
		reviewService = workspace.NewReviewService(reviewBudgets(consoleSettings), governor.ix)
	}
	registerWorkspaceDiffRoutes(mux, reviewService)
	registerWorkspaceFilesRoutes(mux, reviewService)
	registerTaskInputRoutes(mux)
	registerSpeechRoutes(mux)
	registerRuntimeIntegrationRoutes(mux)
	pinnedFor := governorPinSource
	registerOrchestrationProfileRoutes(mux, profileOwner, pinnedFor)
	registerOrchestrationDraftRoutes(mux, profileOwner, pinnedFor)
	registerOrchestrationReviewRoutes(mux, reviewHost, profileOwner)
	registerOrchestrationManagedRoutes(mux, managedHost, profileOwner)
	registerOrchestrationRosterRoutes(mux, rosterSources{profiles: profileOwner, managed: managedHost, review: reviewHost})
	registerModelRouteRoutes(mux, *dataDir, profileOwner, managedHost, reviewHost)
	mux.HandleFunc("POST /api/chat", handleChat)
	mux.HandleFunc("POST /api/runtime-tasks", handleRuntimeTaskCreate)
	mux.HandleFunc("GET /api/runtime-tasks", handleRuntimeTaskList)
	mux.HandleFunc("GET /api/runtime-tasks/stream", handleRuntimeTaskStream)
	mux.HandleFunc("POST /api/runtime-tasks/{task}/interrupt", handleRuntimeTaskInterrupt)
	mux.HandleFunc("GET /api/session-activity", handleSessionActivityList)
	mux.HandleFunc("GET /api/session-activity/stream", handleSessionActivityStream)

	// --- API contract version (D16) — how a BYO-GUI discovers what it built against ---
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, daemonVersion(*dataDir, *addr))
	})

	// --- static UI ---
	// no-store: embedded files carry zero modtime, so browsers heuristically
	// cache the SPA forever and every rebuild looks "broken" until a hard
	// refresh. Localhost + 100KB = revalidation costs nothing.
	// The appearance sheet is generated from the selected appearance module and
	// loads between tokens.css and app.css (session-view-and-console-preferences
	// plan §C2). It guards its own Host, like the redirect below.
	mux.HandleFunc("GET /appearance.css", appearanceCSSHandler(*addr))
	sub, _ := fs.Sub(staticFS, "static")
	staticHandler := http.FileServerFS(sub)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if target := canonicalConsoleRootRedirect(r, *addr); target != "" {
			http.Redirect(w, r, target, http.StatusTemporaryRedirect)
			return
		}
		staticHandler.ServeHTTP(w, r)
	}))

	// Console security posture (target-features §8): bearer token on /api/*
	// plus an Origin check so a malicious webpage cannot POST to this
	// loopback listener (local CSRF / DNS rebinding) and spawn binaries.
	// Token persists across restarts (data/api-token, 0600) so the browser's
	// stored copy keeps working — regenerating each start meant every Ctrl+C
	// invalidated the UI. Delete the file to rotate.
	token := *authToken
	tokenFile := filepath.Join(*dataDir, "api-token")
	if token == "" {
		if b, err := os.ReadFile(tokenFile); err == nil {
			token = strings.TrimSpace(string(b))
		}
	}
	if token == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			log.Fatalf("token: %v", err)
		}
		token = hex.EncodeToString(b)
	}
	// ALWAYS publish the effective token — the token file is how hooks authenticate
	// to us. Persisting only a generated token meant an operator-supplied --token
	// left every hook unable to authenticate (silent 401, observations dropped).
	if strings.TrimSpace(readFileString(tokenFile)) != token {
		if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
			log.Printf("warn: could not persist token (%v) — hooks cannot authenticate", err)
		}
	}
	guarded := requestTimingMiddleware(securityMiddleware(mux, *addr, token, tokenFile))

	fmt.Printf("crossing-guard console — open:  http://%s/#t=%s\n", *addr, token)
	fmt.Printf("(data: %s)\n", *dataDir)
	fmt.Println("NOTE: chat needs a plain terminal — claude OAuth cannot refresh inside a hosted agent session.")
	if err := http.ListenAndServe(*addr, guarded); err != nil {
		stopUnderstandingScheduling(governor)
		log.Printf("console stopped: %v", err)
		return
	}
}

// slowRequestThreshold is when an API request becomes log-worthy. The daemon had no
// per-request visibility at all, so a hung or minute-slow endpoint was undiagnosable
// from daemon.log (2026-08-29 incident). Permanent instrumentation, not a debug flag.
const slowRequestThreshold = 500 * time.Millisecond

type timingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *timingResponseWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *timingResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// requestTimingMiddleware logs slow (>500 ms) and server-error API responses:
// method, path, status, duration. Never query strings or bodies — paths in this API
// are static route shapes while user values travel in queries. Streams flush early
// and run long by design, so only their setup failure (an error status) is loggable.
func requestTimingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		tw := &timingResponseWriter{ResponseWriter: w}
		next.ServeHTTP(tw, r)
		elapsed := time.Since(start)
		status := tw.status
		if status == 0 {
			status = http.StatusOK
		}
		isStream := strings.HasSuffix(r.URL.Path, "/stream")
		if status >= 500 || (elapsed >= slowRequestThreshold && !isStream) {
			log.Printf("http %s %s -> %d in %s", r.Method, r.URL.Path, status, elapsed.Round(time.Millisecond))
		}
	})
}

// apiVersion is the BYO-GUI contract version (D16, console-design §"public
// contract"). The canonical path is /api/v1/…; the unversioned /api/… stays a compat
// alias so a versioning change never breaks a consumer that built against the path we
// told them to. Both are served by the same handlers — versioning is a promise about
// STABILITY, and an alias that diverged would break it.
const apiVersion = "v1"

// securityMiddleware enforces Origin + token on /api/* routes, accepts every documented
// way to present the token, and serves /api/v1/ as an alias of /api/ (D16).
func securityMiddleware(next http.Handler, addr, token, tokenFile string) http.Handler {
	// An empty token would make ConstantTimeCompare("","")==1 open every /api/ route to
	// an unauthenticated caller. Main guarantees a non-empty token, but the safety must
	// not live only there: refuse to build an auth middleware that authenticates nothing.
	if token == "" {
		log.Fatal("securityMiddleware: refusing to serve with an empty API token")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Normalize the path ONCE, so the auth prefix check and the mux route on the SAME
		// value — otherwise a crafted /foo/../api/x could fail the literal /api/ prefix
		// (skipping auth) yet still resolve to an /api/ handler after the mux cleans it.
		r.URL.Path = path.Clean(r.URL.Path)
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		// Origin check: browsers attach Origin on cross-origin requests.
		// Same-origin fetches may omit it; if present it must be ours.
		if o := r.Header.Get("Origin"); o != "" {
			if u, err := url.Parse(o); err != nil || !sameHost(u.Host, addr) {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
		}
		if subtle.ConstantTimeCompare([]byte(presentedToken(r)), []byte(token)) != 1 {
			// Point at the token FILE, not "the URL printed at startup": under launchd
			// there is no visible startup, and the old message sent a caller holding a
			// valid token chasing output that does not exist (D5).
			http.Error(w, "missing or invalid API token — read it from "+tokenFile+
				" and send it as 'Authorization: Bearer <token>' or the 'X-CG-Token' header",
				http.StatusUnauthorized)
			return
		}
		// /api/v1/foo is served by the /api/foo handlers — the version is the contract,
		// the handlers are shared (D16).
		if rest, ok := strings.CutPrefix(r.URL.Path, "/api/"+apiVersion+"/"); ok {
			r.URL.Path = "/api/" + rest
		}
		next.ServeHTTP(w, r)
	})
}

// presentedToken extracts the token from any documented channel: the Authorization
// Bearer header (the standard a BYO-GUI or curl reaches for first — and the one D5
// rejected outright), our own X-CG-Token, or the ?t= query param the console's
// bookmarkable URL uses.
func presentedToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		// RFC 7235 §2.1: the auth scheme is case-INSENSITIVE, and any linear whitespace
		// separates it from the credential. Fields() handles "Bearer x", "bearer x", and
		// tab-separated forms — the case-sensitive CutPrefix("Bearer ") rejected valid
		// clients, the exact D5 interop failure this set out to fix.
		if f := strings.Fields(h); len(f) == 2 && strings.EqualFold(f[0], "Bearer") {
			return f[1]
		}
	}
	if t := r.Header.Get("X-CG-Token"); t != "" {
		return t
	}
	return r.URL.Query().Get("t")
}

// sameHost treats 127.0.0.1 and localhost as equivalent for the same port.
func sameHost(origin, addr string) bool {
	oh, op, _ := strings.Cut(origin, ":")
	ah, ap, _ := strings.Cut(addr, ":")
	norm := func(h string) string {
		if h == "localhost" {
			return "127.0.0.1"
		}
		return h
	}
	return norm(oh) == norm(ah) && op == ap
}

// canonicalConsoleRootRedirect removes the accidental second browser-auth origin.
// Only the safe root document on an equivalent loopback alias is canonicalized: API
// requests, assets, mutations, and arbitrary Host values must never be redirected.
// URL fragments are browser-only and therefore cannot be copied or exposed here.
func canonicalConsoleRootRedirect(r *http.Request, addr string) string {
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || r.URL.Path != "/" {
		return ""
	}
	if r.Host == addr || !sameHost(r.Host, addr) {
		return ""
	}
	return "http://" + addr + r.URL.RequestURI()
}

// defaultDataDir is the product location. (The consoleprobe experiment kept
// data next to its binary; pass -data <old-dir> or copy the directory to
// carry probe-era records/token over.)
func defaultDataDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "data"
	}
	return filepath.Join(h, ".crossing-guard", "console")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
