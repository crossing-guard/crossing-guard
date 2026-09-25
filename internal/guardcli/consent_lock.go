package guardcli

import (
	"crossing-guard/internal/filelock"
)

// withConsentLock serializes the complete consent read-modify-write transaction
// across processes. Atomic rename protects readers from partial JSON; this lock
// separately prevents two valid writers from both reading the same old record and
// losing one update.
func withConsentLock(change func() error) error {
	return filelock.With(consentPath()+".lock", 0o600, change)
}
