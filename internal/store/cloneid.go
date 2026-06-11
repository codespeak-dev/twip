package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codespeak-dev/twip/internal/gitutil"
)

// CloneID returns this clone's stable journal id, generating it on first use.
//
// Each clone gets its own journal ref (refs/twip/journal/<clone-id>) so that
// different machines never write the same ref — there is nothing to merge on
// sync. The id lives under the git common dir (shared across the clone's linked
// worktrees, never part of any tree, so it is not synced as content).
func (r *Recorder) cloneIDPath(ctx context.Context) (string, error) {
	commonDir, err := gitutil.CommonDir(ctx, r.RepoRoot)
	if err != nil {
		return "", fmt.Errorf("locate git common dir: %w", err)
	}
	return filepath.Join(commonDir, "twip", "clone-id"), nil
}

// Enabled reports whether twip recording is active in this repo, i.e. the
// clone-id marker exists (created by `twip init`). The git shim gates on this so
// it stays a no-op in repos the user has not opted into.
func (r *Recorder) Enabled(ctx context.Context) (bool, error) {
	path, err := r.cloneIDPath(ctx)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (r *Recorder) CloneID(ctx context.Context) (string, error) {
	path, err := r.cloneIDPath(ctx)
	if err != nil {
		return "", err
	}
	// Fast path: an existing, fully-written id needs no lock.
	if id := readCloneID(path); id != "" {
		return id, nil
	}

	// Slow path: serialize creation so concurrent first-users converge on ONE id
	// (a bare O_EXCL race let a reader observe the freshly-created-but-empty file
	// and return ""). The lock also covers cross-process first-use.
	release, err := r.Lock(ctx, "clone-id")
	if err != nil {
		return "", err
	}
	defer release()

	if id := readCloneID(path); id != "" {
		return id, nil // another writer created it while we waited
	}

	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate clone id: %w", err)
	}
	id := hex.EncodeToString(buf)

	// Write atomically (temp + rename) so even unlocked fast-path readers in other
	// processes never see a half-written file.
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", fmt.Errorf("create twip dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "clone-id-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(id); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", err
	}
	return id, nil
}

func readCloneID(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
