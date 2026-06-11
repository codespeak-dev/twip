#!/usr/bin/env bash
# Install twip from this checkout: build the binary, install the git shim, and
# print the PATH + JetBrains setup. Safe to re-run.
set -euo pipefail

cd "$(dirname "$0")/.."

echo "Building + installing twip…"
go install ./cmd/twip

gobin="$(go env GOBIN)"
[ -n "$gobin" ] || gobin="$(go env GOPATH)/bin"
twip="$gobin/twip"
shimdir="$HOME/.twip/bin"

echo "Installing git shim…"
"$twip" shim install --dir "$shimdir" >/dev/null

echo
echo "Installed:"
echo "  $("$twip" version | head -1)  ->  $twip"
echo "  git shim                ->  $shimdir/git"
echo
echo "1. Put both on your PATH (shim FIRST so it shadows git). Add to ~/.zshrc:"
echo
echo "       export PATH=\"$shimdir:$gobin:\$PATH\""
echo
echo "   then: source ~/.zshrc   (or open a new terminal)"
echo
echo "2. JetBrains bypasses PATH — set Settings → Version Control → Git →"
echo "   'Path to Git executable' to:"
echo "       $shimdir/git"
echo
echo "3. In each repo you want recorded, run once and commit the result:"
echo "       twip init && git add .claude/settings.json && git commit -m 'twip: record sessions'"
echo
echo "Browse anytime with:  twip serve"
