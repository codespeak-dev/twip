# twip — agent/git history collector

twip gives you a **browsable, append-only timeline of your repo as coding agents develop it**.
Every Claude Code turn is linked to the exact working-tree snapshot at that moment, and every
mutating git op is recorded too — destructive ones (`reset --hard`, `checkout`, `rebase`, …)
snapshot the dirty worktree *before* git touches it. Nothing is ever deleted, so you can scroll
back through how a change actually came to be — prompts, transcripts, diffs, and the git moves in
between — long after the fact. Browse it all at `twip serve`.

It's an **event log**: capture happens unconditionally at event time, everything else is derived at
read time. An inference bug yields a wrong *view* over correct immutable facts, never lost data —
that's the whole reason it's built this way (see `RECORDER-HANDOFF.md` for the design rationale).

## Installation

Each developer records their own sessions + git-ops into their own clone — no server, no sync.
One-time per machine, then per-repo opt-in.

**1. Install (each dev).** Clone this repo and run the installer:

```sh
git clone <twip-repo-url> && cd twip && ./scripts/install.sh
```

It builds `twip` into `~/go/bin`, installs a `git` shim into `~/.twip/bin`, and prints the rest:
add both to your PATH (**shim first**, so it shadows git), and point JetBrains' *Path to Git
executable* at the shim (JetBrains bypasses PATH). `twip version` confirms the build you're on.

**2. Enable a repo (once, committed).** In each repo you want recorded:

```sh
twip init                                                # writes .claude/settings.json hooks
git add .claude/settings.json && git commit -m "twip: record agent sessions"
```

Committing the hooks is safe: they're guarded by `command -v twip || exit 0`, a no-op for anyone
without twip installed. Everyone who *does* have twip then records automatically; the git shim
records destructive ops in any repo that's been `twip init`-ed. All data lives in that repo's
`.git` under `refs/twip/*` — nothing leaves the machine.

Then just work as usual:

```sh
twip log                         # the event timeline, newest first (first column = event id)
twip show <event-id>             # inspect one event: prompt, transcript, diffs
twip audit                       # verify the log has no silent loss (non-zero exit on divergence)
twip serve                       # browse the timeline at http://localhost:7777
```

## How it works

**One journal per clone.** Each clone has a single append-only commit chain on
`refs/twip/journal/<clone-id>`, and every recorded event is one commit on it. Attribution (`kind`,
`session_id`, `worktree_id`, `head`, `branch`) lives in the event record as *fields*, not in the
ref name — so the one journal holds every kind of event, session-bound or not. This buys:

- **No merges.** Different clones write different refs (nothing to reconcile on sync); within a
  clone, concurrent writers append by compare-and-swap, and since each event is one *childless*
  commit, a lost race just re-parents it onto the new tip — never a content merge.
- **No ref explosion.** Refs scale with clones, not sessions. Canonical order is each event's
  timestamp; the read side unions all journals and sorts by it.

**Each event commit's tree carries two things,** so both stay reachable by real git edges (GC-safe,
not via a sha buried in JSON):

- `worktree/` — a full snapshot of the working tree, captured with a throwaway index + `git
  write-tree`: the literal on-disk state, no side effects on HEAD/index/worktree, and unchanged
  trees cost nothing (git content-addressing dedupes them).
- `meta/` — the event record (`event.json`), the turn's transcript delta (`transcript.jsonl`), and
  any subagent sidechain deltas.

**What's recorded.** Claude Code sessions via hooks, and git operations via the shim: every
mutating op (read-only `status`/`log`/`diff` are skipped); destructive ops additionally snapshot
the dirty worktree first (which no git hook can do); history-rewriting ones (`commit --amend`,
`rebase`, `reset`) pin the orphaned pre-rewrite commit; and stash entries (which live in
`refs/stash`, outside the worktree) are pinned before a `stash drop`/`clear`.

**The no-silent-loss guarantee.** `twip audit` checks that every event's worktree snapshot
resolves, seq numbers are contiguous, transcript offsets join end-to-end, and surfaces any
data-quality flags — non-zero exit on any divergence. The shim falls back to real git if twip is
unavailable, so it can never break git.

*Pending:* tripwire hook (detect shim bypass), cross-machine sync (push/fetch `refs/twip/*` to
share timelines — the per-clone-id model is built for it), graded commit↔session links. Only Claude
Code is implemented, but the agent seam (`internal/agent`) makes adding one "implement the
interface + register".

## Layout

```
cmd/twip/                CLI (cobra): init, hook, audit, log, show, serve, version
internal/agent/          agent-extension seam: lean Agent interface + registry + normalized Event
internal/agent/claudecode/  Claude Code: hook parse/install, transcript flush + delta + sidechains
cmd/twip/gitshim.go      the `git` shim capture path (pre-destruction snapshot, recursion guard)
cmd/twip/shim.go         `twip shim install/uninstall` (writes the git wrapper + PATH guidance)
internal/snapshot/       temp-index git write-tree worktree capture
internal/store/          append-only journal: one per-clone ref, CAS-append, schema, flock, read helpers
internal/audit/          integrity audit
internal/readmodel/      derived timeline + verified-link views
internal/web/            server-rendered timeline UI (go:embed)
```
