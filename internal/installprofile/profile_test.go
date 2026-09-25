package installprofile

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestEnsureClassifiesFreshAndPreexistingRoots(t *testing.T) {
	parent := t.TempDir()
	fresh := filepath.Join(parent, "fresh")
	got, err := Ensure(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Created || got.Profile.Cohort != MechanismFirst || got.Profile.Basis != "data-root-created-by-c5c" {
		t.Fatalf("fresh result = %+v", got)
	}
	again, err := Ensure(fresh)
	if err != nil || again.Created || again.Profile != got.Profile {
		t.Fatalf("idempotent result = %+v, %v", again, err)
	}

	legacy := filepath.Join(parent, "legacy")
	if err := os.Mkdir(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err = Ensure(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile.Cohort != Compatibility || got.Profile.Basis != "data-root-preexisting" {
		t.Fatalf("legacy result = %+v", got)
	}
}

func TestPreviewDoesNotCreateRootOrLock(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	profile, err := Preview(root)
	if err != nil || profile.Cohort != MechanismFirst {
		t.Fatalf("preview = %+v, %v", profile, err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("preview created root: %v", err)
	}
	if _, err := os.Lstat(bootstrapLockPath(root)); !os.IsNotExist(err) {
		t.Fatalf("preview created lock: %v", err)
	}
}

func TestEnsureRejectsCorruptProfileAndSymlinkRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if _, err := Ensure(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(root), []byte(`{"format_version":1,"cohort":"c5c-mechanism-first","basis":"data-root-created-by-c5c","first_seen_at":"bad"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(root); err == nil {
		t.Fatal("corrupt profile was accepted")
	}

	realRoot := t.TempDir()
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(link); err == nil {
		t.Fatal("symlink root was accepted")
	}
}

func TestConcurrentEnsureCreatesOneFreshProfile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	const workers = 8
	results := make(chan Result, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := Ensure(root)
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	created := 0
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for result := range results {
		if result.Profile.Cohort != MechanismFirst {
			t.Fatalf("cohort = %s", result.Profile.Cohort)
		}
		if result.Created {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created count = %d, want 1", created)
	}
}
