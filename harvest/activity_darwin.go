//go:build darwin

package harvest

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
)

func observeOpenFiles(ctx context.Context, paths []string) ([]openFileRecord, error) {
	if len(paths) == 0 {
		return []openFileRecord{}, nil
	}
	// One OS snapshot is materially faster than passing hundreds of names or invoking
	// one process per repository. The generic layer exact-path filters these records and
	// discards everything outside the already-discovered session population.
	// Use the OS-owned absolute path. A daemon must not let a modified PATH substitute
	// a different executable for a read-only activity probe.
	out, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP", "-Fpcn").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && len(out) == 0 {
			return []openFileRecord{}, nil
		}
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("open-session observation requires lsof: %w", err)
		}
		return nil, fmt.Errorf("open-session observation failed: %w", err)
	}
	return parseOpenFileRecords(string(out)), nil
}
