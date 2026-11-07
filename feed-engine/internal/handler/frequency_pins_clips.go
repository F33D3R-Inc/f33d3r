package handler

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/f33d3r/feed-engine/internal/auralis"
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

const (
	frequencyPinPreview   = 140
	frequencyPinCacheTTL  = 15 * time.Second
	frequencyClipsPerHour = 5
)

// frequencyClipLengths are the seconds a clip can run.
var frequencyClipLengths = map[int]bool{30: true, 60: true, 140: true}

var frequencyWorkID = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// FrequencyPinData is one pinned work as a room shows it. It is shared by everyone in
// the room, so it carries only what anyone may see.
type FrequencyPinData struct {
	ID           string
	WorkID       string
	AuthorHandle string
	Preview      string
	Thumb        string
	CanRemove    bool
	Adult        bool
}

type frequencyPinCached struct {
	pin FrequencyPinData
	ok  bool
	at  time.Time
}

// frequencyPinCache spares a room a query per pin on every render.
var frequencyPinCache sync.Map

// frequencyAdultWork says whether a work needs an adult-enabled audience.
func frequencyAdultWork(w *model.Work) bool {
	return w.IsNSFW || w.IsGore || w.AuthorIsAdultCreator
}

// frequencyPinOf reads one pinned work. A work deleted or gated since it was pinned
// simply stops showing.
func (h *Handler) frequencyPinOf(workID string) (FrequencyPinData, bool) {
	if v, ok := frequencyPinCache.Load(workID); ok {
		if c := v.(frequencyPinCached); time.Since(c.at) < frequencyPinCacheTTL {
			return c.pin, c.ok
		}
	}
	pin, ok := FrequencyPinData{}, false
	if h.db != nil {
		w, err := dbpkg.GetWorkByID(h.db, workID, "")
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("[frequency] pin %s: %v", workID, err)
		}
		if w != nil && !w.SubscriberOnly {
			ok = true
			pin = FrequencyPinData{
				WorkID:       workID,
				AuthorHandle: w.AuthorHandle,
				Preview:      truncate(strings.TrimSpace(w.Body), frequencyPinPreview),
				Thumb:        w.VideoPosterURL,
			}
			if pin.Thumb == "" && len(w.MediaURLs) > 0 && !frequencyAdultWork(w) {
				pin.Thumb = w.MediaURLs[0]
			}
			if frequencyAdultWork(w) {
				pin.Thumb = ""
				pin.Adult = true
			}
		}
	}
	frequencyPinCache.Store(workID, frequencyPinCached{pin: pin, ok: ok, at: time.Now()})
	return pin, ok
}

// frequencyPinData shapes a room's pins for one viewer.
func (h *Handler) frequencyPinData(v *auralis.View, canModerate bool) []FrequencyPinData {
	out := make([]FrequencyPinData, 0, len(v.Pins))
	for _, p := range v.Pins {
		pin, ok := h.frequencyPinOf(p.WorkID)
		// A work flagged adult after it was pinned stops showing in a room for everyone.
		if !ok || (pin.Adult && !v.Frequency.AdultContent) {
			continue
		}
		pin.ID = v.Frequency.ID
		pin.CanRemove = canModerate
		out = append(out, pin)
	}
	return out
}

// frequencyPin pins a work from a pasted link or id. Only what everyone in the room
// could see may be pinned, so a pin never shows anyone something they are kept from.
func (h *Handler) frequencyPin(w http.ResponseWriter, r *http.Request, user *model.User, id string, raw map[string]json.RawMessage) {
	workID := frequencyWorkID.FindString(eventField(r, raw, "work"))
	if id == "" || workID == "" || h.db == nil {
		h.frequencyProblem(w, r, http.StatusBadRequest, "work_required", "Paste a link to a work.")
		return
	}
	work, err := dbpkg.GetWorkByID(h.db, workID, user.ID)
	if err != nil || work == nil {
		h.frequencyProblem(w, r, http.StatusNotFound, "not_found", "That work isn't available.")
		return
	}
	room, err := h.auralis.Get(r.Context(), id, user.PIALID)
	if err != nil {
		h.frequencyRefuse(w, r, id, err)
		return
	}
	switch {
	case work.SubscriberOnly:
		h.frequencyProblem(w, r, http.StatusBadRequest, "work_not_pinnable", "Subscriber works can't be pinned to a room.")
		return
	case frequencyAdultWork(work) && !room.Frequency.Frequency.AdultContent:
		h.frequencyProblem(w, r, http.StatusBadRequest, "work_not_pinnable", "Adult works can only be pinned in an 18+ Frequency.")
		return
	}
	h.frequencyAnswer(w, r, user, id, func() (*auralis.Answer, error) {
		return h.auralis.Pin(r.Context(), user.PIALID, id, workID, true)
	})
}

