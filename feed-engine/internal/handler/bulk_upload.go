package handler

// bulk_upload.go — POST /api/upload/bulk-init and GET /api/upload/bulk-status/{batch_id}
//
// D-070 note: bulk-init is a write via a dedicated REST endpoint, not through /events,
// because it must return structural data (batch_id + TUS URLs) that the client
// needs to start uploading. The Three-Lane Law applies to social mutations;
// upload coordination is infrastructure, not a social event.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"path/filepath"
	"sync"

	"github.com/google/uuid"
)

const maxBulkSlots = 20

// ── In-memory batch store ─────────────────────────────────────────────────────

type batchSlot struct {
	UploadID  string
	Filename  string
	Status    string // queued | uploading | processing | ready | failed
	MasterURL string
}

type batchState struct {
	BatchID string
	UserID  string
	Slots   []*batchSlot
}

var (
	batchStoreMu sync.RWMutex
	batchStore   = make(map[string]*batchState)
)

// ── Request / Response types ──────────────────────────────────────────────────

type bulkInitRequest struct {
	Count   int      `json:"count"`
	Handles []string `json:"handles"` // filenames — one per slot
}

type bulkSlotResponse struct {
	UploadID string `json:"upload_id"`
	TusURL   string `json:"tus_url"`
	Filename string `json:"filename"`
}

type bulkInitResponse struct {
	BatchID     string             `json:"batch_id"`
	UploadSlots []bulkSlotResponse `json:"upload_slots"`
}

type bulkStatusItem struct {
	UploadID  string `json:"upload_id"`
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	MasterURL string `json:"master_url"`
}

