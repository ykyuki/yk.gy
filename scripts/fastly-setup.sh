#!/usr/bin/env bash
# Reproducible creation of the Fastly resources for ONE environment.
#
#   scripts/fastly-setup.sh <production|development>            # dry run (read-only)
#   scripts/fastly-setup.sh <production|development> --apply    # create what is missing
#
# Everything is environment-scoped and named "yk-gy-<env>-*", and is linked to
# the service under the SAME logical names the Go code uses:
#
#   logical name   resource                         prod / dev physical name
#   ------------   ------------------------------   ---------------------------------
#   links          KV Store (short links)           yk-gy-<env>-links
#   app_config     Config Store (non-secret config) yk-gy-<env>-config
#   secrets        Secret Store (Safe Browsing key) yk-gy-<env>-secrets
#   safebrowsing   Backend safebrowsing.googleapis.com:443
#   app_log        Logging endpoint (created by you, provider of your choice)
#
# The script never deletes or overwrites data: stores/links/backends are only
# created when missing, config entries are upserted, the smoke-test link is
# inserted with --add (no overwrite), and the Safe Browsing key is only set
# when missing (or with --rotate-secret), read from a hidden prompt.
#
# It does NOT deploy or activate code; the first package is deployed by
# GitHub Actions (deploy-dev.yml / deploy-prod.yml).
set -euo pipefail

ENVIRONMENT="${1:-}"; shift || true
APPLY=0; ROTATE=0
for a in "$@"; do
  case "$a" in
    --apply) APPLY=1;;
    --rotate-secret) ROTATE=1;;
    *) echo "unknown option: $a" >&2; exit 2;;
  esac
done

case "$ENVIRONMENT" in
  production)  DOMAIN="yk.gy";     CACHE_TTL=3600; RATE=10;;
  development) DOMAIN="dev.yk.gy"; CACHE_TTL=60;   RATE=20;;
  *) echo "usage: $0 <production|development> [--apply] [--rotate-secret]" >&2; exit 2;;
esac

# Never let an ambient FASTLY_SERVICE_ID silently pick the target service.
unset FASTLY_SERVICE_ID

for bin in fastly jq; do command -v "$bin" >/dev/null || { echo "$bin is required" >&2; exit 1; }; done

PREFIX="yk-gy-${ENVIRONMENT}"
SERVICE_NAME="${PREFIX}"
KV_NAME="${PREFIX}-links"
CFG_NAME="${PREFIX}-config"
SEC_NAME="${PREFIX}-secrets"

run() {
  if [ "$APPLY" = 1 ]; then echo "+ $*" >&2; "$@"; else echo "[dry-run] $*" >&2; fi
}

echo "== Environment: $ENVIRONMENT ($DOMAIN)"
fastly whoami >/dev/null 2>&1 || { echo "Not authenticated. Run: fastly auth login --sso" >&2; exit 1; }

# Preflight: make sure every store API is usable BEFORE changing anything, so a
# permission problem cannot leave a half-configured service behind.
preflight_ok=1
for kind in kv-store config-store secret-store; do
  if ! err=$(fastly "$kind" list --json 2>&1 >/dev/null); then
    echo "PREFLIGHT FAILED: 'fastly $kind list' -> $(printf '%s' "$err" | grep -m1 'ERROR' || echo "$err" | head -1)" >&2
    preflight_ok=0
  fi
done
if [ "$preflight_ok" = 0 ]; then
  cat >&2 <<'MSG'
The current Fastly token cannot use one or more store APIs (403 = token scope /
role, or the product is not enabled on the account). Fix it, then re-run:
  - Create a token with "global" scope (Account > API tokens), or log in again:
      fastly auth login --sso
  - Make sure your user role can manage KV / Config / Secret Stores
    (Engineer or Superuser) and that KV Store is enabled for the account.
MSG
  [ "$APPLY" = 1 ] && { echo "Nothing was changed." >&2; exit 1; }
fi

if [ "$APPLY" = 1 ]; then
  echo "This will create missing Fastly resources for '$ENVIRONMENT' (may incur charges)."
  read -r -p "Type '$DOMAIN' to continue: " ans
  [ "$ans" = "$DOMAIN" ] || { echo "aborted"; exit 1; }
fi

# ---------------------------------------------------------------- service
SID=$(fastly service list --json | jq -r --arg n "$SERVICE_NAME" '[(. // [])[] | select(.Name == $n)][0].ServiceID // empty')
if [ -z "$SID" ]; then
  echo "Service '$SERVICE_NAME' not found -> will create (type wasm)"
  if [ "$APPLY" = 1 ]; then
    fastly service create --name "$SERVICE_NAME" --type wasm --comment "yk.gy $ENVIRONMENT" --non-interactive >&2
    SID=$(fastly service list --json | jq -r --arg n "$SERVICE_NAME" '[(. // [])[] | select(.Name == $n)][0].ServiceID // empty')
  else
    SID="<new-service-id>"
  fi
