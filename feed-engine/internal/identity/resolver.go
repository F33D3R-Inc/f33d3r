package identity

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/f33d3r/feed-engine/internal/model"
)

// Resolver resolves session tokens and verifies the access token each
// resolution carries, so a session is accepted only when the body the
// authority answered with and the token it signed agree about who it is.
//
// Concurrent resolutions of the same token are coalesced: a page that fans out
// into a dozen Facet requests on a cold cache asks the authority once, not a
// dozen times.
type Resolver struct {
	client   *Client
	verifier *Verifier

	mu       sync.Mutex
	inflight map[string]*resolveCall
}

type resolveCall struct {
	done chan struct{}
	sess *Session
	err  error
}

// NewResolver returns a resolver over client.
func NewResolver(client *Client) *Resolver {
	return &Resolver{client: client, verifier: NewVerifier(client), inflight: map[string]*resolveCall{}}
}

// Client returns the client the resolver uses. A nil resolver yields a nil
// client, whose every call fails closed with ErrUnavailable.
func (r *Resolver) Client() *Client {
	if r == nil {
		return nil
	}
	return r.client
}

// Verifier returns the resolver's access-token verifier.
func (r *Resolver) Verifier() *Verifier { return r.verifier }

// Resolve answers who is behind token. ErrSessionInvalid: nobody. ErrUnavailable
// or a token verification failure: nobody could be confirmed, which the caller
// treats exactly as an outage — fail closed.
func (r *Resolver) Resolve(ctx context.Context, token string) (*Session, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: no identity resolver configured", ErrUnavailable)
	}
	r.mu.Lock()
	if call, ok := r.inflight[token]; ok {
		r.mu.Unlock()
		select {
		case <-call.done:
			return call.sess, call.err
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, ctx.Err())
		}
	}
	call := &resolveCall{done: make(chan struct{})}
	r.inflight[token] = call
	r.mu.Unlock()

	// The authority's answer must not die with whichever request happened to
	// ask first, since others are waiting on it; the client's own deadline
	// bounds it.
	call.sess, call.err = r.resolve(context.WithoutCancel(ctx), token)

	r.mu.Lock()
	delete(r.inflight, token)
	r.mu.Unlock()
	close(call.done)
	return call.sess, call.err
}

func (r *Resolver) resolve(ctx context.Context, token string) (*Session, error) {
	sess, err := r.client.ResolveSession(ctx, token)
	if err != nil {
		return nil, err
	}
	claims, err := r.verifier.Verify(ctx, sess.AccessToken)
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			return nil, err
		}
		// The authority handed back a token its own key set does not vouch for.
		// That is not a statement about the person; it is a broken authority,
		// and nothing it said in this answer can be trusted.
		return nil, fmt.Errorf("%w: resolved session carries an unverifiable access token: %v", ErrUnavailable, err)
	}
	if claims.AccountID != sess.AccountID || claims.PIALID != sess.PIALID {
		return nil, fmt.Errorf("%w: resolved session and its access token name different identities", ErrUnavailable)
	}
	sess.SessionID = claims.SessionID
	return sess, nil
}

// Stopped reports whether this session's identity may no longer act on the
// platform: any status but active, or both posting and messaging withdrawn —
// the shape every hold and ban takes.
func (s *Session) Stopped() bool {
	if s == nil {
		return true
	}
	if s.Status != StatusActive {
		return true
	}
	return !s.Can(model.CapPosting) && !s.Can(model.CapMessaging)
}

// Can reports whether the session's identity currently holds capability.
func (s *Session) Can(capability string) bool {
	if s == nil {
		return false
	}
	for _, c := range s.Caps {
		if c == capability {
			return true
		}
	}
	return false
}