type bulkStatusResponse struct {
	Total      int              `json:"total"`
	Queued     int              `json:"queued"`
	Uploading  int              `json:"uploading"`
	Processing int              `json:"processing"`
	Ready      int              `json:"ready"`
	Failed     int              `json:"failed"`
	Items      []bulkStatusItem `json:"items"`
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// bulkInit handles POST /api/upload/bulk-init.
// Accepts {count, handles} — creates N pending TUS sessions and returns their IDs + URLs.
func (h *Handler) bulkInit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	user := h.userFromRequest(w, r)
	if user.Handle == "" {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	var req bulkInitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Count < 1 {
		http.Error(w, "count must be at least 1", http.StatusBadRequest)
		return
	}
	if req.Count > maxBulkSlots {
		req.Count = maxBulkSlots
	}

	// Pad/trim handles list to match count.
	handles := make([]string, req.Count)
	for i := range handles {
		if i < len(req.Handles) {
			handles[i] = filepath.Base(req.Handles[i])
		}
		if handles[i] == "" || handles[i] == "." {
			handles[i] = "upload"
		}
	}

	batchID := uuid.New().String()

	// Persist batch header row to DB.
	if h.db != nil {
		var pialArg interface{}
		if user.PIALID != "" {
			pialArg = user.PIALID
		}
		if _, err := h.db.Exec(
			`INSERT INTO upload_batches (batch_id, user_id, pial_id, total_count)
			 VALUES ($1, $2, $3, $4)`,
			batchID, user.ID, pialArg, req.Count,
		); err != nil {
			log.Printf("[bulk_upload] insert batch error: %v", err)
			// Non-fatal — in-memory store still works.
		}
	}

	slots := make([]*batchSlot, 0, req.Count)
	resp := bulkInitResponse{
		BatchID:     batchID,
		UploadSlots: make([]bulkSlotResponse, 0, req.Count),
	}

	up, err := uploaderOf(user)
	if err != nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	for _, filename := range handles {
		path, err := createSpool()
		if err != nil {
			log.Printf("[bulk_upload] create spool: %v", err)
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		// Registered in the tus store so PATCH /upload/tus/{id} appends to it. The
		// length is unknown until the client's first PATCH states it; the last
		// chunk hands the file to Caeor through the same gated path as any tus
		// upload.
		upload := newTusUpload(up, filename, path, 0)
		globalTusStore.put(upload)
		uploadID := upload.ID

		slot := &batchSlot{
			UploadID: uploadID,
			Filename: filename,
			Status:   "queued",
		}
		slots = append(slots, slot)

		// Persist slot row to DB.
		if h.db != nil {
			if _, err := h.db.Exec(
				`INSERT INTO upload_batch_slots (batch_id, tus_upload_id, filename, status)
				 VALUES ($1, $2, $3, 'queued')`,
				batchID, uploadID, filename,
			); err != nil {
				log.Printf("[bulk_upload] insert slot error: %v", err)
			}
		}

		resp.UploadSlots = append(resp.UploadSlots, bulkSlotResponse{
			UploadID: uploadID,
			TusURL:   "/upload/tus/" + uploadID,
			Filename: filename,
		})
	}

	// Store in-memory batch state.
	batchStoreMu.Lock()
	batchStore[batchID] = &batchState{
		BatchID: batchID,
		UserID:  user.ID,
		Slots:   slots,
	}
	batchStoreMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

// bulkStatus handles GET /api/upload/bulk-status/{batch_id}.
// Returns the current status of every slot from the one upload state
// (videoJobState). A batch is its creator's; anyone else is told it does not
// exist.
func (h *Handler) bulkStatus(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" || user.ID == "demo_user" {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	batchID := r.PathValue("batch_id")
	if batchID == "" {
		http.Error(w, "batch_id required", http.StatusBadRequest)
		return
	}

	batchStoreMu.RLock()
	batch, ok := batchStore[batchID]
	batchStoreMu.RUnlock()
	if !ok && h.db != nil {
		// Post-restart recovery.
		loaded, err := h.loadBatchFromDB(batchID)
		if err != nil {
			log.Printf("[bulk_upload] loading batch %s: %v", batchID, err)
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		batch = loaded
	}
	if batch == nil || batch.UserID != user.ID {
		http.Error(w, "batch not found", http.StatusNotFound)
		return
	}

	statusResp := bulkStatusResponse{
		Total: len(batch.Slots),
		Items: make([]bulkStatusItem, 0, len(batch.Slots)),
	}

	for _, slot := range batch.Slots {
		liveStatus, masterURL := h.bulkSlotStatus(r.Context(), slot)

		if h.db != nil && (liveStatus != slot.Status || masterURL != slot.MasterURL) {
			if _, err := h.db.Exec(
				`UPDATE upload_batch_slots SET status=$1, master_url=$2, updated_at=NOW()
				 WHERE tus_upload_id=$3`,
				liveStatus, masterURL, slot.UploadID,
			); err != nil {
				// The next poll re-derives it; said, not swallowed.
				log.Printf("[bulk-upload] slot status not synced for upload %s: %v", slot.UploadID, err)
			}
		}
		slot.Status = liveStatus
		slot.MasterURL = masterURL

		switch liveStatus {
		case "queued":
			statusResp.Queued++
		case "uploading":
			statusResp.Uploading++
		case "processing":
			statusResp.Processing++
		case "ready":
			statusResp.Ready++
		case "failed":
			statusResp.Failed++
		}
		statusResp.Items = append(statusResp.Items, bulkStatusItem{
			UploadID:  slot.UploadID,
			Filename:  slot.Filename,
			Status:    liveStatus,
			MasterURL: masterURL,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(statusResp); err != nil {
		log.Printf("[bulk_upload] encode status of %s: %v", batchID, err)
	}
}

// bulkSlotStatus is one slot's status in the batch vocabulary
// (queued | uploading | processing | ready | failed) and its master URL once
// ready. A slot this process no longer holds keeps the status last recorded.
func (h *Handler) bulkSlotStatus(ctx context.Context, slot *batchSlot) (string, string) {
	upload, ok := globalTusStore.get(slot.UploadID)
	if !ok {
		return slot.Status, slot.MasterURL
	}
	globalTusStore.mu.RLock()
	complete, offset := upload.complete, upload.Offset
	globalTusStore.mu.RUnlock()
	if !complete {
		if offset > 0 {
			return "uploading", ""
		}
		return "queued", ""
	}
	st, ok := h.videoJobState(ctx, slot.UploadID)
	if !ok {
		return slot.Status, slot.MasterURL
	}
	switch st.Status {
	case videoJobReady, videoJobDuplicate:
		return "ready", st.Output.MasterURL
	case videoJobFailed:
		return "failed", ""
	}
	return "processing", ""
}

// bulkUploadPage serves GET /create/bulk-upload — the standalone bulk import page.
func (h *Handler) bulkUploadPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsCreator && !user.IsAdmin() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	h.render(w, r, "creator_bulk_upload.html", map[string]interface{}{
		"User":  user,
		"Title": "Bulk Upload · F33D3R",
	})
}

// loadBatchFromDB reconstructs a batchState from Postgres (used after server
// restart). nil, nil when there is no such batch.
func (h *Handler) loadBatchFromDB(batchID string) (*batchState, error) {
	var userID string
	err := h.db.QueryRow(`SELECT user_id::text FROM upload_batches WHERE batch_id=$1`, batchID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := h.db.Query(
		`SELECT tus_upload_id, filename, status, COALESCE(master_url, '')
		 FROM upload_batch_slots WHERE batch_id=$1 ORDER BY created_at`,
		batchID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	batch := &batchState{BatchID: batchID, UserID: userID}
	for rows.Next() {
		slot := &batchSlot{}
		if err := rows.Scan(&slot.UploadID, &slot.Filename, &slot.Status, &slot.MasterURL); err != nil {
			return nil, err
		}
		batch.Slots = append(batch.Slots, slot)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	batchStoreMu.Lock()
	batchStore[batchID] = batch
	batchStoreMu.Unlock()
	return batch, nil
}
