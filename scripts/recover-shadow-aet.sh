#!/usr/bin/env bash
# recover-shadow-aet.sh
# Finds all AET trapped in shadow accounts (user account UUIDs) and moves
# it to the correct PIAL accounts in f33d3r_wallet.
#
# A shadow account forms when tipEvent falls back to users.id instead of
# users.pial_id as the recipient identifier, sending AET to an account the
# recipient's wallet UI never reads.
#
# Usage (run on the server where Docker is running):
#   bash scripts/recover-shadow-aet.sh [--dry-run]
#
# Flags:
#   --dry-run   Print what would change without touching the database.

set -euo pipefail

DRY_RUN=false
for arg in "$@"; do
  [[ "$arg" == "--dry-run" ]] && DRY_RUN=true
done

FEED_DB="docker exec f33d3r-prod-postgres-1 psql -U f33d3r -d f33d3r_feed -t -A"
WALLET_DB="docker exec f33d3r-prod-postgres-1 psql -U f33d3r -d f33d3r_wallet -t -A"

# Try local container name if prod name doesn't exist
if ! docker inspect f33d3r-prod-postgres-1 &>/dev/null; then
  FEED_DB="docker exec f33d3r-local-postgres-1 psql -U f33d3r -d f33d3r_feed -t -A"
  WALLET_DB="docker exec f33d3r-local-postgres-1 psql -U f33d3r -d f33d3r_wallet -t -A"
fi

echo "=== F33D3R AET Shadow Account Recovery ==="
echo "Mode: $([ "$DRY_RUN" = true ] && echo 'DRY RUN' || echo 'LIVE')"
echo

# Step 1: Get every user's account UUID → PIAL mapping
MAPPING=$($FEED_DB -c "SELECT id, pial_id FROM users WHERE pial_id IS NOT NULL;")

TOTAL_RECOVERED=0
USERS_FIXED=0

while IFS='|' read -r account_uuid pial_id; do
  [[ -z "$account_uuid" || -z "$pial_id" ]] && continue
  # Skip if account UUID == PIAL (shouldn't happen but be safe)
  [[ "$account_uuid" == "$pial_id" ]] && continue

  # Check if this account UUID has a balance in f33d3r_wallet
  BALANCE=$($WALLET_DB -c "SELECT COALESCE(balance,0) FROM accounts WHERE user_id = '$account_uuid';" 2>/dev/null || echo "0")
  BALANCE="${BALANCE:-0}"

  if [[ "$BALANCE" -gt 0 ]]; then
    HANDLE=$($FEED_DB -c "SELECT handle FROM users WHERE id = '$account_uuid';" 2>/dev/null || echo "?")
    echo "FOUND: @${HANDLE} — shadow account ${account_uuid} has ${BALANCE} units (correct PIAL: ${pial_id})"

    if [[ "$DRY_RUN" = false ]]; then
      $WALLET_DB -c "
BEGIN;

-- Ensure PIAL account exists
INSERT INTO accounts (user_id, balance) VALUES ('$pial_id', 0)
  ON CONFLICT (user_id) DO NOTHING;

-- Move balance to correct PIAL account
UPDATE accounts SET balance = balance + $BALANCE WHERE user_id = '$pial_id';
UPDATE accounts SET balance = 0               WHERE user_id = '$account_uuid';

-- Fix transaction history
UPDATE transactions   SET to_user_id = '$pial_id' WHERE to_user_id = '$account_uuid';
UPDATE ledger_entries SET account_id  = '$pial_id' WHERE account_id  = '$account_uuid';

COMMIT;
" > /dev/null
      echo "  → Migrated ${BALANCE} units to PIAL account"
    else
      echo "  → (dry-run) Would migrate ${BALANCE} units to PIAL account"
    fi

    TOTAL_RECOVERED=$((TOTAL_RECOVERED + BALANCE))
    USERS_FIXED=$((USERS_FIXED + 1))
  fi
done <<< "$MAPPING"

echo
if [[ $USERS_FIXED -eq 0 ]]; then
  echo "No shadow accounts found. All balances are correctly attributed."
else
  echo "=== Summary ==="
  echo "Users fixed:    $USERS_FIXED"
  echo "Units recovered: $TOTAL_RECOVERED"
  [[ "$DRY_RUN" = true ]] && echo "(No changes written — rerun without --dry-run to apply)"
fi
