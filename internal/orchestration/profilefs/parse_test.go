package profilefs

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func validReviewerSource(version, instructions string) []byte {
	return []byte(`---
format-version: 1
kind: crossing-guard-orchestration-profile
id: command-reviewer
version: "` + version + `"
name: Command reviewer
description: Reviews a selected command without session history.
role: reviewer
execution: stateless-review
trigger:
  event: pretool.action
context:
  - kind: pretool-action
    required: true
  - kind: permission-scope
output:
  kind: review-recommendation
authority-requests:
  - advise
requirements:
  capabilities:
    - one-shot-inference
limits:
  timeout: 30s
  max-hops: 1
  max-depth: 1
failure:
  missing-required-context: block
  unavailable-capability: block
  timeout: record-unavailable
  malformed-output: record-unavailable
presentation:
  job: review-tool-calls
---
` + instructions)
}

func TestParseCompilesBoundedProfileAndVisibleDefaults(t *testing.T) {
	document, err := Parse("PROFILE.md", validReviewerSource("1.0.0", "Review the command and return a bounded recommendation.\n"))
	if err != nil {
		t.Fatal(err)
	}
	if document.Profile.ID != "command-reviewer" || document.Profile.Trigger.IgnoreOrigin != "self" ||
		document.Profile.Trigger.Debounce != "0s" || document.Profile.Context[1].MaxBytes != 65_536 ||
		document.Profile.Requirements.Destination.Locality != "local-only" ||
		document.Profile.Output.Schema != "builtin/review-recommendation-v1" ||
		document.Profile.Limits.MaxTokens != 8192 {
		t.Fatalf("compiled defaults = %+v", document.Profile)
	}
	if !validDigest(document.SourceDigest) || !validDigest(document.BundleDigest) || len(document.Canonical) == 0 {
		t.Fatalf("identities missing: source=%q bundle=%q", document.SourceDigest, document.BundleDigest)
	}
}

func TestParseSeparatesExactSourceFromNormalizedBundleIdentity(t *testing.T) {
	lf := validReviewerSource("1.0.0", "Review carefully.\nReturn one answer.\n")
	crlf := bytes.ReplaceAll(lf, []byte("\n"), []byte("\r\n"))
	commented := bytes.Replace(lf, []byte("name: Command reviewer\n"),
		[]byte("# presentation-only source comment\nname: Command reviewer\n"), 1)
	docLF, err := Parse("PROFILE.md", lf)
	if err != nil {
		t.Fatal(err)
	}
	docCRLF, err := Parse("PROFILE.md", crlf)
	if err != nil {
		t.Fatal(err)
	}
	docComment, err := Parse("PROFILE.md", commented)
	if err != nil {
		t.Fatal(err)
	}
	if docLF.SourceDigest == docCRLF.SourceDigest || docLF.SourceDigest == docComment.SourceDigest {
		t.Fatal("exact source variants received the same source identity")
	}
	if docLF.BundleDigest != docCRLF.BundleDigest || docLF.BundleDigest != docComment.BundleDigest {
		t.Fatalf("normalized-equivalent source changed bundle: %s %s %s",
			docLF.BundleDigest, docCRLF.BundleDigest, docComment.BundleDigest)
	}
	// Cross-platform golden vector: changing this requires an explicit format decision.
	const wantSource = "sha256-v1:3471e8da3b2ddbf41fbc1fd543fe73fcd4aa1f60a19a7633c765dd9150f6cd09"
	const wantBundle = "sha256-v1:01074a9199fe95008f94f20f6942e2985ed2ff3cbfae4ee80a67988fb5dc7c30"
	if docLF.SourceDigest != wantSource || docLF.BundleDigest != wantBundle {
		t.Fatalf("golden identities changed: source=%s bundle=%s", docLF.SourceDigest, docLF.BundleDigest)
	}
}

