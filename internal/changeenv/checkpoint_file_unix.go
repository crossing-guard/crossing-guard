//go:build unix

package changeenv

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func openSecureParent(root, relative string) (int, string, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, "", err
	}
	parts := strings.Split(relative, "/")
	for _, part := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(fd, part,
			unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
		closeErr := unix.Close(fd)
		if openErr != nil {
			return -1, "", openErr
		}
		if closeErr != nil {
			_ = unix.Close(next)
			return -1, "", closeErr
		}
		fd = next
	}
	return fd, parts[len(parts)-1], nil
}

func unixFileMode(mode uint32) os.FileMode {
	out := os.FileMode(mode & 0o777)
	if mode&unix.S_ISUID != 0 {
		out |= os.ModeSetuid
	}
	if mode&unix.S_ISGID != 0 {
		out |= os.ModeSetgid
	}
	if mode&unix.S_ISVTX != 0 {
		out |= os.ModeSticky
	}
	switch mode & unix.S_IFMT {
	case unix.S_IFDIR:
		out |= os.ModeDir
	case unix.S_IFLNK:
		out |= os.ModeSymlink
	case unix.S_IFIFO:
		out |= os.ModeNamedPipe
	case unix.S_IFSOCK:
		out |= os.ModeSocket
	case unix.S_IFCHR:
		out |= os.ModeCharDevice | os.ModeDevice
	case unix.S_IFBLK:
		out |= os.ModeDevice
	}
	return out
}

func readManifestPath(root, relative string, retainBody bool) (manifestPathEvidence, error) {
	if !validManifestPath(relative) {
		return manifestPathEvidence{}, fmt.Errorf("unsafe manifest path %q", relative)
	}
	parent, name, err := openSecureParent(root, relative)
	if err != nil {
		return manifestPathEvidence{}, err
	}
	defer func() { _ = unix.Close(parent) }()
	var stat unix.Stat_t
	if err := unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return manifestPathEvidence{}, err
	}
	evidence := manifestPathEvidence{Mode: unixFileMode(uint32(stat.Mode)), Size: stat.Size}
	if evidence.Mode&os.ModeSymlink != 0 {
		target, err := readLinkAt(parent, name)
		if err != nil {
			return manifestPathEvidence{}, err
		}
		evidence.LinkTarget = target
		evidence.Size = int64(len(target))
		return evidence, nil
	}
	if !evidence.Mode.IsRegular() || !retainBody {
		return evidence, nil
	}
	if evidence.Size > maxGitManifestFileBytes {
		return manifestPathEvidence{}, fmt.Errorf("untracked file %q exceeds 16 MiB", relative)
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return manifestPathEvidence{}, err
	}
	f := os.NewFile(uintptr(fd), relative)
	if f == nil {
		_ = unix.Close(fd)
		return manifestPathEvidence{}, fmt.Errorf("open manifest path %q", relative)
	}
	body, readErr := io.ReadAll(io.LimitReader(f, maxGitManifestFileBytes+1))
	after, statErr := f.Stat()
	closeErr := f.Close()
	if readErr != nil {
		return manifestPathEvidence{}, readErr
	}
	if statErr != nil {
		return manifestPathEvidence{}, statErr
	}
	if closeErr != nil {
		return manifestPathEvidence{}, closeErr
	}
	if len(body) > maxGitManifestFileBytes {
		return manifestPathEvidence{}, fmt.Errorf("untracked file %q exceeds 16 MiB", relative)
	}
	if after.Size() != evidence.Size || after.Mode() != evidence.Mode {
		return manifestPathEvidence{}, fmt.Errorf("untracked file %q changed while hashing", relative)
	}
	evidence.Body = body
	return evidence, nil
}

