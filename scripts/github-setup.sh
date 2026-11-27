#!/usr/bin/env bash
# Configure GitHub for the yk.gy branch / deploy model.
#
#   scripts/github-setup.sh            # dry run: show what would be configured
#   scripts/github-setup.sh --apply    # apply (needs repo admin; uses your gh login)
#
# Creates:
#   - Environments `development` (deployable from dev only) and `production`
#     (deployable from main only, required reviewer = you, when the plan allows)
#   - Branch protection for main: PR required, CI checks required, no force
#     push / deletion, admins included.
#   - Branch protection for dev: no force push / deletion.
#
# Secrets are NOT set here. Set them yourself so they never appear in chat or
# shell history, e.g.:  gh secret set FASTLY_API_TOKEN --env production
# (gh prompts for the value with hidden input).
set -euo pipefail
APPLY=0; [ "${1:-}" = "--apply" ] && APPLY=1
REPO="${REPO:-$(gh repo view --json nameWithOwner --jq .nameWithOwner)}"
ME_ID=$(gh api user --jq .id)
CHECKS='["Branch policy","Lint & unit tests","Wasm build & integration tests"]'

api() { # method path json
  if [ "$APPLY" = 1 ]; then
    echo "+ gh api -X $1 $2" >&2
    local out
    if ! out=$(printf '%s' "$3" | gh api -X "$1" "$2" --input - 2>&1); then
      LAST_ERR=$(printf '%s' "$out" | tail -1)
      echo "  -> failed: $LAST_ERR" >&2
      return 1
    fi
  else
    echo "[dry-run] gh api -X $1 $2 $3" >&2
  fi
}

fallback=()
LAST_ERR=""

echo "== Repository: $REPO"

# --------------------------------------------------------------- environments
api PUT "repos/$REPO/environments/development" '{"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}}'
api POST "repos/$REPO/environments/development/deployment-branch-policies" '{"name":"dev","type":"branch"}' || true

prod_env=$(jq -nc --argjson id "$ME_ID" '{wait_timer:0, prevent_self_review:false, reviewers:[{type:"User",id:$id}], deployment_branch_policy:{protected_branches:false,custom_branch_policies:true}}')
if ! api PUT "repos/$REPO/environments/production" "$prod_env"; then
  echo "Required reviewers are not available on this plan/repository; creating production without reviewers." >&2
  api PUT "repos/$REPO/environments/production" '{"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}}'
  fallback+=("production: no required reviewers -> keep PROD_DEPLOY_ON_MERGE unset; deploy manually with workflow_dispatch + confirmation phrase")
fi
api POST "repos/$REPO/environments/production/deployment-branch-policies" '{"name":"main","type":"branch"}' || true

# --------------------------------------------------------------- branch protection
main_rules=$(jq -nc --argjson checks "$CHECKS" '{
  required_status_checks: {strict: true, contexts: $checks},
  enforce_admins: true,
  required_pull_request_reviews: {required_approving_review_count: 0, dismiss_stale_reviews: true},
  restrictions: null,
  allow_force_pushes: false,
  allow_deletions: false,
  required_linear_history: false,
  required_conversation_resolution: true
}')
dev_rules='{"required_status_checks":null,"enforce_admins":false,"required_pull_request_reviews":null,"restrictions":null,"allow_force_pushes":false,"allow_deletions":false}'

if ! api PUT "repos/$REPO/branches/main/protection" "$main_rules"; then
  fallback+=("main: branch protection failed ($LAST_ERR). 'Branch not found' = push main first and re-run; 'Upgrade to GitHub Pro' = plan limit. Alternatives: GitHub Pro, a public repo, or rely on the built-in guards (CI 'Main push guard' + deploy-prod.yml refuses commits that did not come from a merged PR with green CI)")
fi
if ! api PUT "repos/$REPO/branches/dev/protection" "$dev_rules"; then
  fallback+=("dev: branch protection failed ($LAST_ERR)")
fi

if [ "$APPLY" = 1 ]; then
  echo "== Applied."
  if [ "${#fallback[@]}" -gt 0 ]; then
    echo "== Plan limitations detected:"; printf '  - %s\n' "${fallback[@]}"
  fi
fi
cat <<EOF

Next (run yourself; values are prompted, never typed on the command line):
  gh variable set FASTLY_SERVICE_ID --env development --body <dev service id>
  gh variable set BASE_URL          --env development --body https://dev.yk.gy
  gh secret   set FASTLY_API_TOKEN  --env development
  gh variable set FASTLY_SERVICE_ID --env production  --body <prod service id>
  gh variable set BASE_URL          --env production  --body https://yk.gy
  gh secret   set FASTLY_API_TOKEN  --env production
  # only if production has required reviewers:
  gh variable set PROD_DEPLOY_ON_MERGE --body true
EOF
