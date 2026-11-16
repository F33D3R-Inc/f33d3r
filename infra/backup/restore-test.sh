#!/bin/sh
# F33D3R — prove a backup set restores, without touching the live databases.
#
# Usage: restore-test.sh [<dir under /backups/db>]
#   default: the newest complete scheduled set
#
# Each <database>.dump is restored into a scratch database
# (<database>_restore_test) with --exit-on-error, so any error fails the test,
# and every table's row count is compared with the live database. Live tables
# keep changing after the dump, so a count within 1% passes; a table missing
# from the restore fails. Scratch databases are dropped afterwards, pass or
# fail. The cluster needs free space for the largest database while it runs.

set -euo pipefail

TOLERANCE_PCT=1
log() { echo "[restore-test] $*"; }
die() { log "ERROR: $*" >&2; exit 1; }

dir="${1:-}"
if [ -z "${dir}" ]; then
  for d in $(ls -1 /backups/db/scheduled 2>/dev/null | sort -r); do
    if [ -f "/backups/db/scheduled/${d}/COMPLETE" ]; then dir="/backups/db/scheduled/${d}"; break; fi
  done
  [ -n "${dir}" ] || die "no complete scheduled backup set under /backups/db/scheduled"
fi
/backup/backup.sh verify "${dir}"
log "testing ${dir}"

FAILED=0
SCRATCH=""
cleanup() {
  if [ -n "${SCRATCH}" ]; then
    psql -X -q -d template1 -c "DROP DATABASE IF EXISTS \"${SCRATCH}\" WITH (FORCE)"
  fi
}
trap cleanup EXIT

for f in "${dir}"/*.dump; do
  db="$(basename "${f}" .dump)"
  SCRATCH="${db}_restore_test"
  psql -X -q -d template1 -c "DROP DATABASE IF EXISTS \"${SCRATCH}\" WITH (FORCE)"
  psql -X -q -d template1 -c "CREATE DATABASE \"${SCRATCH}\" TEMPLATE template0"
  pg_restore --exit-on-error --no-owner -d "${SCRATCH}" "${f}" || die "${db}: the archive did not restore"

  tables="$(psql -X -At -d "${db}" -c \
    "SELECT quote_ident(schemaname) || '.' || quote_ident(tablename) FROM pg_tables
     WHERE schemaname NOT IN ('pg_catalog','information_schema') ORDER BY 1")"
  for t in ${tables}; do
    live="$(psql -X -At -d "${db}" -c "SELECT count(*) FROM ${t}")"
    restored="$(psql -X -At -d "${SCRATCH}" -c "SELECT count(*) FROM ${t}")" \
      || { log "FAIL ${db} ${t}: missing from the restore"; FAILED=1; continue; }
    diff=$((live - restored)); [ "${diff}" -ge 0 ] || diff=$((-diff))
    if [ "${live}" -gt 0 ] && [ $((diff * 100)) -gt $((live * TOLERANCE_PCT)) ]; then
      log "FAIL ${db} ${t}: live ${live}, restored ${restored}"
      FAILED=1
    fi
  done
  psql -X -q -d template1 -c "DROP DATABASE \"${SCRATCH}\" WITH (FORCE)"
  SCRATCH=""
  log "${db}: restored and compared"
done

[ "${FAILED}" -eq 0 ] || die "at least one table did not match"
log "PASS — ${dir} restores"
