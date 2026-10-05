package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// swapFolderChooser replaces the system dialog for one test; no test ever opens one.
func swapFolderChooser(t *testing.T, chooser func(context.Context, string) (string, error)) {
	t.Helper()
	previous := chooseFolder
	chooseFolder = chooser
	t.Cleanup(func() { chooseFolder = previous })
}

func postFolderChoose(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	registerFolderChooseRoutes(mux)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/folder/choose", strings.NewReader(body)))
	return recorder
}

func TestFolderChooseAnswersTheChosenFolderAndStartsAtAnExistingOne(t *testing.T) {
	start := t.TempDir()
	var startedAt []string
	swapFolderChooser(t, func(_ context.Context, at string) (string, error) {
		startedAt = append(startedAt, at)
		return start + "/", nil
	})
	recorder := postFolderChoose(t, `{"start":"`+start+`"}`)
	var got folderChooseResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, recorder.Body.String())
	}
	if recorder.Code != http.StatusOK || !got.Chosen || got.Path != start {
		t.Fatalf("got %d %+v, want 200 chosen %q", recorder.Code, got, start)
	}
	// A start that is gone, relative, or a file is not handed to the dialog.
	postFolderChoose(t, `{"start":"`+start+`/missing"}`)
	postFolderChoose(t, `{"start":"relative/dir"}`)
	if len(startedAt) != 3 || startedAt[0] != start || startedAt[1] != "" || startedAt[2] != "" {
		t.Fatalf("dialog start folders = %q", startedAt)
	}
}

func TestFolderChooseCancelUnsupportedAndFailureAreToldApart(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"closed without choosing", errFolderChooseCancelled, http.StatusOK, ""},
		{"no dialog on this platform", errFolderChooseUnsupported, http.StatusNotImplemented, folderChooseUnavailable},
		{"the dialog failed", errors.New("boom"), http.StatusInternalServerError, folderChooseFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			swapFolderChooser(t, func(context.Context, string) (string, error) { return "", tc.err })
			recorder := postFolderChoose(t, `{}`)
			if recorder.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, tc.status, recorder.Body.String())
			}
			if tc.code == "" {
				var got folderChooseResponse
				if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil || got.Chosen || got.Path != "" {
					t.Fatalf("a closed dialog must answer chosen:false with no path, got %+v (%v)", got, err)
				}
				return
			}
			var refusal folderChooseError
			if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil || refusal.Code != tc.code {
				t.Fatalf("code = %q, want %q (%v)", refusal.Code, tc.code, err)
			}
		})
	}
	swapFolderChooser(t, func(context.Context, string) (string, error) {
		t.Fatal("a malformed request must not open a dialog")
		return "", nil
	})
	if recorder := postFolderChoose(t, `{"path":"/typed"}`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("an unknown field must be refused, got %d", recorder.Code)
	}
}

// Red-team Low 23: one dialog at a time. A second request while a dialog is open
// answers 409 and opens nothing; once the first ends, the route serves again.
func TestFolderChooseRefusesASecondDialogWhileOneIsOpen(t *testing.T) {
	opened, release := make(chan struct{}), make(chan struct{})
	calls := 0
	swapFolderChooser(t, func(context.Context, string) (string, error) {
		calls++
		if calls == 1 {
			close(opened)
			<-release
		}
		return "/chosen", nil
	})
	first := make(chan *httptest.ResponseRecorder)
	go func() { first <- postFolderChoose(t, `{"start":""}`) }()
	<-opened
	second := postFolderChoose(t, `{"start":""}`)
	var refusal folderChooseError
	if err := json.Unmarshal(second.Body.Bytes(), &refusal); err != nil || second.Code != http.StatusConflict || refusal.Code != folderChooseOpen {
		t.Fatalf("a second dialog while one is open: %d %s", second.Code, second.Body.String())
	}
	if calls != 1 {
		t.Fatalf("the second request opened a dialog: %d", calls)
	}
	close(release)
	if got := <-first; got.Code != http.StatusOK {
		t.Fatalf("the first dialog: %d %s", got.Code, got.Body.String())
	}
	if again := postFolderChoose(t, `{"start":""}`); again.Code != http.StatusOK {
		t.Fatalf("after the dialog closed the route serves again: %d %s", again.Code, again.Body.String())
	}
}
