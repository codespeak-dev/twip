# twip — agent/git history collector

A browsable, append-only timeline of your repo as coding agents develop it. Every Claude Code turn
and every mutating git op is linked to the exact worktree snapshot at that moment; nothing is ever
deleted. Browse it at `twip serve`.

## Installation

```sh
go install github.com/codespeak-dev/twip/cmd/twip@latest   # per machine
twip shim install                                          # git shim; prints PATH + JetBrains setup
```

Then in each repo you want recorded:

```sh
twip init && git add .claude/settings.json && git commit -m "twip: record agent sessions"
```

Committed hooks are a no-op for anyone without twip. Everyday commands: `twip log` /
`show <event-id>` / `audit` / `serve`. (From a clone, `./scripts/install.sh` does the per-machine
steps in one shot.)

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