// frequencyUnpin takes a work off a room.
func (h *Handler) frequencyUnpin(w http.ResponseWriter, r *http.Request, user *model.User, id string, raw map[string]json.RawMessage) {
	workID := frequencyWorkID.FindString(eventField(r, raw, "work"))
	if id == "" || workID == "" {
		h.frequencyProblem(w, r, http.StatusBadRequest, "work_required", "No work named.")
		return
	}
	h.frequencyAnswer(w, r, user, id, func() (*auralis.Answer, error) {
		return h.auralis.Pin(r.Context(), user.PIALID, id, workID, false)
	})
}

// frequencyClipRefusal says why a replay can't be clipped, or nothing. Only a public
// replay safe for everyone can be clipped, because a clip goes anywhere once posted.
func frequencyClipRefusal(s *auralis.Summary) string {
	switch {
	case s.State != "archived" || s.ReplayStatus != "ready":
		return "This replay isn't available."
	case !s.ClipsEnabled:
		return "The host turned off clips for this replay."
	case s.Visibility != "public" || s.AdultContent:
		return "Only public replays can be clipped."
	}
	return ""
}

// facetFrequencyClip — GET /facets/frequency/{id}/clip
// The clip form, for anyone who may make one.
func (h *Handler) facetFrequencyClip(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if h.auralis == nil || !h.auralis.Configured() {
		return
	}
	id := r.PathValue("id")
	viewer := h.userFromRequest(w, r)
	if viewerPIALOf(viewer) == "" {
		return
	}
	ans, err := h.auralis.Get(r.Context(), id, viewerPIALOf(viewer))
	if err != nil || frequencyClipRefusal(&ans.Frequency.Frequency) != "" {
		return
	}
	if _, _, refused := h.frequencyGate(viewer, &ans.Frequency); refused {
		return
	}
	frag, err := h.renderFrequencyFragment("frequency_clip_form", map[string]interface{}{"ID": id})
	if err != nil {
		log.Printf("[frequency] %v", err)
		return
	}
	_, _ = w.Write([]byte(frag))
}

// frequencyClip makes a clip of a replay. The transcoder draws the audio as a video
// and runs it through the same pipeline as any upload, so it carries the moving
// watermark. It becomes a work only when the person posts it, under their own key.
func (h *Handler) frequencyClip(w http.ResponseWriter, r *http.Request, user *model.User, id string, raw map[string]json.RawMessage) {
	start, _ := strconv.Atoi(eventField(r, raw, "start_seconds"))
	length, _ := strconv.Atoi(eventField(r, raw, "length_seconds"))
	if id == "" || start < 0 || !frequencyClipLengths[length] {
		h.frequencyProblem(w, r, http.StatusBadRequest, "invalid_clip", "Choose where the clip starts and how long it runs.")
		return
	}
	ans, err := h.auralis.Get(r.Context(), id, user.PIALID)
	if err != nil {
		h.frequencyRefuse(w, r, id, err)
		return
	}
	s := &ans.Frequency.Frequency
	if code, msg, refused := h.frequencyGate(user, &ans.Frequency); refused {
		h.frequencyProblem(w, r, http.StatusForbidden, code, msg)
		return
	}
	if refusal := frequencyClipRefusal(s); refusal != "" {
		h.frequencyProblem(w, r, http.StatusConflict, "clip_refused", refusal)
		return
	}
	file, err := h.auralis.ReplayFile(r.Context(), user.PIALID, id, h.frequencyAudienceFacts(user, s.HostPialID))
	if err != nil {
		h.frequencyProblem(w, r, http.StatusConflict, "clip_refused", "This replay isn't available.")
		return
	}
	if h.db == nil || h.transcodingURL == "" {
		h.frequencyProblem(w, r, http.StatusServiceUnavailable, "clip_unavailable", "Clips aren't available right now.")
		return
	}
	uploadID := uuid.NewString()
	allowed, err := dbpkg.RecordFrequencyClip(h.db, uploadID, id, user.ID, frequencyClipsPerHour)
	if err != nil {
		log.Printf("[frequency] recording clip: %v", err)
		h.frequencyProblem(w, r, http.StatusInternalServerError, "server_error", "Could not make that clip.")
		return
	}
	if !allowed {
		h.frequencyProblem(w, r, http.StatusTooManyRequests, "clip_limit", fmt.Sprintf("You can make %d clips an hour. Try again soon.", frequencyClipsPerHour))
		return
	}
	host := h.frequencyPeople([]string{s.HostPialID})[s.HostPialID]
	jobID, err := h.requestFrequencyClip(file, start, length, s.Title, host.Handle, user.Handle)
	if err != nil {
		log.Printf("[frequency] starting clip of %s: %v", id, err)
		h.frequencyProblem(w, r, http.StatusBadGateway, "clip_unavailable", "Clips aren't available right now.")
		return
	}

	// From here it is an upload like any other, and the person is told how it ended.
	upload := &tusUpload{
		ID: uploadID, Filename: "Frequency clip.mp4", Created: time.Now(),
		UploaderPIAL: user.PIALID, UploaderHandle: user.Handle, JobID: jobID,
	}
	globalTusStore.mu.Lock()
	globalTusStore.uploads[uploadID] = upload
	globalTusStore.mu.Unlock()
	if job, err := dbpkg.OpenVideoJob(h.db, uploadID, user.Handle, user.PIALID, upload.Filename); err != nil {
		log.Printf("[frequency] clip job %s: %v", uploadID, err)
	} else if job != nil {
		if err := dbpkg.SetVideoJobID(h.db, uploadID, jobID); err != nil {
			log.Printf("[frequency] clip job %s: %v", uploadID, err)
		}
	}
	h.writeFrequencyClipStatus(w, id, uploadID, nil, "")
}

