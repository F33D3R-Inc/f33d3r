package handler

// vision.go — the /visions surface.
//
// /visions is the Vision lane's Playground. It used to read works.kind='vision',
// a kind migration 0008 removed from the Work lane entirely, so the surface was
// reading rows nothing writes any more. The reels feed, the ring rail and the
// sequential viewer all live in vision_view.go and read the visions lane, where
// expiry is enforced in SQL on every read.
//
// What remains here is the capture surface: the camera that posts to POST /visions.

import (
	"net/http"
)

// visionCameraPage — GET /visions/camera
// The capture surface. Its shutter posts to POST /visions, so a captured frame
// enters the ephemeral lane through the lane's one writer.
func (h *Handler) visionCameraPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	h.render(w, r, "vision_camera.html", map[string]interface{}{
		"User": user,
	})
}
