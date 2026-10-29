#!/usr/bin/env bash
# Creates /opt/cleads/.env with generated secrets on first install. Never
# overwrites an existing file.
#
#   DOMAIN=vps-xxxx.vps.ovh.net ./init-env.sh
set -euo pipefail
cd "$(dirname "$0")"

[ -f .env ] && { echo ".env already exists: left untouched"; exit 0; }
: "${DOMAIN:?usage: DOMAIN=host.example.com ./init-env.sh}"

gen() { openssl rand -base64 96 | tr -dc 'A-Za-z0-9' | cut -c1-"$1"; }

umask 077
{
  echo "DOMAIN=$DOMAIN"
  echo "IMAGE=${IMAGE:-ghcr.io/infinitydev4/cleads-extractor}"
  echo "IMAGE_TAG=${IMAGE_TAG:-latest}"
  echo "PUBLIC_URL=https://$DOMAIN"
  echo "POSTGRES_PASSWORD=$(gen 32)"
  echo "API_KEYS=admin:$(gen 48),crm:$(gen 48)"
  echo "WEBHOOK_SECRET=$(gen 48)"
  echo "GEOCODER_USER_AGENT=CLEADS-Business-Extractor/1.0 (+https://github.com/infinitydev4/cleads-extractor)"
  echo "SOURCE_RATE=google_maps_http=4"
} >.env
chmod 600 .env
echo ".env created (secrets generated on this server, not displayed)"