func (h *Handler) requestFrequencyClip(file string, start, length int, title, host, clipper string) (string, error) {
	body, err := json.Marshal(map[string]interface{}{
		"file": file, "start_seconds": start, "length_seconds": length,
		"title": title, "host_handle": host, "creator_handle": clipper,
	})
	if err != nil {
		return "", err
	}
	resp, err := h.httpClient.Post(h.transcodingURL+"/v1/frequency/clip", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("transcoder answered %s", resp.Status)
	}
	var queued struct {
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&queued); err != nil {
		return "", err
	}
	return queued.JobID, nil
}

func (h *Handler) writeFrequencyClipStatus(w http.ResponseWriter, id, uploadID string, out *videoJobOutput, failure string) {
	data := map[string]interface{}{"ID": id, "UploadID": uploadID, "Failure": failure, "Ready": false}
	if out != nil {
		video, err := json.Marshal(map[string]interface{}{
			"master_url": out.MasterURL, "watermarked_mp4_url": out.WatermarkedURL, "poster_url": out.PosterURL,
			"duration_secs": out.DurationSecs, "width": out.Width, "height": out.Height, "upload_id": uploadID,
		})
		if err != nil {
			log.Printf("[frequency] clip %s: %v", uploadID, err)
		}
		data["Ready"] = true
		data["VideoJSON"] = string(video)
		data["Poster"] = out.PosterURL
		data["Download"] = out.WatermarkedURL
	}
	frag, err := h.renderFrequencyFragment("frequency_clip_status", data)
	if err != nil {
		log.Printf("[frequency] %v", err)
		http.Error(w, "fragment unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(frag))
}

// facetFrequencyClipStatus — GET /facets/frequency/{id}/clip/{upload}
// The clip's progress, which asks again until it has an answer.
func (h *Handler) facetFrequencyClipStatus(w http.ResponseWriter, r *http.Request) {
	id, uploadID := r.PathValue("id"), r.PathValue("upload")
	user := liveActor(h.userFromRequest(w, r))
	st, ok := h.videoJobState(uploadID)
	if !ok || user == nil || st.UploaderPIAL != user.PIALID {
		h.writeFrequencyClipStatus(w, id, uploadID, nil, "That clip is no longer available.")
		return
	}
	switch st.Status {
	case videoJobReady:
		h.writeFrequencyClipStatus(w, id, uploadID, st.Output, "")
	case videoJobFailed:
		h.writeFrequencyClipStatus(w, id, uploadID, nil, "That clip could not be made. It may start past the end of the replay.")
	default:
		h.writeFrequencyClipStatus(w, id, uploadID, nil, "")
	}
}

// frequencyClipsEnabled is the host switching clips of their replay on or off.
func (h *Handler) frequencyClipsEnabled(w http.ResponseWriter, r *http.Request, user *model.User, id string, raw map[string]json.RawMessage) {
	on := eventBoolField(r, raw, true, "on")
	ans, err := h.auralis.SetClipsEnabled(r.Context(), user.PIALID, id, on)
	if err != nil {
		h.frequencyRefuse(w, r, id, err)
		return
	}
	h.frequencyDirty(id)
	h.renderFrequencyReplayControls(w, id, ans.Frequency.Frequency.ClipsEnabled)
}

func (h *Handler) renderFrequencyReplayControls(w http.ResponseWriter, id string, clipsEnabled bool) {
	frag, err := h.renderFrequencyFragment("frequency_replay_controls", map[string]interface{}{"ID": id, "ClipsEnabled": clipsEnabled})
	if err != nil {
		log.Printf("[frequency] %v", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(frag))
}
