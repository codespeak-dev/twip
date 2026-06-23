// Package codex implements twip's Agent interface for OpenAI Codex: it parses
// Codex hook payloads, captures transcript deltas (with task_complete-sentinel
// flush handling) and subagent sidechains, and installs/uninstalls the hooks in
// a repo's .codex/hooks.json.
package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/codespeak-dev/twip/internal/agent"
)

// Name is the registry key and hook namespace.
const Name = "codex"

func init() {
	agent.Register(Name, func() agent.Agent { return &Agent{} })
}

// Agent records Codex sessions.
type Agent struct{}

func (a *Agent) Name() string { return Name }

// Codex hook verbs — the values of <verb> in `twip hook codex <verb>`.
const (
	hookSessionStart = "session-start"
	hookUserPrompt   = "user-prompt-submit"
	hookStop         = "stop"
	hookPostToolUse  = "post-tool-use"
	hookSubagentStop = "subagent-stop"
)

func (a *Agent) HookNames() []string {
	return []string{hookSessionStart, hookUserPrompt, hookStop, hookPostToolUse, hookSubagentStop}
}

// SessionID peeks the session_id field common to every Codex hook payload.
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

// commonRaw holds fields present in every Codex hook payload.
// TranscriptPath is a pointer because Codex sets it to null for ephemeral sessions.
type commonRaw struct {
	SessionID      string  `json:"session_id"`
	TurnID         string  `json:"turn_id,omitempty"`
	TranscriptPath *string `json:"transcript_path"`
	Model          string  `json:"model"`
}

type userPromptRaw struct {
	commonRaw
	Prompt string `json:"prompt"`
}

type postToolUseRaw struct {
	commonRaw
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}

type subagentStopRaw struct {
	commonRaw
	AgentID             string  `json:"agent_id"`
	AgentTranscriptPath *string `json:"agent_transcript_path"`
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

// ParseHookEvent translates a Codex hook firing into a normalized Event.
func (a *Agent) ParseHookEvent(_ context.Context, hookName string, stdin io.Reader, prior agent.Cursor) (*agent.Event, error) {
	hookStart := time.Now()
	switch hookName {
	case hookSessionStart:
		raw, err := parseStdin[commonRaw](stdin)
		if err != nil {
			return nil, err
		}
		cur := prior.Clone()
		ev := &agent.Event{
			SessionID: raw.SessionID,
			Kind:      agent.KindSessionStart,
			Model:     raw.Model,
		}
		if raw.TranscriptPath != nil {
			// Single read: derive line count, preamble bytes, and fork parent ID
			// together to avoid a race between separate CountLines and ReadDelta calls.
			data, _ := os.ReadFile(*raw.TranscriptPath) //nolint:gosec
			preamble, total, _ := agent.DeltaFrom(data, 0)
			cur.Main = total
			if forkedFrom := forkParent(data); forkedFrom != "" {
				ev.ForkedFrom = forkedFrom
				ev.Transcript = agent.Delta{Bytes: preamble, From: 0, To: total, Quality: agent.QualityOK}
			}
		}
		ev.Cursor = cur
		return ev, nil

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
		return a.parseStop(stdin, prior, hookStart)

	case hookPostToolUse:
		return a.parsePostToolUse(stdin, prior)

	case hookSubagentStop:
		return a.parseSubagentStop(stdin, prior)

	default:
		return nil, nil
	}
}

// forkParent reads the first session_meta line from transcript bytes and returns
// forked_from_id, or "" for a fresh (non-forked) session. Codex forks copy the
// parent's transcript into the child file as a preamble; the first line
// identifies whether this is a fork and names the parent session.
func forkParent(data []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(data))
	if !sc.Scan() {
		return ""
	}
	var entry struct {
		Type    string `json:"type"`
		Payload struct {
			ForkedFromID string `json:"forked_from_id"`
		} `json:"payload"`
	}
	if json.Unmarshal(sc.Bytes(), &entry) != nil || entry.Type != "session_meta" {
		return ""
	}
	return entry.Payload.ForkedFromID
}

