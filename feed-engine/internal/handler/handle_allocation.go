package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/f33d3r/feed-engine/internal/observ"
	"github.com/f33d3r/feed-engine/internal/registry"
)

// Allocating a handle.
//
// Every path that brings a new handle into existence goes through here, and
// none of them writes users.handle before it returns nil. That is the whole
// point: a handle is a grant made by registry-brain, and this brain's copy of
// it in users.handle is a cache of that grant, not a second source of it.
//
// The error returned is already phrased for a person — these are all
// user-facing signup forms — while the operator-facing detail goes to the log.

// errHandleTaken and errHandleUnavailable are the two things a person needs to
// be told apart: someone else has this handle, or the registrar could not be
// asked at all. Retrying helps with the second and never with the first.
var (
	errHandleTaken       = errors.New("That handle is already taken. Try another.")
	errHandleUnavailable = errors.New("Handle registration is temporarily unavailable — please try again in a moment.")
)

// allocateHandle grants handle to pialID at the registrar, or returns the
// message to show the person.
//
// An unreachable registrar is a refusal, not a reason to proceed. Writing the
// handle locally "just this once" because the authority was down is exactly how
// a second source of truth is born, and the handle plane spent long enough with
// two of them.
func (h *Handler) allocateHandle(r *http.Request, handle, pialID string) error {
	ctx := registry.WithRequestID(r.Context(), observ.RequestID(r.Context()))

	if _, err := h.registry.Claim(ctx, handle, pialID); err != nil {
		switch {
		case errors.Is(err, registry.ErrTaken), errors.Is(err, registry.ErrReserved):
			return errHandleTaken
		default:
			log.Printf("[registry] handle allocation for %q failed: %v", handle, err)
			return errHandleUnavailable
		}
	}
	return nil
}

// ensureHandleAllocated brings the registrar up to date about a handle that
// already exists locally. Used by the repair paths that mint a PIAL for an
// account which has had its handle since before the registrar knew about it.
//
// Unlike allocateHandle this is not a gate: the account and its handle already
// exist, so refusing here would deny a person something they already have. It
// runs detached because one of its callers is on the session path, and it
// reports loudly, with the boot reconciler as the backstop when it fails.
// The caller passes whatever context it has purely so the repair keeps the
// request id of the action that revealed it; callers with no request in hand
// pass context.Background().
func (h *Handler) ensureHandleAllocated(ctx context.Context, handle, pialID string) {
	if handle == "" || pialID == "" || !h.registry.Configured() {
		return
	}
	// Detached from the request: the person is not waiting on this, and the
	// repair must not die with the response it happened to be noticed during.
	rid := observ.RequestID(ctx)
	go func() {
		ctx, cancel := context.WithTimeout(registry.WithRequestID(context.Background(), rid), 15*time.Second)
		defer cancel()
		if _, _, err := registry.EnsureAllocated(ctx, h.registry, handle, pialID); err != nil {
			log.Printf("[registry] could not reconcile handle %q with the registrar: %v", handle, err)
		}
	}()
}
