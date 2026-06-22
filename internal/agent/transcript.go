package agent

import (
	"bufio"
	"errors"
	"io"
	"os"
)

// CountLines returns the number of JSONL lines in a transcript file. A trailing
// segment without a newline counts as a line (an agent may not have flushed the
// final newline yet). Returns 0 if the file is absent or empty. Uses
// bufio.ReadBytes so arbitrarily long lines (e.g. base64 images) are handled.
func CountLines(path string) (int, error) {
	if path == "" {
		return 0, nil
	}
	f, err := os.Open(path) //nolint:gosec // path comes from the agent hook payload
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	r := bufio.NewReader(f)
	n := 0
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				if len(line) > 0 {
					n++ // final line without trailing newline
				}
				return n, nil
			}
			return 0, err
		}
		n++
	}
}

// DeltaFrom returns the raw bytes of all lines after the first `fromLine` lines,
// together with the new total line count. Line offsets are counts (not byte
// positions), so reading "from N" skips the first N lines and returns lines
// (N, total]. Exact bytes are preserved (including the trailing partial line, if
// any) so the stored delta is a faithful slice of the transcript. truncated
// reports whether fromLine pointed past the data we found (a stale/short read);
// callers may flag it, knowing the next turn's delta self-heals.
func DeltaFrom(data []byte, fromLine int) (delta []byte, total int, truncated bool) {
	if fromLine < 0 {
		fromLine = 0
	}
	start := len(data)
	if fromLine == 0 {
		start = 0
	}
	newlines := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			newlines++
			if newlines == fromLine {
				start = i + 1
			}
		}
	}
	total = newlines
	if len(data) > 0 && data[len(data)-1] != '\n' {
		total++
	}
	// Compare against total (not newlines) so a cursor sitting exactly at the
	// end of a file without a trailing newline is not falsely flagged as stale.
	if fromLine > total {
		truncated = true
		start = len(data)
	}
	if start > len(data) {
		start = len(data)
	}
	return data[start:], total, truncated
}

// ReadDelta reads the transcript file and returns the delta after `fromLine`.
func ReadDelta(path string, fromLine int) (delta []byte, total int, truncated bool, err error) {
	if path == "" {
		return nil, 0, false, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // path comes from the agent hook payload
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, false, nil
		}
		return nil, 0, false, err
	}
	d, total, truncated := DeltaFrom(data, fromLine)
	return d, total, truncated, nil
}