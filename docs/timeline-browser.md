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

1. **Vertical rail.** Events render top-to-bottom on a connected vertical rail,
   each as a node with a dot on the rail. Newest at the top.
2. **Per-node legibility.** Each node is visually separated from its neighbours
   (zebra banding / subtle per-row background), with distinct hover and selected
   states. The selected node is clearly marked (accent).
3. **Short annotation.** Each node shows its kind, timestamp, and a one-line
   annotation to the right of the rail (prompt text for turns; argv for git-ops),
   truncated with ellipsis. Data-quality flags are shown inline.
4. **Type-based styling.** Node appearance distinguishes families at a glance:
   agent turns (cool hues, varied per sub-kind) vs. git ops (warm/amber). A
   data-quality flag reads as an alert (red).
5. **Context separators.** A labeled divider is inserted whenever the
   session-or-branch context switches between consecutive events; git ops form
   their own group. The label names the context (e.g. `session ab12cd · main`,
   `git ops · main`).
6. **Empty state.** A clear message when there are no recorded events.

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
6. Transcript delta (with its line range).
7. Worktree snapshot file list (collapsible).

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

## Out of scope (deferred)

- Search / filter (by session, worktree, kind, branch, time range).
- Pagination / windowing (currently loads the full timeline per request).
- Actual line-level **diff content** (only file status + in-HEAD marker today).
- Viewing a file's content at a point in time / browsable snapshot tree.
- Auto-refresh / live updates.
- Cross-clone timelines once sync lands (will union `refs/twip/journal/*`).
