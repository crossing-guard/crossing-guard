package daemon

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"crossing-guard/internal/taskinput"
)

func daemonTaskInputConfig() taskinput.Config {
	return taskinput.Config{FormatVersion: 1, TextExtensions: []string{".txt", ".go", ".md"},
		ImageMediaTypes: []string{"image/png", "image/jpeg", "image/gif", "image/webp"},
		Limits: taskinput.Limits{MaxItemsPerScope: 8, MaxSourceBytesPerItem: 1 << 20,
			MaxSourceBytesPerScope: 4 << 20, MaxGlobalSourceBytes: 8 << 20,
			MaxTextBytesPerItem: 1 << 20, MaxTextBytesPerScope: 2 << 20,
			MaxImageDimension: 1024, MaxImagePixels: 1 << 20,
			MaxNormalizedBytesPerItem: 2 << 20, MaxNormalizedBytesPerScope: 4 << 20,
			MaxBasenameBytes: 255, MaxDisplayCodePoints: 128,
			UnclaimedExpirySeconds: 3600, MaxConcurrentAdmissions: 2}}
}

func installTaskInputsForHTTP(t *testing.T) *taskinput.Service {
	t.Helper()
	service, err := taskinput.NewService(t.TempDir()+"/inputs", daemonTaskInputConfig(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	previousService, previousConfig, previousProblem := taskInputs, taskInputsConfig, taskInputsProblem
	taskInputs, taskInputsConfig, taskInputsProblem = service, daemonTaskInputConfig(), ""
	t.Cleanup(func() {
		taskInputs, taskInputsConfig, taskInputsProblem = previousService, previousConfig, previousProblem
	})
	return service
}

func multipartTaskInputRequest(t *testing.T, name, source string, raw []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("source", source); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/api/task-inputs", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

func TestTaskInputHTTPStageListPreviewAndRemove(t *testing.T) {
	installTaskInputsForHTTP(t)
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	stage := httptest.NewRecorder()
	handleTaskInputStage(stage, multipartTaskInputRequest(t, "screen.png", "paste", imageBytes.Bytes()))
	if stage.Code != http.StatusCreated {
		t.Fatalf("stage status=%d body=%s", stage.Code, stage.Body.String())
	}
	var result struct {
		Scope taskinput.Scope `json:"scope"`
		Input taskinput.Input `json:"input"`
	}
	if err := json.Unmarshal(stage.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Scope.ID == "" || result.Input.Kind != taskinput.KindImage {
		t.Fatalf("stage result=%+v", result)
	}

	listRequest := httptest.NewRequest("GET", "/api/task-inputs", nil)
	listRequest.Header.Set("X-CG-Input-Scope", result.Scope.ID)
	listed := httptest.NewRecorder()
	handleTaskInputList(listed, listRequest)
	if listed.Code != http.StatusOK || !bytes.Contains(listed.Body.Bytes(), []byte(result.Input.ID)) {
		t.Fatalf("list status=%d body=%s", listed.Code, listed.Body.String())
	}

	previewRequest := httptest.NewRequest("GET", "/api/task-inputs/"+result.Input.ID+"/content", nil)
	previewRequest.SetPathValue("input", result.Input.ID)
	previewRequest.Header.Set("X-CG-Input-Scope", result.Scope.ID)
	preview := httptest.NewRecorder()
	handleTaskInputPreview(preview, previewRequest)
	if preview.Code != http.StatusOK || preview.Header().Get("Content-Type") != "image/png" || preview.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("preview status=%d headers=%v body=%s", preview.Code, preview.Header(), preview.Body.String())
	}

	removeRequest := httptest.NewRequest("DELETE", "/api/task-inputs/"+result.Input.ID, nil)
	removeRequest.SetPathValue("input", result.Input.ID)
	removeRequest.Header.Set("X-CG-Input-Scope", result.Scope.ID)
	removed := httptest.NewRecorder()
	handleTaskInputRemove(removed, removeRequest)
	if removed.Code != http.StatusOK || !bytes.Contains(removed.Body.Bytes(), []byte(`"inputs":[]`)) {
		t.Fatalf("remove status=%d body=%s", removed.Code, removed.Body.String())
	}
}

func TestTaskInputHTTPUnavailableAndInvalidSourceAreStructured(t *testing.T) {
	previousService, previousProblem := taskInputs, taskInputsProblem
	taskInputs, taskInputsProblem = nil, "configure task-inputs.json"
	t.Cleanup(func() { taskInputs, taskInputsProblem = previousService, previousProblem })
	recorder := httptest.NewRecorder()
	handleTaskInputList(recorder, httptest.NewRequest("GET", "/api/task-inputs", nil))
	if recorder.Code != http.StatusServiceUnavailable || !bytes.Contains(recorder.Body.Bytes(), []byte(`"code":"unavailable"`)) {
		t.Fatalf("unavailable status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	installTaskInputsForHTTP(t)
	bad := httptest.NewRecorder()
	handleTaskInputStage(bad, multipartTaskInputRequest(t, "note.txt", "remote", []byte("hello")))
	if bad.Code != http.StatusBadRequest || !bytes.Contains(bad.Body.Bytes(), []byte(`"code":"invalid_source"`)) {
		t.Fatalf("invalid source status=%d body=%s", bad.Code, bad.Body.String())
	}
}
