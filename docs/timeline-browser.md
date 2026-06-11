# Timeline Browser — Requirements

The read-side UI for twip, served by `twip serve`. A browsable view of the
append-only journal: every recorded event (agent turns and git operations) in one
time-ordered timeline, with a detail view per event.

## Constraints

- **Single binary, no build step.** Server-rendered shell + vanilla JS + CSS,
  embedded via `go:embed`. No npm, no bundler, no external CDN assets.
- **Read-only.** Pure projection over the immutable journal; never mutates state.
  Every view is recomputable from events (cacheable, but never authoritative).
- **Event-addressed, not session-addressed.** Events are identified by commit id
  everywhere (URLs, API). Session is an attribution/grouping *display* concern
  only — never the addressing key.

## Data presented

One merged timeline of events from `refs/twip/journal/*`, newest first. Event
kinds and their meaning drive the visuals:

- `session-start`, `user-prompt-submit`, `stop`, `session-end`, `post-task`
  (subagent) — **agent turns**, carry a `session` + per-session `seq`.
- `gitop` — **session-independent** git operations (no `session`); carry op/argv,
  before/after HEAD, exit code, dirty flag, and any archived stash shas.

Every event carries: timestamp, branch, worktree id, an optional worktree
snapshot, and a short detail string (the prompt, or the git-op argv).

## Layout

Two-pane master-detail:

- **Left:** the timeline (scrollable).
- **Right:** the detail panel for the selected event (scrollable, fills remaining
  width). Stacks vertically on narrow viewports.

## Timeline (left pane)

A two-lane vertical graph (à la a git-history viewer), newest at top.

1. **Two independent lanes.** There are two semantic lines, and every event
   belongs to exactly one:
   - **Branch lane** — a continuous spine: every event happens on some branch, so
     the line is unbroken *except* where the branch switches (the interrupt).
     Git ops (session-independent events) are the nodes on this lane.
   - **Session lane** — present only across a session's span (its newest to its
     oldest event). Session turns are the nodes on this lane. The line may have
     **big gaps** — it stays alive across git-ops that happen mid-session — and
     **interrupts at session end** (and between back-to-back different sessions).
2. **Per-node legibility.** Zebra banding / subtle per-row background, with
   distinct hover and selected (accent) states.
3. **Short annotation.** Each node shows kind, timestamp, and a one-line
   annotation (prompt for turns; argv for git-ops), truncated with ellipsis.
   Data-quality flags shown inline.
4. **Type-based styling.** Dots are colored by kind (agent sub-kinds in cool hues,
   git ops amber); the branch line and session line are visually distinct. A
   branch chip marks where a branch run begins.
5. **Empty state.** A clear message when there are no recorded events.

## Detail panel (right pane)

Populated when a node is selected. Shows, as applicable to the event kind:

1. Header: kind (+ quality flag).
2. Metadata: event id, time, session + seq (turns only), worktree, HEAD + branch,
   model.
3. **Git-operation block** (git-ops): argv, before→after HEAD, exit code, dirty,
   archived-stash shas.
4. Prompt (prompt-submit events).
5. **Changed files vs the previous snapshot** (of the same worktree), each with a
   verified `in HEAD` / `not at HEAD` marker.
6. Transcript delta (with its line range), **pretty-printed** (each JSONL record
   reformatted; non-JSON lines left as-is).
7. Worktree snapshot file list (collapsible).

Any SHA shown (HEAD, git-op before/after, archived stash) is **clickable**, as is
every changed-file and snapshot-file entry — see the object viewer.

## Object viewer (slide-over)

A right-hand slide-over, opened from a clickable SHA or file, dismissed with
`Esc`/close. Content is fetched **on demand** (never preloaded):

- **Commit** (SHA click): `git show` for that commit, with +/-/@@ diff coloring.
- **File diff** (changed-file click): the per-file unified diff between the
  previous snapshot and this event's snapshot.
- **File content** (snapshot-file click): the file's bytes at that snapshot.

## Interaction

- Clicking a node loads its detail in the right pane **without** a full-page
  navigation.
- Each event is **deep-linkable** at `/event/<commit>`; opening that URL selects
  and scrolls to the event.
- On load with no deep link, the newest event is selected by default.

## API (backing the UI)

- `GET /` and `GET /event/<commit>` — the app shell.
- `GET /api/timeline.json` — the merged event list.
- `GET /api/event/<commit>` — one event's full detail.
- `GET /api/commit/<sha>` — `git show` for a commit (object viewer).
- `GET /api/blob?rev&path` — a file's content at a tree/commit.
- `GET /api/filediff?base&tree&path` — a path's unified diff between two trees.

## Out of scope (deferred)

- Search / filter (by session, worktree, kind, branch, time range).
- Pagination / windowing (currently loads the full timeline per request).
- Lane-separating **concurrently overlapping** sessions (the session lane assumes
  roughly sequential sessions; heavy overlap isn't split into multiple columns).
- Browsable snapshot *tree* navigation (today: a flat file list, click to view).
- Auto-refresh / live updates.
- Cross-clone timelines once sync lands (will union `refs/twip/journal/*`).
