package claudecode

import "github.com/codespeak-dev/twip/internal/agent"

// countLines, deltaFrom, and readDelta delegate to the shared agent package
// utilities so both claudecode and codex use a single implementation.

func countLines(path string) (int, error)                            { return agent.CountLines(path) }
func readDelta(path string, fromLine int) ([]byte, int, bool, error) { return agent.ReadDelta(path, fromLine) }
