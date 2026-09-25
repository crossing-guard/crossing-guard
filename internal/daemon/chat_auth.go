package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const authCommandTimeout = 10 * time.Minute

type vendorAuthStatus struct {
	Runtime    string `json:"runtime"`
	Ready      bool   `json:"ready"`
	Method     string `json:"method,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Flow       string `json:"flow"` // idle | running | succeeded | failed
	CanSignIn  bool   `json:"can_sign_in"`
	StatusHint string `json:"status_hint,omitempty"`
}

type authFlow struct {
	running  bool
	lastFlow string
	started  time.Time
}

var vendorAuthFlows = struct {
	sync.Mutex
	byRuntime map[string]*authFlow
}{byRuntime: map[string]*authFlow{}}

type vendorAuthDriver interface {
	ProbeVendorAuth(context.Context) (ready bool, method, provider string, err error)
	BuildVendorLogin(context.Context) (*exec.Cmd, error)
}

func authDriver(runtimeName string) (vendorAuthDriver, error) {
	driver, ok := chatDrivers[runtimeName]
	if !ok {
		return nil, errors.New("unsupported runtime")
	}
	auth, ok := driver.(vendorAuthDriver)
	if !ok {
		return nil, errors.New("runtime does not provide vendor sign-in")
	}
	return auth, nil
}

func probeVendorAuth(ctx context.Context, runtimeName string) (vendorAuthStatus, error) {
	status := vendorAuthStatus{Runtime: runtimeName, Flow: "idle", CanSignIn: true}
	driver, err := authDriver(runtimeName)
	if err != nil {
		status.CanSignIn = false
		status.StatusHint = err.Error()
		return status, nil
	}
	status.Ready, status.Method, status.Provider, err = driver.ProbeVendorAuth(ctx)
	if err != nil {
		return status, err
	}

	vendorAuthFlows.Lock()
	flow := vendorAuthFlows.byRuntime[runtimeName]
	if flow != nil {
		if flow.running {
			status.Flow = "running"
		} else if flow.lastFlow != "" {
			status.Flow = flow.lastFlow
		}
		if status.Ready {
			flow.running = false
			flow.lastFlow = "succeeded"
			status.Flow = "succeeded"
		}
	}
	vendorAuthFlows.Unlock()
	return status, nil
}

func startVendorAuth(runtimeName string) (vendorAuthStatus, error) {
	driver, err := authDriver(runtimeName)
	if err != nil {
		return vendorAuthStatus{}, err
	}

	vendorAuthFlows.Lock()
	flow := vendorAuthFlows.byRuntime[runtimeName]
	if flow == nil {
		flow = &authFlow{}
		vendorAuthFlows.byRuntime[runtimeName] = flow
	}
	if flow.running {
		vendorAuthFlows.Unlock()
		return vendorAuthStatus{Runtime: runtimeName, Flow: "running", CanSignIn: true}, nil
	}
	flow.running = true
	flow.lastFlow = "running"
	flow.started = time.Now()
	vendorAuthFlows.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), authCommandTimeout)
	cmd, err := driver.BuildVendorLogin(ctx)
	if err != nil {
		cancel()
		vendorAuthFlows.Lock()
		flow.running = false
		flow.lastFlow = "failed"
		vendorAuthFlows.Unlock()
		return vendorAuthStatus{}, err
	}
	// Vendor login owns its browser callback and credential store. Crossing Guard
	// deliberately captures neither stdout/stderr nor credentials.
	if err := cmd.Start(); err != nil {
		cancel()
		vendorAuthFlows.Lock()
		flow.running = false
		flow.lastFlow = "failed"
		vendorAuthFlows.Unlock()
		return vendorAuthStatus{}, err
	}
	go func() {
		err := cmd.Wait()
		cancel()
		vendorAuthFlows.Lock()
		flow.running = false
		if err == nil {
			flow.lastFlow = "succeeded"
		} else {
			flow.lastFlow = "failed"
		}
		vendorAuthFlows.Unlock()
	}()
	return vendorAuthStatus{Runtime: runtimeName, Flow: "running", CanSignIn: true}, nil
}

func handleVendorAuthStatus(w http.ResponseWriter, r *http.Request) {
	runtimeName := r.URL.Query().Get("runtime")
	if _, ok := chatDrivers[runtimeName]; !ok {
		http.Error(w, "unsupported runtime", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	status, err := probeVendorAuth(ctx, runtimeName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, status)
}

func handleVendorAuthStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Runtime string `json:"runtime"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if _, err := authDriver(req.Runtime); err != nil {
		http.Error(w, "unsupported runtime", http.StatusBadRequest)
		return
	}
	probeCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	current, probeErr := probeVendorAuth(probeCtx, req.Runtime)
	cancel()
	if probeErr != nil {
		http.Error(w, "could not verify vendor sign-in status", http.StatusBadGateway)
		return
	}
	if current.Ready {
		writeJSON(w, current)
		return
	}
	status, err := startVendorAuth(req.Runtime)
	if err != nil {
		http.Error(w, "could not start vendor sign-in", http.StatusBadGateway)
		return
	}
	writeJSON(w, status)
}

func isVendorAuthFailure(line string) bool {
	s := strings.ToLower(line)
	return strings.Contains(s, "failed to authenticate") ||
		strings.Contains(s, "oauth session expired") ||
		strings.Contains(s, "not logged in") ||
		strings.Contains(s, "authentication required")
}
