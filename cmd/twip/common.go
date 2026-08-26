package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/codespeak-dev/twip/internal/gitutil"
)

// repoRoot resolves the worktree root of the current directory.
func repoRoot(ctx context.Context) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root, err := gitutil.WorktreeRoot(ctx, cwd)
	if err != nil {
		return "", fmt.Errorf("not inside a git repository: %w", err)
	}
	return root, nil
}

// banner frames a message in rules so it survives the place these messages
// actually appear: interleaved in a hook manager's parallel job output, in the
// wall of text a `git push` prints. A withheld mirror the user scrolls past is
// the same as no message at all.
func banner(msg string) string {
	const rule = "──────────────────────────────────────────────────────────────────────"
	var b strings.Builder
	b.WriteString(rule + "\n")
	for _, line := range strings.Split(strings.TrimRight(msg, "\n"), "\n") {
		b.WriteString("twip │ " + line + "\n")
	}
	b.WriteString(rule)
	return b.String()
}
