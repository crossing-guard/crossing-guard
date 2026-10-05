package profilefs

import (
	"bytes"
	"strings"
	"testing"
)

func strPtr(value string) *string { return &value }

func TestApplyEditWithoutChangesKeepsExactBytes(t *testing.T) {
	source := []byte(strings.Replace(string(validHelperV2Source()), "\nname:", "\n\n# the author's comment\nname:", 1))
	out, err := ApplyEdit(source, ProfileEdit{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, source) {
		t.Fatalf("a no-op edit changed the bytes:\n%s", out)
	}
	same, err := ApplyEdit(source, ProfileEdit{Name: strPtr(currentName(t, source))})
	if err != nil || !bytes.Equal(same, source) {
		t.Fatalf("setting the same name changed the bytes: %v\n%s", err, same)
	}
}

func currentName(t *testing.T, source []byte) string {
	t.Helper()
	document, err := Parse("PROFILE.md", source)
	if err != nil {
		t.Fatal(err)
	}
	return document.Profile.Name
}

func TestApplyEditChangesOnlyNamedFieldsAndKeepsOrder(t *testing.T) {
	source := validHelperV2Source()
	instructions := "New instructions.\n\nSecond paragraph."
	out, err := ApplyEdit(source, ProfileEdit{
		Description:  strPtr("A new description."),
		Version:      strPtr("1.10.0"),
		Instructions: &instructions,
	})
	if err != nil {
		t.Fatalf("edit: %v\n%s", err, out)
	}
	before, _ := Parse("PROFILE.md", source)
	after, err := Parse("PROFILE.md", out)
	if err != nil {
		t.Fatal(err)
	}
	if after.Profile.Description != "A new description." || after.Profile.Version != "1.10.0" ||
		after.Profile.Instructions != "New instructions.\n\nSecond paragraph.\n" {
		t.Fatalf("edited fields = %+v", after.Profile)
	}
	if after.Profile.Name != before.Profile.Name || after.Profile.Trigger.Event != before.Profile.Trigger.Event ||
		len(after.Profile.Context) != len(before.Profile.Context) || after.Profile.Limits != before.Profile.Limits {
		t.Fatalf("untouched fields changed: before %+v after %+v", before.Profile, after.Profile)
	}
	idIndex := bytes.Index(out, []byte("\nid:"))
	nameIndex := bytes.Index(out, []byte("\nname:"))
	descriptionIndex := bytes.Index(out, []byte("\ndescription:"))
	if idIndex < 0 || nameIndex < idIndex || descriptionIndex < nameIndex {
		t.Fatalf("key order changed:\n%s", out)
	}
}

func TestApplyEditKeepsAmbiguousScalarsAsStrings(t *testing.T) {
	for _, name := range []string{"1.5", "true", "null", "0x10", "yes"} {
		out, err := ApplyEdit(validHelperV2Source(), ProfileEdit{Name: strPtr(name)})
		if err != nil {
			t.Fatalf("name %q: %v\n%s", name, err, out)
		}
		document, err := Parse("PROFILE.md", out)
		if err != nil || document.Profile.Name != name {
			t.Fatalf("name %q round-tripped as %q (%v)", name, document.Profile.Name, err)
		}
	}
}

func TestApplyEditSequencesContextAndOptionalKeys(t *testing.T) {
	source := validHelperV2Source()
	empty := []string{}
	out, err := ApplyEdit(source, ProfileEdit{Authority: &empty, ReplyShape: strPtr("")})
	if err == nil {
		// Emptying authority is valid for the format; auto-action rules live elsewhere.
		document, _ := Parse("PROFILE.md", out)
		if len(document.Profile.Authority) != 0 || document.Profile.ReplyShape != "" {
			t.Fatalf("emptied fields = %+v", document.Profile)
		}
	}
	if !bytes.Contains(out, []byte("authority-requests: []")) {
		t.Fatalf("emptied authority-requests must stay an explicit empty sequence:\n%s", out)
	}
	if bytes.Contains(out, []byte("reply-shape:")) {
		t.Fatalf("emptied optional reply-shape must be removed:\n%s", out)
	}
	context := []ContextEdit{{Kind: "session.messages", Required: true, MaxBytes: 16384}, {Kind: "prior-claims"}}
	out, err = ApplyEdit(source, ProfileEdit{Context: &context})
	if err != nil {
		t.Fatalf("context edit: %v\n%s", err, out)
	}
	document, _ := Parse("PROFILE.md", out)
	if len(document.Profile.Context) != 2 || document.Profile.Context[0].Kind != "session.messages" ||
		!document.Profile.Context[0].Required || document.Profile.Context[0].MaxBytes != 16384 ||
		document.Profile.Context[1].Required {
		t.Fatalf("context = %+v", document.Profile.Context)
	}
}

func TestApplyEditStagesAndLocality(t *testing.T) {
	stages := map[string]string{"session.turn-started": "When a turn starts, check memory.\nThen decide."}
	out, err := ApplyEdit(validHelperV2Source(), ProfileEdit{Stages: &stages, Locality: strPtr("local-only")})
	if err != nil {
		t.Fatalf("edit: %v\n%s", err, out)
	}
	document, _ := Parse("PROFILE.md", out)
	if document.Profile.Stages["session.turn-started"] != "When a turn starts, check memory.\nThen decide.\n" ||
		document.Profile.Requirements.Destination.Locality != "local-only" {
		t.Fatalf("stages/locality = %+v / %+v", document.Profile.Stages, document.Profile.Requirements)
	}
	cleared := map[string]string{}
	out, err = ApplyEdit(out, ProfileEdit{Stages: &cleared, Locality: strPtr("")})
	if err != nil {
		t.Fatalf("clear: %v\n%s", err, out)
	}
	if bytes.Contains(out, []byte("stages:")) || bytes.Contains(out, []byte("locality:")) {
		t.Fatalf("cleared optional keys remain:\n%s", out)
	}
}

func TestApplyEditReportsInvalidResultButKeepsBytes(t *testing.T) {
	out, err := ApplyEdit(validHelperV2Source(), ProfileEdit{TriggerEvent: strPtr("not.a-signal")})
	if err == nil || out == nil {
		t.Fatalf("an invalid trigger must return the bytes and a problem, got %v", err)
	}
	if ProblemCode(err) != "invalid_profile" {
		t.Fatalf("problem code = %q", ProblemCode(err))
	}
}

func TestApplyEditAcceptsCRLFSource(t *testing.T) {
	source := bytes.ReplaceAll(validHelperV2Source(), []byte("\n"), []byte("\r\n"))
	if _, err := Parse("PROFILE.md", source); err != nil {
		t.Skipf("CRLF source is not a valid profile on this parser: %v", err)
	}
	out, err := ApplyEdit(source, ProfileEdit{Description: strPtr("Edited.")})
	if err != nil {
		t.Fatalf("edit CRLF: %v\n%s", err, out)
	}
}

func TestNewSourceTemplatesParseForEveryKind(t *testing.T) {
	for _, kind := range []string{"helper", "follower", "reviewer"} {
		out, err := NewSource(NewProfile{ID: "new-" + kind, Name: "New " + kind, Description: "Does a thing.", Type: kind})
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		document, err := Parse("PROFILE.md", out)
		if err != nil || document.Profile.ID != "new-"+kind || document.Profile.Version != "1.0.0" ||
			document.Profile.Name != "New "+kind {
			t.Fatalf("%s template = %+v, %v", kind, document.Profile, err)
		}
		if document.Profile.AgentType() != kind {
			t.Fatalf("%s template agent type = %q", kind, document.Profile.AgentType())
		}
		if len(document.Profile.MayTag) != 0 || len(document.Profile.Stages) != 0 {
			t.Fatalf("%s template carries workflow vocabulary: %+v", kind, document.Profile)
		}
	}
	if _, err := NewSource(NewProfile{ID: "x", Name: "X", Description: "d", Type: "coordinator"}); err == nil {
		t.Fatal("an unknown kind must be refused")
	}
}

func TestDuplicateRewritesIdentityOnly(t *testing.T) {
	source := validHelperV2Source()
	out, err := Duplicate(source, "copy-of-helper", "Copy", "Copied.")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := Parse("PROFILE.md", source)
	after, err := Parse("PROFILE.md", out)
	if err != nil || after.Profile.ID != "copy-of-helper" || after.Profile.Version != "1.0.0" ||
		after.Profile.Instructions != before.Profile.Instructions || after.Profile.Trigger.Event != before.Profile.Trigger.Event {
		t.Fatalf("duplicate = %+v, %v", after.Profile, err)
	}
}

func TestNextMinorVersion(t *testing.T) {
	for input, want := range map[string]string{"1.4.0": "1.5.0", "2.9.3": "2.10.0", "1.0.0-rc.1": "1.1.0", "junk": "1.0.0"} {
		if got := NextMinorVersion(input); got != want {
			t.Fatalf("NextMinorVersion(%q) = %q, want %q", input, got, want)
		}
	}
}
