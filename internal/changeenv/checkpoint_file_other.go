//go:build !unix

package changeenv

import (
	"fmt"
	"os"
)

func readManifestPath(_, relative string, _ bool) (manifestPathEvidence, error) {
	return manifestPathEvidence{Mode: os.FileMode(0)},
		fmt.Errorf("secure manifest capture is unavailable for %q", relative)
}

func openSecureDir(_, _ string) (int, error) { return -1, ErrUnsupportedPlatform }

func statInDir(_ int, _ string) (secureStat, error) { return secureStat{}, ErrUnsupportedPlatform }

func statAndReadBounded(_, _ string, _ int64) (boundedRead, error) {
	return boundedRead{}, ErrUnsupportedPlatform
}

// A platform without component-wise no-follow opens must fail closed. A future
// platform implementation can replace this without weakening checkpoint semantics.
func readCheckpointRegular(_, relative string) ([]byte, int, string, bool, error) {
	if checkpointPathOpenTestHook != nil {
		checkpointPathOpenTestHook(relative)
	}
	return nil, 0, "", false, fmt.Errorf("secure checkpoint file capture is unavailable for %q", relative)
}

func closeFD(_ int) {}
