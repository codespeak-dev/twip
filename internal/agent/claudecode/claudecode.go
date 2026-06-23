// Package claudecode implements twip's Agent interface for Claude Code: it parses
// Claude Code hook payloads, captures transcript deltas (with async-flush
// handling) and subagent sidechains, and installs/uninstalls the hooks in a
// repo's .claude/settings.json.
//
// Capture knowledge here is adapted from entire-cli's claudecode package
// (transcript flush sentinel, offset-delta reading, sidechain handling) but
// reduced to the leaf logic twip needs — twip does not implement Entire's
// TranscriptAnalyzer/TokenCalculator/etc. interface hierarchy.
package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/codespeak-dev/twip/internal/agent"
	"github.com/codespeak-dev/twip/internal/hookutil"
)

// Name is the registry key and hook namespace.
const Name = "claude-code"

func init() {
	agent.Register(Name, func() agent.Agent { return &Agent{} })
}

// Agent records Claude Code sessions.
type Agent struct{}

func (a *Agent) Name() string { return Name }

// Claude Code hook verbs. These are the values of <verb> in
// `twip hook claude-code <verb>` and the keys used when installing hooks.
const (
	hookSessionStart = "session-start"
	hookSessionEnd   = "session-end"
	hookStop         = "stop"
	hookUserPrompt   = "user-prompt-submit"
	hookPostTask     = "post-task"
	hookPostToolUse  = "post-tool-use" // intermediate mutating tool calls (Edit/Write/Bash/…)
)

func (a *Agent) HookNames() []string {
	return []string{hookSessionStart, hookUserPrompt, hookStop, hookPostTask, hookPostToolUse, hookSessionEnd}
}

// SessionID peeks the session_id field common to every Claude Code hook payload.
func (a *Agent) SessionID(payload []byte) (string, error) {
	if len(payload) == 0 {
		return "", nil
	}
	var v struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(payload, &v); err != nil {
		return "", fmt.Errorf("parse session id: %w", err)
	}
	return v.SessionID, nil
}

// --- hook payload shapes (JSON on stdin) ---

type sessionInfoRaw struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Model          string `json:"model,omitempty"`
}

type userPromptRaw struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Prompt         string `json:"prompt"`
}

type postTaskRaw struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	ToolResponse   struct {
		AgentID string `json:"agentId"`
	} `json:"tool_response"`
}

// postToolUseRaw is the PostToolUse payload for the mutating tools twip matches.
// tool_input is left as raw JSON since its shape varies per tool (file_path for
// Edit/Write/NotebookEdit, command for Bash); we extract a short label from it.
type postToolUseRaw struct {
	SessionID string          `json:"session_id"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}


// ParseHookEvent translates a Claude Code hook firing into a normalized Event,
// reading transcript deltas (relative to `prior`) for the events that carry them.
func (a *Agent) ParseHookEvent(_ context.Context, hookName string, stdin io.Reader, prior agent.Cursor) (*agent.Event, error) {
	hookStart := time.Now()
	switch hookName {
	case hookSessionStart:
		raw, err := hookutil.ParseStdin[sessionInfoRaw](stdin)
		if err != nil {
			return nil, err
		}
		// Baseline the cursor at the transcript's current length so a resumed
		// session does not re-capture history recorded under the prior session.
		cur := prior.Clone()
		if n, err := countLines(raw.TranscriptPath); err == nil {
			cur.Main = n
		}
		return &agent.Event{
			SessionID: raw.SessionID,
			Kind:      agent.KindSessionStart,
			Model:     raw.Model,
			Cursor:    cur,
		}, nil

	case hookUserPrompt:
		raw, err := hookutil.ParseStdin[userPromptRaw](stdin)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			SessionID: raw.SessionID,
			Kind:      agent.KindPromptSubmit,
			Prompt:    raw.Prompt,
			Cursor:    prior.Clone(),
		}, nil

	case hookStop:
		return a.parseTranscriptEvent(stdin, prior, hookStart, agent.KindStop)

	case hookSessionEnd:
		return a.parseTranscriptEvent(stdin, prior, hookStart, agent.KindSessionEnd)

	case hookPostTask:
		return a.parsePostTask(stdin, prior)

	case hookPostToolUse:
		return a.parsePostToolUse(stdin, prior)

	default:
		return nil, nil // hook with no recording significance
	}
}

// parsePostToolUse records an intermediate mutating tool call. It carries no
// transcript (the turn's transcript is captured whole at Stop) and an unchanged
// cursor; the core decides whether the call actually changed the worktree.
func (a *Agent) parsePostToolUse(stdin io.Reader, prior agent.Cursor) (*agent.Event, error) {
	raw, err := hookutil.ParseStdin[postToolUseRaw](stdin)
	if err != nil {
		return nil, err
	}
	if raw.ToolName == "" {
		return nil, nil
	}
	return &agent.Event{
		SessionID: raw.SessionID,
		Kind:      agent.KindToolUse,
		Tool:      &agent.ToolUse{Name: raw.ToolName, Detail: toolDetail(raw.ToolName, raw.ToolInput)},
		Cursor:    prior.Clone(),
	}, nil
}

// toolDetail extracts a short human label from a tool's input: the file path for
// file-editing tools, the (truncated) command for Bash. Best-effort — an
// unparseable input just yields an empty detail, never an error.
func toolDetail(tool string, input json.RawMessage) string {
	var v struct {
		FilePath    string `json:"file_path"`
		NotebookB   string `json:"notebook_path"`
		Command     string `json:"command"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(input, &v)
	switch {
	case v.FilePath != "":
		return v.FilePath
	case v.NotebookB != "":
		return v.NotebookB
	case v.Command != "":
		return hookutil.Truncate(strings.TrimSpace(v.Command), 120)
	case v.Description != "":
		return hookutil.Truncate(v.Description, 120)
	}
	return ""
}


