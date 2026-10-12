package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/codespeak-dev/twip/internal/gitutil"
)

// moduleRepoURL is the HTTPS URL `go` fetches twip from when it goes direct.
const moduleRepoURL = "https://" + modulePath

// repoName is twip's GitHub owner/repo, for messages.
var repoName = strings.TrimPrefix(modulePath, "github.com/")

// privatePattern is what we ask users to add to GOPRIVATE. The org glob (not just
// twip's path) so the codespeak-dev modules twip may later depend on resolve too.
const privatePattern = "github.com/codespeak-dev/*"

// accessProblem is one reason `go install`/`twip update` can't fetch twip, with
// the commands that fix it.
type accessProblem struct {
	summary string
	fixes   []string
	// goEnvSet is the `go env -w` assignments that fix this problem automatically
	// (`twip doctor --fix`); nil when the fix needs the user.
	goEnvSet map[string]string
}

// accessReport is the outcome of diagnoseModuleAccess. When skipped is non-empty
// the diagnosis could not run (offline, github unreachable) and says why.
type accessReport struct {
	public   bool
	skipped  string
	problems []accessProblem
}

// diagnoseModuleAccess checks that this machine can fetch twip's module the way
// `go install` will, and explains what to change when it can't. It probes the repo
// anonymously first: a public repo needs no setup, so a missing GOPRIVATE or git
// login is not a problem then. For a private repo it checks the two things a new
// user must set up — Go must skip the public proxy/checksum DB for the module
// (GOPRIVATE), and git must be able to authenticate to GitHub without prompting
// (`go` disables terminal prompts) — each against the user's real config.
func diagnoseModuleAccess(ctx context.Context) accessReport {
	var rep accessReport
	goBin, err := exec.LookPath("go")
	if err != nil {
		rep.problems = append(rep.problems, accessProblem{
			summary: "the `go` toolchain is not on PATH — twip installs and updates are built with it",
			fixes:   []string{"install Go: https://go.dev/dl"},
		})
		return rep
	}

	tmp, err := os.MkdirTemp("", "twip-access-*")
	if err != nil {
		rep.skipped = err.Error()
		return rep
	}
	defer os.RemoveAll(tmp)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	switch kind, msg := classifyGitErr(anonymousProbe(ctx, tmp)); kind {
	case gitOK:
		rep.public = true
		return rep
	case gitNeedsAuth:
		// Private: fall through to the setup checks below.
	case gitNetwork:
		rep.skipped = "could not reach github.com: " + msg
		return rep
	default:
		rep.skipped = "could not probe " + moduleRepoURL + ": " + msg
		return rep
	}

	if p := checkGoPrivate(ctx, goBin, tmp); p != nil {
		rep.problems = append(rep.problems, *p)
	}
	if p := checkGitAuth(ctx, tmp); p != nil {
		rep.problems = append(rep.problems, *p)
	}
	return rep
}

// anonymousProbe runs `git ls-remote` with the user's git config ignored and every
// credential source disabled, so it succeeds only if the repo is public.
func anonymousProbe(ctx context.Context, dir string) error {
	env := []string{
		"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=", "GCM_INTERACTIVE=never",
	}
	_, err := gitutil.Run(ctx, dir, env, nil,
		"-c", "credential.helper=", "-c", "core.askPass=", "ls-remote", moduleRepoURL, "HEAD")
	return err
}

// authProbe runs `git ls-remote` the way `go` does when it fetches a module direct:
// the user's own config (credential helpers, url.insteadOf rewrites), with prompts
// disabled and ssh in batch mode.
func authProbe(ctx context.Context, dir string) error {
	env := []string{"GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never"}
	if os.Getenv("GIT_SSH") == "" && os.Getenv("GIT_SSH_COMMAND") == "" {
		env = append(env, "GIT_SSH_COMMAND=ssh -o ControlMaster=no -o BatchMode=yes")
	}
	_, err := gitutil.Run(ctx, dir, env, nil, "ls-remote", moduleRepoURL, "HEAD")
	return err
}

type gitErrKind int

const (
	gitOK gitErrKind = iota
	gitNeedsAuth
	gitBadCreds
	gitNoAccess
	gitSSH
	gitNetwork
	gitOther
)

