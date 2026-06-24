# Codex Agent Support Spec

This document specifies twip support for OpenAI Codex. It is based on:

- the existing Claude Code implementation in `internal/agent/claudecode/`;
- a live hook-payload spike against Codex CLI `0.142.0-alpha.6`;
- the current Entire Codex implementation at `entireio/cli`
  commit `77cba9fc4ef5908403d64f5511260ce33507c133`;
- OpenAI Codex source commit `e98d43ac372ddf7f513c0e30c56dd8dc35ea5404`.

The implementation goal is to add a new `internal/agent/codex/` package that
implements `agent.Agent` without changing the append-only journal model.

## Goals

- Register a new agent named `codex`.
- Install repo-local Codex hooks into `.codex/hooks.json`.
- Enable Codex hooks for the repo with `.codex/config.toml`:

  ```toml
  [features]
  hooks = true
  ```

- Capture the same normalized event kinds used by Claude Code where Codex has
  equivalent lifecycle hooks.
- Mirror Claude subagent capture: Codex subagent transcripts become
  `agent.Sidechain` deltas on `KindSubagentStop` events.
- Preserve the no-silent-loss guarantee: transcript gaps and flush uncertainty
  must be represented with `agent.Quality` flags, never ignored.

## Non-Goals

- No new journal schema is required.
- No Codex-specific read model is required.
- No dependency on Codex SQLite state. Use hook payload paths and JSONL
  transcripts only.
- No attempt to infer changed files from shell commands. twip snapshots the
  actual worktree and already drops unchanged tool events.

## Agent Name and Hook Verbs

The registry name is:

```text
codex
```

The hook verbs used by `twip hook codex <verb>` are:

| twip verb | Codex hook event | Normalized event |
| --- | --- | --- |
| `session-start` | `SessionStart` | `agent.KindSessionStart` |
| `user-prompt-submit` | `UserPromptSubmit` | `agent.KindPromptSubmit` |
| `stop` | `Stop` | `agent.KindStop` |
| `post-tool-use` | `PostToolUse` | `agent.KindToolUse` |
| `subagent-stop` | `SubagentStop` | `agent.KindSubagentStop` |

Codex also emits `SubagentStart` and `PreToolUse`. twip must not install or
record those hooks initially:

- `SubagentStart` carries useful metadata, but the sidechain bytes are not
  complete until `SubagentStop`. Installing it would add hook-review friction
  without improving retention.
- `PreToolUse` happens before any worktree mutation, so a post-tool snapshot is
  the useful capture point.

## Hook Installation

Install hooks into:

```text
<repo>/.codex/hooks.json
```

Also ensure:

```text
<repo>/.codex/config.toml
```

contains `[features] hooks = true`. If it contains the legacy
`codex_hooks = true`, replace it with `hooks = true`.

Project-local Codex hooks only load when the project `.codex/` layer is trusted.
Codex may also require the user to approve new hook definitions with `/hooks`.
`twip init --agent codex` should mention this when it installs hooks.

The hook command should follow the Claude Code pattern and become a no-op when
`twip` is not installed:

```sh
sh -c 'command -v twip >/dev/null 2>&1 || exit 0; exec twip hook codex <verb>'
```

Codex supports command hook timeouts. Use a short explicit timeout, such as
`30`, for all twip hook handlers.

### Hook Config Shape

Use `hooks.json` rather than inline `[hooks]` in `config.toml`, so hook
configuration remains isolated and easy to preserve.

```json
{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "sh -c 'command -v twip >/dev/null 2>&1 || exit 0; exec twip hook codex session-start'",
            "timeout": 30
          }
        ]
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "sh -c 'command -v twip >/dev/null 2>&1 || exit 0; exec twip hook codex user-prompt-submit'",
            "timeout": 30
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "sh -c 'command -v twip >/dev/null 2>&1 || exit 0; exec twip hook codex stop'",
            "timeout": 30
          }
        ]
      }
    ],
    "PostToolUse": [
      {
        "matcher": "Bash|apply_patch|Edit|Write",
        "hooks": [
          {
            "type": "command",
            "command": "sh -c 'command -v twip >/dev/null 2>&1 || exit 0; exec twip hook codex post-tool-use'",
            "timeout": 30
          }
        ]
      }
    ],
    "SubagentStop": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "sh -c 'command -v twip >/dev/null 2>&1 || exit 0; exec twip hook codex subagent-stop'",
            "timeout": 30
          }
        ]
      }
    ]
  }
}
```

