# twip — agent/git history collector

A browsable, append-only timeline of your repo as coding agents develop it. Every Claude Code turn
and every mutating git op is linked to the exact worktree snapshot at that moment; nothing is ever
deleted. Browse it at `twip serve`.

## Installation

```sh
go install github.com/codespeak-dev/twip/cmd/twip@latest   # get the binary
twip install                                               # once per machine: stable binary + git shim + PATH
```

`twip install` points a stable `~/.twip/bin/twip` at the binary (a symlink when twip is a `go install`
target, a copy when the source is transient), installs the git shim there, and sources `~/.twip/env`
from your shell rc so the shim is on `PATH` (rustup-style; `--no-modify-path` to skip the rc edit,
`twip uninstall` to reverse it). Start a new shell, then in each repo you want recorded:

```sh
twip init && git add .claude/settings.json && git commit -m "twip: record agent sessions"
```

Committed hooks are a no-op for anyone without twip. Optionally `twip init --enforce` also gates
`git push` from the repo, blocking pushes that aren't being recorded (bypass once with
`git push --no-verify`). Everyday commands: `twip log` / `show <event-id>` / `audit` / `serve`.

### Updating

```sh
go install github.com/codespeak-dev/twip/cmd/twip@latest   # that's it
```

After the first `twip install`, `~/.twip/bin/twip` is a symlink to your `go install` target, so a
plain `go install` updates the binary the shim, the hooks, and your shell all run — no re-run needed.
Re-run `twip install` only if you change `GOBIN`/`GOPATH`, or installed via a version manager
(mise/asdf/brew) or `go run`, where twip keeps an independent **copy** that doesn't auto-follow.

### Manual PATH setup (if a new shell can't find the shim)

`twip install` writes `~/.twip/env` (a POSIX-sh snippet that prepends `~/.twip/bin` to `PATH`) and
sources it from your shell's startup files. (For a **macOS zsh** account with no `~/.zshrc` — zsh
ignores `~/.profile`, so the wiring wouldn't take — `twip install` detects this and offers to create
`~/.zshrc`, after a confirmation prompt; `--yes` accepts automatically.) If your environment still
isn't covered — managed dotfiles, a shell whose rc isn't sourced, etc. — wire it by hand. All you
need is `~/.twip/bin` on `PATH`:

```sh
# bash — add to ~/.bashrc (and ~/.bash_profile; macOS Terminal sources that for login shells)
. "$HOME/.twip/env"
# zsh  — add to ~/.zshrc (honors $ZDOTDIR); macOS's default shell
. "$HOME/.twip/env"
# fish — fish auto-sources conf.d, but you can also just:
fish_add_path "$HOME/.twip/bin"
# any shell — or skip the env file and prepend directly:
export PATH="$HOME/.twip/bin:$PATH"
```

Verify in a **new** shell: `which git` → `~/.twip/bin/git`, and `git --version` still works (the
shim falls back to real git, so it can never break git). Install with `--no-modify-path` to do all
of the above except the rc edit (and print the line to add).

**GUI git (JetBrains, GitHub Desktop, …)** bypass `PATH` entirely. Point their "Path to Git
executable" at `~/.twip/bin/git` (an absolute path) — the shim works without any `PATH` wiring.

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

**What's recorded.** Claude Code sessions via hooks — turn boundaries (prompt/stop) *and*
intermediate mutating tool calls (`Edit`/`Write`/`Bash`/…), each snapshotting the worktree at that
moment so the timeline shows mid-turn states, not just turn ends (a tool call that changes nothing
is dropped). And git operations via the shim: every mutating op (read-only `status`/`log`/`diff`
are skipped); destructive ops additionally snapshot the dirty worktree first (which no git hook can
do); history-rewriting ones (`commit --amend`, `rebase`, `reset`) pin the orphaned pre-rewrite
commit; and stash entries (which live in `refs/stash`, outside the worktree) are pinned before a
`stash drop`/`clear`.

**The no-silent-loss guarantee.** `twip audit` checks that every event's worktree snapshot
resolves, seq numbers are contiguous, transcript offsets join end-to-end, and surfaces any
data-quality flags — non-zero exit on any divergence. The shim falls back to real git if twip is
unavailable, so it can never break git.

**Sharing across the team.** Push rides on normal git: `twip init` installs a best-effort `pre-push`
hook (which calls `twip sync push`, pushing with `--no-verify` so it never re-runs your other
pre-push checks) that mirrors your journal to the remote you push to. If a hook manager already owns
`pre-push` (lefthook, husky, pre-commit), twip detects it, leaves it untouched, and prints the exact
config to add — wiring `twip sync push` (and, with `--enforce`, `twip check pre-push`) into the
manager.

Fetching teammates' journals is **opt-in** — a plain `git fetch`/`pull` does *not* pull them. Run
`twip sync fetch [remote]` when you want them; it pulls each clone's journal into
`refs/twip/remotes/<remote>/journal/<clone-id>` (authors/branches stay separate), and pins/stash
flat. It's conflict-free by construction: each clone is the sole writer of its own
`refs/twip/journal/<clone-id>`, so every push is a fast-forward and there is never a merge. The
browser then lanes the whole team's timeline, each clone labeled by its author.

*Pending:* tripwire hook (detect shim bypass), graded commit↔session links. Only Claude Code is
implemented, but the agent seam (`internal/agent`) makes adding one "implement the interface +
register".

## Layout

```
cmd/twip/                CLI (cobra): init, install, hook, check, sync, audit, log, show, serve, version
internal/agent/          agent-extension seam: lean Agent interface + registry + normalized Event
internal/agent/claudecode/  Claude Code: hook parse/install, transcript flush + delta + sidechains
cmd/twip/gitshim.go      the `git` shim capture path (pre-destruction snapshot, recursion guard)
cmd/twip/shim.go         `twip shim install/uninstall` (writes the git wrapper + PATH guidance)
cmd/twip/install.go      `twip install/uninstall` (stable binary copy + shim + shell-rc PATH wiring)
cmd/twip/check.go        `twip check pre-push` (the opt-in push gate)
cmd/twip/sync.go         `twip sync push` (mirror refs/twip/* to a remote; one home for sync)
internal/snapshot/       temp-index git write-tree worktree capture
internal/store/          append-only journal: one per-clone ref, CAS-append, schema, flock, read helpers
internal/audit/          integrity audit
internal/readmodel/      derived timeline + verified-link views
internal/web/            server-rendered timeline UI (go:embed)
```