func TestParseRejectsUnsafeYAMLAndNeverEchoesValues(t *testing.T) {
	secret := "DO-NOT-ECHO-SECRET-SENTINEL"
	base := validReviewerSource("1.0.0", "Review safely.\n")
	cases := map[string][]byte{
		"wrong name":          base,
		"duplicate":           bytes.Replace(base, []byte("name: Command reviewer\n"), []byte("name: Command reviewer\nname: "+secret+"\n"), 1),
		"anchor":              bytes.Replace(base, []byte("name: Command reviewer"), []byte("name: &label "+secret), 1),
		"alias":               bytes.Replace(base, []byte("description: Reviews a selected command without session history."), []byte("description: *label"), 1),
		"merge":               bytes.Replace(base, []byte("trigger:\n"), []byte("trigger:\n  <<: {event: pretool.action}\n"), 1),
		"explicit tag":        bytes.Replace(base, []byte("name: Command reviewer"), []byte("name: !!str "+secret), 1),
		"unknown field":       bytes.Replace(base, []byte("role: reviewer\n"), []byte("role: reviewer\nsecret-field: "+secret+"\n"), 1),
		"missing authority":   bytes.Replace(base, []byte("authority-requests:\n  - advise\n"), nil, 1),
		"empty body":          base[:bytes.LastIndex(base, []byte("---\n"))+4],
		"bom":                 append([]byte{0xef, 0xbb, 0xbf}, base...),
		"nul":                 append(append([]byte(nil), base...), 0),
		"invalid utf8":        append(append([]byte(nil), base...), 0xff),
		"document terminator": bytes.Replace(base, []byte("name: Command reviewer\n"), []byte("...\nname: "+secret+"\n"), 1),
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			sourceName := "PROFILE.md"
			if name == "wrong name" {
				sourceName = "profile.md"
			}
			_, err := Parse(sourceName, source)
			if err == nil {
				t.Fatal("unsafe profile was accepted")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error echoed authored value: %v", err)
			}
		})
	}
}

func TestParseEnforcesResourceAndSemanticBounds(t *testing.T) {
	oversized := append(validReviewerSource("1.0.0", "Review.\n"), bytes.Repeat([]byte("x"), MaxSourceBytes)...)
	if _, err := Parse("PROFILE.md", oversized); ProblemCode(err) != "source_too_large" {
		t.Fatalf("oversize error = %v", err)
	}
	cases := map[string][]byte{
		"invalid semver":     bytes.Replace(validReviewerSource("1.0.0", "Review.\n"), []byte(`version: "1.0.0"`), []byte(`version: "01.0.0"`), 1),
		"wrong execution":    bytes.Replace(validReviewerSource("1.0.0", "Review.\n"), []byte("execution: stateless-review"), []byte("execution: managed-turn"), 1),
		"wrong authority":    bytes.Replace(validReviewerSource("1.0.0", "Review.\n"), []byte("  - advise\n"), []byte("  - request-interrupt\n"), 1),
		"unknown capability": bytes.Replace(validReviewerSource("1.0.0", "Review.\n"), []byte("    - one-shot-inference\n"), []byte("    - shell-anything\n"), 1),
		"depth zero":         bytes.Replace(validReviewerSource("1.0.0", "Review.\n"), []byte("  max-depth: 1"), []byte("  max-depth: 0"), 1),
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse("PROFILE.md", source); err == nil {
				t.Fatal("invalid semantic combination was accepted")
			}
		})
	}
}

func TestParseEnforcesYAMLResourceCeilings(t *testing.T) {
	base := validReviewerSource("1.0.0", "Review.\n")
	insert := func(value string) []byte {
		return bytes.Replace(base, []byte("role: reviewer\n"), []byte(value+"role: reviewer\n"), 1)
	}
	var manyKeys strings.Builder
	for index := range 129 {
		fmt.Fprintf(&manyKeys, "unknown-%03d: value\n", index)
	}
	var manyItems strings.Builder
	manyItems.WriteString("unknown-list:\n")
	for index := range 129 {
		fmt.Fprintf(&manyItems, "  - value-%03d\n", index)
	}
	var manyNodes strings.Builder
	manyNodes.WriteString("unknown-nodes:\n")
	for index := range 128 {
		fmt.Fprintf(&manyNodes, "  - {a: %d, b: %d, c: %d, d: %d}\n", index, index, index, index)
	}
	deep := "unknown-depth: " + strings.Repeat("[", 18) + "value" + strings.Repeat("]", 18) + "\n"
	frontTooLarge := bytes.Replace(base, []byte("role: reviewer\n"),
		[]byte(strings.Repeat("# padding padding padding\n", 3000)+"role: reviewer\n"), 1)
	bodyTooLarge := validReviewerSource("1.0.0", strings.Repeat("x", maxInstructionBytes+1))
	cases := map[string][]byte{
		"map keys":          insert(manyKeys.String()),
		"sequence items":    insert(manyItems.String()),
		"node count":        insert(manyNodes.String()),
		"depth":             insert(deep),
		"scalar":            bytes.Replace(base, []byte("name: Command reviewer"), []byte("name: "+strings.Repeat("x", maxYAMLScalar+1)), 1),
		"frontmatter bytes": frontTooLarge,
		"instruction bytes": bodyTooLarge,
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse("PROFILE.md", source); err == nil {
				t.Fatal("resource ceiling was not enforced")
			}
		})
	}
}
