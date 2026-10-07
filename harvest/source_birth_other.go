//go:build !darwin

package harvest

import (
	"os"
	"time"
)

// sourceBirthTime reports no birth time where the platform's stat does not
// carry one; copies of a call are then ordered by session id (plan D-4).
func sourceBirthTime(os.FileInfo) time.Time { return time.Time{} }
