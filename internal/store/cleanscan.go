package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/codespeak-dev/twip/internal/gitutil"
)

// CleanScan is what a secrets scan has already proven clean on this clone, so a
// repeated `twip redact` re-scans only what is new.
//
// Redaction scopes its scan to the commits the sync remote lacks, which is the
// right bound for the pre-push case — but a journal the remote has never seen
// (the common one: `twip sync` is opt-in) has nothing to narrow with, so every
// run re-scanned all of history. On a journal of half a million event commits
// that is many minutes of scanning before the first useful byte of output, paid
// again on every run, including the run you do immediately after the last one to
// check your work.
//
// Fingerprint is what keeps this honest: it pins the scanner binary, the rule
// set, and the way twip reads the scanner's result, and any change to one of
// them discards the whole record rather than trusting an old tool's "clean".
// `--all` ignores it outright.
//
// It can only ever narrow THIS command's scan. The mirror push gate
// (gateMirrorPush) scopes itself against the remote and never reads this file,
// so a stale or wrong record here cannot widen what leaves the machine.
type CleanScan struct {
	Fingerprint string `json:"fingerprint"`           // scanner + rules the verdicts below were reached with
	JournalTip  string `json:"journal_tip,omitempty"` // journal commit up to and including which nothing was found
	KeepRefs    string `json:"keep_refs,omitempty"`   // ScanRefsDigest of the ref state the keep-ref scan was clean against
	TS          string `json:"ts,omitempty"`
}

// cleanScanPath lives under the git common dir beside the clone-id and the
// pending-propagation marker, so linked worktrees of the clone share one record.
func (r *Recorder) cleanScanPath(ctx context.Context) (string, error) {
	commonDir, err := gitutil.CommonDir(ctx, r.RepoRoot)
	if err != nil {
		return "", err
	}
	return filepath.Join(commonDir, "twip", "clean-scan.json"), nil
}

// LoadCleanScan returns the recorded clean scan, or nil when there is none (or
// it is unreadable — an absent record just means "scan everything").
func (r *Recorder) LoadCleanScan(ctx context.Context) *CleanScan {
	path, err := r.cleanScanPath(ctx)
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // our own marker under the git dir
	if err != nil {
		return nil
	}
	var cs CleanScan
	if json.Unmarshal(b, &cs) != nil || cs.Fingerprint == "" {
		return nil
	}
	return &cs
}

// SaveCleanScan records (or replaces) what is now proven clean. Best-effort:
// failing to write it costs a future run some time, never correctness.
func (r *Recorder) SaveCleanScan(ctx context.Context, cs *CleanScan) {
	path, err := r.cleanScanPath(ctx)
	if err != nil || cs == nil || cs.Fingerprint == "" {
		return
	}
	cs.TS = time.Now().UTC().Format(time.RFC3339)
	b, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(path), 0o750) != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o600)
}

// ClearCleanScan drops the record, so the next redaction scans from scratch.
func (r *Recorder) ClearCleanScan(ctx context.Context) {
	if path, err := r.cleanScanPath(ctx); err == nil {
		_ = os.Remove(path)
	}
}

// ScanRefsDigest fingerprints every ref the keep-ref scan's result depends on:
// the pins and archived stash entries it walks, AND the branches, tags and
// remote-tracking refs it subtracts (the scan reports only what ordinary history
// does not already hold, so deleting a branch can put previously-excluded
// commits back in scope). Any change to any of them invalidates a cached
// "keep-refs are clean".
//
// One for-each-ref, hashed — cheap even with the thousands of pins a long-lived
// clone accumulates.
func (r *Recorder) ScanRefsDigest(ctx context.Context) (string, error) {
	out, err := gitutil.Run(ctx, r.RepoRoot, nil, nil, "for-each-ref",
		"--format=%(refname) %(objectname)",
		PinRefPrefix, StashRefPrefix, "refs/heads/", "refs/tags/", "refs/remotes/")
	if err != nil {
		return "", err
	}
	// for-each-ref already sorts by refname, so the digest is stable.
	sum := sha256.Sum256([]byte(strings.TrimSpace(string(out))))
	return hex.EncodeToString(sum[:]), nil
}
