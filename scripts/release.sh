#!/usr/bin/env bash
# Helpers used by the deploy / rollback workflows (also usable read-only locally).
#
#   release.sh active <service_id>             -> active version number (empty if none)
#   release.sh base <service_id>               -> "active" if a version is active, else "latest"
#   release.sh describe <service_id> <version> -> JSON of one version
#   release.sh good <env>                      -> known-good versions "version sha" (from git tags), newest first
#   release.sh tag <env> <version> <sha> <msg> -> create + push annotated tag deployed/<env>/v<version>
#
# Known-good releases are recorded as git tags `deployed/<env>/v<fastly_version>`
# pointing at the deployed commit; they are only created after the smoke test passed.
set -euo pipefail
cmd="${1:-}"; shift || true

versions() { fastly service version list --service-id "$1" --json; }

case "$cmd" in
  active)
    versions "$1" | jq -r '[(. // [])[] | select(.Active == true)][0].Number // empty';;
  base)
    a=$(versions "$1" | jq -r '[(. // [])[] | select(.Active == true)][0].Number // empty')
    if [ -n "$a" ]; then echo active; else echo latest; fi;;
  describe)
    versions "$1" | jq --argjson v "$2" '(. // [])[] | select(.Number == $v)';;
  good)
    env="$1"
    git fetch -q --force origin "refs/tags/deployed/$env/*:refs/tags/deployed/$env/*" 2>/dev/null || true
    git for-each-ref --format='%(refname:strip=3)' "refs/tags/deployed/$env/" |
      while read -r tag; do
        v="${tag#v}"; sha=$(git rev-list -n1 "refs/tags/deployed/$env/$tag")
        echo "$v $sha"
      done | sort -rn;;
  tag)
    env="$1"; ver="$2"; sha="$3"; msg="$4"
    name="deployed/$env/v$ver"
    git config user.name  "github-actions[bot]"
    git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
    git tag -f -a "$name" "$sha" -m "$msg"
    git push -f origin "refs/tags/$name";;
  *)
    sed -n '2,12p' "$0"; exit 2;;
esac
