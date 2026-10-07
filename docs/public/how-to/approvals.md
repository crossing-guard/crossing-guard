# Review held actions in the approvals inbox

A locally produced `ask` or `confirm-and-record` policy decision can hold a supported live
action while the local console waits for a human response. A supported console-owned
runtime can also hold its provider-native tool request in the same inbox. The approvals
inbox shows the origin, action, target, reason approval is needed, grant scope and lifetime,
response deadline, exact available identity, and outcome.

Open the installed console with `crossing-guard console --open`. Do not copy or share a
console URL containing its local authentication token.

## What happens when an approval is requested

1. A policy hook sends its rule and bounded/redacted action, or a supported runtime bridge
   sends its task/session/tool identity and bounded summary, to the local daemon.
2. The action appears in **Governance → Approvals**.
3. You choose an offered approval length or **Deny** before the response deadline.
4. The policy hook or provider callback receives the decision and the daemon records it.

If no answer arrives before the verified policy-hook budget ends, the request expires and
the hook denies the action. A later response can be retained as advisory-late, but it cannot
change the action already denied at the deadline.

This is the failure mode after a policy hold exists. If the earlier daemon-backed stateful
consult is unavailable, it fails open and never produces that hold. Runtime-tool callbacks
have their own bounded deadline and fail closed on denial, expiry, cancellation, malformed
input, or daemon/bridge unavailability. Provider modes can decide that a tool is already
allowed or denied before a callback is reached; the inbox does not rewrite those native
mode semantics.

Console-owned Claude Code and OpenCode turns use the inbox. The OpenCode adapter is pinned
to its verified server version. Codex, Cursor, and other harnesses remain capability-gated
until their own native response adapters are implemented and verified; their names in a
session list are not an approval-support claim.

## Decide safely

- `deny` is always available for a pending request.
- **Allow once** applies only to the displayed request and lasts until that request
  finishes. It is not remembered for another tool request or another message.
- An OpenCode request can also offer **Allow exact matches for this run**. It applies
  only to the same permission with the exact same ordered targets during the
  current message. Crossing Guard records each later application. Stopping or finishing
  the message releases it, and the next message can ask again.
- `ask` may be allowed or denied.
- `confirm-and-record` requires a typed reason when allowed. It is attributed user input,
  not a verified fact, and allowing does not erase the session's concern state.
- `hard-block` is non-overridable, including after expiry.

Review the action, target, reason, scope, lifetime, runtime, session, rule, fired tags,
coverage boundary, and response deadline. The deadline is how long you have to answer;
it is separate from how long an approval lasts.
Detector coverage is best-effort, and a displayed command may be redacted.

## Recover from common states

- **No approval appears:** run `doctor`, verify firing, and confirm the active rule matches.
- **Expired:** the action was denied. An advisory-late allow does not replay it.
- **Already decided:** reload the inbox; each pending identifier has one enforced result.
- **An exact-run approval asks again:** the permission or one of its targets changed, the
  message ended, the daemon restarted, or the saved run capability could not be verified.
  Review the new request normally; Crossing Guard never widens the earlier choice.
- **Reload or switch sessions while pending:** the global banner should reappear from the
  daemon snapshot. The hold remains active; navigation is not a decision.
- **GUI is closed or in the background:** the hold still reaches the daemon. A best-effort
  OS notification may provide attention, but all decisions are made in the authenticated
  console inbox.
- **Daemon becomes unavailable after a local rule produces a hold:** the inbox cannot
  answer, so that held policy action is denied.
- **Stateful rule never appears:** check governor/platform readiness. An unavailable
  stateful consult proceeds fail-open and therefore creates no approval.

Durable decision records may contain command and claimed-reason data. Protect the local
data directory and exports as sensitive artifacts.
