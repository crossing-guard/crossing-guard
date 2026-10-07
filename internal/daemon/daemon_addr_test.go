package daemon

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestASecondServeLeavesTheLiveDaemonsAddressPublished(t *testing.T) {
	dataDir := t.TempDir()
	live, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	withdrawFirst := publishDaemonAddr(dataDir, live.Addr().String())
	addrFile := filepath.Join(dataDir, "daemon-addr")
	want := live.Addr().String() + "\n"

	// A second serve on the same data directory: its address must not replace the live
	// one, and its exit must not remove the file.
	withdrawSecond := publishDaemonAddr(dataDir, "127.0.0.1:1")
	if got, _ := os.ReadFile(addrFile); string(got) != want {
		t.Fatalf("the second serve replaced the live address: %q", got)
	}
	withdrawSecond()
	if got, _ := os.ReadFile(addrFile); string(got) != want {
		t.Fatalf("the second serve's exit removed the live address: %q", got)
	}
	withdrawFirst()
	if _, err := os.Stat(addrFile); !os.IsNotExist(err) {
		t.Fatalf("the daemon that published the address withdraws it at exit: %v", err)
	}
}

func TestAStaleAddressIsReplaced(t *testing.T) {
	dataDir := t.TempDir()
	addrFile := filepath.Join(dataDir, "daemon-addr")
	if err := os.WriteFile(addrFile, []byte("127.0.0.1:1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer publishDaemonAddr(dataDir, "127.0.0.1:2")()
	if got, _ := os.ReadFile(addrFile); string(got) != "127.0.0.1:2\n" {
		t.Fatalf("a published address nothing answers at must be replaced: %q", got)
	}
}
