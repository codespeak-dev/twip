# twip — agent/git history collector

A browsable, append-only timeline of your repo as coding agents develop it. Every Claude Code and
Codex turn and every mutating git op is linked to the exact worktree snapshot at that moment;
nothing is ever deleted. Browse it at `twip serve`.

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
twip init && git add .claude/settings.json .codex/hooks.json .codex/config.toml && git commit -m "twip: record agent sessions"
```

`twip init` installs hooks for all supported agents (Claude Code and Codex) by default. Pass
`--agent claude-code` or `--agent codex` to install only one. Committed hooks are a no-op for
anyone without twip. Optionally `twip init --enforce` also gates `git push` from the repo,
blocking pushes that aren't being recorded (bypass once with `git push --no-verify`). Everyday
commands: `twip log` / `show <event-id>` / `audit` / `serve`.

> **Codex note:** after `twip init`, open Codex in the repo and run `/hooks` to approve the hooks
> in its project layer.

### Updating

```sh
go install github.com/codespeak-dev/twip/cmd/twip@latest   # that's it
```

After the first `twip install`, `~/.twip/bin/twip` is a symlink to your `go install` target, so a
plain `go install` updates the binary that the shim, the hooks, and your shell all run — no re-run
needed. Re-run `twip install` only if you change `GOBIN`/`GOPATH`, or installed via a version manager
(mise/asdf/brew) or `go run`, where twip keeps an independent **copy** that doesn't auto-follow.

Or just run `twip update`: it does the `go install` and re-runs `twip install` for you, so the update
propagates everywhere even when the stable binary is a copy rather than a symlink (`--version` to pin
a version, `--dry-run` to preview). `twip doctor` reports when a newer version is available.

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

`twip doctor` checks this for you — it flags when another directory (Homebrew/conda/nvm or an IDE)
shadows `~/.twip/bin` on `PATH`, the silent failure that stops git ops from being recorded, and also
reports this repo's recording status and whether a newer twip is available.

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
  trees cost nothing (git content-addressing dedupes them). An event that captures no snapshot of
  its own (e.g. a clean git op) carries the previous event's `worktree/` forward unchanged, so
  consecutive commits share the subtree and a diff scanner walking the journal (gitleaks/
  betterleaks) processes only real changes — never a full-tree delete + re-add. Whether an event
  captured a snapshot is recorded in `event.json` (`worktree_tree`), not by the subtree's presence.
- `meta/` — the event record (`event.json`), the turn's transcript delta (`transcript.jsonl`), and
  any subagent sidechain deltas.

**What's recorded.** Claude Code and Codex sessions via hooks — turn boundaries (prompt/stop) *and*
intermediate mutating tool calls (`Edit`/`Write`/`Bash`/`apply_patch`/…), each snapshotting the
worktree at that moment so the timeline shows mid-turn states, not just turn ends (a tool call that
changes nothing is dropped). And git operations via the shim: every mutating op (read-only
`status`/`log`/`diff` are skipped); destructive ops additionally snapshot the dirty worktree first
(which no git hook can do); history-rewriting ones (`commit --amend`, `rebase`, `reset`) pin the
orphaned pre-rewrite commit; and stash entries (which live in `refs/stash`, outside the worktree)
are pinned before a `stash drop`/`clear`.

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

**Redacting a leaked secret.** If a secret an agent touched lands in the journal — a transcript
line, a prompt, or a worktree snapshot — and an all-refs secrets gate blocks your push, `twip redact`
scans this clone's journal and rewrites it in place, replacing each flagged secret with a placeholder
(the clean prefix is kept verbatim, so an already-pushed prefix stays a fast-forward). It scans with
**betterleaks** by default; `--scanner gitleaks` uses gitleaks instead, and `--scanner auto` prefers
betterleaks and falls back to gitleaks (each mode checks for its binary and, if missing, tells you how
to get the other). A project `.gitleaks.toml`/`.betterleaks.toml` at the repo root is honored
automatically, and `--dry-run` previews without rewriting. Redaction is *not* rotation — treat any
exposed secret as compromised and rotate it.

*Pending:* tripwire hook (detect shim bypass), graded commit↔session links.

## Layout

```
cmd/twip/                CLI (cobra): init, install, update, doctor, hook, check, sync, audit, redact, report, log, show, serve, version
internal/agent/          agent-extension seam: lean Agent interface + registry + normalized Event
internal/agent/claudecode/  Claude Code: hook parse/install, transcript flush + delta + sidechains
internal/agent/codex/    Codex: hook parse/install, transcript flush + delta + sidechains
cmd/twip/gitshim.go      the `git` shim capture path (pre-destruction snapshot, recursion guard)
cmd/twip/shim.go         `twip shim install/uninstall` (writes the git wrapper + PATH guidance)
cmd/twip/install.go      `twip install/uninstall` (stable binary copy + shim + shell-rc PATH wiring)
cmd/twip/check.go        `twip check pre-push` (the opt-in push gate)
cmd/twip/sync.go         `twip sync push` (mirror refs/twip/* to a remote; one home for sync)
cmd/twip/redact.go       `twip redact` (scan the journal with betterleaks/gitleaks; rewrite out flagged secrets)
cmd/twip/doctor.go       `twip doctor` (PATH-shadow + recording-status + update diagnostics)
cmd/twip/update.go       `twip update` (go install latest, then re-run twip install)
cmd/twip/report.go       `twip report` (shareable Markdown bug report from recent activity)
internal/snapshot/       temp-index git write-tree worktree capture
internal/store/          append-only journal: one per-clone ref, CAS-append, schema, flock, read helpers
internal/audit/          integrity audit
internal/readmodel/      derived timeline + verified-link views
internal/web/            server-rendered timeline UI (go:embed)
internal/gitutil/        thin wrappers over git plumbing (run, resolve-ref, cat-file, hash-object, …)
internal/hookutil/       shared agent-hook infrastructure: payload parse, formatting, JSON/command helpers
```
