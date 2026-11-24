package export

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

type delta struct {
	from, to int
	data     string
}

func rebuild(t *testing.T, deltas ...delta) (*Transcript, string) {
	t.Helper()
	tr := NewTranscript(filepath.Join(t.TempDir(), "t.jsonl"))
	for _, d := range deltas {
		if err := tr.Apply(d.from, d.to, []byte(d.data), "ok"); err != nil {
			t.Fatal(err)
		}
	}
	if err := tr.Finish(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(tr.Path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return tr, string(data)
}

func kinds(tr *Transcript) []string {
	var out []string
	for _, i := range tr.Issues {
		out = append(out, i.Kind)
	}
	return out
}

func expectKinds(t *testing.T, tr *Transcript, want ...string) {
	t.Helper()
	got := kinds(tr)
	if len(got) != len(want) {
		t.Fatalf("issues %v, want kinds %v", tr.Issues, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("issues %v, want kinds %v", tr.Issues, want)
		}
	}
}

func TestDeltasThatJoinRebuildTheFile(t *testing.T) {
	tr, got := rebuild(t, delta{0, 2, "a\nb\n"}, delta{2, 2, ""}, delta{2, 3, "c\n"})
	if got != "a\nb\nc\n" || tr.Lines() != 3 || tr.Qualities["ok"] != 3 {
		t.Fatalf("got %q lines %d qualities %v", got, tr.Lines(), tr.Qualities)
	}
	expectKinds(t, tr)
}

func TestMatchingOverlapIsDropped(t *testing.T) {
	tr, got := rebuild(t, delta{0, 2, "a\nb\n"}, delta{1, 3, "b\nc\n"})
	if got != "a\nb\nc\n" {
		t.Fatalf("got %q", got)
	}
	expectKinds(t, tr)
}

func TestDifferingBytesForALineKeepTheFirstAndAreReported(t *testing.T) {
	tr, got := rebuild(t, delta{0, 2, "a\nb\n"}, delta{1, 3, "B\nc\n"})
	if got != "a\nb\nc\n" {
		t.Fatalf("got %q", got)
	}
	expectKinds(t, tr, IssueConflict)
}

func TestGapsAreReportedUnlessALaterResendFillsThem(t *testing.T) {
	tr, got := rebuild(t, delta{3, 4, "d\n"}, delta{6, 8, "g\nh\n"})
	if got != "d\ng\nh\n" || tr.Lines() != 8 {
		t.Fatalf("got %q lines %d", got, tr.Lines())
	}
	expectKinds(t, tr, IssueHeadMissing, IssueHole)

	tr, got = rebuild(t, delta{3, 4, "d\n"}, delta{6, 8, "g\nh\n"}, delta{0, 9, "a\nb\nc\nd\ne\nf\ng\nh\ni\n"})
	if got != "a\nb\nc\nd\ne\nf\ng\nh\ni\n" {
		t.Fatalf("got %q", got)
	}
	expectKinds(t, tr)
}

func TestLineRecordedMidWriteIsCompletedByALaterResend(t *testing.T) {
	tr, got := rebuild(t, delta{0, 2, "a\nb-par"}, delta{2, 3, "c\n"})
	if got != "a\nb-par\nc\n" {
		t.Fatalf("got %q", got)
	}
	expectKinds(t, tr, IssueCutLine)

	tr, got = rebuild(t, delta{0, 2, "a\nb-par"}, delta{2, 3, "c\n"}, delta{0, 3, "a\nb-partial\nc\n"})
	if got != "a\nb-partial\nc\n" {
		t.Fatalf("got %q", got)
	}
	expectKinds(t, tr)
}

func TestUnfinishedLastLineStaysAsRecorded(t *testing.T) {
	tr, got := rebuild(t, delta{0, 2, "a\nb-par"})
	if got != "a\nb-par" {
		t.Fatalf("got %q", got)
	}
	expectKinds(t, tr)
}

func TestRangeThatDisagreesWithItsBytesIsReported(t *testing.T) {
	tr, got := rebuild(t, delta{0, 3, "a\nb\n"}, delta{2, 3, "c\n"})
	if got != "a\nb\nc\n" {
		t.Fatalf("got %q", got)
	}
	expectKinds(t, tr, IssueCount)
}

func TestRedactedLinesAreCounted(t *testing.T) {
	tr, _ := rebuild(t, delta{0, 2, "token [twip-redacted]\nb\n"})
	if tr.Redacted != 1 || !tr.Faithful() {
		t.Fatalf("redacted %d issues %v", tr.Redacted, tr.Issues)
	}
}

func TestNativeLayouts(t *testing.T) {
	if got := ClaudeProjectDir("/Users/ann/src/spindle.v2"); got != "-Users-ann-src-spindle-v2" {
		t.Fatalf("claude dir %q", got)
	}
	ts := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	want := ".codex/sessions/2026/03/04/rollout-2026-03-04T05-06-07-0199.jsonl"
	if got := CodexRolloutPath("0199", ts); got != want {
		t.Fatalf("codex path %q", got)
	}
}
