package engine

import "testing"

func TestEntityID(t *testing.T) {
	cases := []struct{ kind, in, want string }{
		{"session", "claude/abc123", "session:claude/abc123"},
		{"memory", "never-publish-artifacts", "memory:never-publish-artifacts"},
		{"db", "prod", "db:prod"},
		{"mcp", "mcp__amazon_ads__getProfiles", "mcp:getProfiles"},
		{"mcp", "Bash", "mcp:Bash"}, // no prefix → unchanged
		{"url", "https://Evil.EXAMPLE.com:8443/steal?x=1", "url:evil.example.com"},
		{"url", "pim.example.internal/path", "url:pim.example.internal"},
		{"url", "http://user:pass@example.com/x", "url:example.com"}, // userinfo stripped, not "url:user"
		{"url", "http://[::1]:8080/x", "url:[::1]"},                  // IPv6 literal kept whole
		{"url", "https://host.com/a@b", "url:host.com"},              // @ in path is not userinfo
		{"file", "/repo/../repo/./secrets/.env", "file:/repo/secrets/.env"},
		{"file", "  /a/b/c  ", "file:/a/b/c"},
	}
	for _, c := range cases {
		if got := EntityID(c.kind, c.in); got != c.want {
			t.Errorf("EntityID(%q,%q) = %q, want %q", c.kind, c.in, got, c.want)
		}
	}
}

// TestEntityIDStable: the same resource, described two ways, must resolve to one id —
// the property session-accumulation and resource-gating both rely on.
func TestEntityIDStable(t *testing.T) {
	if EntityID("url", "https://pim.example.internal/a") != EntityID("url", "http://pim.example.internal/b") {
		t.Error("same host via different scheme/path must be one entity")
	}
	if EntityID("mcp", "mcp__x__getOrders") != EntityID("mcp", "getOrders") {
		t.Error("mcp tool with and without transport prefix must be one entity")
	}
}
