//go:build darwin

package daemon

import (
	"bytes"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// physicalDirectory asks the file system for the open folder's own path
// (fcntl F_GETPATH, through libSystem): symlinks, the /private prefix,
// firmlinks and the on-disk case are resolved by the volume that owns them,
// so it never guesses at case. O_DIRECTORY refuses a FIFO or a file before any
// blocking open. A folder the OS will not open (privacy-protected, gone)
// returns the error; the caller falls back.
func physicalDirectory(clean string) (string, error) {
	dir, err := os.OpenFile(clean, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return "", err
	}
	// A read-only directory handle: a close error changes nothing we return.
	defer func() { _ = dir.Close() }()
	buf := make([]byte, unix.PathMax)
	// fcntl takes the buffer's address as an integer; pinning keeps it valid
	// (and heap-allocated) for the call.
	var pinner runtime.Pinner
	pinner.Pin(&buf[0])
	defer pinner.Unpin()
	if _, err := unix.FcntlInt(dir.Fd(), unix.F_GETPATH, int(uintptr(unsafe.Pointer(&buf[0])))); err != nil {
		return "", err
	}
	end := bytes.IndexByte(buf, 0)
	if end <= 0 {
		return "", syscall.EINVAL
	}
	return string(buf[:end]), nil
}
