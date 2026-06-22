#!/bin/sh
# Install (or update) twip globally:
#   1. build the binary with `go install`, then
#   2. promote it to the stable per-machine install via `twip install`
#      (stable copy at ~/.twip/bin/twip + git shim + PATH wiring).
# Idempotent — re-run any time to update.
#
# No clone required:
#     curl -fsSL https://raw.githubusercontent.com/codespeak-dev/twip/main/scripts/install.sh | sh
#
# From a checkout (installs the local source — e.g. an unreleased change):
#     ./scripts/install.sh
#
# Knobs:
#   TWIP_VERSION   version/branch/commit to fetch when not run from a checkout
#                  (default "latest" = newest release tag; use "main" for the
#                  latest commit, or a tag like "v0.3.2").
#   Extra args are forwarded to `twip install` (e.g. --no-modify-path, -y).
set -eu

MODULE="github.com/codespeak-dev/twip"
VERSION="${TWIP_VERSION:-latest}"

command -v go >/dev/null 2>&1 || {
	echo "twip needs Go to build (https://go.dev/dl). Install Go, then re-run." >&2
	exit 1
}

# Install from a local checkout when this script lives in one (gives you exactly
# the source you have); otherwise fetch by version, so no clone is needed.
src=""
here=$(unset CDPATH; cd -- "$(dirname -- "$0")" 2>/dev/null && pwd) || here=""
if [ -n "$here" ] && [ -f "$here/../go.mod" ] && grep -q "^module $MODULE\$" "$here/../go.mod" 2>/dev/null; then
	src="$here/.."
fi

if [ -n "$src" ]; then
	echo "Installing twip from local checkout ($src)…"
	( cd "$src" && go install ./cmd/twip )
else
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
