package daemon

import (
	"bytes"
	"regexp"
	"testing"
)

// The dictation module contract: provisional text never enters the textarea,
// the composer's replaceRange is the only write path, the mount order and
// send guard in chat.js hold, and the routes match the middleware rewrite.
func TestDictationNeverWritesProvisionalTextIntoTheComposer(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		body, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	dictation := read("js/task/dictation.js")
	for _, forbidden := range []string{"textarea.value =", "textarea.value=", "localStorage", "sessionStorage", "setRangeText"} {
		if bytes.Contains(dictation, []byte(forbidden)) {
			t.Fatalf("dictation.js must not contain %q", forbidden)
		}
	}
	for _, required := range []string{"composer.replaceRange(", "dictation-overlay", "adjustOffset(", "requires_disclosure", "confirmDisclosure(",
		"'cancelled'", "'low_confidence'", "'no_speech'", "'timeout'", "'provider_error'", "This may not be right", "Nothing heard in",
		"Microphone is blocked", "isLive()"} {
		if !bytes.Contains(dictation, []byte(required)) {
			t.Fatalf("dictation.js must contain %q", required)
		}
	}
	if matched, _ := regexp.Match(`\b(engine|session_id|partial decode|disclosure token)\b`, bytes.ToLower(regexp.MustCompile(`'[^']*'`).Find(dictation))); matched {
		t.Fatal("dictation.js user-facing strings must not narrate the plumbing")
	}

	composer := read("js/task/composer.js")
	for _, required := range []string{"replaceRange(start, end, value", "metrics()", "caret()"} {
		if !bytes.Contains(composer, []byte(required)) {
			t.Fatalf("composer.js must expose %q", required)
		}
	}

	chat := read("js/views/chat.js")
	attachAt := bytes.Index(chat, []byte("createAttachments({"))
	dictateAt := bytes.Index(chat, []byte("createDictation({"))
	if attachAt < 0 || dictateAt < 0 || dictateAt < attachAt {
		t.Fatal("chat.js must mount attachments before dictation")
	}
	if !bytes.Contains(chat, []byte("dictationController.isLive()")) || !bytes.Contains(chat, []byte("Finish dictating first")) {
		t.Fatal("chat.js must refuse send while a dictation is live")
	}

	api := read("js/speech/speech-api.js")
	if !bytes.Contains(api, []byte("'/api/v1/speech'")) || !bytes.Contains(api, []byte("/api/v1/speech-disclosures")) {
		t.Fatal("speech-api.js must call the canonical /api/v1 surface")
	}

	settings := read("js/views/settings-system.js")
	if !bytes.Contains(settings, []byte("renderSpeechSettings")) || !bytes.Contains(settings, []byte("cg:speech-capabilities-changed")) {
		t.Fatal("settings.js must render the speech card")
	}
	if !bytes.Contains(read("css/app.css"), []byte(".dictation-overlay")) {
		t.Fatal("app.css must style the dictation overlay")
	}
}
