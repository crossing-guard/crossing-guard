package changeenv

import "context"

// untrustedFoldersKey marks a context whose git reads run in folders the
// owner did not pick for this read (recall-mcp-v1-plan §13 item 9, owner
// decision 2026-09-26): the peers route reads every open session's checkout
// on any authenticated request.
type untrustedFoldersKey struct{}

// WithUntrustedFolders returns a context under which this package's git
// reads refuse repository-configured commands a read could otherwise run:
// core.fsmonitor names a program git starts to refresh the index.
func WithUntrustedFolders(ctx context.Context) context.Context {
	return context.WithValue(ctx, untrustedFoldersKey{}, true)
}

// gitReadGuard is the extra git configuration a read under ctx carries.
func gitReadGuard(ctx context.Context) []string {
	if untrusted, _ := ctx.Value(untrustedFoldersKey{}).(bool); untrusted {
		return []string{"-c", "core.fsmonitor=false"}
	}
	return nil
}
