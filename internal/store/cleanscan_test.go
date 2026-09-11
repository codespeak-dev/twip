package store

import (
	"context"
	"testing"

	"github.com/codespeak-dev/twip/internal/gitutil"
)

// TestCleanScanRoundTrip: the record survives a save/load and is discarded when
// unreadable or unfingerprinted — a redaction that cannot read it must scan
// everything, never silently skip history.
func TestCleanScanRoundTrip(t *testing.T) {
	ctx := context.Background()
	rec := New(initRepo(t))

	if got := rec.LoadCleanScan(ctx); got != nil {
		t.Fatalf("LoadCleanScan on a fresh repo = %+v, want nil", got)
	}
	rec.SaveCleanScan(ctx, &CleanScan{Fingerprint: "fp1", JournalTip: "abc123", KeepRefs: "kd1"})
	got := rec.LoadCleanScan(ctx)
	if got == nil || got.Fingerprint != "fp1" || got.JournalTip != "abc123" || got.KeepRefs != "kd1" {
		t.Fatalf("LoadCleanScan = %+v, want the saved record", got)
	}
	if got.TS == "" {
		t.Error("saved record carries no timestamp")
	}

	// A record without a fingerprint names no rule set, so it vouches for nothing.
	rec.SaveCleanScan(ctx, &CleanScan{JournalTip: "def456"})
	if got := rec.LoadCleanScan(ctx); got == nil || got.JournalTip != "abc123" {
		t.Errorf("an unfingerprinted save replaced the record: %+v", got)
	}

	rec.ClearCleanScan(ctx)
	if got := rec.LoadCleanScan(ctx); got != nil {
		t.Errorf("LoadCleanScan after clear = %+v, want nil", got)
	}
}

// TestScanRefsDigest_TracksEveryRefTheKeepScanDependsOn: the keep-ref scan walks
// the pins and subtracts ordinary history, so its verdict is only reusable while
// BOTH sides are unchanged. Moving a branch has to invalidate it just as adding a
// pin does — otherwise deleting a branch would put previously-excluded commits
// back in scope with the cache still claiming they were clean.
func TestScanRefsDigest_TracksEveryRefTheKeepScanDependsOn(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)

	base, err := rec.ScanRefsDigest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if base == "" {
		t.Fatal("ScanRefsDigest returned an empty digest")
	}
	if again, _ := rec.ScanRefsDigest(ctx); again != base {
		t.Errorf("digest is not stable across calls: %s vs %s", base, again)
	}

	c := buildJournalCommit(t, repo, "", "pinned\n", "1700000000 +0000",
		map[string]string{"meta/event.json": `{"kind":"x"}`})
	rec.PinCommit(ctx, c)
	withPin, _ := rec.ScanRefsDigest(ctx)
	if withPin == base {
		t.Error("adding a pin left the digest unchanged")
	}

	if err := gitutil.UpdateRef(ctx, repo, "refs/heads/side", c, ""); err != nil {
		t.Fatal(err)
	}
	withBranch, _ := rec.ScanRefsDigest(ctx)
	if withBranch == withPin {
		t.Error("adding a branch left the digest unchanged (the keep-ref scan subtracts branches)")
	}
}
