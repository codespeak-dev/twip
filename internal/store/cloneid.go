package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codespeak/twip/internal/gitutil"
)

// CloneID returns this clone's stable journal id, generating it on first use.
//
// Each clone gets its own journal ref (refs/twip/journal/<clone-id>) so that
// different machines never write the same ref — there is nothing to merge on
// sync. The id lives under the git common dir (shared across the clone's linked
// worktrees, never part of any tree, so it is not synced as content).
func (r *Recorder) CloneID(ctx context.Context) (string, error) {
	commonDir, err := gitutil.CommonDir(ctx, r.RepoRoot)
	if err != nil {
		return "", fmt.Errorf("locate git common dir: %w", err)
	}
	dir := filepath.Join(commonDir, "twip")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create twip dir: %w", err)
	}
	path := filepath.Join(dir, "clone-id")

	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
	}

	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate clone id: %w", err)
	}
	id := hex.EncodeToString(buf)

	// O_EXCL so a concurrent first-use can't produce two ids; loser re-reads.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return "", rerr
			}
			return strings.TrimSpace(string(b)), nil
		}
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(id); err != nil {
		return "", err
	}
	return id, nil
}