fi
echo "Service ID: $SID"

# Pick a version to edit: version 1 of a brand-new service, otherwise clone the active/latest one once.
VERSION="latest"
if [ "$APPLY" = 1 ]; then
  info=$(fastly service version list --service-id "$SID" --json)
  latest=$(echo "$info" | jq -r '(. // []) | max_by(.Number).Number')
  locked=$(echo "$info" | jq -r '(. // []) | max_by(.Number).Locked')
  if [ "$locked" = "true" ]; then
    fastly service version clone --service-id "$SID" --version "$latest" >&2
    latest=$((latest + 1))
  fi
  VERSION="$latest"
  echo "Editing service version $VERSION"
fi

have_backend() { [ "$APPLY" = 1 ] && fastly service backend list --service-id "$SID" --version "$VERSION" --json | jq -e --arg b "$1" 'any((. // [])[]; .Name == $b)' >/dev/null; }
have_link() { [ "$APPLY" = 1 ] && fastly service resource-link list --service-id "$SID" --version "$VERSION" --json | jq -e --arg n "$1" 'any((. // [])[]; (.name // .Name) == $n)' >/dev/null; }

# ---------------------------------------------------------------- domain
# Domains use the Domain Management API (account-level, not versioned); the
# classic version-scoped domain API is deprecated.
dom_json=$(fastly domain list --fqdn "$DOMAIN" --json 2>/dev/null | jq -c --arg d "$DOMAIN" '
  [((.data // .Data // []))[] | select((.fqdn // .FQDN) == $d)][0] // empty' 2>/dev/null || true)
dom_id=$(jq -r '.id // .ID // empty' <<<"${dom_json:-{\}}")
dom_owner=$(jq -r '.service_id // .ServiceID // empty' <<<"${dom_json:-{\}}")
if [ -z "$dom_json" ]; then
  run fastly domain create --fqdn "$DOMAIN" --service-id "$SID" --description "yk.gy $ENVIRONMENT"
elif [ "$dom_owner" = "$SID" ]; then
  echo "Domain $DOMAIN already bound to this service"
elif [ -z "$dom_owner" ]; then
  # e.g. created by a TLS subscription before the service existed
  echo "Domain $DOMAIN exists but is not bound to a service -> will bind it"
  run fastly domain update --domain-id "$dom_id" --service-id "$SID"
else
  echo "ERROR: domain $DOMAIN is bound to another service ($dom_owner), not $SID. Fix it in the Fastly UI (Domains) and re-run." >&2
  [ "$APPLY" = 1 ] && exit 1
fi

# ---------------------------------------------------------------- backend
have_backend safebrowsing || run fastly service backend create --service-id "$SID" --version "$VERSION" \
  --name safebrowsing --address safebrowsing.googleapis.com --port 443 --use-ssl \
  --override-host safebrowsing.googleapis.com \
  --ssl-cert-hostname safebrowsing.googleapis.com --ssl-sni-hostname safebrowsing.googleapis.com \
  --connect-timeout 2000 --first-byte-timeout 5000 --between-bytes-timeout 5000

# ---------------------------------------------------------------- stores
store_id() { # $1=kv-store|config-store|secret-store $2=name
  local out
  if ! out=$(fastly "$1" list --json 2>&1); then
    echo "WARNING: 'fastly $1 list' failed (${out##*ERROR: }). Check that the product is enabled on the account and that the token has access." >&2
    [ "$APPLY" = 1 ] && exit 1
    return 0
  fi
  echo "$out" | jq -r --arg n "$2" '
    (if type == "object" then (.Data // .data // []) else (. // []) end)
    | [.[] | select((.Name // .name) == $n)][0] | (.StoreID // .ID // .id // empty)'
}

KV_ID=$(store_id kv-store "$KV_NAME")
[ -n "$KV_ID" ] || { run fastly kv-store create --name "$KV_NAME"; [ "$APPLY" = 1 ] && KV_ID=$(store_id kv-store "$KV_NAME"); }
CFG_ID=$(store_id config-store "$CFG_NAME")
[ -n "$CFG_ID" ] || { run fastly config-store create --name "$CFG_NAME"; [ "$APPLY" = 1 ] && CFG_ID=$(store_id config-store "$CFG_NAME"); }
SEC_ID=$(store_id secret-store "$SEC_NAME")
[ -n "$SEC_ID" ] || { run fastly secret-store create --name "$SEC_NAME"; [ "$APPLY" = 1 ] && SEC_ID=$(store_id secret-store "$SEC_NAME"); }
echo "KV: ${KV_ID:-<new>}  Config: ${CFG_ID:-<new>}  Secret: ${SEC_ID:-<new>}"

have_link links      || run fastly service resource-link create --service-id "$SID" --version "$VERSION" --resource-id "${KV_ID:-<kv>}"  --name links
have_link app_config || run fastly service resource-link create --service-id "$SID" --version "$VERSION" --resource-id "${CFG_ID:-<cfg>}" --name app_config
have_link secrets    || run fastly service resource-link create --service-id "$SID" --version "$VERSION" --resource-id "${SEC_ID:-<sec>}" --name secrets

# ---------------------------------------------------------------- config entries (upsert)
cfg() { run fastly config-store-entry update --store-id "${CFG_ID:-<cfg>}" --key "$1" --value "$2" --upsert; }
cfg environment "$ENVIRONMENT"
cfg base_url "https://$DOMAIN"
# Every hostname that serves a yk.gy service is "our own": refusing them
# prevents chaining short links. This includes the *.edgecompute.app default
# domain that `fastly compute deploy` attaches on the first deploy, so re-run
# this script after the first deploy.
short_hosts="yk.gy,dev.yk.gy,www.yk.gy"
for other in yk-gy-production yk-gy-development; do
  osid=$(fastly service list --json | jq -r --arg n "$other" '[(. // [])[] | select(.Name == $n)][0].ServiceID // empty')
  [ -n "$osid" ] || continue
  for h in $(fastly service domain list --service-id "$osid" --version latest --json 2>/dev/null | jq -r '(. // [])[] | .Name'); do
    short_hosts="$short_hosts,$h"
  done
done
echo "short_hosts = $short_hosts"
cfg short_hosts "$short_hosts"
cfg safebrowsing_mode "live"
cfg ratelimit_per_minute "$RATE"
cfg ratelimit_penalty_seconds "300"
cfg ratelimit_on_error "deny"
cfg redirect_cache_ttl_seconds "$CACHE_TTL"

# ---------------------------------------------------------------- secret (hidden prompt, never echoed)
has_secret=0
if [ "$APPLY" = 1 ] && fastly secret-store-entry list --store-id "$SEC_ID" --json 2>/dev/null | jq -e '
    (if type == "object" then (.Data // .data // []) else (. // []) end) | any((. // [])[]; (.Name // .name) == "safebrowsing_api_key")' >/dev/null; then
  has_secret=1
fi
if [ "$has_secret" = 0 ] || [ "$ROTATE" = 1 ]; then
  if [ "$APPLY" = 1 ]; then
    echo "Enter the Google Safe Browsing API key for $ENVIRONMENT (input hidden):"
    flag=""; [ "$has_secret" = 1 ] && flag="--recreate"
    # The CLI reads the value from a hidden prompt; it is never put on the command line or in logs.
    fastly secret-store-entry create --store-id "$SEC_ID" --name safebrowsing_api_key $flag
  else
    echo "[dry-run] fastly secret-store-entry create --store-id <sec> --name safebrowsing_api_key   (hidden prompt)"
  fi
fi

# ---------------------------------------------------------------- smoke-test seed (no overwrite)
SMOKE='{"url":"https://example.com/?yk-gy-smoke=1","created_at":"2026-01-01T00:00:00Z"}'
if [ "$APPLY" = 1 ]; then
  fastly kv-store-entry create --store-id "$KV_ID" --key SmokeT1 --value "$SMOKE" --add >/dev/null 2>&1 \
    && echo "Seeded smoke link SmokeT1" || echo "Smoke link SmokeT1 already exists (kept)"
else
  echo "[dry-run] fastly kv-store-entry create --store-id <kv> --key SmokeT1 --value '$SMOKE' --add"
fi

if [ "$APPLY" = 1 ]; then
  if fastly service version validate --service-id "$SID" --version "$VERSION" >/dev/null 2>&1; then
    echo "Service version $VERSION is valid."
  else
    echo "NOTE: version $VERSION is not valid yet because no Compute package has been uploaded."
    echo "      This is expected; the first GitHub Actions deploy uploads the package and activates it."
  fi
fi

cat <<EOF

== Next steps for $ENVIRONMENT
1. GitHub Environment '$ENVIRONMENT' -> Variables:
     FASTLY_SERVICE_ID = $SID
     BASE_URL          = https://$DOMAIN
   Secret:
     FASTLY_API_TOKEN  = (a token scoped to service $SID only; see README)
2. Logging: create an endpoint named 'app_log' on this service with your provider, e.g.
     fastly service logging <provider> create --service-id $SID --version $VERSION --name app_log ...
3. Deploy from GitHub Actions; then configure DNS/TLS for $DOMAIN (see README).
   (Rate limiting uses the Compute cache; no extra product such as ERL is needed.)
EOF
