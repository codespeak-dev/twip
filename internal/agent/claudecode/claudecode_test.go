package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codespeak-dev/twip/internal/agent"
)

func TestDeltaFrom(t *testing.T) {
	cases := []struct {
		name      string
		data      string
		from      int
		wantDelta string
		wantTotal int
		wantTrunc bool
	}{
		{"whole, trailing newline", "a\nb\nc\n", 0, "a\nb\nc\n", 3, false},
		{"skip one", "a\nb\nc\n", 1, "b\nc\n", 3, false},
		{"skip all", "a\nb\nc\n", 3, "", 3, false},
		{"no trailing newline, whole", "a\nb\nc", 0, "a\nb\nc", 3, false},
		{"no trailing newline, last line", "a\nb\nc", 2, "c", 3, false},
		{"empty", "", 0, "", 0, false},
		{"from beyond data", "a\nb\n", 5, "", 2, true},
		{"single partial line", "x", 0, "x", 1, false},
		// cursor exactly at total with no trailing newline must not be truncated
		{"cursor at total, no trailing newline", "a\nb", 2, "", 2, false},
		{"cursor past total, no trailing newline", "a\nb", 3, "", 2, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			delta, total, trunc := agent.DeltaFrom([]byte(tc.data), tc.from)
			if string(delta) != tc.wantDelta {
				t.Errorf("delta = %q, want %q", delta, tc.wantDelta)
			}
			if total != tc.wantTotal {
				t.Errorf("total = %d, want %d", total, tc.wantTotal)
			}
			if trunc != tc.wantTrunc {
				t.Errorf("truncated = %v, want %v", trunc, tc.wantTrunc)
			}
		})
	}
}

func TestCountLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.jsonl")
	if n, err := countLines(filepath.Join(dir, "missing.jsonl")); err != nil || n != 0 {
		t.Fatalf("missing file: n=%d err=%v", n, err)
	}
	mustWrite(t, p, "a\nb\nc")
	if n, _ := countLines(p); n != 3 {
		t.Errorf("countLines = %d, want 3", n)
	}
	mustWrite(t, p, "a\nb\nc\n")
	if n, _ := countLines(p); n != 3 {
		t.Errorf("countLines (trailing nl) = %d, want 3", n)
	}
}

func TestValidateAgentID(t *testing.T) {
	for _, ok := range []string{"a089f8e", "abc-123_X"} {
		if err := agent.ValidateAgentID(ok); err != nil {
			t.Errorf("agent.ValidateAgentID(%q) unexpected error: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "../etc", "a/b", "a.jsonl", "a b"} {
		if err := agent.ValidateAgentID(bad); err == nil {
			t.Errorf("agent.ValidateAgentID(%q) = nil, want error", bad)
		}
	}
}

func TestSidechainPath(t *testing.T) {
	dir := t.TempDir()
	sid := "11111111-2222-3333-4444-555555555555"
	main := filepath.Join(dir, sid+".jsonl")

	// Flat fallback when nested layout absent.
	if got := sidechainPath(main, "aaa"); got != filepath.Join(dir, "agent-aaa.jsonl") {
		t.Errorf("flat fallback = %q", got)
	}
	// Nested layout preferred when present.
	nested := filepath.Join(dir, sid, "subagents", "agent-bbb.jsonl")
	if err := os.MkdirAll(filepath.Dir(nested), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, nested, "{}\n")
	if got := sidechainPath(main, "bbb"); got != nested {
		t.Errorf("nested = %q, want %q", got, nested)
	}
}

func TestParseHookEvent_SessionStartCapturesUnrecordedLines(t *testing.T) {
	for _, tc := range []struct {
		name string
		from int
		want string
	}{
		{"fresh", 0, "old1\nold2\nold3\n"},
		{"resumed", 2, "old3\n"},
		{"already captured", 3, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp := filepath.Join(t.TempDir(), "s.jsonl")
			mustWrite(t, tp, "old1\nold2\nold3\n")
			prior := agent.Cursor{Main: tc.from, Sidechain: map[string]int{"child": 2}}
			ev := mustParse(t, &Agent{}, hookSessionStart, `{"session_id":"S","transcript_path":"`+tp+`","model":"claude-opus-4-8"}`, prior)
			if ev.Kind != agent.KindSessionStart || ev.SessionID != "S" || ev.Model != "claude-opus-4-8" {
				t.Fatalf("event = %+v", ev)
			}
			if string(ev.Transcript.Bytes) != tc.want || ev.Transcript.From != tc.from || ev.Transcript.To != 3 || ev.Transcript.Quality != agent.QualityOK {
				t.Errorf("transcript = %+v, want (%d,3] %q", ev.Transcript, tc.from, tc.want)
			}
			if ev.Cursor.Main != 3 || ev.Cursor.Sidechain["child"] != 2 {
				t.Errorf("cursor = %+v", ev.Cursor)
			}
			ev.Cursor.Sidechain["child"] = 4
			if prior.Main != tc.from || prior.Sidechain["child"] != 2 {
				t.Errorf("prior cursor mutated: %+v", prior)
			}
		})
	}
}

func TestParseHookEvent_SessionStartUnavailableTranscriptPreservesCursor(t *testing.T) {
	for _, path := range []string{"", filepath.Join(t.TempDir(), "missing.jsonl")} {
		ev := mustParse(t, &Agent{}, hookSessionStart, `{"session_id":"S","transcript_path":"`+path+`"}`, agent.Cursor{Main: 3})
		if ev.Cursor.Main != 3 || len(ev.Transcript.Bytes) != 0 {
			t.Errorf("unavailable transcript: %+v", ev)
		}
	}
}

func TestParseHookEvent_SessionStartReadFailureDoesNotAdvanceCursor(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		path string
	}{
		{"read error", dir},
		{"truncated", filepath.Join(dir, "short.jsonl")},
		{"empty", filepath.Join(dir, "empty.jsonl")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "truncated" {
				mustWrite(t, tc.path, "one\n")
			} else if tc.name == "empty" {
				mustWrite(t, tc.path, "")
			}
			prior := agent.Cursor{Main: 3}
			ev, err := (&Agent{}).ParseHookEvent(context.Background(), hookSessionStart, strings.NewReader(`{"session_id":"S","transcript_path":"`+tc.path+`"}`), prior)
			if err == nil || ev != nil || prior.Main != 3 {
				t.Fatalf("event=%+v err=%v prior=%+v", ev, err, prior)
			}
		})
	}
}

