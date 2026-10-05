//go:build !darwin

package daemon

import "context"

// systemChooseFolder has no dialog to open here: the route answers
// picker_unavailable and the console offers only the folders it lists.
func systemChooseFolder(context.Context, string) (string, error) {
	return "", errFolderChooseUnsupported
}
