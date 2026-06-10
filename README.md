# twip — agent/git history collector

twip preserves the full timeline of a project's transformations as it is developed by coding
agents. Every agent turn is linked to the exact repository tree at that moment, preserved forever
and browsable. It is an **append-only event log**: capture happens unconditionally at event time,
everything else is derived at read time, and nothing is ever deleted — so an inference bug yields a
wrong *view* over correct immutable facts, never lost data. See `RECORDER-HANDOFF.md` for the full
design rationale.

> **v1 scope.** Records Claude Code sessions via hooks, plus a CLI and a minimal web timeline.
> Destructive-git-op capture (a `git` shim/tripwire), cross-machine sync, and graded commit↔session
> links are planned for v2. Only Claude Code is implemented, but the agent-extension seam
> (`internal/agent`) is in place so adding another agent is "implement the interface + register".

## How it works

Each agent session is an append-only commit chain on `refs/twip/sessions/<session-id>`: one commit
per hook firing, parented to the previous event. Every event commit's tree holds two things, so both
stay reachable by real git edges (GC-safe) rather than via a sha buried in JSON:

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

# ...run Claude Code sessions as usual; turns are recorded automatically...

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
internal/snapshot/       temp-index git write-tree worktree capture
internal/store/          append-only event log on per-session refs (schema, flock, read helpers)
internal/audit/          integrity audit
internal/readmodel/      derived timeline + verified-link views
internal/web/            server-rendered timeline UI (go:embed)
internal/gitutil/        thin git-plumbing wrapper
```
