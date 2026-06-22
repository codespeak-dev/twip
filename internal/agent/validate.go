package agent

import (
	"fmt"
	"regexp"
)

// pathSafe matches identifiers safe to embed in a filename. Subagent ids from
// hook payloads become part of sidechain paths (agent-<id>.jsonl), so this is a
// path-injection guard, not cosmetic.
var pathSafe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ValidateAgentID returns an error if id is empty or contains characters that
// are not safe to embed in a file path component.
func ValidateAgentID(id string) error {
	if id == "" {
		return fmt.Errorf("empty agent id")
	}
	if !pathSafe.MatchString(id) {
		return fmt.Errorf("agent id %q is not path-safe", id)
	}
	return nil
}