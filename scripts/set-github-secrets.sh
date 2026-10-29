#!/usr/bin/env bash
# Stores the deployment secrets in the GitHub repository (Actions secrets).
# Requires an authenticated GitHub CLI:  gh auth login
#
#   ./scripts/set-github-secrets.sh [owner/repo]
#
# Reads the values from $CLEADS_SECRETS_DIR (default ~/.config/cleads):
#   vps.env            VPS_HOST=...  VPS_DEPLOY_USER=deploy
#   deploy_ed25519     private key of the CI deploy user
#   known_hosts        pinned SSH host keys of the VPS
set -euo pipefail

REPO="${1:-infinitydev4/cleads-extractor}"
DIR="${CLEADS_SECRETS_DIR:-$HOME/.config/cleads}"

command -v gh >/dev/null || { echo "gh is not installed" >&2; exit 1; }
gh auth status >/dev/null 2>&1 || { echo "gh is not authenticated: run 'gh auth login' first" >&2; exit 1; }
for f in vps.env deploy_ed25519 known_hosts; do
  [ -s "$DIR/$f" ] || { echo "missing $DIR/$f" >&2; exit 1; }
done

val() { sed -n "s/^$1=//p" "$DIR/vps.env" | tail -n1; }

gh secret set VPS_HOST --repo "$REPO" --body "$(val VPS_HOST)"
gh secret set VPS_USER --repo "$REPO" --body "$(val VPS_DEPLOY_USER)"
gh secret set VPS_SSH_KEY --repo "$REPO" <"$DIR/deploy_ed25519"
gh secret set VPS_KNOWN_HOSTS --repo "$REPO" <"$DIR/known_hosts"

echo "secrets set on $REPO:"
gh secret list --repo "$REPO"
