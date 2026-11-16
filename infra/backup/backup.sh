#!/bin/sh
# F33D3R — backups.
#
# Runs inside the backup service, on the postgres image the database itself
# runs (postgres:18-alpine), so pg_dump is always the server's own version.
# Everything lands under /backups, a host directory (BACKUP_DIR, default
# /var/backups/f33d3r) bind-mounted into the service:
#
#   /backups/db/<kind>/<stamp>/       one database backup set
#       globals.sql                   pg_dumpall --globals-only (roles)
#       <database>.dump               pg_dump -Fc, one per database
#       SHA256SUMS                    checksums of the files above
#       COMPLETE                      written last; a set without it is partial
#     kind: scheduled | predeploy | prerollback
#   /backups/media/<tree>/<stamp>/    hard-link snapshots of a media volume
#   /backups/keys/<stamp>.tar.gz      every signing-key volume, whole
#   /backups/.last_success            touched when a scheduled run succeeds
#
# Why one archive per database and not one pg_dumpall script: a pg_dumpall
# script re-creates the bootstrap role it is restored as, so it cannot be
# replayed onto the running cluster with ON_ERROR_STOP — it fails on its own
# first statements. Per-database custom-format archives restore with
# pg_restore --clean --if-exists --create --exit-on-error, which either
# succeeds exactly or stops at the first error (restore.sh).
#
# Usage:
#   backup.sh db <dir>          dump every database into <dir> (created)
#   backup.sh scheduled         db + keys + media snapshots, then retention
#   backup.sh verify <dir>      check a db set is complete and intact
#   backup.sh prune <kind> <n>  keep the newest <n> db sets of <kind>
#
# Env: PGHOST PGUSER PGPASSWORD (the connection), BACKUP_KEEP (scheduled db
# sets, key archives and media snapshots kept; default 14).

set -euo pipefail
umask 077

ROOT=/backups
KEEP="${BACKUP_KEEP:-14}"

log() { echo "[backup] $(date -u +%Y-%m-%dT%H:%M:%SZ) $*"; }
die() { log "ERROR: $*" >&2; exit 1; }

stamp() { date -u +%Y%m%dT%H%M%SZ; }

[ -d "${ROOT}" ] || die "${ROOT} is not mounted"

# ── databases ────────────────────────────────────────────────────────────────
databases() {
  # Every database that accepts connections, templates excluded. Read from
  # the cluster, never a list in this file: a database added by a later
  # migration is backed up the night it exists.
  psql -X -At -d template1 -c \
    "SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate ORDER BY datname"
}

