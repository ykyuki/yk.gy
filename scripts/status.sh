#!/usr/bin/env bash
# Show where you are: git branch / sync state, local server, Dev & Prod health,
# and the latest GitHub Actions runs. Read-only.
set -uo pipefail
PROD_URL="${PROD_URL:-https://yk.gy}"
DEV_URL="${DEV_URL:-https://dev.yk.gy}"
LOCAL_URL="${LOCAL_URL:-http://127.0.0.1:7676}"

hr() { printf '\n\033[1m== %s\033[0m\n' "$1"; }

hr "Git"
branch=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "?")
echo "branch : $branch ($(git rev-parse --short HEAD 2>/dev/null || echo 'no commits'))"
case "$branch" in
  main)      echo "env    : Production (yk.gy) - do not commit here; use a PR from dev or hotfix/*";;
  dev)       echo "env    : Development (dev.yk.gy) - pushes deploy automatically after CI";;
  feature/*) echo "env    : Local only - open a PR to dev when ready (make pr-dev)";;
  hotfix/*)  echo "env    : Hotfix - PR to main, then merge main back into dev";;
  *)         echo "env    : (non-standard branch name)";;
esac
git fetch -q origin 2>/dev/null || true
if git rev-parse -q --verify "origin/$branch" >/dev/null; then
  git rev-list --left-right --count "origin/$branch...HEAD" | awk -v b="$branch" '{print "remote : behind " $1 ", ahead " $2 " vs origin/" b}'
fi
echo "changes: $(git status --porcelain | wc -l | tr -d ' ') uncommitted file(s)"
if [ -n "${FASTLY_SERVICE_ID:-}" ]; then
  echo "WARNING: FASTLY_SERVICE_ID is set in your shell. Unset it so fastly commands cannot hit a remote service by accident."
fi

health() {
  local name="$1" url="$2" out
  out=$(curl -s --max-time 5 "$url/healthz" 2>/dev/null) || out=""
  if [ -n "$out" ]; then echo "$name: $out"; else echo "$name: unreachable ($url)"; fi
}
hr "Environments"
health "local      " "$LOCAL_URL"
health "development" "$DEV_URL"
health "production " "$PROD_URL"

if command -v gh >/dev/null 2>&1; then
  hr "GitHub Actions (latest runs)"
  gh run list --limit 8 2>/dev/null || echo "(gh not authenticated or no runs yet)"
fi