// parseStop handles the Stop hook: wait for task_complete flush then read delta.
func (a *Agent) parseStop(stdin io.Reader, prior agent.Cursor, hookStart time.Time) (*agent.Event, error) {
	raw, err := parseStdin[commonRaw](stdin)
	if err != nil {
		return nil, err
	}

	cur := prior.Clone()
	ev := &agent.Event{
		SessionID: raw.SessionID,
		Kind:      agent.KindStop,
		Model:     raw.Model,
		Cursor:    cur,
	}

	if raw.TranscriptPath == nil {
		ev.Transcript = agent.Delta{Quality: agent.QualityTranscriptUnavailable}
		return ev, nil
	}

	path := *raw.TranscriptPath
	quality := waitForFlush(path, raw.TurnID, hookStart)

	deltaBytes, total, truncated, err := agent.ReadDelta(path, prior.Main)
	if err != nil {
		return nil, fmt.Errorf("read transcript delta: %w", err)
	}
	if truncated && quality == agent.QualityOK {
		quality = agent.QualityTruncated
	}

	cur.Main = total
	ev.Transcript = agent.Delta{
		Bytes:   deltaBytes,
		From:    prior.Main,
		To:      total,
		Quality: quality,
	}
	ev.Cursor = cur
	return ev, nil
}

// parsePostToolUse records an intermediate mutating tool call.
func (a *Agent) parsePostToolUse(stdin io.Reader, prior agent.Cursor) (*agent.Event, error) {
	raw, err := parseStdin[postToolUseRaw](stdin)
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

// toolDetail extracts a short human label from a Codex tool's input.
func toolDetail(tool string, input json.RawMessage) string {
	var v struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &v)
	switch tool {
	case "Bash":
		return truncate(strings.TrimSpace(v.Command), 120)
	case "apply_patch":
		return patchPathSummary(v.Command)
	}
	return ""
}

// patchPathSummary extracts a compact file-path summary from an apply_patch
// command string. It scans for "*** Add/Update/Delete File:" markers and
// returns the first path found, or "" if none.
func patchPathSummary(cmd string) string {
	for _, line := range strings.Split(cmd, "\n") {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{"*** Add File: ", "*** Update File: ", "*** Delete File: ", "+++ "} {
			if strings.HasPrefix(line, prefix) {
				return strings.TrimSpace(strings.TrimPrefix(line, prefix))
			}
		}
	}
	return ""
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// parseSubagentStop captures the finished subagent's sidechain delta.
// The subagent transcript path is provided directly in the payload.
func (a *Agent) parseSubagentStop(stdin io.Reader, prior agent.Cursor) (*agent.Event, error) {
	raw, err := parseStdin[subagentStopRaw](stdin)
	if err != nil {
		return nil, err
	}

	ev := &agent.Event{
		SessionID: raw.SessionID,
		Kind:      agent.KindSubagentStop,
		Cursor:    prior.Clone(),
	}

	id := raw.AgentID
	if id == "" {
		return ev, nil
	}
	if err := agent.ValidateAgentID(id); err != nil {
		return nil, err
	}

	from := prior.Sidechain[id]

	if raw.AgentTranscriptPath == nil {
		ev.Sidechains = []agent.Sidechain{{
			ID:    id,
			Delta: agent.Delta{From: from, To: from, Quality: agent.QualityTranscriptUnavailable},
		}}
		return ev, nil
	}

	deltaBytes, total, truncated, err := agent.ReadDelta(*raw.AgentTranscriptPath, from)
	if err != nil {
		return nil, fmt.Errorf("read sidechain delta: %w", err)
	}
	quality := agent.QualityOK
	if truncated {
		quality = agent.QualityTruncated
	}

	ev.Sidechains = []agent.Sidechain{{
		ID:    id,
		Delta: agent.Delta{Bytes: deltaBytes, From: from, To: total, Quality: quality},
	}}
	if ev.Cursor.Sidechain == nil {
		ev.Cursor.Sidechain = map[string]int{}
	}
	ev.Cursor.Sidechain[id] = total
	return ev, nil
}