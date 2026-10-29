#!/usr/bin/env bash
# Logical backup of the database (custom format, restorable with pg_restore).
# Verifies that the archive is readable before keeping it, then purges the
# ones older than KEEP_DAYS. Run nightly by /etc/cron.d/cleads-backup.
#
# Restore:  docker compose exec -T postgres pg_restore -U cleads -d cleads \
#             --clean --if-exists < backups/cleads-YYYYMMDDTHHMMSSZ.dump
set -euo pipefail
cd "$(dirname "$0")"

KEEP_DAYS="${KEEP_DAYS:-7}"
mkdir -p backups
out="backups/cleads-$(date -u +%Y%m%dT%H%M%SZ).dump"

docker compose exec -T postgres pg_dump -U cleads -Fc cleads >"$out.partial"
docker compose exec -T postgres pg_restore -l <"$out.partial" >/dev/null
mv "$out.partial" "$out"
find backups -name 'cleads-*.dump' -mtime +"$KEEP_DAYS" -delete
find backups -name '*.partial' -mmin +60 -delete

echo "backup ok: $out ($(du -h "$out" | cut -f1))"
