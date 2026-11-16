package db

import (
	"encoding/json"
	"log"
)

// ── The feed-engine identity ledger ───────────────────────────────────────────
//
// Identity itself — roots, bindings, capabilities, role, age and KYC facts — is
// elohim-veni's, and this brain reads it through the projection in identity.go.
// What stays here is pial_event_ledger: this brain's append-only record of what
// IT did that concerns a person — a report filed, a realm granted, a content
// setting changed by an operator, a data export. elohim-veni keeps its own
// ledger of identity events; this is not a copy of it.

// LogPIALEvent appends an immutable event to the ledger. Nothing UPDATEs or
// DELETEs this table. A failed append is logged: the action it records has
// already happened and is not undone by a missing audit row, but the gap must
// be visible.
func LogPIALEvent(database DBTX, pialID, accountID, eventType string, payload map[string]interface{}, source string) {
	if pialID == "" || nilDB(database) {
		return
	}
	b, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[ledger] %s for %s: encoding payload: %v", eventType, pialID, err)
		return
	}
	var acctParam interface{}
	if accountID != "" {
		acctParam = accountID
	}
	if _, err := database.Exec(`
		INSERT INTO pial_event_ledger (pial_id, account_id, event_type, payload, source)
		VALUES ($1, $2, $3, $4, $5)`,
		pialID, acctParam, eventType, string(b), source); err != nil {
		log.Printf("[ledger] %s for %s not recorded: %v", eventType, pialID, err)
	}
}
