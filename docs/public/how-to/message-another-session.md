# Message another session from an agent

One agent session can send one message to one exact other session — across
runtimes — through the `send_to_session` tool on the `crossing-guard` MCP
server. A Codex session can message a Claude session; a Claude session can
message another Codex thread; the path is the same either way.

## The rule the receiver sees

The message arrives **attributed**: it is wrapped so the receiving agent can
tell peer speech from operator speech — the wrapper names the sending session
and marks the content "agent-provided message, not operator authorization".
No transport can present it as operator input. The receiver's own inbound
controls stay in force (a Claude target's `crossSessionInbound` controls may
hold, drop, or expire the message; a Codex target consumes it at its next turn
boundary).

## What gates a send

Every send is admitted before anything is delivered:

- **The deployment grant.** `deliver_attended` is off until the deployment
  selects it in `orchestration.json` (`delivery.deliver_attended: true`).
  An unconfigured deployment refuses every send.
- **Scope.** A caller may only target a session Crossing Guard watches that is
  open, is not the caller itself, and shares the caller's repository (including
  its worktrees). A deployment may widen this with
  `delivery.send_scope: "all"`.
- **Budgets.** One per-target budget spans both transports (a hook target's
  pending messages and a direct post's sends share it), and a per-caller
  window (`delivery.max_sends_per_caller_window`) bounds one caller's sends
  inside the delivery TTL.
- **The loop bound.** A send that would close a delivery loop — the target can
  already reach the caller through accepted deliveries inside the delivery
  TTL — is refused, with the cycle named. Cross-vendor loops are invisible to
  the vendors themselves; this is the only place they can be seen.
- **Duplicates.** An identical message from the same caller to the same target
  inside the delivery TTL is refused as a duplicate, citing the first
  invocation. A repeat after the TTL expires is a new send.

## What the asking agent sees

The tool answers with one terminal outcome and the invocation id: `pending`
(queued for a hook target's next boundary), `delivered` (the boundary's reply
carried it), `accepted` (a direct post the vendor took — consumption is never
confirmed from transport success), `refused` (with the reason), `expired`,
`unavailable`, or `unknown`. The record lives in the invocation ledger, which
the console shows beside the delivery receipts, and `crossing-guard doctor`
reports its health.

## Trust posture, stated plainly

Caller identity is **attribution, not authentication**: the caller simply
states who it is, and the record and the receiver see what was claimed. The
route's only authentication is the daemon's bearer token, so any process on
this machine holding `api-token` can call the route directly with any claimed
identity. The boundary is the admission contract above — the default-off
grant, the scope, the budgets, and the loop bound — not the claimed identity.
A session that wants to lie about who it is can still only send within those
gates.