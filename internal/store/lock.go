package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/codespeak/twip/internal/gitutil"
)

// lockKey takes an exclusive cross-process lock for a key (a session id), so a
// session's own hook processes are serialized and its journal back-scan sees the
// prior event. The lock lives under the git common dir (shared across the clone's
// linked worktrees). Distinct keys never contend; cross-key journal races are
// handled by CAS in Append, not here.
func lockKey(ctx context.Context, repoRoot, key string) (release func(), err error) {
	commonDir, err := gitutil.CommonDir(ctx, repoRoot)
	if err != nil {
		return nil, fmt.Errorf("locate git common dir: %w", err)
	}
	lockDir := filepath.Join(commonDir, "twip", "locks")
	if err := os.MkdirAll(lockDir, 0o750); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}
	lockPath := filepath.Join(lockDir, key+".lock")
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // key validated by keySafe
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
