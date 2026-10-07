package vendorpaths

// AntigravityHooksRelative is the shared hook file read by CLI 1.2.14.
// The adapter filters native transcript paths so desktop events are ignored.
const AntigravityHooksRelative = ".gemini/config/hooks.json"

// AntigravityMCPFromHooksDir locates the sibling shared MCP configuration.
const AntigravityMCPFromHooksDir = "mcp_config.json"

// CLI-specific skill paths; the shared GUI config directory has different roots.
const AntigravitySkillsRelative = ".gemini/antigravity-cli/skills"
const AntigravityPluginsRelative = ".gemini/antigravity-cli/plugins"
