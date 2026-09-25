package taskinput

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"testing"
	"time"
)

func pngFixture(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	var raw bytes.Buffer
	if err := png.Encode(&raw, img); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func TestServiceStagesListsPreviewsRemovesAndClaims(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	service, err := NewService(t.TempDir()+"/private-inputs", testConfig(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	scope, imageInput, err := service.Stage(context.Background(), "", SourcePaste, "camera.png", bytes.NewReader(pngFixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	if imageInput.Kind != KindImage || imageInput.MediaType != "image/png" || len(scope.Inputs) != 1 {
		t.Fatalf("unexpected staged image: %#v %#v", scope, imageInput)
	}
	scope, textInput, err := service.Stage(context.Background(), scope.ID, SourceDrop, "notes.txt", bytes.NewBufferString("hello diff\n"))
	if err != nil {
		t.Fatal(err)
	}
	listed, err := service.List(scope.ID)
	if err != nil || len(listed.Inputs) != 2 {
		t.Fatalf("list: %#v %v", listed, err)
	}
	preview, metadata, err := service.Preview(scope.ID, imageInput.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(preview)
	_ = preview.Close()
	if err != nil || metadata.Kind != KindImage {
		t.Fatalf("preview: %v %#v", err, metadata)
	}
	if _, err := png.Decode(bytes.NewReader(raw)); err != nil {
		t.Fatalf("preview is not normalized PNG: %v", err)
	}
	removed, err := service.Remove(scope.ID, textInput.ID)
	if err != nil || len(removed.Inputs) != 1 {
		t.Fatalf("remove: %#v %v", removed, err)
	}
	preflight, err := service.Preflight(scope.ID, []string{imageInput.ID})
	if err != nil || len(preflight) != 1 || preflight[0].Path == "" {
		t.Fatalf("preflight: %#v %v", preflight, err)
	}
	claim, err := service.Claim(scope.ID, []string{imageInput.ID}, "task_fixture")
	if err != nil || claim.TaskID != "task_fixture" {
		t.Fatalf("claim: %#v %v", claim, err)
	}
	if _, err := service.Remove(scope.ID, imageInput.ID); err == nil {
		t.Fatal("claimed input was removable")
	}
	if err := service.ConsumeTask("task_fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(scope.ID); err == nil {
		t.Fatal("consumed scope still exists")
	}
}

func TestServiceRejectsMismatchLimitsAndExpiredScope(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	config := testConfig()
	config.Limits.MaxSourceBytesPerItem = 64
	config.Limits.MaxSourceBytesPerScope = 128
	config.Limits.MaxGlobalSourceBytes = 256
	config.Limits.MaxTextBytesPerItem = 64
	service, err := NewService(t.TempDir()+"/private-inputs", config, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Stage(context.Background(), "", SourcePicker, "wrong.txt", bytes.NewReader(pngFixture(t))); err == nil {
		t.Fatal("image renamed as text passed")
	}
	if _, _, err := service.Stage(context.Background(), "", SourcePicker, "big.txt", bytes.NewReader(bytes.Repeat([]byte("x"), 65))); err == nil {
		t.Fatal("oversized input passed")
	}
	scope, _, err := service.Stage(context.Background(), "", SourcePicker, "ok.txt", bytes.NewBufferString("ok"))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	if _, err := service.List(scope.ID); err == nil {
		t.Fatal("expired scope passed")
	}
}

func TestServiceRecoveryDeletesInterruptedClaim(t *testing.T) {
	root := t.TempDir() + "/private-inputs"
	service, err := NewService(root, testConfig(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	scope, input, err := service.Stage(context.Background(), "", SourcePicker, "ok.txt", bytes.NewBufferString("ok"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Claim(scope.ID, []string{input.ID}, "task_interrupted"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(root, testConfig(), time.Now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(service.fs.scopeDir(scope.ID)); !os.IsNotExist(err) {
		t.Fatalf("claimed scope survived recovery: %v", err)
	}
}
