//go:build !darwin

package harvest

import "context"

func observeOpenFiles(context.Context, []string) ([]openFileRecord, error) {
	return nil, errActivityUnsupported
}
