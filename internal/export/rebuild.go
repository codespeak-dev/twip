// Package export rebuilds whole agent transcripts from the journal's per-event
// line-range deltas, so sessions recorded by twip can be handed to another
// store byte-for-byte. Rebuilding is strict: every way the deltas fail to cover
// the transcript is reported on it instead of being papered over.
package export

import (
	"bytes"
	"fmt"
	"os"
)

// Issue kinds a rebuilt transcript can carry. Any issue means the rebuilt file
// is not a faithful copy of what the agent wrote.
const (
	IssueHeadMissing = "head-missing" // no delta ever covered the first lines
	IssueHole        = "hole"         // no delta ever covered lines further in
	IssueConflict    = "conflict"     // two deltas carry different bytes for one line
	IssueCount       = "count"        // a delta's line count disagrees with its range
	IssueCutLine     = "cut-line"     // a line was only ever recorded unfinished
)

// Issue is one place where the deltas did not cover the transcript cleanly.
type Issue struct {
	Kind   string `json:"kind"`
	Line   int    `json:"line"`
	Detail string `json:"detail,omitempty"`
}

type lineRef struct {
	offset int64
	size   int32
	set    bool
}

// Transcript collects one transcript from deltas applied in journal order.
// Line numbers are the agent's own, so a delta can fill lines an earlier one
// skipped: twip re-sends a whole transcript from line 0 whenever it loses a
// session's cursor. Line bytes are spooled to Path+".lines" and the rebuilt file
// is written to Path by Finish.
type Transcript struct {
	Path      string
	Issues    []Issue
	Qualities map[string]int
	Redacted  int

	lines []lineRef
	spool int64
	size  int64
}

// NewTranscript starts an empty transcript that Finish will write to path.
func NewTranscript(path string) *Transcript {
	return &Transcript{Path: path, Qualities: map[string]int{}}
}

// Lines reports how many agent lines the transcript spans, gaps included.
func (t *Transcript) Lines() int { return len(t.lines) }

// Size reports the rebuilt file's size in bytes; it is known after Finish.
func (t *Transcript) Size() int64 { return t.size }

// Faithful reports whether the rebuilt file is an exact copy of the agent's
// transcript up to the last line seen.
func (t *Transcript) Faithful() bool { return len(t.Issues) == 0 }

func (t *Transcript) issue(kind string, line int, detail string) {
	t.Issues = append(t.Issues, Issue{Kind: kind, Line: line, Detail: detail})
}

func (t *Transcript) spoolPath() string { return t.Path + ".lines" }

// splitLines splits data into lines, each keeping its trailing newline; a final
// segment without one is its own line.
func splitLines(data []byte) [][]byte {
	var out [][]byte
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			out = append(out, data)
			break
		}
		out = append(out, data[:i+1])
		data = data[i+1:]
	}
	return out
}

func unfinished(line []byte) bool { return len(line) > 0 && line[len(line)-1] != '\n' }

// Apply records the delta covering agent lines (from, to] with the given bytes.
func (t *Transcript) Apply(from, to int, data []byte, quality string) error {
	if quality != "" {
		t.Qualities[quality]++
	}
	lines := splitLines(data)
	if len(lines) != to-from {
		t.issue(IssueCount, from, fmt.Sprintf("range %d..%d carries %d lines", from, to, len(lines)))
	}
	if len(lines) == 0 {
		return nil
	}
	if need := from + len(lines); need > len(t.lines) {
		t.lines = append(t.lines, make([]lineRef, need-len(t.lines))...)
	}
	f, err := os.OpenFile(t.spoolPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for k, line := range lines {
		n := from + k
		ref := t.lines[n]
		if ref.set {
			kept := make([]byte, ref.size)
			if _, err := f.ReadAt(kept, ref.offset); err != nil {
				return err
			}
			switch {
			case bytes.Equal(kept, line):
				continue
			case unfinished(kept) && bytes.HasPrefix(line, kept):
			case unfinished(line) && bytes.HasPrefix(kept, line):
				continue
			default:
				t.issue(IssueConflict, n, "line re-sent with different bytes")
				continue
			}
		}
		if _, err := f.WriteAt(line, t.spool); err != nil {
			return err
		}
		t.lines[n] = lineRef{offset: t.spool, size: int32(len(line)), set: true}
		t.spool += int64(len(line))
	}
	return f.Close()
}

// Finish writes the rebuilt transcript to Path, reports the lines no delta
// covered, and removes the spool.
func (t *Transcript) Finish() error {
	if len(t.lines) == 0 {
		return nil
	}
	in, err := os.Open(t.spoolPath())
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(t.Path)
	if err != nil {
		return err
	}
	defer out.Close()
	gapStart := -1
	closeGap := func(end int) {
		if gapStart < 0 {
			return
		}
		kind := IssueHole
		if gapStart == 0 {
			kind = IssueHeadMissing
		}
		t.issue(kind, gapStart, fmt.Sprintf("lines %d..%d never recorded", gapStart, end))
		gapStart = -1
	}
	last := len(t.lines) - 1
	for n, ref := range t.lines {
		if !ref.set {
			if gapStart < 0 {
				gapStart = n
			}
			continue
		}
		closeGap(n)
		line := make([]byte, ref.size)
		if _, err := in.ReadAt(line, ref.offset); err != nil {
			return err
		}
		if bytes.Contains(line, []byte(RedactPlaceholder)) {
			t.Redacted++
		}
		if unfinished(line) && n != last {
			t.issue(IssueCutLine, n, "line was only recorded before the agent finished writing it")
			line = append(line, '\n')
		}
		if _, err := out.Write(line); err != nil {
			return err
		}
		t.size += int64(len(line))
	}
	closeGap(len(t.lines))
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(t.spoolPath())
}

// RedactPlaceholder is what `twip redact` writes in place of a secret.
const RedactPlaceholder = "[twip-redacted]"
