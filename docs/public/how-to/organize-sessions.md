# Organize sessions with tags and saved views

When you run many sessions across several repositories, the ones waiting on you sink
under newer ones. Tags and saved views keep them in front of you. Crossing Guard ships
no tags and no views: both are yours, and a fresh install shows the session rail exactly
as it always was until you add one.

Open the installed console with `crossing-guard console --open`.

## Tag a session

Open a session and click **+ tag** in its header (or press `t`, or right-click its row
in the rail). Type anything up to 64 characters and press Enter. If the tag is new, the
first line reads *Create "your text"* — that is how a tag comes to exist.

- Write `key:value` — `topic:amazon-routing`, `customer:acme` — if you want to group
  by it later. A tag without a colon is just a tag.
- Capitals are kept as you first typed them and ignored when matching.
- Your most recently used tags appear in every session header as one-click toggles.
- To tag several sessions, Cmd/Ctrl-click or Shift-click their rows and press **Tag…**.
- **Manage tags…** at the foot of the tag editor renames a tag on every session at
  once, or removes it everywhere.

A tag is amber when it is yours, purple when an agent you deployed claimed it, and blue
when a detector saw it. Hover says who and when. Detector facts show only in the session
header and in filters, never on rail rows. Your tags organize; they are never an input
to a rule and are never shown to an agent.

**Note to self**, under the tags, is one line per session. It shows on the session's row
inside your views, and `note:` searches it.

## Filter

Type in the bar above the rail. Plain words search every transcript, as before. A term
with a colon filters:

| you type | you get |
| --- | --- |
| `tag:approved` | sessions with that tag, whoever applied it |
| `tag:phase=plan` or `tag:phase:plan` | a tag under a key |
| `tag:topic=amazon*` | any `topic` starting "amazon" |
| `mine:follow-up` | only tags you applied |
| `-tag:vcs=commit` | exclude |
| `repo:oms` `runtime:codex` `branch:feat/` `title:"due diligence"` `note:lawyer` | the obvious |
| `touched:>14d` `tagged:>5d` | age of last activity / of your tag (`h`, `d`, `w`) |
| `status:running` `open:yes` | live sessions only |
| `repricer margin` | transcript text, inside what the terms above left |

Every term is required. Repeating a single-valued field (`repo:a repo:b`) or one tag key
(`tag:topic=a tag:topic=b`) means either. A term that cannot be read is named under the
bar and the list below is left as it was.

## Save a view

With a filter in the bar, choose how to group it (repository, runtime, none, or any tag
key you use) and press **Save view**. The view appears at the top of the rail, above
your repositories, with the number of sessions in it across every repository.

A view can admit sessions you never touched. Detectors already record facts such as
`phase=plan`, `fs=edit` and `vcs=commit`, so a view like

```text
tag:phase=plan -tag:vcs=commit -tag:approved
```

lists every session that planned something and committed nothing, in any repository,
until you tag it `approved`. Sorted **longest here first**, the oldest waiting plan is at
the top. Group `repo:your-repo` by `tag-key:topic` for groups inside one repository.

A view shows no number when its filter uses `status:`, `open:` or search words: live
status covers only recently active sessions and text search is ranked, so either number
would quietly be wrong.

Right-click a view to rename it, edit its filter, change its sort, move it, or delete
it. Deleting a view deletes a name and a filter; no session changes.

Views are stored in `session-views.json` in the data directory. It is a plain file you
can edit, copy to another machine, or keep under version control; see
[Saved session views](../../framework-and-configuration.md#saved-session-views).

## A tagged session outlives its transcript

Runtimes delete old transcript files. A session you tagged keeps its place in the rail
and in your views after that, and opens to the text that was kept for search — your
prompts and the replies, without tool detail — with no composer. Untagged sessions
disappear from the rail when their file does.

## When something goes wrong

- *"… is not a field"* under the bar: the word before the colon is not one of the fields
  above. Quote plain text that contains a colon: `"12:30 standup"`.
- A view line shows a problem in red: that entry of `session-views.json` cannot be read.
  Fix or remove it in the file. While it is there the console will not save views, so it
  cannot overwrite your edit.
- *"The saved views changed since they were loaded"*: another browser or an editor
  changed the file first. The list reloads; repeat the change.
