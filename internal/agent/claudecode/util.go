package claudecode

import (
	"os"
	"strings"
)

func trimJSONLExt(name string) string {
	return strings.TrimSuffix(name, ".jsonl")
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
