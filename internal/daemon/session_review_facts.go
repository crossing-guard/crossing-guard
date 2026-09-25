package daemon

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"crossing-guard/internal/changeenv"
	"crossing-guard/internal/transcriptindex"
)

type sessionStatementsResponse struct {
	Section    string                   `json:"section"`
	Statements changeenv.StatementFacts `json:"statements"`
	Coverage   transcriptindex.Coverage `json:"coverage"`
}

type sessionChecksResponse struct {
	Section string                       `json:"section"`
	Checks  changeenv.ObservedCheckFacts `json:"checks"`
}

func reviewPageParams(q queryGetter, prefix string) (int, int, error) {
	offset, limit := 0, 25
	var err error
	if raw := strings.TrimSpace(q.Get(prefix + "_offset")); raw != "" {
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 {
			return 0, 0, errors.New(prefix + "_offset must be a non-negative integer")
		}
	}
	if raw := strings.TrimSpace(q.Get(prefix + "_limit")); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			return 0, 0, errors.New(prefix + "_limit must be between 1 and 100")
		}
	}
	return offset, limit, nil
}

// SessionStatementFacts reads one bounded attributed statement page.
func (g *Governor) SessionStatementFacts(runtime, sessionID string, offset, limit int) (changeenv.StatementFacts, error) {
	return changeenv.BuildStatementFacts(g.ix, runtime, sessionID, offset, limit)
}

// SessionObservedCheckFacts reads one bounded observed check/action page.
func (g *Governor) SessionObservedCheckFacts(sessionID string, offset, limit int) (changeenv.ObservedCheckFacts, error) {
	return changeenv.BuildObservedCheckFacts(g.ix, sessionID, offset, limit)
}

func handleGovernSessionStatements(w http.ResponseWriter, q queryGetter, sessionID, section string) {
	offset, limit, err := reviewPageParams(q, "statement")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	runtime := strings.TrimSpace(q.Get("runtime"))
	if len(runtime) > 128 {
		http.Error(w, "runtime must not exceed 128 bytes", http.StatusBadRequest)
		return
	}
	facts, err := governor.SessionStatementFacts(runtime, sessionID, offset, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	coverage := daemonTranscriptIndexCoverage.Load().ForSession(
		transcriptindex.SessionKey{Runtime: runtime, SessionID: sessionID})
	writeJSON(w, sessionStatementsResponse{Section: section, Statements: facts, Coverage: coverage})
}

func handleGovernSessionChecks(w http.ResponseWriter, q queryGetter, sessionID, section string) {
	offset, limit, err := reviewPageParams(q, "check")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	facts, err := governor.SessionObservedCheckFacts(sessionID, offset, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, sessionChecksResponse{Section: section, Checks: facts})
}