// gitErrSignatures maps git/ssh stderr fragments (lowercase) to the case they
// identify, checked in order: network before ssh so "ssh: Could not resolve
// hostname" reads as offline, not as a key problem.
var gitErrSignatures = []struct {
	kind    gitErrKind
	needles []string
}{
	{gitNeedsAuth, []string{"could not read username", "could not read password", "terminal prompts disabled"}},
	{gitNetwork, []string{"could not resolve host", "failed to connect", "connection timed out",
		"network is unreachable", "signal: killed", "context deadline exceeded"}},
	{gitSSH, []string{"permission denied (publickey", "host key verification failed",
		"connection closed by remote host", "ssh: "}},
	{gitBadCreds, []string{"invalid username or token", "authentication failed", "returned error: 401"}},
	{gitNoAccess, []string{"repository not found", "returned error: 403", "returned error: 404"}},
}

// classifyGitErr sorts a failed ls-remote into the cases we have a fix for, and
// returns the stderr line that identified it (else git's "fatal:" line) for display.
func classifyGitErr(err error) (gitErrKind, string) {
	if err == nil {
		return gitOK, ""
	}
	s := gitStderr(err.Error())
	l := strings.ToLower(s)
	for _, sig := range gitErrSignatures {
		for _, n := range sig.needles {
			if strings.Contains(l, n) {
				return sig.kind, lineContaining(s, n)
			}
		}
	}
	return gitOther, condenseGitErr(s)
}

// gitStderr strips gitutil.Run's "git <args>: exit status N: " prefix, leaving
// git's own stderr.
func gitStderr(s string) string {
	if i := strings.Index(s, "exit status "); i >= 0 {
		if j := strings.Index(s[i:], ": "); j >= 0 {
			return s[i+j+2:]
		}
	}
	return s
}

// lineContaining returns the line of s containing needle (case-insensitively).
func lineContaining(s, needle string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(strings.ToLower(line), needle) {
			return strings.TrimSpace(line)
		}
	}
	return condenseGitErr(s)
}

// condenseGitErr picks git's "fatal:" line out of a multi-line error, falling back
// to the last non-empty line.
func condenseGitErr(s string) string {
	last := ""
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, "fatal: "); i >= 0 {
			return line[i:]
		}
		if line != "" {
			last = line
		}
	}
	return last
}

// goPrivacyEnv is the subset of `go env` that decides whether `go` fetches a module
// direct and skips checksum-DB verification.
type goPrivacyEnv struct {
	GOPRIVATE, GONOPROXY, GONOSUMDB, GOSUMDB, GOPROXY string
}

func checkGoPrivate(ctx context.Context, goBin, dir string) *accessProblem {
	c := exec.CommandContext(ctx, goBin, "env", "-json", "GOPRIVATE", "GONOPROXY", "GONOSUMDB", "GOSUMDB", "GOPROXY")
	c.Dir = dir
	out, err := c.Output()
	if err != nil {
		return &accessProblem{summary: fmt.Sprintf("could not read `go env`: %v", err)}
	}
	var ge goPrivacyEnv
	if err := json.Unmarshal(out, &ge); err != nil {
		return &accessProblem{summary: fmt.Sprintf("could not parse `go env -json`: %v", err)}
	}
	return goPrivateProblem(ge, os.Getenv)
}

// goPrivateProblem reports whether ge lets `go` fetch twip privately. `go env`
// reports GONOPROXY/GONOSUMDB with their GOPRIVATE default already applied, so a
// module covered by GOPRIVATE but not by one of them means the user overrode that
// one explicitly — and that's the variable the fix has to extend. getenv
// distinguishes a value set in the shell (where `go env -w` has no effect) from
// one in Go's own env file.
func goPrivateProblem(ge goPrivacyEnv, getenv func(string) string) *accessProblem {
	direct := matchGlobs(ge.GONOPROXY, modulePath) || onlyDirect(ge.GOPROXY)
	noSum := matchGlobs(ge.GONOSUMDB, modulePath) || ge.GOSUMDB == "off"
	if direct && noSum {
		return nil
	}

	var vars []string
	switch {
	case !matchGlobs(ge.GOPRIVATE, modulePath):
		vars = []string{"GOPRIVATE"}
	default:
		if !direct {
			vars = append(vars, "GONOPROXY")
		}
		if !noSum {
			vars = append(vars, "GONOSUMDB")
		}
	}
	values := map[string]string{"GOPRIVATE": ge.GOPRIVATE, "GONOPROXY": ge.GONOPROXY, "GONOSUMDB": ge.GONOSUMDB}

	p := &accessProblem{summary: fmt.Sprintf(
		"%s does not cover %s, so `go` asks the public proxy/checksum DB, which can't see a private repo",
		strings.Join(vars, "/"), modulePath)}
	for _, v := range vars {
		next := appendGlob(values[v], privatePattern)
		if getenv(v) != "" {
			p.fixes = append(p.fixes, fmt.Sprintf(
				"%s is set in your shell environment, which overrides `go env -w` — update it in your shell profile:  export %s='%s'",
				v, v, next))
			continue
		}
		p.fixes = append(p.fixes, fmt.Sprintf("go env -w %s='%s'", v, next))
		if p.goEnvSet == nil {
			p.goEnvSet = map[string]string{}
		}
		p.goEnvSet[v] = next
	}
	return p
}