func TestParseHookEvent_StopReadsDelta(t *testing.T) {
	dir := t.TempDir()
	tp := filepath.Join(dir, "s.jsonl")
	mustWrite(t, tp, "l1\nl2\nl3\nl4\nl5\n")
	makeStale(t, tp) // trigger the flush fast-path so the test is instant

	a := &Agent{}
	prior := agent.Cursor{Main: 2}
	ev := mustParse(t, a, hookStop, `{"session_id":"S","transcript_path":"`+tp+`"}`, prior)
	if ev.Transcript.From != 2 || ev.Transcript.To != 5 {
		t.Errorf("offsets = (%d,%d), want (2,5)", ev.Transcript.From, ev.Transcript.To)
	}
	if string(ev.Transcript.Bytes) != "l3\nl4\nl5\n" {
		t.Errorf("delta = %q", ev.Transcript.Bytes)
	}
	if ev.Cursor.Main != 5 {
		t.Errorf("cursor advanced to %d, want 5", ev.Cursor.Main)
	}
}

func TestParseHookEvent_PostTaskCapturesSidechain(t *testing.T) {
	dir := t.TempDir()
	sid := "11111111-2222-3333-4444-555555555555"
	main := filepath.Join(dir, sid+".jsonl")
	mustWrite(t, main, "{}\n")
	side := filepath.Join(dir, sid, "subagents", "agent-deadbee.jsonl")
	if err := os.MkdirAll(filepath.Dir(side), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, side, "s1\ns2\n")

	a := &Agent{}
	ev := mustParse(t, a, hookPostTask,
		`{"session_id":"S","transcript_path":"`+main+`","tool_response":{"agentId":"deadbee"}}`,
		agent.Cursor{})
	if len(ev.Sidechains) != 1 {
		t.Fatalf("sidechains = %d, want 1", len(ev.Sidechains))
	}
	sc := ev.Sidechains[0]
	if sc.ID != "deadbee" || string(sc.Delta.Bytes) != "s1\ns2\n" || sc.Delta.To != 2 {
		t.Errorf("sidechain = %+v", sc)
	}
	if ev.Cursor.Sidechain["deadbee"] != 2 {
		t.Errorf("sidechain cursor = %d, want 2", ev.Cursor.Sidechain["deadbee"])
	}
}

func TestParseHookEvent_PostTaskRejectsUnsafeID(t *testing.T) {
	a := &Agent{}
	_, err := a.ParseHookEvent(context.Background(), hookPostTask, strings.NewReader(
		`{"session_id":"S","transcript_path":"/tmp/x.jsonl","tool_response":{"agentId":"../escape"}}`),
		agent.Cursor{})
	if err == nil {
		t.Fatal("expected error for path-unsafe agent id")
	}
}

func TestCheckStopSentinel(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	now := time.Now()
	inWindow := now.Format(time.RFC3339Nano)
	stale := now.Add(-time.Hour).Format(time.RFC3339Nano)

	// Matching command + in-window timestamp => detected.
	mustWrite(t, p, `{"data":{"command":"sh -c exec twip hook claude-code stop"},"timestamp":"`+inWindow+`"}`+"\n")
	if !checkStopSentinel(p, now) {
		t.Error("expected sentinel match for in-window timestamp")
	}
	// Same string but stale timestamp (e.g. a prior turn, or quoted in content) => rejected.
	mustWrite(t, p, `{"data":{"command":"... twip hook claude-code stop ..."},"timestamp":"`+stale+`"}`+"\n")
	if checkStopSentinel(p, now) {
		t.Error("stale timestamp should not match (skew guard)")
	}
}

// --- helpers ---

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func makeStale(t *testing.T, path string) {
	t.Helper()
	old := time.Now().Add(-5 * time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func mustParse(t *testing.T, a *Agent, hook, payload string, prior agent.Cursor) *agent.Event {
	t.Helper()
	ev, err := a.ParseHookEvent(context.Background(), hook, strings.NewReader(payload), prior)
	if err != nil {
		t.Fatalf("ParseHookEvent(%s): %v", hook, err)
	}
	if ev == nil {
		t.Fatalf("ParseHookEvent(%s) returned nil event", hook)
	}
	return ev
}