Notes:

- `PostToolUse` should include `Bash` because shell commands can mutate the
  worktree. `recordHook` already skips unchanged snapshots.
- `apply_patch` is the canonical Codex file edit tool serialized in hook stdin.
  `Edit` and `Write` are matcher-only aliases that select the same apply-patch
  hooks for Claude-style compatibility. A matcher of
  `Bash|apply_patch|Edit|Write` is therefore correct for built-in Codex
  shell/edit worktree mutators.
- Codex can also emit `PostToolUse` for other function tools, MCP tools,
  extension tools, and `spawn_agent` (`Agent` matcher alias). twip intentionally
  does not match those by default because they are not built-in worktree
  mutators, and broad `*` matching would increase hook noise.
- Preserve unknown top-level keys and unknown hook event keys.
- `--force` removes only twip-owned commands and leaves foreign hooks intact.

## Hook Payloads

All payloads are JSON on stdin. Unknown fields must be ignored.

Common fields:

```go
type commonRaw struct {
	SessionID      string  `json:"session_id"`
	TurnID         string  `json:"turn_id,omitempty"`
	TranscriptPath *string `json:"transcript_path"`
	CWD            string  `json:"cwd"`
	HookEventName  string  `json:"hook_event_name"`
	Model          string  `json:"model"`
	PermissionMode string  `json:"permission_mode"`
}
```

`transcript_path` is nullable, for example in ephemeral sessions. A missing
transcript path should not fail prompt/tool events. For stop/subagent-stop,
return an event with empty transcript bytes and
`agent.QualityTranscriptUnavailable` if no transcript can be read.

### SessionStart

Example:

```json
{
  "session_id": "019eeff0-06e9-7c50-be7e-711437a32e8e",
  "transcript_path": "/Users/me/.codex/sessions/2026/06/22/rollout-2026-06-22T16-25-45-019eeff0-06e9-7c50-be7e-711437a32e8e.jsonl",
  "cwd": "/repo",
  "hook_event_name": "SessionStart",
  "model": "gpt-5.5",
  "permission_mode": "bypassPermissions",
  "source": "startup"
}
```

Behavior:

- Return `KindSessionStart`.
- Read the transcript file once to get the line count, the new bytes since the
  prior cursor, and the fork parent ID together (avoids a race between separate
  reads).
- Store `Event.Transcript` as the delta from the prior `Cursor.Main` to the
  current transcript line count when any new lines are already present. This
  captures Codex startup records that can be appended before the `SessionStart`
  hook fires.
- If there is no prior cursor (`Cursor.Main == 0`) and the session is not a
  fork, check the first transcript line. If its timestamp is more than three
  days older than the `SessionStart` hook time, binary-search for the first
  line that is recent or timestamp-ambiguous and baseline `Cursor.Main` there,
  storing only that suffix. This avoids re-storing old resumed history when the
  journal has no prior event. Timestamp-ambiguous lines are kept as part of
  the suffix. Forked sessions always capture from line 0 regardless of age.
- Set `Cursor.Main` to the current transcript line count. On a resumed session,
  the prior cursor keeps this delta to newly appended resume/startup lines.
