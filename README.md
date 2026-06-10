# twip — agent/git history collector

twip preserves the full timeline of a project's transformations as it is developed by coding
agents. Every agent turn is linked to the exact repository tree at that moment, preserved forever
and browsable. It is an **append-only event log**: capture happens unconditionally at event time,
everything else is derived at read time, and nothing is ever deleted — so an inference bug yields a
wrong *view* over correct immutable facts, never lost data. See `RECORDER-HANDOFF.md` for the full
design rationale.

> **Scope.** Records Claude Code sessions via hooks **and destructive git operations via a `git`
> shim** (`reset --hard`, `checkout -- <path>`, `clean`, `stash drop`, `rebase`, …) — snapshotting
> the dirty worktree *before* git destroys it, which no git hook can do. Stash entries (which live
> in `refs/stash`, outside the worktree) are pinned before a `stash drop`/`clear` can orphan them.
> Still pending: the tripwire hook (detect shim bypass), cross-machine sync, and graded
> commit↔session links. Only Claude Code is implemented, but the agent seam (`internal/agent`)
> makes adding an agent "implement the interface + register".

## How it works

Each clone has one **journal** — an append-only commit chain on `refs/twip/journal/<clone-id>` —
and every recorded event is one commit appended to it. Attribution (`kind`, `session_id`,
`worktree_id`, `head`, `branch`) lives in the event record as *fields*, not in the ref name, so the
journal holds every kind of event — including session-independent ones (the v2 git-op capture). This
also means:

- **No merges.** Different clones write different refs (nothing to reconcile on sync); within a
  clone, concurrent writers append via compare-and-swap, and since each event is one childless
  commit, a lost race just re-parents it onto the new tip — never a content merge.
- **No ref explosion.** Refs scale with clones, not sessions.
- The canonical order is each event's timestamp; the read side unions all journals and sorts by it.

Every event commit's tree holds two things, so both stay reachable by real git edges (GC-safe)
rather than via a sha buried in JSON:

- `worktree/` — a full snapshot of the working tree at that moment (captured with a throwaway index
  + `git write-tree`, so it's the literal on-disk state with no side effects on HEAD/index/worktree,
  and unchanged trees cost nothing thanks to git content-addressing).
- `meta/` — the event record (`event.json`), the transcript delta for the turn (`transcript.jsonl`),
  and any subagent sidechain deltas.

## Usage

```sh
go build -o twip ./cmd/twip      # build (put it on your PATH)

twip init                        # install Claude Code hooks into ./.claude/settings.json
                                 #   (preserves any hooks twip doesn't own)

twip shim install                # optional: install a `git` wrapper that records destructive
                                 #   git ops; then put the printed dir on the FRONT of your PATH.
                                 #   Records only in repos where you ran `twip init`; falls back
                                 #   to real git if twip is unavailable, so it can't break git.

# ...run Claude Code sessions as usual; turns are recorded automatically...
# ...destructive git ops (reset --hard, checkout --, clean, rebase, …) are recorded too...

twip log                         # list recorded sessions and turns
twip show <session-id> <seq>     # inspect one event: prompt, transcript, changed files
twip audit                       # verify the log has no silent loss (non-zero exit on divergence)
twip serve                       # browse the timeline at http://localhost:7777
```

`twip audit` is the guarantee behind "silent loss is unacceptable": it checks that every event's
worktree snapshot resolves, seq numbers are contiguous, transcript offsets join end-to-end, and it
surfaces any data-quality flags.

## Layout

```
cmd/twip/                CLI (cobra): init, hook, audit, log, show, serve
internal/agent/          agent-extension seam: lean Agent interface + registry + normalized Event
internal/agent/claudecode/  Claude Code: hook parse/install, transcript flush + delta + sidechains
cmd/twip/gitshim.go      the `git` shim capture path (pre-destruction snapshot, recursion guard)
cmd/twip/shim.go         `twip shim install/uninstall` (writes the git wrapper + PATH guidance)
internal/snapshot/       temp-index git write-tree worktree capture
internal/store/          append-only journal: one per-clone ref, CAS-append, schema, flock, read helpers
internal/audit/          integrity audit
internal/readmodel/      derived timeline + verified-link views
internal/web/            server-rendered timeline UI (go:embed)
internal/gitutil/        thin git-plumbing wrapper
```
