# Agent profile examples

These are inert configuration candidates. They are not installed or enabled by opening
these files. Source and a verified public installation walkthrough are still in preparation.

| Profile | Trigger and result |
| --- | --- |
| [Recall helper](recall-helper/PROFILE.md) | At task completion, retrieve useful existing memory and propose an attributed reply. |
| [Recall message](recall-message/PROFILE.md) | At a managed message-completion event, retrieve memory and propose a queued message. |
| [Document message](document-message/PROFILE.md) | Use the same event/delivery contract to retrieve relevant project documentation. |

Preview and select a profile in Agents, choose the model/runtime route, scope deployment to
the intended session, and grant only the requested action. Keep automatic action off while
inspecting proposals. Profiles do not themselves authorize the helper's tool use or select
provider credentials. Local execution does not guarantee that an attached provider stays
offline.

Memory examples require existing memory tools in the chosen runtime and the intended memory
store. Missing tools or context must be reported as unavailable. Do not embed machine paths,
credentials or copied memory records in shared profiles.

The current message adapter targets Codex managed sessions and queues for a later turn
boundary. It does not wake an idle session or interrupt current reasoning. Claude and
OpenCode message delivery is unavailable. Natural lifecycle events do not provide the
managed message context these message examples require.

Disable the binding after evaluation. See [agents and memory](../../docs/agents.md).

Natural-session helper configuration is part of this integration candidate. Delivery depends on the selected runtime and event surface; profiles do not provide a universal before-call interruption.
