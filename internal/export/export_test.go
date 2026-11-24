package export

import (
	"archive/zip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codespeak-dev/twip/internal/agent"
	"github.com/codespeak-dev/twip/internal/gitutil"
	"github.com/codespeak-dev/twip/internal/snapshot"
	"github.com/codespeak-dev/twip/internal/store"
)

func journalRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "ann@twip.test"},
		{"config", "user.name", "Ann"},
		{"config", "commit.gpgsign", "false"},
		{"remote", "add", "origin", "https://example.test/acme/app.git"},
	} {
		if _, err := gitutil.Run(ctx, dir, nil, nil, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		if _, err := gitutil.Run(ctx, dir, nil, nil, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	return dir
}

type recorder struct {
	t    *testing.T
	repo string
	rec  *store.Recorder
	seq  map[string]int
	now  int64
}

func (r *recorder) append(ev *agent.Event) {
	r.t.Helper()
	ctx := context.Background()
	snap, err := snapshot.Capture(ctx, r.repo)
	if err != nil {
		r.t.Fatal(err)
	}
	r.now++
	if _, err := r.rec.Append(ctx, ev, snap, "main", r.seq[ev.SessionID], time.Unix(1000+r.now, 0)); err != nil {
		r.t.Fatal(err)
	}
	r.seq[ev.SessionID]++
}

func journalDelta(from, to int, text string) agent.Delta {
	return agent.Delta{Bytes: []byte(text), From: from, To: to, Quality: agent.QualityOK}
}

const (
	sidA = "11111111-1111-1111-1111-111111111111"
	sidB = "22222222-2222-2222-2222-222222222222"
	sidC = "33333333-3333-3333-3333-333333333333"
	sidD = "44444444-4444-4444-4444-444444444444"
)

func line(n int) string {
	return `{"type":"user","cwd":"/Users/ann/app","n":` + string(rune('0'+n)) + "}\n"
}

func readZip(t *testing.T, path string) map[string]string {
	t.Helper()
	z, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	out := map[string]string{}
	for _, f := range z.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = string(data)
	}
	return out
}

func TestExportRebuildsSessionsIntoArchives(t *testing.T) {
	repo := journalRepo(t)
	r := &recorder{t: t, repo: repo, rec: store.New(repo), seq: map[string]int{}}

	r.append(&agent.Event{Agent: "claude-code", SessionID: sidA, Kind: agent.KindSessionStart})
	r.append(&agent.Event{Agent: "claude-code", SessionID: sidA, Kind: agent.KindPromptSubmit, Prompt: "fix the bug"})
	r.append(&agent.Event{Agent: "claude-code", SessionID: sidA, Kind: agent.KindStop, Transcript: journalDelta(0, 2, line(0)+line(1)), Cursor: agent.Cursor{Main: 2}})
	r.append(&agent.Event{Agent: "claude-code", SessionID: sidA, Kind: agent.KindSubagentStop,
		Sidechains: []agent.Sidechain{{ID: "a1", Delta: journalDelta(0, 1, "{\"side\":1}\n")}}})
	r.append(&agent.Event{Agent: "claude-code", SessionID: sidA, Kind: agent.KindStop, Transcript: journalDelta(0, 3, line(0)+line(1)+line(2)), Cursor: agent.Cursor{Main: 3}})

	r.append(&agent.Event{Agent: "claude-code", SessionID: sidB, Kind: agent.KindStop, Transcript: journalDelta(1, 2, line(1)), Cursor: agent.Cursor{Main: 2}})

	r.append(&agent.Event{Agent: "claude-code", SessionID: sidC, Kind: agent.KindPromptSubmit, Prompt: "# You are the driver agent\nrun it"})
	r.append(&agent.Event{Agent: "claude-code", SessionID: sidC, Kind: agent.KindStop, Transcript: journalDelta(0, 1, line(0)), Cursor: agent.Cursor{Main: 1}})

	r.append(&agent.Event{Agent: "claude-code", SessionID: sidD, Kind: agent.KindSessionEnd})

	out := t.TempDir()
	index, err := Run(context.Background(), Options{
		Repos: []string{repo}, Out: out, SkipPromptPrefixes: []string{"# You are the driver agent"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(index.Clones) != 1 || index.Clones[0].RepoURL != "https://example.test/acme/app.git" ||
		len(index.Clones[0].Authors) != 1 || index.Clones[0].Authors[0].Email != "ann@twip.test" {
		t.Fatalf("clones %+v", index.Clones)
	}
	clone := index.Clones[0].ID
	bySession := map[string]SessionReport{}
	for _, s := range index.Sessions {
		bySession[s.SessionID] = s
	}
	if got := bySession[sidC].Skipped; got != SkippedPromptPrefix {
		t.Fatalf("driver session skipped=%q", got)
	}
	if got := bySession[sidD].Skipped; got != SkippedEmpty {
		t.Fatalf("empty session skipped=%q", got)
	}
	if len(index.Archives) != 2 {
		t.Fatalf("archives %+v", index.Archives)
	}

	dir := ".claude/projects/-Users-ann-app/"
	for _, a := range index.Archives {
		if a.ProjectPath != "/Users/ann/app" || a.Clone != clone || a.Sessions != 1 {
			t.Fatalf("archive %+v", a)
		}
		files := readZip(t, filepath.Join(out, "archives", a.Name))
		var manifest Manifest
		if err := json.Unmarshal([]byte(files["manifest.json"]), &manifest); err != nil {
			t.Fatal(err)
		}
		if manifest.Incomplete != a.Incomplete || manifest.ProjectPath != a.ProjectPath {
			t.Fatalf("manifest %+v for archive %+v", manifest, a)
		}
		if a.Incomplete {
			if got := files["sessions/"+dir+sidB+".jsonl"]; got != line(1) {
				t.Fatalf("incomplete archive %v", files)
			}
			continue
		}
		if got := files["sessions/"+dir+sidA+".jsonl"]; got != line(0)+line(1)+line(2) {
			t.Fatalf("main transcript %q", got)
		}
		if got := files["sessions/"+dir+sidA+"/subagents/agent-a1.jsonl"]; got != "{\"side\":1}\n" {
			t.Fatalf("sidechain %q", got)
		}
	}
	report := bySession[sidB].Files
	if len(report) != 1 || report[0].Faithful || report[0].Issues[0].Kind != IssueHeadMissing {
		t.Fatalf("incomplete session report %+v", report)
	}
	if _, err := os.Stat(filepath.Join(out, "work")); !os.IsNotExist(err) {
		t.Fatalf("work dir left behind: %v", err)
	}
}
