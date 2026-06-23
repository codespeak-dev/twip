// Package agent defines the agent-extension seam for twip: a lean interface that
// every supported coding agent implements, plus a registry. Everything
// agent-specific (hook config format, hook-payload schema, transcript discovery,
// flush semantics, sidechains) lives behind this interface; the rest of twip
// (snapshot, store, audit, readmodel, web) consumes the normalized Event and is
// agent-agnostic.
//
// v1 ships only Claude Code (internal/agent/claudecode), but the seam exists so a
// second agent is "implement Agent + Register()", not a refactor.
package agent

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
)

// Kind is the normalized lifecycle event kind, independent of any agent's hook names.
type Kind string

const (
	KindSessionStart Kind = "session-start"
	KindPromptSubmit Kind = "user-prompt-submit"
	KindStop         Kind = "stop"
	KindSessionEnd   Kind = "session-end"
	KindSubagentStop Kind = "post-task"
	KindToolUse      Kind = "tool-use" // an intermediate (mid-turn) mutating tool call
)

// Quality records how trustworthy a transcript delta is. Anything other than
// QualityOK is a data-quality flag surfaced by the audit — never silent loss.
type Quality string

const (
	QualityOK           Quality = "ok"
	QualityFlushTimeout          Quality = "flush_timeout"          // sentinel/quiescence not reached before timeout
	QualityStaleSkip             Quality = "stale_skip"             // transcript untouched for a while; agent likely gone
	QualityTruncated             Quality = "truncated"              // read ended early; next turn self-heals (not SessionEnd)
	QualityTranscriptUnavailable Quality = "transcript_unavailable" // hook payload carried no usable transcript path
)

// Delta is a slice of transcript bytes captured for one event: the raw lines in
// (From, To] line-offset range. From/To are line counts, so turn N+1's From ==
// turn N's To, which is how the audit checks contiguity and how truncation
// self-heals across turns.
type Delta struct {
	Bytes   []byte  `json:"-"`
	From    int     `json:"from"`
	To      int     `json:"to"`
	Quality Quality `json:"quality"`
}

// Sidechain is a captured subagent transcript delta, keyed by the agent id that
// Claude Code reports in the post-task hook payload.
type Sidechain struct {
	ID    string `json:"id"`
	Delta Delta  `json:"delta"`
}

// Cursor is the per-session read position carried forward in the event log: each
// event records the cursor it advanced to, and the next capture reads it back as
// its baseline. This keeps offsets in the append-only log rather than a side file.
type Cursor struct {
	Main      int            `json:"main"`
	Sidechain map[string]int `json:"sidechain,omitempty"`
}

// Clone returns a deep copy so callers can advance a cursor without mutating the
// prior one loaded from the log.
func (c Cursor) Clone() Cursor {
	out := Cursor{Main: c.Main}
	if len(c.Sidechain) > 0 {
		out.Sidechain = make(map[string]int, len(c.Sidechain))
		for k, v := range c.Sidechain {
			out.Sidechain[k] = v
		}
	}
	return out
}

// ToolUse describes an intermediate mutating tool call (Edit/Write/Bash/…),
// captured so the timeline shows mid-turn worktree changes, not just turn
// boundaries. Populated on KindToolUse events.
type ToolUse struct {
	Name   string // the agent's tool name, e.g. "Edit", "Write", "Bash"
	Detail string // a short human label: the target file path, or a command summary
}

// Event is the normalized record the core records for one hook firing. The agent
// fills the agent-specific parts (kind, prompt, transcript bytes, cursor); the
// core stamps timestamp/HEAD and takes the worktree snapshot.
type Event struct {
	SessionID  string
	Kind       Kind
	Prompt     string // PromptSubmit only
	Model      string
	ForkedFrom string      // parent session ID when this session was forked (Codex only)
	Transcript Delta       // populated on Stop/SessionEnd; also on SessionStart for fork preamble
	Sidechains []Sidechain // populated on SubagentStop
	Tool       *ToolUse    // populated on KindToolUse
	Cursor     Cursor      // advanced cursor after this event
}

// Agent is the lean interface a coding agent implements to be recorded by twip.
type Agent interface {
	// Name is the registry key and hook namespace (e.g. "claude-code").
	Name() string
	// HookNames are the agent's own hook verbs, used to install hooks and to
	// route `twip hook <agent> <verb>` invocations.
	HookNames() []string
	// SessionID extracts the session identifier from a raw hook payload. The core
	// uses it to acquire the per-session lock before the full ParseHookEvent.
	SessionID(payload []byte) (string, error)
	// ParseHookEvent reads the hook payload from stdin, and for events that carry
	// transcript data reads the delta since `prior` (waiting for flush as needed),
	// returning the normalized event with Delta(s) and advanced Cursor populated.
	// Returns (nil, nil) for hooks with no recording significance.
	ParseHookEvent(ctx context.Context, hookName string, stdin io.Reader, prior Cursor) (*Event, error)
	// InstallHooks installs this agent's hooks into the repo, merge-preserving any
	// hooks twip does not own. Returns the number of hooks added.
	InstallHooks(ctx context.Context, repoRoot string, force bool) (int, error)
	// UninstallHooks removes only twip-owned hooks from the repo's agent config.
	UninstallHooks(ctx context.Context, repoRoot string) error
}

var (
	mu       sync.RWMutex
	registry = map[string]func() Agent{}
)

// Register adds an agent factory under its name. Called from agent packages' init().
func Register(name string, factory func() Agent) {
	mu.Lock()
	defer mu.Unlock()
	registry[name] = factory
}

// Get returns a fresh instance of the named agent.
func Get(name string) (Agent, error) {
	mu.RLock()
	defer mu.RUnlock()
	factory, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown agent %q", name)
	}
	return factory(), nil
}

// List returns the registered agent names, sorted.
func List() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
