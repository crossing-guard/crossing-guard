//go:build darwin

package harvest

import (
	"os"
	"syscall"
	"time"
)

// sourceBirthTime is a file's birth time, which macOS reports. It orders
// copies of one call: an original transcript is born before its copies
// (token-usage-analytics plan §3.5).
func sourceBirthTime(info os.FileInfo) time.Time {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}
	}
	return time.Unix(stat.Birthtimespec.Unix())
}
