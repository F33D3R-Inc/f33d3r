#!/bin/sh
# F33D3R — strict database restore from one backup set (backup.sh db).
#
# Usage: restore.sh <dir under /backups/db>
#
# Restores every <database>.dump in the set over the running cluster with
#   pg_restore --clean --if-exists --create --exit-on-error
# connected to template1: each database is dropped and recreated from the
# archive, and the first error stops the restore and fails it. Nothing is
# skipped and no error is tolerated.
#
# It refuses to start when:
#   - the set is not complete or a checksum does not match;
#   - a role the set's globals name does not exist in the cluster (a set can
#     only be restored onto a cluster that has its roles; on a fresh cluster
#     the postgres image creates POSTGRES_USER, which is the only role this
#     platform uses);
#   - any other session is connected to the cluster. Every service must be
#     stopped first — scripts/rollback.sh does that.
#
# Databases present in the cluster but absent from the set are left alone
# and named in the log.

set -euo pipefail

dir="${1:-}"
log() { echo "[restore] $(date -u +%Y-%m-%dT%H:%M:%SZ) $*"; }
die() { log "ERROR: $*" >&2; exit 1; }

[ -n "${dir}" ] || die "usage: restore.sh <backup set directory>"
case "${dir}" in /backups/db/*) ;; *) die "${dir} is not a backup set under /backups/db" ;; esac
[ -d "${dir}" ] || die "${dir} does not exist"

/backup/backup.sh verify "${dir}"

# Roles.
roles="$(sed -n 's/^CREATE ROLE \(.*\);$/\1/p' "${dir}/globals.sql" | tr -d '"')"
[ -n "${roles}" ] || die "${dir}/globals.sql names no roles"
for role in ${roles}; do
  have="$(psql -X -At -d template1 -v r="${role}" -c "SELECT count(*) FROM pg_roles WHERE rolname = :'r'")"
  [ "${have}" = "1" ] || die "role ${role} from the backup does not exist in this cluster; create it before restoring"
done

# Nobody else connected.
others="$(psql -X -At -d template1 -c \
  "SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()")"
[ "${others}" = "0" ] || die "${others} other session(s) are connected; stop every service except postgres first"

in_set=""
for f in "${dir}"/*.dump; do
  db="$(basename "${f}" .dump)"
  in_set="${in_set} ${db}"
  log "restoring ${db}"
  pg_restore --clean --if-exists --create --exit-on-error -d template1 "${f}" \
    || die "restoring ${db} failed; databases restored before it are at the backup, ${db} and those after it are not"
  log "restored ${db}"
done

for db in $(psql -X -At -d template1 -c \
    "SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate ORDER BY datname"); do
  case " ${in_set} " in *" ${db} "*) ;; *) log "note: ${db} exists in the cluster but not in this set; left as it is" ;; esac
done

log "restore complete from ${dir}"
