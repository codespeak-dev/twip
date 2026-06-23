package codex

// Codex writes the transcript asynchronously. When the Stop hook fires the final
// task_complete JSONL line may not be on disk yet. We detect "flushed" two ways:
//
//  1. task_complete sentinel (fast-path): scan the tail for an event_msg line
//     whose payload.type=="task_complete" and payload.turn_id matches (when
//     turn_id is known). This is the durable signal that the turn is finished.
//  2. Write-quiescence (primary): the file size stops growing for quietFor.
//
// On timeout we proceed anyway (QualityFlushTimeout) — the next turn's delta
// re-reads from this turn's offset, so a short read self-heals.

import (
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/codespeak-dev/twip/internal/agent"
)

const (
	flushMaxWait   = 3 * time.Second
	flushPoll      = 50 * time.Millisecond
	flushQuietFor  = 200 * time.Millisecond
	flushTailBytes = 4096
	flushStaleAge  = 2 * time.Minute
)

// waitForFlush blocks until the transcript at path looks fully written for the
// current turn, or a timeout elapses. turnID is matched against the
// task_complete sentinel; pass "" to accept any task_complete line.
func waitForFlush(path string, turnID string, hookStart time.Time) agent.Quality {
	info, err := os.Stat(path)
	if err != nil {
		return agent.QualityOK
	}
	if time.Since(info.ModTime()) > flushStaleAge {
		return agent.QualityStaleSkip
	}

	deadline := hookStart.Add(flushMaxWait)
	lastSize := info.Size()
	stableSince := time.Now()
	for time.Now().Before(deadline) {
		if checkTaskComplete(path, turnID) {
			return agent.QualityOK
		}
		time.Sleep(flushPoll)

		fi, err := os.Stat(path)
		if err != nil {
			return agent.QualityOK
		}
		if fi.Size() != lastSize {
			lastSize = fi.Size()
			stableSince = time.Now()
			continue
		}
		if time.Since(stableSince) >= flushQuietFor {
			return agent.QualityOK
		}
	}
	return agent.QualityFlushTimeout
}

// checkTaskComplete scans the tail of the file for a task_complete sentinel.
// It looks for an event_msg JSONL line whose payload.type=="task_complete" and,
// when turnID is non-empty, whose payload.turn_id matches.
func checkTaskComplete(path string, turnID string) bool {
	f, err := os.Open(path) //nolint:gosec // path comes from the agent hook payload
	if err != nil {
		return false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return false
	}
	offset := info.Size() - flushTailBytes
	if offset < 0 {
		offset = 0
	}
	buf := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(buf, offset); err != nil {
		return false
	}

	for _, line := range strings.Split(string(buf), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "task_complete") {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Payload struct {
				Type   string `json:"type"`
				TurnID string `json:"turn_id"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if entry.Type != "event_msg" || entry.Payload.Type != "task_complete" {
			continue
		}
		if turnID != "" && entry.Payload.TurnID != turnID {
			continue
		}
		return true
	}
	return false
}