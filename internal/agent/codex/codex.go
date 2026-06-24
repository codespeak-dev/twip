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
	"github.com/codespeak-dev/twip/internal/hookutil"
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

// ParseHookEvent translates a Codex hook firing into a normalized Event.
func (a *Agent) ParseHookEvent(_ context.Context, hookName string, stdin io.Reader, prior agent.Cursor) (*agent.Event, error) {
	hookStart := time.Now()
	switch hookName {
	case hookSessionStart:
		raw, err := hookutil.ParseStdin[commonRaw](stdin)
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
			// On read failure keep prior.Main so the next Stop doesn't re-read from 0.
			if data, err := os.ReadFile(*raw.TranscriptPath); err == nil { //nolint:gosec
				from := prior.Main
				forkedFrom := forkParent(data)
				// Forked sessions must capture from line 0 to preserve the full
				// fork preamble; apply the old-history heuristic only for non-forks.
				if from == 0 && forkedFrom == "" {
					from = recentTranscriptSuffixStartLine(data, hookStart)
				}
				preamble, total, truncated := agent.DeltaFrom(data, from)
				cur.Main = total
				quality := agent.QualityOK
				if truncated {
					quality = agent.QualityTruncated
				}
				if forkedFrom != "" {
					ev.ForkedFrom = forkedFrom
				}
				if len(preamble) > 0 || truncated {
					ev.Transcript = agent.Delta{Bytes: preamble, From: from, To: total, Quality: quality}
				}
			}
		}
		ev.Cursor = cur
		return ev, nil

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

// recentTranscriptSuffixStartLine returns the line offset where the current
// session's transcript suffix starts. It is a fallback for first-observed
// sessions when the journal has no prior cursor.
//
// If the first line is recent (within three days) or has no parseable
// timestamp, return 0 — capture everything. Otherwise the transcript begins
// with old history: build a line-start index once and binary-search for the
// first line that is recent or timestamp-ambiguous. Assumes timestamps are
// monotonically non-decreasing, which holds for Codex append-only logs.
func recentTranscriptSuffixStartLine(data []byte, now time.Time) int {
	if len(data) == 0 {
		return 0
	}
	cutoff := now.Add(-3 * 24 * time.Hour)

	// Fast path: first line is recent or has no timestamp — capture all.
	firstEnd := bytes.IndexByte(data, '\n')
	firstLine := data
	if firstEnd >= 0 {
		firstLine = data[:firstEnd]
	}
	if ts, ok := transcriptLineTimestamp(firstLine); !ok || !ts.Before(cutoff) {
		return 0
	}

	// First line is old. Build a byte-offset index of line starts once, then
	// binary-search — each lookup is O(1) instead of re-scanning from the start.
	starts := []int{0}
	for i, b := range data {
		if b == '\n' && i+1 < len(data) {
			starts = append(starts, i+1)
		}
	}
	lineAt := func(n int) []byte {
		if n >= len(starts) {
			return nil
		}
		s := starts[n]
		if n+1 < len(starts) {
			return data[s : starts[n+1]-1] // exclude the newline
		}
		return data[s:]
	}

	lo, hi := 1, len(starts)
	for lo < hi {
		mid := (lo + hi) / 2
		ts, ok := transcriptLineTimestamp(lineAt(mid))
		if !ok || !ts.Before(cutoff) {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

func transcriptLineTimestamp(line []byte) (time.Time, bool) {
	var entry struct {
		Timestamp string `json:"timestamp"`
		Payload   struct {
			Timestamp string `json:"timestamp"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &entry); err != nil {
		return time.Time{}, false
	}
	raw := entry.Timestamp
	if raw == "" {
		raw = entry.Payload.Timestamp
	}
	if raw == "" {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// parseStop handles the Stop hook: wait for task_complete flush then read delta.
func (a *Agent) parseStop(stdin io.Reader, prior agent.Cursor, hookStart time.Time) (*agent.Event, error) {
	raw, err := hookutil.ParseStdin[commonRaw](stdin)
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

// toolDetail extracts a short human label from a Codex tool's input.
func toolDetail(tool string, input json.RawMessage) string {
	var v struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &v)
	switch tool {
	case "Bash":
		return hookutil.Truncate(strings.TrimSpace(v.Command), 120)
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
		for _, prefix := range []string{"*** Add File: ", "*** Update File: ", "*** Delete File: "} {
			if strings.HasPrefix(line, prefix) {
				return strings.TrimSpace(strings.TrimPrefix(line, prefix))
			}
		}
	}
	return ""
}

// parseSubagentStop captures the finished subagent's sidechain delta.
// The subagent transcript path is provided directly in the payload.
func (a *Agent) parseSubagentStop(stdin io.Reader, prior agent.Cursor) (*agent.Event, error) {
	raw, err := hookutil.ParseStdin[subagentStopRaw](stdin)
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
