package store

import (
	"context"
	"testing"
)

// TestCommitShas_MaxCount pins the bounded back-scan: maxCount caps the tip-first
// walk to the newest N commits (a prefix of the unbounded tip-first list), which is
// what stops PriorSessionState from walking the whole journal on a SessionStart miss.
func TestCommitShas_MaxCount(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)
	cloneID, err := rec.CloneID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		appendEvent(t, rec, repo, "s", int64(1000+i))
	}
	ref := journalRef(cloneID)

	all, err := rec.commitShas(ctx, ref, false, 0) // unbounded, tip-first
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Fatalf("unbounded commitShas = %d, want 5", len(all))
	}

	bounded, err := rec.commitShas(ctx, ref, false, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded) != 2 {
		t.Fatalf("commitShas maxCount=2 = %d, want 2", len(bounded))
	}
	if bounded[0] != all[0] || bounded[1] != all[1] {
		t.Errorf("bounded scan should be the newest commits (a tip-first prefix): got %v, want prefix of %v", bounded, all)
	}
}
