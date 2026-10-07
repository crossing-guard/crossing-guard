//go:build !darwin

package daemon

import "errors"

// physicalDirectory has no portable "path of this open folder" call here, so
// projectRootKey falls back to the symlink-resolved spelling; case stays as
// spelled.
func physicalDirectory(string) (string, error) {
	return "", errors.ErrUnsupported
}
