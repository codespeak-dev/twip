package main

import (
	"errors"
	"strings"
	"testing"
)

func TestMatchGlobs(t *testing.T) {
	cases := []struct {
		list string
		want bool
	}{
		{"", false},
		{"github.com/codespeak-dev/*", true},
		{"github.com/codespeak-dev", true},
		{"github.com/codespeak-dev/twip", true},
		{"github.com/*", true},
		{"*.com", true},
		{"github.com/acme/*", false},
		{"github.com/acme/*, github.com/codespeak-dev/*", true},
		{",github.com/codespeak-dev/*", true},
		{"github.com/codespeak-dev/twip/cmd", false},
	}
	for _, c := range cases {
		if got := matchGlobs(c.list, modulePath); got != c.want {
			t.Errorf("matchGlobs(%q) = %v, want %v", c.list, got, c.want)
		}
	}
}

func TestAppendGlob(t *testing.T) {
	cases := []struct{ list, want string }{
		{"", privatePattern},
		{"github.com/acme/*", "github.com/acme/*," + privatePattern},
		{" github.com/acme/*, ", "github.com/acme/*," + privatePattern},
	}
	for _, c := range cases {
		if got := appendGlob(c.list, privatePattern); got != c.want {
			t.Errorf("appendGlob(%q) = %q, want %q", c.list, got, c.want)
		}
	}
}

func TestGoPrivateProblem(t *testing.T) {
	noShell := func(string) string { return "" }
	defaults := goPrivacyEnv{GOPROXY: "https://proxy.golang.org,direct", GOSUMDB: "sum.golang.org"}
	with := func(f func(*goPrivacyEnv)) goPrivacyEnv { ge := defaults; f(&ge); return ge }

	// Covered by GOPRIVATE (go env reports the derived GONOPROXY/GONOSUMDB too).
	ok := with(func(g *goPrivacyEnv) {
		g.GOPRIVATE, g.GONOPROXY, g.GONOSUMDB = privatePattern, privatePattern, privatePattern
	})
	if p := goPrivateProblem(ok, noShell); p != nil {
		t.Errorf("covered: got problem %+v", p)
	}

	// Unset: the fix sets GOPRIVATE.
	p := goPrivateProblem(defaults, noShell)
	if p == nil || len(p.fixes) != 1 || p.fixes[0] != "go env -w GOPRIVATE='"+privatePattern+"'" {
		t.Fatalf("unset: got %+v", p)
	}

	// Existing patterns are kept, not clobbered.
	acme := with(func(g *goPrivacyEnv) {
		g.GOPRIVATE, g.GONOPROXY, g.GONOSUMDB = "github.com/acme/*", "github.com/acme/*", "github.com/acme/*"
	})
	p = goPrivateProblem(acme, noShell)
	if p == nil || p.fixes[0] != "go env -w GOPRIVATE='github.com/acme/*,"+privatePattern+"'" {
		t.Fatalf("existing: got %+v", p)
	}

	if p.goEnvSet["GOPRIVATE"] != "github.com/acme/*,"+privatePattern {
		t.Fatalf("existing: --fix would write %+v", p.goEnvSet)
	}

	// Set in the shell: `go env -w` would be ignored, so point at the profile.
	p = goPrivateProblem(acme, func(k string) string {
		if k == "GOPRIVATE" {
			return "github.com/acme/*"
		}
		return ""
	})
	if p == nil || !strings.Contains(p.fixes[0], "shell") || !strings.Contains(p.fixes[0], "export GOPRIVATE='github.com/acme/*,"+privatePattern+"'") {
		t.Fatalf("shell: got %+v", p)
	}
	if len(p.goEnvSet) != 0 {
		t.Fatalf("shell: --fix must not write a shell-set var, got %+v", p.goEnvSet)
	}

	// GOPRIVATE covers it but GONOPROXY was overridden explicitly: fix GONOPROXY.
	override := with(func(g *goPrivacyEnv) {
		g.GOPRIVATE, g.GONOPROXY, g.GONOSUMDB = privatePattern, "corp.example/*", privatePattern
	})
	p = goPrivateProblem(override, noShell)
	if p == nil || len(p.fixes) != 1 || p.fixes[0] != "go env -w GONOPROXY='corp.example/*,"+privatePattern+"'" {
		t.Fatalf("override: got %+v", p)
	}

	// GOPROXY=direct + GOSUMDB=off need nothing more.
	direct := with(func(g *goPrivacyEnv) { g.GOPROXY, g.GOSUMDB = "direct", "off" })
	if p := goPrivateProblem(direct, noShell); p != nil {
		t.Errorf("direct/off: got problem %+v", p)
	}
}