dump_db() {
  dest="$1"
  [ -n "${dest}" ] || die "db: destination required"
  case "${dest}" in "${ROOT}"/*) ;; *) die "db: ${dest} is not under ${ROOT}" ;; esac
  [ ! -e "${dest}/COMPLETE" ] || die "db: ${dest} already holds a complete set"
  mkdir -p "${dest}"

  log "dumping roles → ${dest}/globals.sql"
  pg_dumpall -X --globals-only -d template1 -f "${dest}/globals.sql"

  dbs="$(databases)"
  [ -n "${dbs}" ] || die "db: the cluster reported no databases"
  for db in ${dbs}; do
    log "dumping ${db}"
    pg_dump -X -Fc -d "${db}" -f "${dest}/${db}.dump"
  done

  ( cd "${dest}" && sha256sum globals.sql ./*.dump > SHA256SUMS )
  {
    echo "stamp=$(basename "${dest}")"
    echo "server=$(psql -X -At -d template1 -c 'SHOW server_version')"
    echo "databases=$(echo ${dbs})"
  } > "${dest}/COMPLETE"
  log "database set complete: ${dest} ($(du -sh "${dest}" | cut -f1))"
}

verify_db() {
  dir="$1"
  [ -f "${dir}/COMPLETE" ] || die "verify: ${dir} has no COMPLETE marker — a partial set"
  ( cd "${dir}" && sha256sum -c -s SHA256SUMS ) || die "verify: checksum mismatch in ${dir}"
  for f in "${dir}"/*.dump; do
    pg_restore -l "${f}" > /dev/null || die "verify: ${f} is not a readable archive"
  done
  log "verified: ${dir}"
}

# keep_newest DIR N — remove all but the newest N entries of DIR. Entry names
# are UTC stamps, so they sort by time; an interrupted set ages out like any
# other.
keep_newest() {
  dir="$1"; n="$2"
  [ -d "${dir}" ] || return 0
  case "${n}" in ''|*[!0-9]*) die "keep: '${n}' is not a count" ;; esac
  [ "${n}" -ge 1 ] || die "keep: refusing to keep fewer than one"
  total="$(ls -1 "${dir}" | wc -l)"
  excess=$((total - n))
  [ "${excess}" -gt 0 ] || return 0
  ls -1 "${dir}" | sort | head -n "${excess}" | while IFS= read -r old; do
    rm -rf "${dir:?}/${old}"
    log "retention: removed ${dir}/${old}"
  done
}

prune_kind() {
  kind="$1"; n="$2"
  case "${kind}" in scheduled|predeploy|prerollback) ;; *) die "prune: unknown kind '${kind}'" ;; esac
  keep_newest "${ROOT}/db/${kind}" "${n}"
}

# ── signing keys ─────────────────────────────────────────────────────────────
# Every directory mounted under /src/keys is a key volume (elohim-veni's PIAL
# signing keys, the tor hidden-service identity, and any added later — one
# mount line each in the compose file). They are small and are archived whole.
backup_keys() {
  st="$1"
  [ -d /src/keys ] || die "keys: /src/keys is not mounted"
  set -- /src/keys/*
  [ -e "$1" ] || die "keys: no key volumes are mounted under /src/keys"
  mkdir -p "${ROOT}/keys"
  tar -czf "${ROOT}/keys/${st}.tar.gz.partial" -C /src keys
  mv "${ROOT}/keys/${st}.tar.gz.partial" "${ROOT}/keys/${st}.tar.gz"
  log "keys archived: ${ROOT}/keys/${st}.tar.gz"
  keep_newest "${ROOT}/keys" "${KEEP}"
}

# ── media ────────────────────────────────────────────────────────────────────
# Hard-link snapshots. Each snapshot is a full tree, but a file unchanged since
# the previous snapshot is a hard link to the same inode, so a night costs only
# what changed. Media files are written once and replaced, never edited in
# place, so a file is taken as changed when it is absent from the previous
# snapshot or its mtime is newer than the copy's. A changed file is unlinked
# from the snapshot before it is copied: writing through the link would alter
# every older snapshot too.
#
# This protects against deletion and corruption on the live volumes. It is on
# the same disk: copy /backups off the host as well (see deploy runbook).
snapshot_tree() {
  src="$1"; name="$2"; exclude="${3:-}"
  [ -d "${src}" ] || die "media: ${src} is not mounted"
  base="${ROOT}/media/${name}"
  mkdir -p "${base}"
  rm -rf "${base:?}"/*.partial
  prev="$(ls -1 "${base}" | sort | tail -n 1)"
  new="${base}/${STAMP}.partial"

  if [ -n "${prev}" ]; then
    cp -al "${base}/${prev}" "${new}"
    # Drop what left the source or changed since.
    ( cd "${new}" && find . -type f ) | while IFS= read -r f; do
      if [ ! -f "${src}/${f}" ] || [ "${src}/${f}" -nt "${new}/${f}" ]; then
        rm -f "${new}/${f}"
      fi
    done
  else
    mkdir -p "${new}"
  fi

  # Copy in what is not there.
  if [ -n "${exclude}" ]; then
    list() { ( cd "${src}" && find . -path "./${exclude}" -prune -o -type f -print ); }
  else
    list() { ( cd "${src}" && find . -type f ); }
  fi
  list | while IFS= read -r f; do
    if [ ! -e "${new}/${f}" ]; then
      mkdir -p "$(dirname "${new}/${f}")"
      cp -p "${src}/${f}" "${new}/${f}"
    fi
  done
  find "${new}" -type d -empty ! -path "${new}" -delete
  mv "${new}" "${base}/${STAMP}"
  log "media snapshot: ${base}/${STAMP} ($(find "${base}/${STAMP}" -type f | wc -l) files)"
  keep_newest "${base}" "${KEEP}"
}

scheduled() {
  STAMP="$(stamp)"
  lock="${ROOT}/.scheduled.lock"
  mkdir "${lock}" 2>/dev/null || die "a scheduled backup is already running (${lock})"
  trap 'rmdir "${lock}"' EXIT

  dump_db "${ROOT}/db/scheduled/${STAMP}"
  verify_db "${ROOT}/db/scheduled/${STAMP}"
  prune_kind scheduled "${KEEP}"
  backup_keys "${STAMP}"
  # live/ is excluded: it is the ladder's rolling HLS window, rewritten every
  # second and deleted when a broadcast ends.
  snapshot_tree /src/media media live
  snapshot_tree /src/media-private media-private
  touch "${ROOT}/.last_success"
  log "scheduled backup complete: ${STAMP}"
}

cmd="${1:-}"
case "${cmd}" in
  db)        dump_db "${2:-}"; verify_db "${2}" ;;
  scheduled) scheduled ;;
  verify)    verify_db "${2:?verify: directory required}" ;;
  prune)     prune_kind "${2:?prune: kind required}" "${3:?prune: count required}" ;;
  *)         die "usage: backup.sh db <dir> | scheduled | verify <dir> | prune <kind> <n>" ;;
esac
