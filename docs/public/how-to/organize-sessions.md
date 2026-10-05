# Organize sessions with tags and saved views

When you run many sessions across several repositories, the ones waiting on you sink
under newer ones. Tags and saved views keep them in front of you. Crossing Guard ships
no tags and no views: both are yours, and a fresh install shows the session rail exactly
as it always was until you add one.

Open the installed console with `crossing-guard console --open`.

## Native subagents under a conversation

A conversation can have a **native subagents** fold for child sessions reported by its
runtime. Expand it to open a child's transcript. Automatic approval reviewers and spawned
Codex subagents can share the parent's title; the fold keeps them together. Children of
children appear under the same top-level ancestor. Agents launched by Crossing Guard
have their own separate fold. A child whose parent is unavailable is not shown as a
separate conversation in the rail; inspect related sessions for relationship evidence.

## Tag a session

Open a session and click **+ tag** in its header (or press `t`, or right-click its row
in the rail). Type anything up to 64 characters and press Enter. If the tag is new, the
first line reads *Create "your text"* — that is how a tag comes to exist.

- Write `key:value` — `topic:order-routing`, `customer:acme` — if you want to group
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
| `tag:approved` | a tag whose value is approved, under any key, whoever applied it |
| `tag:phase=plan` or `tag:phase:plan` | a tag under a key |
| `tag:phase=*` | any tag under the key phase |
| `tag:topic=order*` | any `topic` starting "order" |
| `mine:follow-up` | only tags you applied |
| `-tag:vcs=commit` | exclude |
| `repo:app` `runtime:codex` `branch:feat/` `title:"sprint retro"` `note:designer` | the obvious |
| `touched:>14d` `tagged:>5d` | age of last activity / of your tag (`h`, `d`, `w`) |
| `calls:>5` `lines:<20` | how much a session did: model calls (the "N calls" on a rail row) / transcript lines |
| `status:running` `open:yes` | live sessions only |
| `indexer latency` | transcript text, inside what the terms above left |

Every term is required. Repeating a single-valued field (`repo:a repo:b`) or one tag key
(`tag:topic=a tag:topic=b`) means either. A term that cannot be read is named under the
bar and the list below is left as it was.

A key alone (`tag:phase`) looks for a tag whose value is phase, not for the key. When
no session has that value but phase is one of your keys, the console says so beside the
query or view, under the bar, under the view's name, and on its board, and the bar
offers a **Use `tag:phase=*`** button that rewrites just that term.

## Save a view

With a filter in the bar, choose how to group it (repository, runtime, none, or any tag
key you use) and press **Save view**. The view appears at the top of the rail, above
your repositories, with the number of sessions in it across every repository. To
manage views later — edit, rename, add another — open **Settings › Session views**.

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

**Settings › Session views** is the management surface. It lists every view on one page:
what it shows, its filter, how it is grouped and how many sessions it holds now. Open a
view to read what it is and what it holds, with **Open in Sessions** to jump to it in the
rail. **Edit** opens a form for its name, filter, grouping and sort; nothing is written
until you press **Save**, and **Cancel** puts everything back. While you type a filter,
the line under it says how many sessions it matches now, or what the filter got wrong; the
filter terms are one click away beside the field. **Duplicate** copies a view whole,
**Delete** removes it, and **New view** starts an empty one.

To change the order views have in the rail, drag a view by its grip on the list, or focus
its row and press Alt+Up or Alt+Down. If a view is changed somewhere else while you are
editing it (from the rail, or by hand in the file), the page tells you before saving and
lets you keep your edit or drop it. An edit you leave unfinished stays, marked on the
list, for as long as the console is open. The same page writes `session-views.json`;
nothing is stored anywhere else.

## Show a view as a board

A board shows a view's sessions as cards in columns. On **Settings › Session views**,
open a view and press **Edit** (or add one), set **Shown as** to **Board**, and:

1. Name the tag key the board writes your moves under. Any key works, used before or not.
2. Add the columns, in the order you want them. Drag a column or a rule by its grip to
   move it, or press Alt+Up or Alt+Down in its row.
3. Add **placement rules** if you want sessions to land in columns by what they did,
   without tagging each one. A rule is a column and a filter. A session you have not
   placed yourself goes to the first rule it matches, top to bottom; a session no rule
   matches is in a last column of its own.

For example (yours will differ; nothing like this ships), with columns `planning`,
`building` and `pushed`, and the rules

```text
pushed     tag:vcs=push
building   tag:fs=edit
planning   tag:phase=plan
```

a session that has pushed is in `pushed` even though it also planned and edited, because
that rule comes first. A rule cannot use `status:`, `open:` or search words; what a
session is doing right now shows as the dot on its card. A saved rule shows how many of
the view's sessions it places.

On the board:

- **Open a session in its desktop app** from its card: hover the card and click the ↗
  mark at its corner, or right-click the card (or focus it and press `m`) and choose
  *Open in …*. A plain click on the card opens the session here, as before. Cards whose
  runtime has no desktop app show neither.
- **Drag a card** to another column, or right-click it (or focus it and press `m`) and
  choose the column.
  That writes your tag, and from then on the card stays where you put it: your placement
  wins over the rules. If its facts would put it elsewhere, the card says which column
  (*matches pushed*).
- **Pin here** (in the `m` menu) keeps a card in the column the rules put it in, whatever
  it does next.
- **Follow observed** (in the `m` menu of a card you placed) removes your placement, and
  the rules place the card again.
- Arrow keys move between cards and columns; Enter opens the session.
- The board reads again when you come back to the page, or press **Refresh**. A rule
  never writes a tag; only your moves do.
- **Needs you** in the board's heading says how many of the cards shown are waiting on
  you: an approval, an agent's question, a failure, interruption or result you have not
  seen, and any session at rest after its turn ended, read or not. Press it to see only those cards,
  each still in its column; press it again to see them all. The number follows the
  sessions as they run and stop. It counts the cards on the board, so a session past a
  column's limit or outside the view is not in it.

To keep throwaway sessions out of a board or a list, give the view a floor, for example
`touched:<7d calls:>5`. `calls` is zero for a session with no usage records (one runtime
writes none); `lines` works for those, though what a line is differs by runtime. A flow
cannot use either term.

In a narrow window a board view shows as its grouped list in the rail.

Views are stored in `session-views.json` in the data directory. It is a plain file you
can edit, copy to another machine, or keep under version control; see
[Saved session views](../reference/configuration.md#saved-session-views).

## A tagged session outlives its transcript

Runtimes delete old transcript files. A session you tagged keeps its place in the rail
and in your views after that, and opens to the text that was kept for search — your
prompts and the replies, without tool detail — with no composer. Untagged sessions
disappear from the rail when their file does. Such a session has no call or line count
left, so a view with a `calls:` or `lines:` floor leaves it out.

## When something goes wrong

- *"write a count like calls:>5 or calls:<20"* under the bar: `calls:` and `lines:` take
  `<` or `>` and a whole number, nothing else.
- *"… is not a field"* under the bar: the word before the colon is not one of the fields
  above. Quote plain text that contains a colon: `"12:30 standup"`.
- A view line shows a problem in red: that entry of `session-views.json` cannot be read.
  Fix or remove it in the file. While it is there the console will not save views, so it
  cannot overwrite your edit.
- *"The saved views changed since they were loaded"*: another browser or an editor
  changed the file first. The list reloads; repeat the change.