- Set `Model`.
- If the first line is a `session_meta` entry with a non-empty `forked_from_id`:
  - Set `Event.ForkedFrom` to the parent session ID.
  - The same session-start transcript delta stores the fork preamble bytes when
    the prior cursor starts at 0, so the complete parent context is stored in
    the journal under the child's session-start commit.
  - See [Session Forking](#session-forking) below.
- Set `ForkedFrom` to the parent session ID if this is a forked session (see
  [Session Forking](#session-forking) below).

### UserPromptSubmit

Example:

```json
{
  "session_id": "019eeff0-06e9-7c50-be7e-711437a32e8e",
  "turn_id": "019eeff0-0780-7782-9524-13676e1ee8bc",
  "transcript_path": "/Users/me/.codex/sessions/2026/06/22/rollout-2026-06-22T16-25-45-019eeff0-06e9-7c50-be7e-711437a32e8e.jsonl",
  "cwd": "/repo",
  "hook_event_name": "UserPromptSubmit",
  "model": "gpt-5.5",
  "permission_mode": "bypassPermissions",
  "prompt": "Fix the bug"
}
```

Behavior:

- Return `KindPromptSubmit`.
- Store `Prompt`.
- Leave cursor unchanged.

### PostToolUse

`Bash` example:

```json
{
  "session_id": "019eefed-f20a-7061-bf7d-29e39c61f8d0",
  "turn_id": "019eefed-f29c-7423-8ccf-9569d1de72f7",
  "transcript_path": "/Users/me/.codex/sessions/2026/06/22/rollout-2026-06-22T16-23-29-019eefed-f20a-7061-bf7d-29e39c61f8d0.jsonl",
  "cwd": "/repo",
  "hook_event_name": "PostToolUse",
  "model": "gpt-5.5",
  "permission_mode": "bypassPermissions",
  "tool_name": "Bash",
  "tool_input": {"command": "go test ./..."},
  "tool_response": "ok ...",
  "tool_use_id": "call_abc"
}
```

`apply_patch` example:

```json
{
  "session_id": "019eeff0-d6d6-7e31-981e-a0ae5d7fad82",
  "turn_id": "019eeff0-d7a6-7f33-8461-c18c28c53d16",
  "transcript_path": "/Users/me/.codex/sessions/2026/06/22/rollout-2026-06-22T16-26-38-019eeff0-d6d6-7e31-981e-a0ae5d7fad82.jsonl",
  "cwd": "/repo",
  "hook_event_name": "PostToolUse",
  "model": "gpt-5.5",
  "permission_mode": "bypassPermissions",
  "tool_name": "apply_patch",
  "tool_input": {
    "command": "*** Begin Patch\n*** Add File: patch_target.txt\n+patched-by-codex\n*** End Patch\n"
  },
  "tool_response": "Exit code: 0\n...",
  "tool_use_id": "call_def"
}
```

Behavior:

- Return `KindToolUse`.
- Set `Tool.Name` to `tool_name`.
- Set `Tool.Detail` best-effort:
  - for `Bash`, use a truncated `tool_input.command`;
  - for `apply_patch`, extract a compact path summary from `tool_input.command`
    if convenient, otherwise leave empty;
  - for other tools, leave empty.
- Leave cursor unchanged.
- Let `recordHook` skip unchanged snapshots.

## Session Forking

Codex supports forking a session mid-flight. When a session is forked, Codex
creates a new transcript file that begins with the full content of the parent
session copied verbatim, then appends the child session's own turns.

The first line of a forked transcript file is a `session_meta` entry that
identifies the parent:

```json
{
  "timestamp": "2026-06-23T11:14:34.861Z",
  "type": "session_meta",
  "payload": {
    "id": "019ef430-658c-7d00-9377-68b932474994",
    "forked_from_id": "019ef42d-6d82-75d1-95e9-4bb6200586ed",
    ...
  }
}
```

### Why the cursor baseline matters

Because the transcript file can already contain lines when the `SessionStart`
hook fires, `Cursor.Main` can be non-zero from the start. For forked sessions
those lines are copied parent context; for fresh sessions they can be Codex
startup records such as `session_meta`, `task_started`, and `turn_context`.
Without special handling, those lines would be silently skipped — they would not
appear in any delta, making it impossible to reconstruct the full transcript
from the journal.

### What twip stores

On `KindSessionStart`, twip reads the transcript file once and stores any new
lines since the prior cursor as `Event.Transcript`. For a brand-new session this
is lines 0→`Cursor.Main`; for a resumed session this is only the new
resume/startup lines appended after the last recorded cursor. This lands in the
journal under `meta/transcript.jsonl` on the session-start commit. Forked
sessions also carry `forked_from` (the parent session ID) in `meta/event.json`.

When no prior cursor is found, twip captures as much of the transcript as
possible, skipping only content that is clearly more than three days old. Lines
without parseable timestamps are always kept.

The result is that a forked session's journal contains its complete transcript:

| event | what is stored |
| --- | --- |
| session-start | preamble blob (parent context, lines 0→N) |
| stop (turn 1) | delta (lines N→M) |
| stop (turn 2) | delta (lines M→P) |

Concatenating these in order reconstructs the full transcript, including the
parent context that was in scope at fork time.

Non-forked sessions leave `ForkedFrom` empty, but still store any startup lines
that are present at `SessionStart`.

The parent session is recorded independently under its own events. The child
session's deltas start from the fork point; the parent's history is not
duplicated in the journal — it is stored once under the parent session's events,
and once as the child's session-start preamble.

## Transcript Capture

Codex transcripts are JSONL files under:

```text
$CODEX_HOME/sessions/YYYY/MM/DD/rollout-...-<session-or-agent-id>.jsonl
```

Hook payloads provide absolute paths. Do not derive paths when payload paths are
available.

Use line-count cursors, the same model as Claude:

- `Cursor.Main` tracks the main transcript line offset.
- `Cursor.Sidechain[agent_id]` tracks each Codex subagent transcript offset.
- `Delta.From` is the prior line offset.
- `Delta.To` is the current line count.
- `Delta.Bytes` contains the raw JSONL lines in `(From, To]`.

### Stop Flush

A live Codex `Stop` payload can arrive before the final `task_complete` JSONL
line is appended. In the spike, the transcript already contained the final
assistant message and token count at Stop time, but `task_complete` appeared
after the hook returned.

Real `task_complete` JSONL line from the live spike transcript:

```json
{"timestamp":"2026-06-22T15:25:49.356Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"019eeff0-0780-7782-9524-13676e1ee8bc","last_agent_message":"final-check","completed_at":1782141949,"duration_ms":3619,"time_to_first_token_ms":3346}}
```

The matching `Stop` hook payload carried the same `turn_id` at top level. In
the transcript line, it appears at `payload.turn_id`.

Before reading the final Stop delta:

1. If `transcript_path` is empty, return an empty delta with
   `QualityTranscriptUnavailable`.
2. Poll the transcript briefly for an `event_msg` line whose payload has
   `type == "task_complete"` and, when present, matching `turn_id`.
3. If no sentinel appears, accept file quiescence as a fallback.
4. If neither happens before timeout, read what exists and mark
   `QualityFlushTimeout`.

This is intentionally simpler than Claude's hook-progress sentinel, but the
quality flag rule is the same: uncertainty must be visible to `twip audit`.

## Subagent Sidechains

Codex subagent support must mirror Claude Code sidechains.

Claude stores subagent transcript deltas in `Event.Sidechains` on
`KindSubagentStop`. Codex should do the same:

- `Sidechain.ID` is Codex `agent_id`.
- `Sidechain.Delta` is read from `agent_transcript_path`.
- `Cursor.Sidechain[agent_id]` advances to the subagent transcript's line count.
- The main cursor remains unchanged for `KindSubagentStop`, except for normal
  clone/deep-copy behavior.

### SubagentStop Payload

Live Codex payload:

```json
{
  "agent_id": "019eefee-ffb4-7cf2-b97a-f4b6c08fda64",
  "agent_transcript_path": "/Users/me/.codex/sessions/2026/06/22/rollout-2026-06-22T16-24-38-019eefee-ffb4-7cf2-b97a-f4b6c08fda64.jsonl",
  "agent_type": "explorer",
  "cwd": "/repo",
  "hook_event_name": "SubagentStop",
  "last_assistant_message": "...",
  "model": "gpt-5.5",
  "permission_mode": "bypassPermissions",
  "session_id": "019eefee-d284-70f2-b918-40c106802e87",
  "stop_hook_active": false,
  "transcript_path": "/Users/me/.codex/sessions/2026/06/22/rollout-2026-06-22T16-24-26-019eefee-d284-70f2-b918-40c106802e87.jsonl",
  "turn_id": "019eefef-0015-7802-9448-cc437b0c7231"
}
```

Behavior:

- Return `KindSubagentStop`.
- Validate `agent_id` as path-safe even though Codex gives an absolute
  transcript path. The ID becomes a journal filename (`agent-<id>.jsonl`) in
  `internal/store`.
- Read the delta from `agent_transcript_path`, not from the parent
  `transcript_path`.
- If `agent_id` is empty, return the event with no sidechains.
- If `agent_transcript_path` is empty, return the event with a sidechain delta
  marked `QualityTranscriptUnavailable`.
- If the read is truncated, mark `QualityTruncated`.

Unlike Claude, no sidechain path guessing is needed. Codex gives both the parent
session transcript path and the subagent transcript path directly.

### SubagentStart

Live Codex emits `SubagentStart` with:

```json
{
  "agent_id": "019eefee-ffb4-7cf2-b97a-f4b6c08fda64",
  "agent_type": "explorer",
  "session_id": "019eefee-d284-70f2-b918-40c106802e87",
  "turn_id": "019eefef-0015-7802-9448-cc437b0c7231",
  "transcript_path": "/Users/me/.codex/sessions/2026/06/22/rollout-2026-06-22T16-24-38-019eefee-ffb4-7cf2-b97a-f4b6c08fda64.jsonl"
}
```

Do not install or record it. It is useful test fixture material, but
`SubagentStop` is the durable capture point.

## Implementation Shape

Expected files:

```text
internal/agent/codex/codex.go       hook parsing, event mapping, registration
internal/agent/codex/install.go     hooks.json and config.toml installation
internal/agent/codex/transcript.go  line counting and delta reads
internal/agent/codex/flush.go       Stop task_complete/quiescence wait
internal/agent/codex/validate.go    agent_id validation
internal/agent/codex/*_test.go      fixtures from live payloads
```

Register the package from `cmd/twip/main.go`:

```go
_ "github.com/codespeak-dev/twip/internal/agent/codex"
```

Update `twip init` messaging so it no longer assumes every agent installs into
`.claude/settings.json`.

Add a shared quality value in `internal/agent`:

```go
QualityTranscriptUnavailable Quality = "transcript_unavailable"
```

Use it only when Codex explicitly provides no usable transcript path, or when a
required Codex transcript path cannot be read at all. Continue to use
`QualityFlushTimeout` for flush uncertainty and `QualityTruncated` for partial
line reads.

## Tests

Required unit tests:

- `SessionID` returns `session_id` for every supported payload.
- `SessionStart` advances `Cursor.Main` to current transcript line count.
- `SessionStart` stores transcript bytes from the prior cursor to the current
  line count, including non-fork startup records and fork preambles.
- `SessionStart` stores only recent content (within three days) when no prior
  cursor is available; lines without parseable timestamps are always kept.
- `SessionStart` sets `ForkedFrom` only for forked sessions.
- `UserPromptSubmit` captures `prompt`.
- `PostToolUse` captures `Bash` detail and `apply_patch` detail without
  advancing cursors.
- `Stop` waits for `task_complete` when it appears shortly after hook start.
- `Stop` marks `QualityFlushTimeout` when the sentinel/quiescence wait fails.
- `Stop` marks `QualityTranscriptUnavailable` when `transcript_path` is absent.
- `SubagentStop` captures `Sidechains[0].ID == agent_id`.
- `SubagentStop` reads from `agent_transcript_path`.
- `SubagentStop` advances `Cursor.Sidechain[agent_id]`.
- `SubagentStop` marks `QualityTranscriptUnavailable` when
  `agent_transcript_path` is absent.
- Unsafe `agent_id` is rejected.
- Hook install creates `.codex/hooks.json` and `.codex/config.toml`.
- Hook install preserves foreign hook entries and unknown top-level keys.
- Hook uninstall removes only twip-owned commands.
- Legacy `codex_hooks = true` is replaced with `hooks = true`.
- Hook install does not install `SubagentStart` or `PreToolUse`.

Recommended integration-style test:

- Feed a sequence of captured payload fixtures through `recordHook` and verify
  the journal contains:
  - session start;
  - prompt submit;
  - tool use with changed snapshot;
  - subagent stop with sidechain bytes;
  - stop with main transcript bytes.

## Decisions

- Do not install `SubagentStart`. It is not a durable capture point, and
  installing it would increase Codex hook trust/review burden for no retention
  benefit.
- Use `Bash|apply_patch|Edit|Write` as the initial `PostToolUse` matcher.
  OpenAI Codex source verifies `Bash` as the shell hook name, `apply_patch` as
  the canonical edit hook name, and `Edit`/`Write` as matcher-only aliases for
  `apply_patch`. Other post-tool hook names exist for non-mutating or dynamic
  tools, but they are outside the default capture set.
- Add `agent.QualityTranscriptUnavailable` with wire value
  `transcript_unavailable` for Codex hooks that have no usable transcript path.

## Open Questions

- None.
