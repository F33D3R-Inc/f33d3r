#!/usr/bin/env sh
# F33D3R — nightly Postgres backup to MinIO
# Runs inside the backup container. Dumps each database, compresses, uploads.
# Retention: keeps last 7 days; deletes older objects automatically.
#
# Env vars (injected by docker-compose):
#   POSTGRES_HOST, POSTGRES_USER, POSTGRES_PASSWORD
#   MINIO_ENDPOINT, MINIO_ACCESS_KEY, MINIO_SECRET_KEY, MINIO_BUCKET
#   BACKUP_RETENTION_DAYS (default 7)

set -eu

RETENTION=${BACKUP_RETENTION_DAYS:-7}
STAMP=$(date -u +"%Y-%m-%dT%H%M%SZ")
FAILED=0

DATABASES="
f33d3r_feed
f33d3r_msg
f33d3r_wallet
f33d3r_security
f33d3r_safety
f33d3r_registry
f33d3r_verity
f33d3r_ledger
"

echo "[backup] starting — stamp=${STAMP}"

# Configure mc
mc alias set store "http://${MINIO_ENDPOINT}" "${MINIO_ACCESS_KEY}" "${MINIO_SECRET_KEY}" --quiet

for DB in $DATABASES; do
  DB=$(echo "$DB" | tr -d '[:space:]')
  [ -z "$DB" ] && continue

  FILENAME="${DB}_${STAMP}.sql.gz"
  OBJECT="postgres/${DB}/${FILENAME}"
  TMPFILE="/tmp/${FILENAME}"

  echo "[backup] dumping ${DB}…"

  PGPASSWORD="${POSTGRES_PASSWORD}" pg_dump \
    -h "${POSTGRES_HOST}" \
    -U "${POSTGRES_USER}" \
    -d "${DB}" \
    --no-password \
    --format=plain \
    --no-owner \
    --no-acl \
    | gzip -9 > "${TMPFILE}" || { echo "[backup] ERROR: dump failed for ${DB}"; FAILED=1; rm -f "${TMPFILE}"; continue; }

  SIZE=$(du -sh "${TMPFILE}" | cut -f1)
  echo "[backup] uploading ${OBJECT} (${SIZE})…"

  mc cp "${TMPFILE}" "store/${MINIO_BUCKET}/${OBJECT}" --quiet \
    || { echo "[backup] ERROR: upload failed for ${DB}"; FAILED=1; rm -f "${TMPFILE}"; continue; }

  rm -f "${TMPFILE}"
  echo "[backup] ${DB} OK"
done

# Prune backups older than RETENTION days
echo "[backup] pruning objects older than ${RETENTION} days…"
CUTOFF=$(date -u -d "-${RETENTION} days" +"%Y-%m-%dT%H:%M:%S" 2>/dev/null \
  || date -u -v-${RETENTION}d +"%Y-%m-%dT%H:%M:%S")  # GNU vs BSD date

mc find "store/${MINIO_BUCKET}/postgres/" \
  --older-than "${RETENTION}d" \
  --name "*.sql.gz" \
  | while read -r obj; do
      mc rm "$obj" --quiet && echo "[backup] pruned $obj"
    done

if [ "${FAILED}" -ne 0 ]; then
  echo "[backup] COMPLETED WITH ERRORS — check above"
  exit 1
fi

echo "[backup] all databases backed up successfully — stamp=${STAMP}"
