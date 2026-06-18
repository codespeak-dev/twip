# Session Recorder — Handoff from Entire Fork Evaluation

> **Status — historical design rationale.** This records *why* we built our own recorder (vs.
> forking Entire) and the design that survived scrutiny, as of the pre-implementation handoff. It is
> kept as a reasoning record and is **not** revised as the code evolves, so some names and open
> questions here are superseded: refs live under `refs/twip/*` (not `refs/recorder/*`), the shim
> guard env is `TWIP_SHIM_ACTIVE`, and cross-machine sync now ships (`twip sync push` + a
> `remote.*.fetch` refspec — the "storage sync" open question is resolved). For current behavior and
> commands see [README.md](README.md) and `twip <command> --help`.

Outcome of evaluating a fork of Entire CLI (entire-cli repo) for internal-team session/repo-state
recording. **Decision: build our own recorder instead of forking.** This doc distills the goal,
the design that survived scrutiny, hard-won implementation knowledge to steal from Entire, and
the reasoning record.

## Goal

An **append-only timeline** of repository states for internal team use:

1. Every agent-session turn (user message) linked to the exact repo tree at that moment — preserved forever.
2. Events for git operations that modify the tree or refs (checkout, reset --hard, stash, branch, amend, rebase), with pre-destruction snapshots of dirty worktree state where possible.
3. Browsable timeline: each point linked to an agent session turn or a tracked git operation.

Constraints: internal-only (privacy not a concern), **silent data loss is unacceptable**,
we never need rewind/restore functionality — the timeline is purely observational.

## Why not fork Entire

- Entire IS ~70% event-sourced already (shadow branch = per-turn snapshot log, condensation =
  projection, session state file = projection cache) — but it **deletes the raw log after
  projecting** (shadow branch deleted post-condensation, `strategy/manual_commit_hooks.go:1032`).
- Worse: its live derived state **gates destructive transitions**. State bugs translate into
  permanent loss, not just wrong hints:
  - Condensation is skipped/triggered based on state-machine phase + transcript offsets; the
    shadow branch is deleted once state says "all condensed". Wrong state → record never written
    AND raw snapshots deleted.
  - The only durable transcript copy is written at condensation time, sliced by live offsets.
    Transcripts live in `~/.claude/` and rotate — a skipped condensation can lose the conversation.
  - Orphan auto-reset: shadow branch without a state file is judged stale and reset.
- The machinery that keeps that state correct (post-rewrite SHA remapping, base-commit
  reconciliation, shadow-branch migration) serves interactive features we don't need:
  rewind, live status, write-time attribution.
- Key asymmetry that decided it: in Entire, a write-time projection bug produces a permanently
  wrong record (inputs gone). In an event-sourced recorder, an inference bug produces a wrong
  view over correct immutable facts — fix code, re-derive, history heals.

What we lose by not forking: Entire's multi-agent breadth (7 agents) and read-side UX.
Acceptable: we need Claude Code first, and we're building our own timeline UI anyway.

## Design principles (the load-bearing ones)

1. **Append-only event log is ground truth. Capture unconditionally at event time; derive at
   read time.** Capture ≠ derivation: offsets, tree snapshots, transcript bytes must be grabbed
   when they exist; everything else (links, attribution, stats) is inferred later and cacheable.
2. **Derived state may only ever ADD records — never gate retention, never trigger deletion.**
   Simplest form: never delete. Keep-ref every git object the log references (rewrites orphan
   old SHAs; unpinned references dangle after GC).
3. **Precomputed links are untrusted hints; content is the verified truth.** Commit↔session
   links are confirmed at read time by diffing archived per-turn trees against the commit's
   actual diff → graded links ("commit C contains ~80% of session S; X-file hunks dropped").
   No write-time link can be trusted: git lets humans rewrite content while preserving messages
   (partial rebase-conflict resolution keeps a trailer on a commit with partial session content)
   and rewrite messages while preserving content (reset + recommit severs any trailer).

## Architecture

### Capture components

- **Agent hooks (Claude Code)**: SessionStart / UserPromptSubmit / Stop / SessionEnd /
  PreTask / PostTask. Each turn event captures: worktree tree snapshot (committed to an archive
  ref WITHOUT touching index/worktree — `git stash create`-style plumbing), transcript delta
  (lines N..EOF + new offset), session id, HEAD, timestamp.
- **Git shim on PATH** — the primary capture layer for destructive git ops
  (`reset --hard`, `checkout -f`/pathspec, `restore`, `clean`, `stash pop/drop`, `rebase`):
  snapshot dirty worktree BEFORE exec'ing real git; record event {op, argv, before/after HEAD,
  exit code}. Sets an env var (e.g. `RECORDER_SHIM=1`). Rationale: git offers only post-op
  hooks or none for worktree-destructive ops — a shim is the only guaranteed pre-destruction
  snapshot. Team-internal tool → controlling PATH is viable. JetBrains needs its git executable
  pointed at the shim; VS Code and shells use PATH.
- **Tripwire git hook** (post-checkout and/or reference-transaction, ~30 lines): if the shim
  env var is absent, a git binary ran outside the shim → record an out-of-band event and flag
  loudly. Converts the shim's blind spot (absolute-path git, hardcoded paths) from silent gap
  to audited gap. No capture role — no recursion guards or mid-transaction snapshot risk.
- **Optional trailer injection** (prepare-commit-msg): cheap write-once correlation hint.
  Failure modes benign (missing/spurious hint, recovered by content matching). NOT the fragile
  part of Entire — that's the derived-state maintenance, which we don't have.
