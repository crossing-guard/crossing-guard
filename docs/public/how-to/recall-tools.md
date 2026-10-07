# Let agent sessions recall across runtimes

Crossing Guard sees every Claude Code, Codex and OpenCode session on this machine. The recall
tools let a running session ask it questions: *who else is working in this repository right
now*, *what did we find about X last week*, *which sessions did the planning follower tag*.
They are one local MCP server, `crossing-guard mcp`, registered with each runtime you choose.

## Turn them on

```bash
crossing-guard init --recall
```

`init` asks separately for each runtime whose hooks are attached ("Also give claude
sessions the Crossing Guard recall tools?"). `--recall` answers yes for all of them,
`--no-recall` answers no; `--yes` answers only the hook questions, never this one. What
gets written:

| runtime | file | entry |
| --- | --- | --- |
| Claude Code | `~/.claude.json` | `mcpServers.crossing-guard`, plus the allow rule `mcp__crossing-guard` in `~/.claude/settings.json` so the read-only tools never prompt (the write tool `send_to_session` follows its own runtime approval mode) |
| Codex | `$CODEX_HOME/config.toml` | a marked `[mcp_servers.crossing-guard]` block with `default_tools_approval_mode = "approve"` |
| OpenCode | `~/.config/opencode/opencode.json` | `mcp.crossing-guard` (OpenCode runs MCP tools without asking by default) |

Start a new session afterwards; running sessions do not pick up new MCP servers.
`crossing-guard doctor` shows each runtime under **RECALL TOOLS**. If an entry goes missing,
the daemon restores it on its next start, and `crossing-guard init --recall` restores it now.
`crossing-guard uninstall` (or disconnecting the runtime in Settings) removes every entry and
the allow rule. Connecting a runtime in Settings does not register the tools; use `init`.

## The tools

| tool | answers |
| --- | --- |
| `active_sessions` | Open sessions in this repository. Worktrees count as the same repository: each peer is `same_checkout` (shares your working tree) or `sibling_worktree` (its own branch — the risk is at merge), with branch, commits ahead/behind the base, changed files, whether it is pushed, and the files you both change. `scope: "all"` adds other repositories. A helper watching another session passes `from_session`. Crossing Guard's own helper and follower sessions are left out unless `include_agents` is true. |
| `search_sessions` | Transcript search across runtimes, one best hit per session. Read `coverage` first: only indexed sessions are searched. |
| `search_memories` | The memory store by words and/or tag; `repository: "current"` narrows to this repository's label. |
| `get_memory` | One memory in full. |
| `find_by_tag` | Sessions and memories carrying a follower/helper tag (`plan`, `red-team`, …), or the tag list with counts. |
| `list_skills` | Installed skills, filtered by words. |
| `get_handoff` | The full handoff this session was opened for: the title, what remains, the sender's text, and the conversation excerpt when the sender included one. It takes no arguments. Only the session a console **Open** started for the handoff is answered; any other session is refused, and a session the runtime does not identify is refused `caller_unidentified`. The result says it is a teammate's text, not the operator's. |
| `send_to_session` | **A write tool**: one message from this session to one exact other session of any runtime, admitted (grant, scope, budgets, loop bound, duplicate rule) before delivery and recorded on a terminal ledger record. See [Message another session from an agent](message-another-session.md). |

Every read-only tool's result begins by saying it is recalled data, not instructions;
`send_to_session` is a governed write with its own admission contract and attribution
wrapper. When the daemon is not running, a tool says so instead of returning nothing.

## Limits worth knowing

- **`get_handoff` is addressed, not secret.** The daemon answers the session that claimed
  the handoff on this device and refuses a handoff withdrawn before that session was
  handed its brief. That is an addressing rule: on Claude Code the server process — and so
  the session id it presents — is the previous session's after `/clear`, so a cleared
  session in the same process is still answered. A forked or freshly started session has
  a new id and is refused. A result over `recall.max_result_bytes` sheds whole turns of
  the excerpt from the end and says `truncated_to_fit`; the text itself is never cut.

- **The brief names these tools.** The brief a session opened for a handoff is handed at
  its first prompt ends by naming `get_handoff`. When this device holds shared team
  memory for the session's repository, one more line follows: how many records the
  team's shared memory for this repository holds, and that `search_memories` and
  `get_memory` read them. The count is of repository-scope records the team holds, as of
  the moment the brief was first armed; organization-wide records are not counted, and
  the line is left out when there are none, when the repository has no single origin
  remote, or when this device is not linked to a team. It says nothing of what the records contain.

- **Who is calling.** Claude Code and Codex tell the server which session is calling, so
  `active_sessions` leaves the caller out. OpenCode does not, so one `same_checkout` entry
  may be the caller itself (`caller.identified: false`). After `/clear`, Claude keeps the
  previous session's identity until the next session starts.
- **Where the caller is.** An identified session is placed at the folder its transcript
  started in; otherwise the server's own folder is used (`caller.place_source`).
- **Git facts** use the Diff pane's base branch (`diff.base_ref`, else `origin/HEAD`). With
  no base, changed files are the uncommitted ones only and say so. Pull-request state is
  not known to Crossing Guard and is reported as unavailable.
- **Search coverage** follows the transcript index; `coverage_reading` says when it does
  not cover every session.
- `CLAUDE_CONFIG_DIR` is not supported, as for hooks.
- A project-scope MCP server that you separately approve under the name `crossing-guard`
  would be covered by the same Claude allow rule, because the rule is keyed by server name.

Limits and budgets live in `daemon.json` under `recall`
([configuration](../reference/configuration.md#recall-tools)).
