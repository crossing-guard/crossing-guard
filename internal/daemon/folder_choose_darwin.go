//go:build darwin

package daemon

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// folderDialogScript asks macOS for a folder. The start folder arrives as an
// argument, never spliced into the script text.
const folderDialogScript = `on run argv
	set startAt to item 1 of argv
	if startAt is "" then
		return POSIX path of (choose folder with prompt "Choose the folder the session starts in")
	end if
	return POSIX path of (choose folder with prompt "Choose the folder the session starts in" default location (POSIX file startAt))
end run`

// userCancelledMark is AppleScript's error number for a dialog the person cancelled.
const userCancelledMark = "-128"

// systemChooseFolder shows the macOS folder dialog through osascript, the system's
// own scripting host, with a fixed script.
func systemChooseFolder(ctx context.Context, start string) (string, error) {
	command := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", folderDialogScript, start)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		if strings.Contains(stderr.String(), userCancelledMark) {
			return "", errFolderChooseCancelled
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("osascript: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	path := strings.TrimSpace(stdout.String())
	if path == "" {
		return "", errFolderChooseCancelled
	}
	return path, nil
}
