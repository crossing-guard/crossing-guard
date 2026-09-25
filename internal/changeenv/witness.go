package changeenv

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"crossing-guard/store"
)

type VerifyInput struct {
	SessionID, Runtime, Title, RepoDir, CWD, Name, Boundary string
	Timeout                                                 time.Duration
	Command                                                 []string
}

func fileDigest(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return "sha256-v1:" + hex.EncodeToString(h.Sum(nil)), nil
}
func argvDigest(v []string) string { return digest("sha256-v1:", []byte(strings.Join(v, "\x00"))) }

// VerifyRun witnesses process mechanics. Name and boundary remain claimed metadata.
func VerifyRun(ix *store.Index, in VerifyInput) (*store.ChangeRecord, int, error) {
	if strings.TrimSpace(in.SessionID) == "" || strings.TrimSpace(in.RepoDir) == "" || strings.TrimSpace(in.CWD) == "" {
		return nil, 2, fmt.Errorf("verify-run requires session, repo, and cwd")
	}
	if strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.Boundary) == "" {
		return nil, 2, fmt.Errorf("verify-run requires name and boundary")
	}
	if len(in.Command) == 0 {
		return nil, 2, fmt.Errorf("missing command")
	}
	if in.Timeout <= 0 || in.Timeout > 24*time.Hour {
		return nil, 2, fmt.Errorf("timeout must be >0 and <=24h")
	}
	repo, err := ResolveRepository(in.RepoDir)
	if err != nil {
		return nil, 2, err
	}
	cwd, err := filepath.Abs(in.CWD)
	if err != nil {
		return nil, 2, err
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, 2, err
	}
	rel, err := filepath.Rel(repo.Root, cwd)
	if err != nil || outside(rel) {
		return nil, 2, fmt.Errorf("cwd outside repository")
	}
	exe, err := exec.LookPath(in.Command[0])
	if err != nil {
		return recordUnavailable(ix, repo, in, "start-failed", err.Error(), 127)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return nil, 2, err
	}
	exeDigest, digErr := fileDigest(exe)
	limitation := ""
	if digErr != nil {
		limitation = "executable identity unavailable: " + digErr.Error()
	}
	cmd := exec.Command(exe, in.Command[1:]...)
	cmd.Dir = cwd
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	configureProcessGroup(cmd)
	start := time.Now().UnixNano()
	runErr := cmd.Start()
	timedOut := false
	interrupted := false
	childCleanup := processCleanupCapability()
	if runErr == nil {
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		timer := time.NewTimer(in.Timeout)
		interrupts := make(chan os.Signal, 1)
		signal.Notify(interrupts, os.Interrupt)
		defer signal.Stop(interrupts)
		select {
		case runErr = <-done:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			timedOut = true
			childCleanup = terminateProcess(cmd)
			runErr = <-done
		case <-interrupts:
			interrupted = true
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			childCleanup = terminateProcess(cmd)
			runErr = <-done
		}
	}
	end := time.Now().UnixNano()
	exit := 0
	termination := "exit"
	signalName := ""
	if timedOut {
		termination = "timeout"
		exit = 124
	} else if interrupted {
		termination = "signal"
		signalName = "interrupt"
		exit = 130
	} else if runErr != nil {
		var ee *exec.ExitError
		if ok := errorAs(runErr, &ee); ok {
			var signaled bool
			exit, signalName, signaled = processExitStatus(ee)
			if signaled {
				termination = "signal"
			}
		} else {
			termination = "start-failed"
			exit = 127
			limitation = runErr.Error()
		}
	}
	r := &store.ChangeRecord{SessionID: in.SessionID, RepositoryID: repo.ID, CheckoutID: repo.CheckoutID, RepositoryIdentityKind: repo.IdentityKind, CheckoutRoot: repo.Root, SessionRuntimeClaim: in.Runtime, SessionTitleClaim: in.Title, Kind: "verification", EvidenceClass: "observed", SourceKind: "process", SourceRef: "process:" + argvDigest(in.Command), SourceDisplay: filepath.Base(exe), SourceDigest: exeDigest, RecordedAt: end, CaptureStartedAt: start, CaptureEndedAt: end, VerificationName: in.Name, VerificationBoundary: in.Boundary, VerificationNameClass: "claimed", VerificationResult: map[bool]string{true: "pass", false: "fail"}[exit == 0], ExecutablePath: exe, ExecutableDigest: exeDigest, ArgvDigest: argvDigest(in.Command), ExitCode: &exit, Signal: signalName, Termination: termination, EnvironmentState: "inherited-unrecorded", ChildCleanup: childCleanup, Limitation: limitation}
	if err := ix.AppendChange(r); err != nil {
		return nil, 2, fmt.Errorf("process result %s exit=%d NOT RECORDED: %w", termination, exit, err)
	}
	return r, exit, nil
}

// tiny local helper avoids importing a generic runner just to unwrap ExitError.
func errorAs(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}
func recordUnavailable(ix *store.Index, repo Repository, in VerifyInput, term, why string, code int) (*store.ChangeRecord, int, error) {
	now := time.Now().UnixNano()
	r := &store.ChangeRecord{SessionID: in.SessionID, RepositoryID: repo.ID, CheckoutID: repo.CheckoutID, RepositoryIdentityKind: repo.IdentityKind, CheckoutRoot: repo.Root, SessionRuntimeClaim: in.Runtime, SessionTitleClaim: in.Title, Kind: "verification", EvidenceClass: "observed", SourceKind: "process", SourceRef: "process:" + argvDigest(in.Command), SourceDisplay: in.Command[0], SourceDigest: "identity-unavailable", RecordedAt: now, CaptureStartedAt: now, CaptureEndedAt: now, VerificationName: in.Name, VerificationBoundary: in.Boundary, VerificationNameClass: "claimed", VerificationResult: "unavailable", ArgvDigest: argvDigest(in.Command), ExitCode: &code, Termination: term, EnvironmentState: "inherited-unrecorded", ChildCleanup: "not-started", Limitation: why}
	if e := ix.AppendChange(r); e != nil {
		return nil, 2, e
	}
	return r, code, nil
}
