#!/usr/bin/env bash
# Smoke test for a running yk.gy environment.
#
#   scripts/smoke-test.sh <base_url> [--env <name>] [--expect-commit <sha>] [--write] [--deep]
#
#   --env            expected value of "environment" in /healthz
#   --expect-commit  wait (up to SMOKE_WAIT seconds) until /healthz reports this commit
#   --write          also issue ONE short link to a dedicated test URL and follow it
#                    (local / development only; production smoke tests are read-only)
#   --deep           call /healthz?deep=1 (verifies Safe Browsing + API key, no writes)
#
# Production smoke tests never create links: they only use the pre-seeded smoke
# link "SmokeT1" and requests that are rejected before anything is stored.
set -euo pipefail

BASE="${1:?usage: smoke-test.sh <base_url> [...]}"; shift
BASE="${BASE%/}"
ENV_EXPECT=""; COMMIT=""; WRITE=0; DEEP=0
while [ $# -gt 0 ]; do
  case "$1" in
    --env) ENV_EXPECT="$2"; shift 2;;
    --expect-commit) COMMIT="$2"; shift 2;;
    --write) WRITE=1; shift;;
    --deep) DEEP=1; shift;;
    *) echo "unknown option $1" >&2; exit 2;;
  esac
done
WAIT="${SMOKE_WAIT:-300}"
SMOKE_TARGET="https://example.com/?yk-gy-smoke=1"
UA="yk-gy-smoke-test"
pass=0; fail=0; warn=0
ok()   { echo "  ✅ $*"; pass=$((pass+1)); }
ng()   { echo "  ❌ $*"; fail=$((fail+1)); }
wn()   { echo "  ⚠️  $*"; warn=$((warn+1)); }
req()  { curl -sS -A "$UA" --max-time 15 "$@"; }

echo "== Smoke test: $BASE"

# 1. Health (and wait for propagation of the expected commit)
deadline=$(( $(date +%s) + WAIT ))
while :; do
  body=$(req -w '\n%{http_code}' "$BASE/healthz" 2>/dev/null || echo $'\n000')
  code=${body##*$'\n'}; json=${body%$'\n'*}
  cur=$(printf '%s' "$json" | sed -n 's/.*"commit":"\([^"]*\)".*/\1/p')
  if [ "$code" = 200 ] && { [ -z "$COMMIT" ] || [ "${cur#"$COMMIT"}" != "$cur" ] || [ "$cur" = "$COMMIT" ]; }; then break; fi
  if [ "$(date +%s)" -ge "$deadline" ]; then break; fi
  echo "  … waiting for $BASE/healthz (http=$code commit=${cur:-?})"; sleep 10
done
if [ "$code" = 200 ]; then ok "healthz 200: $json"; else ng "healthz returned $code: $json"; fi
if [ -n "$COMMIT" ]; then
  case "$cur" in "$COMMIT"*) ok "commit matches $COMMIT";; *) ng "commit is '${cur}', expected '$COMMIT'";; esac
fi
if [ -n "$ENV_EXPECT" ]; then
  printf '%s' "$json" | grep -q "\"environment\":\"$ENV_EXPECT\"" && ok "environment=$ENV_EXPECT" || ng "environment mismatch (expected $ENV_EXPECT)"
fi
if [ "$DEEP" = 1 ]; then
  d=$(req -w '\n%{http_code}' "$BASE/healthz?deep=1" || true)
  [ "${d##*$'\n'}" = 200 ] && ok "deep health (Safe Browsing reachable, key valid)" || ng "deep health failed: ${d%$'\n'*}"
fi

# 2. Top page + security headers
hdrs=$(req -D - -o /dev/null "$BASE/")
echo "$hdrs" | head -1 | grep -q ' 200' && ok "GET / 200" || ng "GET / not 200"
echo "$hdrs" | grep -qi '^content-security-policy:' && ok "CSP header present" || ng "CSP header missing"

# 3. Redirect of the pre-seeded smoke link
loc=$(req -o /dev/null -w '%{http_code} %{redirect_url}' "$BASE/SmokeT1")
[ "$loc" = "302 $SMOKE_TARGET" ] && ok "GET /SmokeT1 -> 302 $SMOKE_TARGET" || ng "GET /SmokeT1 returned '$loc' (seed missing? see scripts/fastly-setup.sh)"

# 4. Unknown code
c=$(req -o /dev/null -w '%{http_code}' "$BASE/Zz0Zz0Z")
[ "$c" = 404 ] && ok "unknown code -> 404" || ng "unknown code returned $c"

# 5. Rejections (nothing is stored for these)
post() { req -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' -H "Origin: $BASE" -d "$1" "$BASE/api/shorten"; }
c=$(post '{"url":"javascript:alert(1)"}'); [ "$c" = 400 ] && ok "javascript: rejected (400)" || ng "javascript: returned $c"
c=$(post '{"url":"http://127.0.0.1/"}');   [ "$c" = 400 ] && ok "loopback rejected (400)" || ng "loopback returned $c"
host=$(printf '%s' "$BASE" | sed -E 's#^https?://([^/:]+).*#\1#')
c=$(post "{\"url\":\"https://$host/SmokeT1\"}"); [ "$c" = 400 ] && ok "recursive rejected (400)" || ng "recursive returned $c"

# 6. Optional write test (one link to a dedicated test URL)
if [ "$WRITE" = 1 ]; then
  target="https://example.com/?yk-gy-smoke=${GITHUB_RUN_ID:-local}-$(date +%s)"
  resp=$(req -w '\n%{http_code}' -H 'Content-Type: application/json' -H "Origin: $BASE" -d "{\"url\":\"$target\"}" "$BASE/api/shorten")
  code=${resp##*$'\n'}; json=${resp%$'\n'*}
  short=$(printf '%s' "$json" | sed -n 's/.*"short_url":"\([^"]*\)".*/\1/p')
  scode=$(printf '%s' "$json" | sed -n 's/.*"code":"\([^"]*\)".*/\1/p')
  if [ "$code" = 201 ] && printf '%s' "$scode" | grep -Eq '^[0-9A-Za-z]{7}$'; then
    ok "issued $short"
    loc=""
    for _ in 1 2 3 4 5 6; do # KV is eventually consistent across POPs
      loc=$(req -o /dev/null -w '%{http_code} %{redirect_url}' "$BASE/$scode")
      [ "$loc" = "302 $target" ] && break; sleep 5
    done
    [ "$loc" = "302 $target" ] && ok "new link redirects (302)" || ng "new link returned '$loc'"
  else
    ng "issuing link failed: $code $json"
  fi
  c=$(post '{"url":"http://testsafebrowsing.appspot.com/s/malware.html"}')
  [ "$c" = 422 ] && ok "Safe Browsing test malware URL rejected (422)" || wn "malware test URL returned $c (expected 422)"
fi

echo "== Result: $pass passed, $fail failed, $warn warnings"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  echo "### Smoke test \`$BASE\`: $pass passed, $fail failed, $warn warnings" >> "$GITHUB_STEP_SUMMARY"
fi
[ "$fail" = 0 ]
