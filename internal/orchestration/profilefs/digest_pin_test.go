package profilefs

import (
	"sort"
	"testing"
)

// The parser moved to crossing-guard/profiledoc as a behavior-preserving move (team
// rest-of-release plan §4.1 decision 2). These digests were computed by the parser
// as it stood in this package at origin/main 55e20a79, before the move; a change
// here means stored selections and adopted bundles no longer name the same bytes.
var digestsBeforeMove = map[string][2]string{
	"helper-v2":                    {"sha256-v1:421eda5e10f49ee12fef5d196773fdd824886bf965ce944de95686e18aa7e9d1", "sha256-v1:66a70cbd1c8fc0473a2bcc307180a599efe884da152a33dc5039c8a166b3257d"},
	"reviewer-1.0.0":               {"sha256-v1:6b9bc44a5a1cf2f2db6f3af77a2a690dace830cb09f046311ebc6f9d95703c38", "sha256-v1:038ed670d63956fbc605c5ac250bc6f978d881aed89330e3a25a892e6d37aede"},
	"reviewer-2.3.4-crlf":          {"sha256-v1:9b80b6f71021bfc7cd0e8fbb80ffa61246327ffd61391ac453a4ff9cd666975a", "sha256-v1:6e0d922947639269d121c78aace628848e2834cf835f42e19d54d2c2bce2e2f0"},
	"template:follower.PROFILE.md": {"sha256-v1:29a37b55c90527cac6211d60b190ba873999af31a1fdd28ff2b98ce0a387ce50", "sha256-v1:2f01d8f572a19e5b03b568792eb93131fadacdbaa9e5cb50a8a2cfbd4f587b80"},
	"template:helper.PROFILE.md":   {"sha256-v1:13e8e0ccf0a1b0a1a65b8400107216f3cc5781d4b9895015064dc986bc9fb0ab", "sha256-v1:e5aaa06f80f30ec1fda67d27bbcbaf39e8b9eeca79db4bbdbe8e1d709c0453bd"},
	"template:reviewer.PROFILE.md": {"sha256-v1:71edb25119338cb7d8fad713bcdbfb2eaefdff54a3908f94d8f6e242f078df2f", "sha256-v1:d01bb5a8c7ae8abd8a751688b63272b7fdb30a51e0ceb4e943c802c6c51d075d"},
}

func TestParserMoveKeepsEveryFixtureDigest(t *testing.T) {
	sources := map[string][]byte{
		"reviewer-1.0.0":      validReviewerSource("1.0.0", "Review the command.\n"),
		"reviewer-2.3.4-crlf": validReviewerSource("2.3.4", "Line one.\r\nLine two.\r\n"),
		"helper-v2":           validHelperV2Source(),
	}
	entries, err := profileTemplates.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		body, err := profileTemplates.ReadFile("templates/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		sources["template:"+entry.Name()] = body
	}
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) != len(digestsBeforeMove) {
		t.Fatalf("fixtures = %v, pinned = %d; pin a new fixture's digests when adding one", names, len(digestsBeforeMove))
	}
	for _, name := range names {
		document, err := Parse("PROFILE.md", sources[name])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := [2]string{document.SourceDigest, document.BundleDigest}; got != digestsBeforeMove[name] {
			t.Errorf("%s: digests = %v, before the move %v", name, got, digestsBeforeMove[name])
		}
	}
}
