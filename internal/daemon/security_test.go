package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAuthAcceptsEveryDocumentedTokenChannel pins D5: the middleware must accept the
// Authorization: Bearer header (which it used to reject outright, sending a caller
// holding a VALID token chasing a nonexistent startup URL), as well as X-CG-Token and
// the ?t= query param.
func TestAuthAcceptsEveryDocumentedTokenChannel(t *testing.T) {
	const tok = "sekret"
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mw := securityMiddleware(ok, "127.0.0.1:7788", tok, "/data/api-token")

	cases := []struct {
		name string
		set  func(*http.Request)
	}{
		{"bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }},
		{"x-cg-token", func(r *http.Request) { r.Header.Set("X-CG-Token", tok) }},
		{"query param", func(r *http.Request) { r.URL.RawQuery = "t=" + tok }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/sessions", nil)
			c.set(r)
			w := httptest.NewRecorder()
			mw.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Fatalf("%s token rejected: %d — %s", c.name, w.Code, w.Body.String())
			}
		})
	}
}

// TestAuthMessagePointsAtTheTokenFile pins the other half of D5: the 401 must be
// followable. The old text said "open the URL printed at startup", which does not
// exist under launchd; the new one names the token file and the Bearer header.
func TestAuthMessagePointsAtTheTokenFile(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mw := securityMiddleware(ok, "127.0.0.1:7788", "sekret", "/home/u/.crossing-guard/api-token")
	r := httptest.NewRequest("GET", "/api/sessions", nil) // no token
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("missing token should be 401, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "/home/u/.crossing-guard/api-token") {
		t.Errorf("401 does not point at the token file: %q", body)
	}
	if strings.Contains(body, "printed at startup") {
		t.Errorf("401 still uses the unfollowable startup-URL message: %q", body)
	}
}

// TestAPIVersionAliasServesSameHandler pins D16: /api/v1/foo reaches the /api/foo
// handler, so a BYO-GUI that builds against the versioned path gets the same behavior
// and the unversioned path stays a working alias.
func TestAPIVersionAliasServesSameHandler(t *testing.T) {
	var gotPath string
	sink := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(200)
	})
	mw := securityMiddleware(sink, "127.0.0.1:7788", "sekret", "/data/api-token")

	r := httptest.NewRequest("GET", "/api/v1/sessions", nil)
	r.Header.Set("Authorization", "Bearer sekret")
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("versioned path rejected: %d", w.Code)
	}
	if gotPath != "/api/sessions" {
		t.Fatalf("/api/v1/sessions did not rewrite to /api/sessions, got %q", gotPath)
	}
}

// TestBearerSchemeIsCaseInsensitive pins the red-team fix: RFC 7235 makes the auth
// scheme case-insensitive, so "bearer <tok>" and tab-separated forms must be accepted —
// rejecting them was the exact D5 interop failure this set out to fix.
func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	const tok = "sekret"
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mw := securityMiddleware(ok, "127.0.0.1:7788", tok, "/data/api-token")
	for _, h := range []string{"bearer " + tok, "BEARER " + tok, "Bearer\t" + tok} {
		r := httptest.NewRequest("GET", "/api/sessions", nil)
		r.Header.Set("Authorization", h)
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Errorf("Authorization %q rejected: %d", h, w.Code)
		}
	}
}

func TestCanonicalConsoleRootRedirect(t *testing.T) {
	const addr = "127.0.0.1:7788"
	cases := []struct {
		name, method, target, host, want string
	}{
		{"alias root", http.MethodGet, "http://localhost:7788/?view=sessions", "localhost:7788", "http://127.0.0.1:7788/?view=sessions"},
		{"alias root head", http.MethodHead, "http://localhost:7788/", "localhost:7788", "http://127.0.0.1:7788/"},
		{"canonical root", http.MethodGet, "http://127.0.0.1:7788/", "127.0.0.1:7788", ""},
		{"api", http.MethodGet, "http://localhost:7788/api/sessions", "localhost:7788", ""},
		{"asset", http.MethodGet, "http://localhost:7788/js/app.js", "localhost:7788", ""},
		{"mutation", http.MethodPost, "http://localhost:7788/", "localhost:7788", ""},
		{"unrelated host", http.MethodGet, "http://example.test:7788/", "example.test:7788", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.target, nil)
			r.Host = tc.host
			if got := canonicalConsoleRootRedirect(r, addr); got != tc.want {
				t.Fatalf("redirect = %q, want %q", got, tc.want)
			}
		})
	}
}
