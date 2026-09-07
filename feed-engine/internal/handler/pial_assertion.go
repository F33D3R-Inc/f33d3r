package handler

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/f33d3r/feed-engine/internal/observ"
)

// Asserting a PIAL to the identity authority.
//
// A PIAL is minted in this brain — feed-engine writes pial_roots and binds the
// account to it — but it is GOVERNED in elohim-veni, which holds its status,
// its tier and its trust, and which keys every contact row and every messaging
// key to it. Every one of those tables references pial_states, so an identity
// this brain has never asserted cannot hold a Number, a contact policy, a
// contact link, a contact grant, a contact request or a messaging key: the
// authority refuses the write, and contact fails closed for a person who did
// nothing wrong.
//
// The assertion is the contract elohim-veni already publishes — POST
// /v1/pial/bootstrap, the feed-engine → elohim-veni edge in the dependency
// graph — and it is an idempotent upsert on the far side that can neither
// resurrect a revoked PIAL nor change the tier of a live one. It is made here
// rather than only at the moment of signup because the identities that predate
// it need it just as much, and an identity that exists in one brain and not in
// the authority is precisely the drift the assertion exists to close.
//
// Shape follows ensureHandleAllocated: a gate where the person is already
// waiting on a mutation that cannot succeed without it, and a detached repair
// on the session path for every identity that is merely being read.

// pialAssertionRetry is how long a failed assertion is remembered before it is
// attempted again. Without it an unreachable authority would be re-dialled on
// every request; with it the cost of an outage is bounded to one attempt per
// identity per interval.
const pialAssertionRetry = 30 * time.Second

// pialAssertionTimeout bounds a single assertion. It is deliberately far below
// the shared HTTP client's timeout: this call sits in front of work the person
// is waiting for, and an authority that cannot answer in a second is an
// authority that is down.
const pialAssertionTimeout = 2 * time.Second

// pialAsserted remembers what this process has already told the authority.
// A successful assertion is remembered for the life of the process, because it
// cannot become untrue; a failure is remembered only until the retry window
// closes.
var pialAsserted sync.Map // pialID → time.Time (zero = asserted, else last failure)

// assertPIAL tells elohim-veni that this identity exists, and reports whether
// the authority accepted it.
//
// Callers are the paths that are about to ask the authority to store something
// keyed to this identity. They already have an error path for an unreachable
// authority, and this returns into it: refusing here is the same refusal, one
// step earlier and with a cause in the log.
func (h *Handler) assertPIAL(ctx context.Context, pialID string) error {
	if pialID == "" {
		return nil
	}
	if v, ok := pialAsserted.Load(pialID); ok {
		last := v.(time.Time)
		if last.IsZero() {
			return nil
		}
		if time.Since(last) < pialAssertionRetry {
			return errPolicyAuthorityDown
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, pialAssertionTimeout)
	defer cancel()
	if err := h.callElohim(callCtx, http.MethodPost, "/v1/pial/bootstrap",
		map[string]interface{}{"pial_id": pialID}, nil); err != nil {
		pialAsserted.Store(pialID, time.Now())
		log.Printf("[pial] the identity authority would not accept PIAL %s: %v", pialID, err)
		return err
	}
	pialAsserted.Store(pialID, time.Time{})
	return nil
}

// ensurePIALAsserted brings the identity authority up to date about a PIAL that
// already exists locally, without making anyone wait for it.
//
// Unlike assertPIAL this is not a gate: its caller is reading a session, not
// mutating anything, and refusing a page view because another brain is slow
// would deny a person something they already have. It runs detached so the
// repair does not die with the response that happened to reveal it, and it
// reports loudly when the authority refuses.
func (h *Handler) ensurePIALAsserted(ctx context.Context, pialID string) {
	if pialID == "" || h.cfg == nil || h.cfg.ElohimVeniURL == "" {
		return
	}
	if v, ok := pialAsserted.Load(pialID); ok {
		last := v.(time.Time)
		if last.IsZero() || time.Since(last) < pialAssertionRetry {
			return
		}
	}
	rid := observ.RequestID(ctx)
	go func() {
		bg, cancel := context.WithTimeout(observ.WithRequestID(context.Background(), rid), pialAssertionTimeout)
		defer cancel()
		_ = h.assertPIAL(bg, pialID)
	}()
}
