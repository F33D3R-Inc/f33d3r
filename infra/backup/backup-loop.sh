#!/bin/sh
# F33D3R — the backup service's schedule.
#
# One scheduled backup a day at BACKUP_AT (UTC, HH:MM, default 03:30). On start
# it runs one at once if the last success is older than a day or there has
# never been one, so a fresh install or a long outage does not wait for the
# next night. A failed run is retried after an hour; it is never silent — the
# container's healthcheck reads /backups/.last_success and turns unhealthy
# once it is more than 26 hours old.

set -euo pipefail

AT="${BACKUP_AT:-03:30}"
case "${AT}" in [0-2][0-9]:[0-5][0-9]) ;; *) echo "[backup-loop] BACKUP_AT='${AT}' is not HH:MM" >&2; exit 1 ;; esac

log() { echo "[backup-loop] $(date -u +%Y-%m-%dT%H:%M:%SZ) $*"; }

run() {
  if /backup/backup.sh scheduled; then
    return 0
  fi
  log "scheduled backup FAILED — retrying in one hour"
  sleep 3600
  return 1
}

seconds_until_next() {
  now="$(date -u +%s)"
  target="$(date -u -d "$(date -u +%Y-%m-%d) ${AT}" +%s)"
  [ "${target}" -gt "${now}" ] || target=$((target + 86400))
  echo $((target - now))
}

# This loop is the only thing that takes the scheduled lock, and it is not
# running a backup yet, so a lock present now was left by a container that was
# killed mid-run.
if [ -d /backups/.scheduled.lock ]; then
  log "removing the lock a killed run left behind"
  rmdir /backups/.scheduled.lock
fi

if [ ! -e /backups/.last_success ] || [ -z "$(find /backups/.last_success -mmin -1440)" ]; then
  log "no successful backup in the last day — running one now"
  until run; do :; done
fi

while :; do
  wait_s="$(seconds_until_next)"
  log "next backup at ${AT} UTC, in ${wait_s}s"
  sleep "${wait_s}"
  until run; do :; done
done
