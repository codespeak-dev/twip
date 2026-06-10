package claudecode

import (
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/codespeak/twip/internal/agent"
)

// Claude Code writes the transcript asynchronously, so when the Stop hook fires
// the last lines of the turn may not be on disk yet. We detect "flushed" two ways:
//
//  1. Write-quiescence (primary): the file size stops growing for quietFor. This
//     is version-independent and is what twip relies on.
//  2. Stop sentinel (fast-path): Claude appends a hook_progress entry naming the
//     stop hook command. Entire matches this string, but empirically on Claude
//     Code v2.1.x the hook_progress entries observed carry command:"callback"
//     rather than the literal command, so the sentinel often never appears. We
//     still check it (cheap, and a positive flush signal when present) but do not
//     depend on it.
//
// On timeout we proceed anyway (QualityFlushTimeout) — the next turn's delta
// re-reads from this turn's offset, so a short read self-heals. The exception is
// SessionEnd, which has no next turn; the audit surfaces its quality flag.
const stopSentinelSubstr = "hook claude-code stop"

const (
	flushMaxWait   = 3 * time.Second
	flushPoll      = 50 * time.Millisecond
	flushQuietFor  = 200 * time.Millisecond
	flushTailBytes = 4096
	flushMaxSkew   = 2 * time.Second
	flushStaleAge  = 2 * time.Minute
)

// waitForFlush blocks until the transcript at path looks fully written for the
// current turn, or a timeout elapses. It returns a Quality describing the outcome.
func waitForFlush(path string, hookStart time.Time) agent.Quality {
	info, err := os.Stat(path)
	if err != nil {
		// Missing (or unreadable): nothing to wait for. countLines/readDelta
		// handle absence; treat as ok rather than inventing a flag.
		return agent.QualityOK
	}
	if time.Since(info.ModTime()) > flushStaleAge {
		// Untouched for a while → agent likely gone; don't burn the full timeout.
		return agent.QualityStaleSkip
	}

	deadline := time.Now().Add(flushMaxWait)
	lastSize := info.Size()
	stableSince := time.Now()
	for time.Now().Before(deadline) {
		if checkStopSentinel(path, hookStart) {
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
		// Size unchanged; once it has been stable for quietFor, consider it flushed.
		if time.Since(stableSince) >= flushQuietFor {
			return agent.QualityOK
		}
	}
	return agent.QualityFlushTimeout
}

// checkStopSentinel scans the tail of the file for the stop-hook sentinel,
// requiring the matching JSON entry's timestamp to fall within ±maxSkew of the
// hook start. The skew check rejects stale sentinels from previous turns and
// false positives (e.g. a transcript that merely quotes the sentinel string in a
// tool result).
func checkStopSentinel(path string, hookStart time.Time) bool {
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
		if line == "" || !strings.Contains(line, stopSentinelSubstr) {
			continue
		}
		var entry struct {
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil || entry.Timestamp == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
		if err != nil {
			if ts, err = time.Parse(time.RFC3339, entry.Timestamp); err != nil {
				continue
			}
		}
		if ts.After(hookStart.Add(-flushMaxSkew)) && ts.Before(hookStart.Add(flushMaxSkew)) {
			return true
		}
	}
	return false
}
