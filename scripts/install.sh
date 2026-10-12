#!/bin/sh
# Install (or update) twip globally:
#   1. build the binary with `go install`, then
#   2. promote it to the stable per-machine install via `twip install`
#      (stable copy at ~/.twip/bin/twip + git shim + PATH wiring).
# Idempotent — re-run any time to update.
#
# The repo is private, so you need read access to codespeak-dev/twip and a
# GitHub login git can use. With the GitHub CLI logged in, no clone required:
#     gh api repos/codespeak-dev/twip/contents/scripts/install.sh -H 'Accept: application/vnd.github.raw' | sh
#
# From a checkout (installs the local source — e.g. an unreleased change):
#     ./scripts/install.sh
#
# Before building, it sets up what `go install` / `twip update` need to fetch a
# private module: it adds github.com/codespeak-dev/* to GOPRIVATE (appending —
# existing patterns are kept), and if git can't authenticate to GitHub without
# prompting but gh is logged in, it runs `gh auth setup-git`.
#
# Knobs:
#   TWIP_VERSION   version/branch/commit to fetch when not run from a checkout
#                  (default "latest" = newest release tag; use "main" for the
#                  latest commit, or a tag like "v0.3.2").
#   Extra args are forwarded to `twip install` (e.g. --no-modify-path, -y).
set -eu

MODULE="github.com/codespeak-dev/twip"
PRIVATE_PATTERN="github.com/codespeak-dev/*"
VERSION="${TWIP_VERSION:-latest}"

command -v go >/dev/null 2>&1 || {
	echo "twip needs Go to build (https://go.dev/dl). Install Go, then re-run." >&2
	exit 1
}

# ensure_goprivate makes Go fetch twip direct from GitHub instead of the public
# proxy/checksum DB, which can't see a private repo. `go env -w` has no append
# mode, so build the new value from the current one rather than overwriting it.
ensure_goprivate() {
	cur=$(go env GOPRIVATE)
	case ",$cur," in *",$PRIVATE_PATTERN,"*) return 0 ;; esac
	next="${cur:+$cur,}$PRIVATE_PATTERN"
	if [ -n "${GOPRIVATE:-}" ]; then
		# Set in the shell: `go env -w` would be ignored. Use it for this run and
		# tell the user to persist it.
		export GOPRIVATE="$next"
		echo "GOPRIVATE is set in your shell, so add twip's pattern there (e.g. in your shell profile):" >&2
		echo "    export GOPRIVATE='$next'" >&2
		return 0
	fi
	echo "Adding $PRIVATE_PATTERN to GOPRIVATE:  go env -w GOPRIVATE='$next'"
	go env -w GOPRIVATE="$next"
}

# can_fetch reports whether git can read the repo the way `go` does: no prompts,
# ssh in batch mode.
can_fetch() {
	(
		export GIT_TERMINAL_PROMPT=0 GCM_INTERACTIVE=never
		[ -n "${GIT_SSH:-}${GIT_SSH_COMMAND:-}" ] || export GIT_SSH_COMMAND="ssh -o BatchMode=yes"
		git ls-remote "https://$MODULE" HEAD >/dev/null 2>&1
	)
}

# ensure_git_auth makes sure git can authenticate to GitHub non-interactively
# (`go` disables prompts), wiring up gh's credential helper when that's the
# missing piece. Fatal only when the build needs the fetch; a checkout install
# just warns, since only later `twip update`s depend on it.
ensure_git_auth() {
	can_fetch && return 0
	if command -v gh >/dev/null 2>&1 && gh auth status --hostname github.com >/dev/null 2>&1; then
		echo "git can't authenticate to GitHub without prompting; letting it use your gh login:  gh auth setup-git"
		gh auth setup-git
		can_fetch && return 0
	fi
	{
		echo "git can't read https://$MODULE without prompting, so \`go install\` can't fetch twip."
		echo "Fix one of:"
		echo "  - GitHub CLI:  gh auth login && gh auth setup-git"
		echo "  - SSH:         git config --global url.\"git@github.com:\".insteadOf \"https://github.com/\""
		echo "and make sure your GitHub account has read access to codespeak-dev/twip."
		echo "\`twip doctor\` diagnoses this in more detail once twip is installed."
	} >&2
	return 1
}

# Install from a local checkout when this script lives in one (gives you exactly
# the source you have); otherwise fetch by version, so no clone is needed.
src=""
here=$(unset CDPATH; cd -- "$(dirname -- "$0")" 2>/dev/null && pwd) || here=""
if [ -n "$here" ] && [ -f "$here/../go.mod" ] && grep -q "^module $MODULE\$" "$here/../go.mod" 2>/dev/null; then
	src="$here/.."
fi

ensure_goprivate
if [ -n "$src" ]; then
	ensure_git_auth || echo "(continuing: the local build doesn't need it, but \`twip update\` will)" >&2
	echo "Installing twip from local checkout ($src)…"
	( cd "$src" && go install ./cmd/twip )
else
	ensure_git_auth || exit 1
	echo "Installing twip from $MODULE/cmd/twip@$VERSION…"
	go install "$MODULE/cmd/twip@$VERSION"
fi

# Locate the binary `go install` just produced.
gobin="$(go env GOBIN)"
[ -n "$gobin" ] || gobin="$(go env GOPATH)/bin"
twip="$gobin/twip"
[ -x "$twip" ] || {
	echo "expected the twip binary at $twip but it isn't there" >&2
	exit 1
}

# Promote to the stable global install. This MUST be `install`, not
# `shim install`: only `install` self-copies the new binary over
# ~/.twip/bin/twip — the path the agent hooks and git shim actually run — so a
# re-run genuinely updates it. (`shim install` would keep the old stable copy.)
echo
"$twip" install "$@"

echo
echo "Next — enable recording in a repo (run once, then commit the hooks):"
echo "    twip init && git add .claude/settings.json && git commit -m 'twip: record agent sessions'"
echo "Browse the timeline any time with:  twip serve"
