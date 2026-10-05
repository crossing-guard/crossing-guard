package daemon

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProbeDataDirAnswersAbsentAnsweringOrUnknown(t *testing.T) {
	publish := func(t *testing.T, addr string) string {
		t.Helper()
		dir := t.TempDir()
		if addr != "" {
			if err := os.WriteFile(filepath.Join(dir, "daemon-addr"), []byte(addr+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	if presence, why := ProbeDataDir(publish(t, "")); presence != DaemonAbsent {
		t.Fatalf("no daemon-addr: %v %s", presence, why)
	}
	// A published address nothing listens on: the stale file a crash leaves behind.
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stale := closed.Addr().String()
	closed.Close()
	if presence, why := ProbeDataDir(publish(t, stale)); presence != DaemonAbsent {
		t.Fatalf("refused connection: %v %s", presence, why)
	}
	for name, status := range map[string]int{"healthy": http.StatusOK, "rejected token": http.StatusUnauthorized, "unhealthy": http.StatusInternalServerError} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		presence, why := ProbeDataDir(publish(t, strings.TrimPrefix(server.URL, "http://")))
		server.Close()
		if presence != DaemonAnswering {
			t.Fatalf("%s: %v %s", name, presence, why)
		}
	}
	// A listener that accepts and never answers is a wedged daemon, not an absent one.
	wedged, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer wedged.Close()
	if presence, why := ProbeDataDir(publish(t, wedged.Addr().String())); presence != DaemonUnknown {
		t.Fatalf("wedged listener: %v %s", presence, why)
	}
}
