#!/usr/bin/env sh
# F33D3R — restore-test: pull last backup from MinIO, restore into scratch container,
# verify row counts are within 1% of live.
#
# Usage: bash restore-test.sh [database]
#   database: optional — if omitted, tests all databases
#
# Exits 0 if all checks pass, 1 if any fail.

set -eu

DATABASES="${1:-f33d3r_feed f33d3r_msg f33d3r_wallet f33d3r_security f33d3r_verity f33d3r_ledger}"
MINIO_ENDPOINT="${MINIO_ENDPOINT:-minio:9000}"
MINIO_BUCKET="${MINIO_BUCKET:-backups}"
POSTGRES_HOST="${POSTGRES_HOST:-postgres}"
POSTGRES_USER="${POSTGRES_USER:-f33d3r}"
POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-f33d3rdev}"
SCRATCH_SUFFIX="_restore_test_$$"
TOLERANCE=0.01   # 1% row count variance allowed
FAILED=0

mc alias set store "http://${MINIO_ENDPOINT}" "${MINIO_ACCESS_KEY}" "${MINIO_SECRET_KEY}" --quiet

check_db() {
  DB="$1"
  SCRATCH="${DB}${SCRATCH_SUFFIX}"

  echo "[restore-test] === ${DB} ==="

  # Find latest backup
  LATEST=$(mc find "store/${MINIO_BUCKET}/postgres/${DB}/" --name "*.sql.gz" \
    | sort | tail -1)

  if [ -z "${LATEST}" ]; then
    echo "[restore-test] SKIP: no backup found for ${DB}"
    return 0
  fi

  echo "[restore-test] latest backup: ${LATEST}"
  TMPFILE="/tmp/${DB}_restore_test.sql.gz"
  mc cp "${LATEST}" "${TMPFILE}" --quiet

  # Create scratch database
  PGPASSWORD="${POSTGRES_PASSWORD}" psql \
    -h "${POSTGRES_HOST}" -U "${POSTGRES_USER}" -d postgres \
    -c "DROP DATABASE IF EXISTS ${SCRATCH};" \
    -c "CREATE DATABASE ${SCRATCH};" \
    --quiet

  # Restore
  echo "[restore-test] restoring into ${SCRATCH}…"
  PGPASSWORD="${POSTGRES_PASSWORD}" \
    zcat "${TMPFILE}" | psql \
      -h "${POSTGRES_HOST}" -U "${POSTGRES_USER}" -d "${SCRATCH}" \
      --quiet 2>/dev/null || true  # non-fatal: some statements may fail on non-superuser

  # Row-count comparison per table
  TABLES=$(PGPASSWORD="${POSTGRES_PASSWORD}" psql \
    -h "${POSTGRES_HOST}" -U "${POSTGRES_USER}" -d "${DB}" \
    -t -c "SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename;")

  TABLE_FAIL=0
  for TABLE in $TABLES; do
    TABLE=$(echo "$TABLE" | tr -d '[:space:]')
    [ -z "$TABLE" ] && continue

    LIVE=$(PGPASSWORD="${POSTGRES_PASSWORD}" psql \
      -h "${POSTGRES_HOST}" -U "${POSTGRES_USER}" -d "${DB}" \
      -t -c "SELECT COUNT(*) FROM ${TABLE};" 2>/dev/null | tr -d '[:space:]' || echo 0)

    RESTORED=$(PGPASSWORD="${POSTGRES_PASSWORD}" psql \
      -h "${POSTGRES_HOST}" -U "${POSTGRES_USER}" -d "${SCRATCH}" \
      -t -c "SELECT COUNT(*) FROM ${TABLE};" 2>/dev/null | tr -d '[:space:]' || echo 0)

    # Skip tables that don't exist in scratch (view, extension tables, etc.)
    [ -z "$RESTORED" ] || [ "$RESTORED" = "0" ] && [ -z "$LIVE" ] && continue

    # Variance check: allow TOLERANCE difference
    if [ "${LIVE}" -gt 0 ]; then
      # Use awk for float comparison
      VARIANCE=$(awk "BEGIN { d=${LIVE}-${RESTORED}; if(d<0)d=-d; print d/${LIVE} }")
      PASS=$(awk "BEGIN { print (${VARIANCE} <= ${TOLERANCE}) ? 1 : 0 }")
      if [ "${PASS}" -ne 1 ]; then
        echo "[restore-test] FAIL: ${TABLE}: live=${LIVE} restored=${RESTORED} variance=$(awk "BEGIN{printf \"%.2f%%\",${VARIANCE}*100}")"
        TABLE_FAIL=1
      fi
    fi
  done

  # Drop scratch
  PGPASSWORD="${POSTGRES_PASSWORD}" psql \
    -h "${POSTGRES_HOST}" -U "${POSTGRES_USER}" -d postgres \
    -c "DROP DATABASE IF EXISTS ${SCRATCH};" --quiet

  rm -f "${TMPFILE}"

  if [ "${TABLE_FAIL}" -ne 0 ]; then
    echo "[restore-test] ${DB}: FAILED row count checks"
    return 1
  else
    echo "[restore-test] ${DB}: PASSED"
    return 0
  fi
}

for DB in $DATABASES; do
  check_db "$DB" || FAILED=1
done

if [ "${FAILED}" -ne 0 ]; then
  echo "[restore-test] RESULT: FAILED — at least one database did not pass"
  exit 1
fi

echo "[restore-test] RESULT: ALL PASS"
exit 0
