package vendorpaths

const ClaudeSettingsRelative = ".claude/settings.json"

// ClaudeSessionsRelative is the per-session registry directory the vendor
// documents for its own live sessions (pid-keyed JSON rows carrying the
// session's inbox socket path). Data-only; callers resolve HOME and do all I/O.
const ClaudeSessionsRelative = ".claude/sessions"