// applyAccessFixes writes the `go env -w` fixes in rep, reporting each, and
// returns whether it changed anything. The values were built from the current
// setting plus twip's pattern (appendGlob), so this extends a user's GOPRIVATE
// rather than replacing it — `go env -w` itself has no append mode. Variables set
// in the shell are never in goEnvSet: `go env -w` can't override them.
func applyAccessFixes(ctx context.Context, out io.Writer, rep accessReport) (bool, error) {
	var vars []string
	set := map[string]string{}
	for _, p := range rep.problems {
		for k, v := range p.goEnvSet {
			if _, dup := set[k]; !dup {
				vars = append(vars, k)
			}
			set[k] = v
		}
	}
	if len(vars) == 0 {
		return false, nil
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		return false, fmt.Errorf("the `go` toolchain is not on PATH")
	}
	sort.Strings(vars)
	for _, k := range vars {
		if msg, err := exec.CommandContext(ctx, goBin, "env", "-w", k+"="+set[k]).CombinedOutput(); err != nil {
			return true, fmt.Errorf("go env -w %s: %v: %s", k, err, strings.TrimSpace(string(msg)))
		}
		fmt.Fprintf(out, "  → fixed: go env -w %s='%s'\n", k, set[k])
	}
	return true, nil
}

func checkGitAuth(ctx context.Context, dir string) *accessProblem {
	kind, msg := classifyGitErr(authProbe(ctx, dir))
	if kind == gitOK {
		return nil
	}
	rewrite := sshRewrite(ctx, dir)
	gh := ghState(ctx)
	return gitAuthProblem(kind, msg, rewrite, gh)
}

// ghStatus is how far a user's GitHub CLI is set up.
type ghStatus int

const (
	ghMissing ghStatus = iota
	ghLoggedOut
	ghLoggedIn
)

func ghState(ctx context.Context) ghStatus {
	bin, err := exec.LookPath("gh")
	if err != nil {
		return ghMissing
	}
	if exec.CommandContext(ctx, bin, "auth", "status", "--hostname", "github.com").Run() != nil {
		return ghLoggedOut
	}
	return ghLoggedIn
}

// sshRewrite returns the url.<base>.insteadOf key that rewrites twip's HTTPS URL to
// another (typically SSH) URL, or "" when none applies.
func sshRewrite(ctx context.Context, dir string) string {
	out, err := gitutil.Run(ctx, dir, nil, nil, "config", "--get-regexp", `^url\..*\.insteadof$`)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		key, prefix, ok := strings.Cut(line, " ")
		if ok && prefix != "" && strings.HasPrefix(moduleRepoURL, prefix) {
			return key
		}
	}
	return ""
}

