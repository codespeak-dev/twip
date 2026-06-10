package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/codespeak/twip/internal/agent"
	"github.com/codespeak/twip/internal/gitutil"
	"github.com/codespeak/twip/internal/snapshot"
)

// TestAppend_ConcurrentNoMergeNoLoss races multiple writers appending to the same
// clone journal and asserts the CAS path yields a linear chain (no merge commits)
// with every event present (no loss). This is the property behind "no merges":
// a lost CAS race re-parents the one childless commit onto the new tip.
func TestAppend_ConcurrentNoMergeNoLoss(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)
	snap, err := snapshot.Capture(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}

	const writers, perWriter = 4, 10
	total := writers * perWriter

	var wg sync.WaitGroup
	errCh := make(chan error, total)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			sid := fmt.Sprintf("sess-%d", w)
			for i := 0; i < perWriter; i++ {
				// Distinct event per (writer, i) → distinct commit; appends within
				// a writer are sequential, so prevSeq=i is correct for that session.
				ev := &agent.Event{
					SessionID:  sid,
					Kind:       agent.KindStop,
					Transcript: agent.Delta{Bytes: []byte(fmt.Sprintf("w%d-%d\n", w, i)), From: i, To: i + 1, Quality: agent.QualityOK},
					Cursor:     agent.Cursor{Main: i + 1},
				}
				if _, err := rec.Append(ctx, ev, snap, "main", i, time.Unix(int64(w*1000+i), 0)); err != nil {
					errCh <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent append failed: %v", err)
	}

	cloneID, err := rec.CloneID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	jref := JournalRefPrefix + cloneID

	// No loss: every event is on the chain.
	if got, _ := gitutil.Out(ctx, repo, "rev-list", "--count", jref); got != fmt.Sprint(total) {
		t.Errorf("journal has %s commits, want %d (lost or duplicated events)", got, total)
	}

	// No merges: the chain is strictly linear (no commit has >1 parent).
	if merges, _ := gitutil.Out(ctx, repo, "rev-list", "--min-parents=2", jref); merges != "" {
		t.Errorf("found merge commits in journal:\n%s", merges)
	}

	// No loss, per session: each writer's events are all present with contiguous
	// seqs 1..perWriter.
	for w := 0; w < writers; w++ {
		sid := fmt.Sprintf("sess-%d", w)
		events, err := rec.LoadSessionEvents(ctx, sid)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != perWriter {
			t.Errorf("session %s has %d events, want %d", sid, len(events), perWriter)
			continue
		}
		for i, ec := range events {
			if ec.Record.Seq != i+1 {
				t.Errorf("session %s event %d seq = %d, want %d", sid, i, ec.Record.Seq, i+1)
			}
		}
	}

	// All commits distinct (CAS never collapsed two events into one sha).
	all, err := rec.LoadAllEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != total {
		t.Errorf("LoadAllEvents returned %d, want %d", len(all), total)
	}
	seen := map[string]bool{}
	for _, ec := range all {
		if seen[ec.Commit] {
			t.Errorf("duplicate commit in journal: %s", ec.Commit)
		}
		seen[ec.Commit] = true
	}
}