- **Rejected: background/periodic snapshot daemon** (catches manual edits between sessions) —
  decided too much. Loss window for purely-manual destruction outside git is accepted.

### Storage

- Git objects + refs in the repo: archive refs for snapshots, an ops/event log ref
  (e.g. `refs/recorder/...`). Refs under a custom namespace are GC-protected.
- Concurrent appends (parallel sessions/worktrees, hooks are separate processes): per-session
  refs or CAS-retry loops on ref updates; file locking if needed.
- Also archive `refs/stash` updates (git already commits stashed state there; `stash drop/pop`
  discards it — keep-ref each stash entry).
- Transcript bytes go INTO the log at capture time (per-turn deltas). Never leave the only
  copy in `~/.claude/` awaiting a later collection step.
- Open: push/fetch sync rules across machines.

### Read side

- Timeline reader: merge turn events + git-op events by time; content-match commits against
  archived session trees for verified links; cache derived views freely (invalidation is
  trivial over an append-only log).
- **Integrity audit** (the answer to "silent loss"): every recorded turn resolves to a live
  tree object; event chain contiguous; captures flagged when the flush sentinel timed out.
  Run in CI / on demand. Any divergence between a cached/derived view and re-derivation
  indicates an inference bug AND hands you the correct answer.

## Hard-won capture knowledge to steal from Entire

The `cmd/entire/cli/agent/claudecode/` package (~1.5k LOC non-test, ~2.1k test) is
self-contained — depends only on the `agent` interfaces package, not strategy/checkpoint.
Prime cherry-pick material.

1. **Async transcript flush (the big one)** — `lifecycle.go:196-294`. When the Stop hook fires,
   Claude Code may not have flushed the transcript. Claude appends a `hook_progress` entry to
   the transcript itself after flushing — containing the hook command string
   (`"hooks claude-code stop"`). Entire polls the tail (4KB window, 50ms interval, 3s max) for
   that sentinel, with:
   - **±2s timestamp-skew validation** vs hook start time — the sentinel appears every turn;
     without this you match the PREVIOUS turn's sentinel and read stale data.
   - **Stale-file fast path**: transcript unmodified 2+ min → skip wait (crashed agent;
     avoids 3s penalty per stale session).
   - **Silent fallback** after timeout → proceed, possibly truncated. Record a data-quality
     flag in our event when this happens.
   - Undocumented, string-matched, version-sensitive. No better mechanism exists.
2. **Offset-delta capture self-heals truncation mid-session**: a truncated read at turn N is
   picked up by turn N+1's delta (boundary precision degrades, data retention doesn't).
   The risky read is **SessionEnd** — nothing after it to self-heal; sentinel-wait + flag there,
   consider a later sweep re-read.
3. **Subagent sidechains** — `transcript.go:203-292`. Task subagents write separate
   `agent-<id>.jsonl` files; the ONLY link from the parent transcript is the literal text
   `agentId: <id>` inside tool_result prose. Grep it out; validate the ID is path-safe (it
   becomes a filename — injection surface); capture sidechain files too or subagent
   conversations vanish from the timeline. Token usage aggregates across main + sidechains.
4. **Transcript path discovery** — `claude.go:265`. Transcripts live at
   `~/.claude/projects/<sanitized-cwd>/<session-id>.jsonl`; sanitization = every
   non-alphanumeric → `-` (lossy, collision-prone, cwd-dependent). Always re-derive from
   current repo location; never store transcript paths.
5. **Parsing details**: transcript lines can be huge (base64 images) — unbounded line reader;
   final-line-without-newline edge case (`claude.go:276`). Hook payloads = JSON on stdin.
6. **go-git v5 bug**: `worktree.Reset(HardReset)` / `Checkout()` delete ignored untracked
   directories. Use git CLI for reset/checkout operations (see entire-cli CLAUDE.md).
7. Reusable code beyond the agent package: in-memory tree building from worktree
   (`strategy/common.go`), the `Entire-Checkpoint` trailer convention, cross-process flock
   pattern (`strategy/session_state.go`).

## Git facts established (verify marked items empirically)

- **post-checkout** fires AFTER the worktree is updated — for both branch switches and pathspec
  checkouts (flag distinguishes). Useless for pre-destruction snapshots; fine as tripwire.
- **`git restore` and `git clean`** fire no relevant hooks (restore: VERIFY on our git version).
- **`git reset --hard`** has no worktree hook; reference-transaction covers the ref move only
  (and `reset --hard HEAD` may produce no usable ref signal — VERIFY).
- **reference-transaction** fires per ref update (NOT symbolic refs), in multiple states
  (prepared/committed/aborted — use committed), including for our own ref writes (would need
  self-guard if ever used for capture; as tripwire it's fine).
- **`git stash`** preserves the dirty state itself in `refs/stash` (it's a commit) — archive it.
- **Rebase "pick theirs" full-drop** usually self-resolves: commit becomes empty, rebase drops
  it, trailer vanishes (correctly absent). The dangerous case is PARTIAL conflict resolution:
  trailer survives on a commit containing only part of the session's changes.
- **Trailer durability is passive**: survives amend/rebase/cherry-pick/cross-machine (travels in
  the message); dies on message rewrite. Observer-chain durability is active: survives only
  witnessed rewrites — one unobserved hop (CI squash, teammate's machine) severs silently.

## Open questions

- Snapshot performance on large repos; dedupe by tree hash (skip if unchanged since last snapshot).
- Storage sync / push-fetch policy across team machines.
- Which agents after Claude Code (each adds its own capture quirks — Entire's other agent
  packages are the reference when needed).
- Event log format/schema; per-session vs global ref layout under concurrency.
