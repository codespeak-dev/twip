package claudecode

import (
	"fmt"
	"regexp"
)

// pathSafe matches identifiers safe to embed in a filename. Subagent ids from the
// hook payload become part of the sidechain path (agent-<id>.jsonl), so this is a
// path-injection guard, not cosmetic.
var pathSafe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func validateAgentID(id string) error {
	if id == "" {
		return fmt.Errorf("empty agent id")
	}
	if !pathSafe.MatchString(id) {
		return fmt.Errorf("agent id %q is not path-safe", id)
	}
	return nil
}
