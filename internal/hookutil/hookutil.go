// Package hookutil provides shared infrastructure for twip's agent hook
// implementations: payload parsing, string formatting, JSON encoding, and
// command-string helpers. It has no dependency on the agent domain types.
package hookutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ParseStdin reads and unmarshals a hook payload from r into T.
// Returns zero value and nil error for an empty payload.
func ParseStdin[T any](r io.Reader) (T, error) {
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

// Truncate returns s shortened to at most n runes, appending "…" when truncated.
func Truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// HookCommand returns the shell guard that runs twip for a hook verb,
// silently no-op-ing when twip is not on PATH.
func HookCommand(agentName, verb string) string {
	return fmt.Sprintf(
		"sh -c 'command -v twip >/dev/null 2>&1 || exit 0; exec twip hook %s %s'",
		agentName, verb,
	)
}

// IsTwipHook reports whether cmd is a twip-owned hook command.
func IsTwipHook(cmd string) bool {
	return strings.Contains(cmd, "twip hook ")
}

// MarshalNoEscape encodes v as JSON without HTML escaping, with no trailing newline.
func MarshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// MarshalIndentNoEscape encodes v as indented JSON without HTML escaping.
func MarshalIndentNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}