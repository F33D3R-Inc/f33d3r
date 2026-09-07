package handler

// bulk_upload.go — POST /api/upload/bulk-init and GET /api/upload/bulk-status/{batch_id}
//
// D-070 note: bulk-init is a write via a dedicated REST endpoint, not through /events,
// because it must return structural data (batch_id + TUS URLs) that the client
// needs to start uploading. The Three-Lane Law applies to social mutations;
// upload coordination is infrastructure, not a social event.

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

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

	for _, filename := range handles {
		uploadID := uuid.New().String()
		path := filepath.Join(tusTempDir, uploadID)

		f, err := os.Create(path)
		if err != nil {
			log.Printf("[bulk_upload] create temp file error: %v", err)
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		f.Close()

		// Register in the global TUS store so PATCH /upload/tus/{id} works.
		// Length is 0 here; the client sends Upload-Length on the TUS POST/PATCH.
		upload := &tusUpload{
			ID:             uploadID,
			Length:         0,
			Offset:         0,
			Filename:       filename,
			Created:        time.Now(),
			UploaderPIAL:   user.PIALID,
			UploaderHandle: user.Handle,
			path:           path,
		}
		globalTusStore.mu.Lock()
		globalTusStore.uploads[uploadID] = upload
		globalTusStore.mu.Unlock()

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
// Returns current status of all slots by consulting the live TUS store + transcoding brain.
func (h *Handler) bulkStatus(w http.ResponseWriter, r *http.Request) {
	batchID := r.PathValue("batch_id")
	if batchID == "" {
		http.Error(w, "batch_id required", http.StatusBadRequest)
		return
	}

	batchStoreMu.RLock()
	batch, ok := batchStore[batchID]
	batchStoreMu.RUnlock()

	if !ok {
		// Fall back to DB (post-restart recovery).
		if h.db != nil {
			batch = h.loadBatchFromDB(batchID)
		}
		if batch == nil {
			http.Error(w, "batch not found", http.StatusNotFound)
			return
		}
	}

	statusResp := bulkStatusResponse{
		Total: len(batch.Slots),
		Items: make([]bulkStatusItem, 0, len(batch.Slots)),
	}

	for _, slot := range batch.Slots {
		liveStatus := slot.Status
		masterURL := slot.MasterURL

		globalTusStore.mu.RLock()
		tusUp, hasTus := globalTusStore.uploads[slot.UploadID]
		globalTusStore.mu.RUnlock()

		if hasTus {
			switch {
			case tusUp.DupInfo != nil:
				liveStatus = "ready"
				masterURL = tusUp.DupInfo.ExistingMasterURL
			case tusUp.JobID != "":
				liveStatus, masterURL = h.bulkPollJobStatus(tusUp.JobID, liveStatus, masterURL)
			case tusUp.Length > 0 && tusUp.Offset >= tusUp.Length:
				liveStatus = "processing"
			case tusUp.Offset > 0:
				liveStatus = "uploading"
			}
		}

		// Persist status change to DB (fire-and-forget).
		if h.db != nil && (liveStatus != slot.Status || masterURL != slot.MasterURL) {
			uploadID := slot.UploadID
			st := liveStatus
			mu := masterURL
			go func() {
				// Detached status sync; the next poll re-derives it, so a failure is
				// recoverable — but it is said out loud rather than swallowed.
				if _, err := h.db.Exec(
					`UPDATE upload_batch_slots SET status=$1, master_url=$2, updated_at=NOW()
					 WHERE tus_upload_id=$3`,
					st, mu, uploadID,
				); err != nil {
					log.Printf("[bulk-upload] slot status not synced for upload %s: %v", uploadID, err)
				}
			}()
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
	json.NewEncoder(w).Encode(statusResp)
}

// bulkPollJobStatus fetches current transcoding status for a job ID.
// Returns (status, masterURL) — status is one of: processing | ready | failed.
func (h *Handler) bulkPollJobStatus(jobID, currentStatus, currentMasterURL string) (string, string) {
	if h.transcodingURL == "" {
		return "processing", ""
	}
	resp, err := h.httpClient.Get(h.transcodingURL + "/v1/video/job/" + jobID)
	if err != nil {
		return currentStatus, currentMasterURL
	}
	defer resp.Body.Close()

	var jobResp struct {
		Status string `json:"status"`
		Output struct {
			MasterURL string `json:"master_url"`
		} `json:"output"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jobResp); err != nil {
		return currentStatus, currentMasterURL
	}

	switch jobResp.Status {
	case "ready":
		return "ready", jobResp.Output.MasterURL
	case "failed", "error":
		return "failed", ""
	default:
		return "processing", ""
	}
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

// loadBatchFromDB reconstructs a batchState from Postgres (used after server restart).
func (h *Handler) loadBatchFromDB(batchID string) *batchState {
	rows, err := h.db.Query(
		`SELECT tus_upload_id, filename, status, master_url
		 FROM upload_batch_slots WHERE batch_id=$1 ORDER BY created_at`,
		batchID,
	)
	if err != nil {
		log.Printf("[bulk_upload] loadBatchFromDB query error: %v", err)
		return nil
	}
	defer rows.Close()

	var userID string
	_ = h.db.QueryRow(`SELECT user_id FROM upload_batches WHERE batch_id=$1`, batchID).Scan(&userID)

	batch := &batchState{BatchID: batchID, UserID: userID}
	for rows.Next() {
		slot := &batchSlot{}
		if err := rows.Scan(&slot.UploadID, &slot.Filename, &slot.Status, &slot.MasterURL); err == nil {
			batch.Slots = append(batch.Slots, slot)
		}
	}
	if len(batch.Slots) == 0 {
		return nil
	}

	batchStoreMu.Lock()
	batchStore[batchID] = batch
	batchStoreMu.Unlock()
	return batch
}