func TestClassifyGitErr(t *testing.T) {
	cases := []struct {
		stderr string
		want   gitErrKind
	}{
		{"fatal: could not read Username for 'https://github.com': terminal prompts disabled", gitNeedsAuth},
		{"git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.", gitSSH},
		{"Host key verification failed.\nfatal: Could not read from remote repository.", gitSSH},
		{"ssh: Could not resolve hostname github.com: Name or service not known", gitNetwork},
		{"fatal: unable to access 'https://github.com/x/': Could not resolve host: github.com", gitNetwork},
		{"remote: Repository not found.\nfatal: repository 'https://github.com/codespeak-dev/twip/' not found", gitNoAccess},
		{"remote: Invalid username or token.\nfatal: Authentication failed for 'https://github.com/codespeak-dev/twip/'", gitBadCreds},
		{"fatal: something else", gitOther},
	}
	for _, c := range cases {
		if got, _ := classifyGitErr(errors.New("git ls-remote: exit status 128: " + c.stderr)); got != c.want {
			t.Errorf("classifyGitErr(%q) = %v, want %v", c.stderr, got, c.want)
		}
	}
	if k, _ := classifyGitErr(nil); k != gitOK {
		t.Errorf("nil err = %v, want gitOK", k)
	}
	// The line that identified the case is the one shown, not git's generic fatal.
	_, msg := classifyGitErr(errors.New("git ls-remote: exit status 128: git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository."))
	if msg != "git@github.com: Permission denied (publickey)." {
		t.Errorf("ssh msg = %q", msg)
	}
	_, msg = classifyGitErr(errors.New("git ls-remote: exit status 128: fatal: something else"))
	if msg != "fatal: something else" {
		t.Errorf("fallback msg = %q", msg)
	}
}

func TestGitAuthProblemFixes(t *testing.T) {
	has := func(p *accessProblem, sub string) bool {
		for _, f := range p.fixes {
			if strings.Contains(f, sub) {
				return true
			}
		}
		return false
	}
	if p := gitAuthProblem(gitNeedsAuth, "", "", ghLoggedIn); len(p.fixes) != 1 || p.fixes[0] != "gh auth setup-git" {
		t.Errorf("logged-in gh: %+v", p.fixes)
	}
	if p := gitAuthProblem(gitNeedsAuth, "", "", ghLoggedOut); !has(p, "gh auth login") || !has(p, "gh auth setup-git") {
		t.Errorf("logged-out gh: %+v", p.fixes)
	}
	if p := gitAuthProblem(gitNeedsAuth, "", "", ghMissing); !has(p, "cli.github.com") || !has(p, "insteadOf") {
		t.Errorf("no gh: %+v", p.fixes)
	}
	key := "url.git@github.com:.insteadof"
	if p := gitAuthProblem(gitSSH, "", key, ghLoggedIn); !has(p, "ssh -T git@github.com") || !has(p, "--unset "+key) {
		t.Errorf("ssh rewrite: %+v", p.fixes)
	}
	if p := gitAuthProblem(gitBadCreds, "", "", ghLoggedOut); !has(p, "gh auth login") {
		t.Errorf("bad creds, gh logged out: %+v", p.fixes)
	}
	if p := gitAuthProblem(gitNoAccess, "", "", ghLoggedIn); !has(p, "read access") || !has(p, "gh auth status") {
		t.Errorf("no access: %+v", p.fixes)
	}
}
