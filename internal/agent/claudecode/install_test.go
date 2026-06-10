package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readSettingsFile(t *testing.T, repoRoot string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, settingsFile))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse settings: %v", err)
	}
	return m
}

func decodeHooks(t *testing.T, settings map[string]json.RawMessage, event string) []hookMatcher {
	t.Helper()
	var hooks map[string]json.RawMessage
	if err := json.Unmarshal(settings["hooks"], &hooks); err != nil {
		t.Fatalf("parse hooks: %v", err)
	}
	return decodeMatchers(hooks[event])
}

func TestInstallHooks_FreshRepo(t *testing.T) {
	repo := t.TempDir()
	a := &Agent{}
	n, err := a.InstallHooks(context.Background(), repo, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("installed %d hooks, want 5", n)
	}
	s := readSettingsFile(t, repo)

	stop := decodeHooks(t, s, "Stop")
	if len(stop) != 1 || len(stop[0].Hooks) != 1 || !strings.Contains(stop[0].Hooks[0].Command, "twip hook claude-code stop") {
		t.Errorf("Stop hook not as expected: %+v", stop)
	}
	post := decodeHooks(t, s, "PostToolUse")
	if len(post) != 1 || post[0].Matcher != "Task" {
		t.Errorf("PostToolUse matcher not Task: %+v", post)
	}

	// Idempotent.
	n2, err := a.InstallHooks(context.Background(), repo, false)
	if err != nil || n2 != 0 {
		t.Errorf("second install: n=%d err=%v, want 0/nil", n2, err)
	}
}

func TestInstallHooks_PreservesForeignHooksAndKeys(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{
  "model": "opus",
  "hooks": {
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "my-own-tool stop"}]}],
    "Notification": [{"matcher": "", "hooks": [{"type": "command", "command": "notify"}]}]
  }
}`
	mustWrite(t, filepath.Join(repo, settingsFile), existing)

	a := &Agent{}
	if _, err := a.InstallHooks(context.Background(), repo, false); err != nil {
		t.Fatal(err)
	}
	s := readSettingsFile(t, repo)

	if string(s["model"]) != `"opus"` {
		t.Errorf("top-level key clobbered: model=%s", s["model"])
	}
	var hooks map[string]json.RawMessage
	if err := json.Unmarshal(s["hooks"], &hooks); err != nil {
		t.Fatal(err)
	}
	if _, ok := hooks["Notification"]; !ok {
		t.Error("unknown hook type Notification was dropped")
	}
	stop := decodeMatchers(hooks["Stop"])
	var foundForeign, foundTwip bool
	for _, m := range stop {
		for _, h := range m.Hooks {
			if h.Command == "my-own-tool stop" {
				foundForeign = true
			}
			if isTwipHook(h.Command) {
				foundTwip = true
			}
		}
	}
	if !foundForeign || !foundTwip {
		t.Errorf("expected both foreign and twip Stop hooks; foreign=%v twip=%v", foundForeign, foundTwip)
	}
}

func TestUninstallHooks_RemovesOnlyTwip(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(repo, settingsFile),
		`{"hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"my-own-tool stop"}]}]}}`)

	a := &Agent{}
	if _, err := a.InstallHooks(context.Background(), repo, false); err != nil {
		t.Fatal(err)
	}
	if err := a.UninstallHooks(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	s := readSettingsFile(t, repo)

	var hooks map[string]json.RawMessage
	if err := json.Unmarshal(s["hooks"], &hooks); err != nil {
		t.Fatal(err)
	}
	// SessionStart etc. were twip-only => their keys should be gone entirely.
	for _, k := range []string{"SessionStart", "UserPromptSubmit", "SessionEnd", "PostToolUse"} {
		if _, ok := hooks[k]; ok {
			t.Errorf("twip-only event %q should be removed after uninstall", k)
		}
	}
	// Stop retains the foreign hook only.
	stop := decodeMatchers(hooks["Stop"])
	for _, m := range stop {
		for _, h := range m.Hooks {
			if isTwipHook(h.Command) {
				t.Errorf("twip Stop hook survived uninstall: %q", h.Command)
			}
		}
	}
}

func TestUninstallHooks_MissingFileIsNoop(t *testing.T) {
	a := &Agent{}
	if err := a.UninstallHooks(context.Background(), t.TempDir()); err != nil {
		t.Errorf("uninstall on missing settings should be a no-op, got %v", err)
	}
}

func TestCommandIsShellGuarded(t *testing.T) {
	cmd := (&Agent{}).command("stop")
	if !strings.Contains(cmd, "command -v twip") || !strings.Contains(cmd, "exec twip hook claude-code stop") {
		t.Errorf("hook command missing guard/exec: %q", cmd)
	}
}