// parseTranscriptEvent handles Stop and SessionEnd: wait for the async flush,
// then read the main-transcript delta since the prior cursor.
func (a *Agent) parseTranscriptEvent(stdin io.Reader, prior agent.Cursor, hookStart time.Time, kind agent.Kind) (*agent.Event, error) {
	raw, err := hookutil.ParseStdin[sessionInfoRaw](stdin)
	if err != nil {
		return nil, err
	}
	quality := waitForFlush(raw.TranscriptPath, hookStart)

	bytes, total, truncated, err := readDelta(raw.TranscriptPath, prior.Main)
	if err != nil {
		return nil, fmt.Errorf("read transcript delta: %w", err)
	}
	// A short read self-heals next turn — except SessionEnd, which has no next
	// turn, so a truncated read there is a genuine quality concern.
	if truncated && quality == agent.QualityOK {
		quality = agent.QualityTruncated
	}

	cur := prior.Clone()
	cur.Main = total
	return &agent.Event{
		SessionID: raw.SessionID,
		Kind:      kind,
		Model:     raw.Model,
		Transcript: agent.Delta{
			Bytes:   bytes,
			From:    prior.Main,
			To:      total,
			Quality: quality,
		},
		Cursor: cur,
	}, nil
}

// parsePostTask captures the finished subagent's sidechain delta. The subagent id
// comes straight from the payload; its sidechain file sits beside the main
// transcript at <dir>/subagents/agent-<id>.jsonl (older layouts:
// <dir>/agent-<id>.jsonl), so no path-discovery is needed — but the id is
// validated as path-safe before it becomes part of a filename.
func (a *Agent) parsePostTask(stdin io.Reader, prior agent.Cursor) (*agent.Event, error) {
	raw, err := hookutil.ParseStdin[postTaskRaw](stdin)
	if err != nil {
		return nil, err
	}
	ev := &agent.Event{
		SessionID: raw.SessionID,
		Kind:      agent.KindSubagentStop,
		Cursor:    prior.Clone(),
	}
	id := raw.ToolResponse.AgentID
	if id == "" {
		return ev, nil // Task that spawned no recorded subagent
	}
	if err := agent.ValidateAgentID(id); err != nil {
		return nil, err
	}
	path := sidechainPath(raw.TranscriptPath, id)
	from := prior.Sidechain[id]
	bytes, total, truncated, err := readDelta(path, from)
	if err != nil {
		return nil, fmt.Errorf("read sidechain delta: %w", err)
	}
	quality := agent.QualityOK
	if truncated {
		quality = agent.QualityTruncated
	}
	ev.Sidechains = []agent.Sidechain{{
		ID:    id,
		Delta: agent.Delta{Bytes: bytes, From: from, To: total, Quality: quality},
	}}
	if ev.Cursor.Sidechain == nil {
		ev.Cursor.Sidechain = map[string]int{}
	}
	ev.Cursor.Sidechain[id] = total
	return ev, nil
}

// sidechainPath returns the subagent transcript path. Claude Code (v2.1.x) stores
// these under <transcript-dir>/<session-id>/subagents/agent-<id>.jsonl; older
// layouts placed agent-<id>.jsonl directly beside the main transcript. We prefer
// the newer layout and fall back to the flat one.
func sidechainPath(transcriptPath, agentID string) string {
	dir := filepath.Dir(transcriptPath)
	sessionID := trimJSONLExt(filepath.Base(transcriptPath))
	nested := filepath.Join(dir, sessionID, "subagents", "agent-"+agentID+".jsonl")
	if fileExists(nested) {
		return nested
	}
	return filepath.Join(dir, "agent-"+agentID+".jsonl")
}
