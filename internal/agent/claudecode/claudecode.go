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
	"time"

	"github.com/codespeak/twip/internal/agent"
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
)

func (a *Agent) HookNames() []string {
	return []string{hookSessionStart, hookUserPrompt, hookStop, hookPostTask, hookSessionEnd}
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

func parseStdin[T any](r io.Reader) (T, error) {
	var v T
	data, err := io.ReadAll(r)
	if err != nil {
		return v, fmt.Errorf("read hook stdin: %w", err)
	}
	if len(data) == 0 {
		return v, nil
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return v, fmt.Errorf("parse hook payload: %w", err)
	}
	return v, nil
}

// ParseHookEvent translates a Claude Code hook firing into a normalized Event,
// reading transcript deltas (relative to `prior`) for the events that carry them.
func (a *Agent) ParseHookEvent(_ context.Context, hookName string, stdin io.Reader, prior agent.Cursor) (*agent.Event, error) {
	hookStart := time.Now()
	switch hookName {
	case hookSessionStart:
		raw, err := parseStdin[sessionInfoRaw](stdin)
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
		raw, err := parseStdin[userPromptRaw](stdin)
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

	default:
		return nil, nil // hook with no recording significance
	}
}

// parseTranscriptEvent handles Stop and SessionEnd: wait for the async flush,
// then read the main-transcript delta since the prior cursor.
func (a *Agent) parseTranscriptEvent(stdin io.Reader, prior agent.Cursor, hookStart time.Time, kind agent.Kind) (*agent.Event, error) {
	raw, err := parseStdin[sessionInfoRaw](stdin)
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
	raw, err := parseStdin[postTaskRaw](stdin)
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
	if err := validateAgentID(id); err != nil {
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