// gitAuthProblem turns a failed authenticated probe into a diagnosis with fixes
// suited to how this user's git and gh are set up.
func gitAuthProblem(kind gitErrKind, msg, rewrite string, gh ghStatus) *accessProblem {
	setupGh := map[ghStatus][]string{
		ghLoggedIn:  {"gh auth setup-git"},
		ghLoggedOut: {"gh auth login", "gh auth setup-git"},
		ghMissing:   {"install the GitHub CLI (https://cli.github.com), then:  gh auth login && gh auth setup-git"},
	}[gh]

	switch kind {
	case gitNeedsAuth:
		p := &accessProblem{summary: "git has no GitHub credentials it can use without prompting (`go` disables prompts): " + msg}
		p.fixes = append(p.fixes, setupGh...)
		if gh == ghMissing {
			p.fixes = append(p.fixes, `or use SSH:  git config --global url."git@github.com:".insteadOf "https://github.com/"`)
		}
		return p
	case gitSSH:
		p := &accessProblem{summary: "git fetches GitHub over SSH and SSH authentication failed: " + msg}
		if rewrite != "" {
			p.summary = fmt.Sprintf("git rewrites GitHub HTTPS URLs to SSH (%s) and SSH authentication failed: %s", rewrite, msg)
		}
		p.fixes = append(p.fixes, "check your key works:  ssh -T git@github.com")
		if rewrite != "" {
			alt := "or drop the rewrite and use HTTPS:  git config --global --unset " + rewrite
			if gh != ghMissing {
				alt += " && " + strings.Join(setupGh, " && ")
			}
			p.fixes = append(p.fixes, alt)
		}
		return p
	case gitBadCreds:
		p := &accessProblem{summary: "GitHub rejected the credentials git sent: " + msg}
		switch gh {
		case ghLoggedIn:
			p.fixes = append(p.fixes,
				"another credential helper may be answering first with a stale token — check `git config --get-all credential.https://github.com.helper`, then:  gh auth setup-git")
		case ghLoggedOut:
			p.fixes = append(p.fixes, "your gh login has expired or is invalid:  gh auth login", "gh auth setup-git")
		default:
			p.fixes = append(p.fixes, "refresh the token your git credential helper stores for github.com, or use gh:  gh auth login && gh auth setup-git")
		}
		return p
	case gitNoAccess:
		p := &accessProblem{summary: fmt.Sprintf("git authenticated to GitHub but cannot read %s: %s", repoName, msg)}
		p.fixes = append(p.fixes, "ask for read access to "+moduleRepoURL)
		if gh == ghLoggedIn {
			p.fixes = append(p.fixes,
				"check `gh auth status` shows the account that has access (and a token with `repo` scope), then:  gh auth setup-git")
		}
		return p
	case gitNetwork:
		return &accessProblem{summary: "could not reach github.com with your git config: " + msg}
	}
	return &accessProblem{summary: "git could not fetch " + moduleRepoURL + ": " + msg}
}

// printAccessReport writes the diagnosis in doctor's style and reports whether it
// found a problem.
func printAccessReport(out io.Writer, rep accessReport) bool {
	switch {
	case rep.skipped != "":
		fmt.Fprintf(out, "  ? skipped: %s\n", rep.skipped)
		return false
	case rep.public:
		fmt.Fprintf(out, "  ✓ %s is public — `go install` needs no GOPRIVATE or git login\n", modulePath)
		return false
	case len(rep.problems) == 0:
		fmt.Fprintf(out, "  ✓ %s is private and this machine can fetch it (GOPRIVATE + git auth OK)\n", modulePath)
		return false
	}
	fixable := false
	for _, p := range rep.problems {
		fmt.Fprintf(out, "  ⚠ %s\n", p.summary)
		for _, f := range p.fixes {
			fmt.Fprintf(out, "      fix: %s\n", f)
		}
		fixable = fixable || len(p.goEnvSet) > 0
	}
	if fixable {
		fmt.Fprintln(out, "      (`twip doctor --fix` applies the go env fix, keeping your existing patterns)")
	}
	return true
}

// matchGlobs reports whether target matches any of the comma-separated glob
// patterns in list, using GOPRIVATE's rule: a pattern matches a path prefix with
// the same number of elements (so "github.com/org/*" matches every module in org).
func matchGlobs(list, target string) bool {
	for _, pat := range strings.Split(list, ",") {
		pat = strings.Trim(strings.TrimSpace(pat), "/")
		if pat == "" {
			continue
		}
		n := strings.Count(pat, "/") + 1
		elems := strings.SplitN(target, "/", n+1)
		if len(elems) < n {
			continue
		}
		prefix := strings.Join(elems[:n], "/")
		if ok, _ := path.Match(pat, prefix); ok {
			return true
		}
	}
	return false
}

// onlyDirect reports whether GOPROXY never consults a proxy, in which case every
// module is already fetched direct.
func onlyDirect(goproxy string) bool {
	return strings.TrimSpace(goproxy) == "direct"
}

// appendGlob adds pat to a comma-separated glob list, keeping what's already there.
func appendGlob(list, pat string) string {
	list = strings.Trim(strings.TrimSpace(list), ",")
	if list == "" {
		return pat
	}
	return list + "," + pat
}