// openSecureDir opens a directory of the checkout without following any symlink
// component; "" is the root. The caller closes the fd.
func openSecureDir(root, dir string) (int, error) {
	if dir == "" {
		return unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	parent, name, err := openSecureParent(root, dir)
	if err != nil {
		return -1, err
	}
	fd, openErr := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	closeErr := unix.Close(parent)
	if openErr != nil {
		return -1, openErr
	}
	if closeErr != nil {
		_ = unix.Close(fd)
		return -1, closeErr
	}
	return fd, nil
}

// linkTargetBytes bounds a symlink target read; a longer target is refused,
// never truncated into a different path.
const linkTargetBytes = 4096

// readLinkAt reads one symlink's target inside an open directory fd.
func readLinkAt(dirFD int, name string) (string, error) {
	buffer := make([]byte, linkTargetBytes)
	n, err := unix.Readlinkat(dirFD, name, buffer)
	if err != nil {
		return "", err
	}
	if n == len(buffer) {
		return "", fmt.Errorf("symlink target for %q exceeds %d bytes", name, len(buffer))
	}
	return string(buffer[:n]), nil
}

// statInDir stats one name inside an open directory fd without following a
// symlink. The name must be a single component: a slash would make Fstatat
// traverse, and traversal follows intermediate symlinks.
func statInDir(dirFD int, name string) (secureStat, error) {
	if strings.ContainsRune(name, '/') {
		return secureStat{}, fmt.Errorf("statInDir needs one path component, got %q", name)
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(dirFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return secureStat{}, err
	}
	return secureStat{Mode: unixFileMode(uint32(stat.Mode)), Size: stat.Size, ModTime: time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec)}, nil
}

// statAndReadBounded is the Files reader's seam (files plan §3): every component
// opened without following symlinks, a symlink reported by its target, a regular
// file read up to maxBytes and no further (no whole-file hash), anything else
// reported by its mode. readCheckpointRegular beside it is untouched.
func statAndReadBounded(root, relative string, maxBytes int64) (boundedRead, error) {
	if !validManifestPath(relative) {
		return boundedRead{}, fmt.Errorf("unsafe path %q", relative)
	}
	if maxBytes <= 0 {
		return boundedRead{}, fmt.Errorf("bounded read of %q needs a positive byte budget", relative)
	}
	parent, name, err := openSecureParent(root, relative)
	if err != nil {
		return boundedRead{}, err
	}
	defer func() { _ = unix.Close(parent) }()
	stat, err := statInDir(parent, name)
	if err != nil {
		return boundedRead{}, err
	}
	out := boundedRead{secureStat: stat}
	if stat.Mode&os.ModeSymlink != 0 {
		target, err := readLinkAt(parent, name)
		if err != nil {
			return boundedRead{}, err
		}
		out.LinkTarget = target
		return out, nil
	}
	if !stat.Mode.IsRegular() {
		return out, nil
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return boundedRead{}, err
	}
	f := os.NewFile(uintptr(fd), relative)
	if f == nil {
		closeFD(fd)
		return boundedRead{}, fmt.Errorf("open %q", relative)
	}
	defer f.Close() // read-only; a close error changes nothing the caller holds
	// Read one byte past the budget so truncation is what was read, not what a
	// stat said before the read: a file that grows or shrinks meanwhile is
	// still reported by its bytes.
	body, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return boundedRead{}, err
	}
	if int64(len(body)) > maxBytes {
		body = body[:maxBytes]
		out.Truncated = true
	}
	out.Body = body
	return out, nil
}

// readCheckpointRegular opens every path component without following symlinks.
// The returned bytes and digest therefore come from the opened inode, not from a
// pathname that can be redirected between a pre-check and os.Open.
func readCheckpointRegular(root, relative string) ([]byte, int, string, bool, error) {
	if !validManifestPath(relative) {
		return nil, 0, "", false, fmt.Errorf("unsafe checkpoint path %q", relative)
	}
	if checkpointPathOpenTestHook != nil {
		checkpointPathOpenTestHook(relative)
	}
	parent, name, err := openSecureParent(root, relative)
	if err != nil {
		return nil, 0, "", false, err
	}
	fd, openErr := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	closeErr := unix.Close(parent)
	if openErr != nil {
		return nil, 0, "", false, openErr
	}
	if closeErr != nil {
		_ = unix.Close(fd)
		return nil, 0, "", false, closeErr
	}
	f := os.NewFile(uintptr(fd), relative)
	if f == nil {
		_ = unix.Close(fd)
		return nil, 0, "", false, fmt.Errorf("open checkpoint path %q", relative)
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, 0, "", false, err
	}
	if !before.Mode().IsRegular() {
		return nil, int(before.Size()), "", false, fmt.Errorf("checkpoint path %q is not regular", relative)
	}
	if before.Size() > maxGitManifestFileBytes {
		return nil, int(before.Size()), "", true, fmt.Errorf("checkpoint path %q exceeds 16 MiB", relative)
	}
	if checkpointReadTestHook != nil {
		checkpointReadTestHook()
	}
	hasher := sha256.New()
	out := &cappedOutput{max: maxCheckpointPathBytes, hasher: hasher}
	if _, err := io.Copy(out, f); err != nil {
		return nil, out.total, "", out.exceeded, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, out.total, "", out.exceeded, err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() ||
		before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		return nil, out.total, "", out.exceeded, fmt.Errorf("checkpoint path %q changed while reading", relative)
	}
	digest := fmt.Sprintf("sha256-v1:%x", hasher.Sum(nil))
	if out.exceeded {
		return nil, out.total, digest, true, nil
	}
	body := make([]byte, out.buf.Len())
	copy(body, out.buf.Bytes())
	return body, out.total, digest, false, nil
}

func closeFD(fd int) { _ = unix.Close(fd) }
