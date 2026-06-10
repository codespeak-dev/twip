package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/codespeak/twip/internal/gitutil"
)

// lockSession takes an exclusive cross-process lock for a session, serializing
// concurrent hook processes that target the same session ref. The lock lives
// under the git common dir (shared across linked worktrees), keyed by session id.
// Different sessions use different lock files and never contend.
func lockSession(ctx context.Context, repoRoot, sessionID string) (release func(), err error) {
	commonDir, err := gitutil.CommonDir(ctx, repoRoot)
	if err != nil {
		return nil, fmt.Errorf("locate git common dir: %w", err)
	}
	lockDir := filepath.Join(commonDir, "twip-locks")
	if err := os.MkdirAll(lockDir, 0o750); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}
	lockPath := filepath.Join(lockDir, sessionID+".lock")
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // path built from validated session id
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
