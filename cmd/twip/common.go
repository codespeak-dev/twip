package main

import (
	"context"
	"fmt"
	"os"

	"github.com/codespeak/twip/internal/gitutil"
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
