package handler

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"

	"github.com/f33d3r/feed-engine/internal/gnosis"
)

// Sealed-mode endpoints. The server is a blind relay here: it stores and routes
// ciphertext + per-recipient wrapped keys and never sees plaintext or any private
// key. Key material is generated and wrapped in the browser (sealcore WASM); only
// opaque blobs land on these routes. Identity is per-ACCOUNT, not per-PIAL — each
// persona is its own sealed island.

// Bounds on the opaque blobs a client may store, so a member cannot exhaust storage.
const (
	maxSealedBodyB64  = 16000 // ~12 KiB of ciphertext (covers maxMessageBytes plaintext)
	maxSealedFieldB64 = 512   // nonce / eph-pub / wrapped-key fields
	maxSealedRecips   = 256   // per-message recipient fan-out cap
)

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// gnosisBootstrap returns the caller's own wrapped private key so the browser can
// unwrap it after login (with the login-derived key), plus whether an identity
// exists. There is NO separate recovery — the account's backup codes are the sole
// recovery for everything.
// GET /api/gnosis/bootstrap
func (h *Handler) gnosisBootstrap(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, has, _ := gnosis.GetIdentity(h.db, user.ID)
	writeJSON(w, map[string]interface{}{
		"has":          has,
		"wrapped_priv": id.WrappedPrivB64,
		"wrap_nonce":   id.WrapNonceB64,
	})
}

// gnosisProvision stores (or rotates) the calling ACCOUNT's messaging identity: the
// public key in the directory and the private key wrapped under the login-derived
// key. Server stores opaque blobs only. A rotation (overwriting a different key) is
// logged as a security event so a silent key swap can't go unnoticed.
// POST /api/gnosis/provision
func (h *Handler) gnosisProvision(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		PubB64      string `json:"pub_b64"`
		WrappedPriv string `json:"wrapped_priv"`
		WrapNonce   string `json:"wrap_nonce"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil || req.PubB64 == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(req.PubB64) > maxSealedFieldB64 || len(req.WrappedPriv) > maxSealedFieldB64 || len(req.WrapNonce) > maxSealedFieldB64 {
		http.Error(w, "bad key", http.StatusBadRequest)
		return
	}
	// The key directory is elohim-veni's: Manhattan assigns the `key` node kind to
	// that brain, and the contact key bundle is assembled from its rows. Register
	// there first — a failure here must not leave a local key the directory has
	// never heard of.
	if user.PIALID != "" {
		// The directory keys every entry to the identity, so the identity has to
		// exist in the authority before a key can be filed under it.
		if err := h.assertPIAL(r.Context(), user.PIALID); err != nil {
			http.Error(w, "key directory unavailable", http.StatusBadGateway)
			return
		}
		if err := h.callElohim(r.Context(), http.MethodPost, "/v1/pial/messaging-key/register",
			map[string]string{"pial_id": user.PIALID, "public_key_b64": req.PubB64}, nil); err != nil {
			log.Printf("[gnosis] registering messaging key for pial %s: %v", user.PIALID, err)
			http.Error(w, "key directory unavailable", http.StatusBadGateway)
			return
		}
	}

	changed, err := gnosis.UpsertIdentity(h.db, user.ID, req.PubB64, req.WrappedPriv, req.WrapNonce)
	if err != nil {
		http.Error(w, "store failed", http.StatusInternalServerError)
		return
	}
	if changed {
		go LogSecurityEvent(h.db, "gnosis_key_rotated", "medium", user.PIALID, requestIP(r), r.UserAgent(),
			"/api/gnosis/provision", map[string]interface{}{"account": user.ID})
	}
	w.WriteHeader(http.StatusNoContent)
}

// gnosisDirectory returns the X25519 public keys of a conversation's members so the
// caller (who must be a member) can seal to them. Scoping to a conversation the
// caller belongs to prevents arbitrary directory enumeration/harvesting.
// GET /api/gnosis/directory?c=<conversation_id>
func (h *Handler) gnosisDirectory(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	convoID := r.URL.Query().Get("c")
	if convoID == "" {
		http.Error(w, "conversation required", http.StatusBadRequest)
		return
	}
	if ok, _ := gnosis.IsMember(h.db, convoID, user.ID); !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	accounts, _ := gnosis.MemberAccountList(h.db, convoID)
	pubs, _ := gnosis.GetIdentityPubs(h.db, accounts)
	out := make([]map[string]string, 0, len(pubs))
	for a, k := range pubs {
		out = append(out, map[string]string{"account": a, "pub_b64": k})
	}
	writeJSON(w, out)
}

// gnosisSendSealed stores a client-sealed envelope and fans out each recipient's
// own sealed bubble. The server never sees plaintext. Recipients are validated
// against actual membership and all blob sizes are bounded.
// POST /api/gnosis/send-sealed   {c, envelope:{body_ct_b64, body_nonce_b64, sealed:[...]}}
func (h *Handler) gnosisSendSealed(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		C        string `json:"c"`
		Envelope struct {
			BodyCtB64    string `json:"body_ct_b64"`
			BodyNonceB64 string `json:"body_nonce_b64"`
			Sealed       []struct {
				RecipientAccount string `json:"recipient_account"`
				EphPubB64        string `json:"eph_pub_b64"`
				SealedB64        string `json:"sealed_b64"`
				SealedNonceB64   string `json:"sealed_nonce_b64"`
			} `json:"sealed"`
		} `json:"envelope"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || req.C == "" || req.Envelope.BodyCtB64 == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	env := req.Envelope
	if len(env.BodyCtB64) > maxSealedBodyB64 || len(env.BodyNonceB64) > maxSealedFieldB64 {
		http.Error(w, "message too long", http.StatusRequestEntityTooLarge)
		return
	}
	if len(env.Sealed) == 0 || len(env.Sealed) > maxSealedRecips {
		http.Error(w, "bad recipient set", http.StatusBadRequest)
		return
	}
	if ok, _ := gnosis.IsMember(h.db, req.C, user.ID); !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	convo, err := gnosis.GetConversation(h.db, req.C)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if convo.Mode != gnosis.ModeSealed {
		http.Error(w, "not a sealed conversation", http.StatusConflict)
		return
	}

	// Only members may be sealed to; ignore any extras a client tries to stuff.
	members, _ := gnosis.MembersWithPIAL(h.db, req.C)
	memberSet := map[string]bool{}
	for _, m := range members {
		memberSet[m.AccountID] = true
	}

	keys := make([]gnosis.SealedKey, 0, len(env.Sealed))
	keyByAccount := map[string]gnosis.SealedKey{}
	for _, s := range env.Sealed {
		if !memberSet[s.RecipientAccount] {
			continue // not a participant — drop
		}
		if len(s.EphPubB64) > maxSealedFieldB64 || len(s.SealedB64) > maxSealedFieldB64 || len(s.SealedNonceB64) > maxSealedFieldB64 {
			http.Error(w, "bad sealed key", http.StatusBadRequest)
			return
		}
		k := gnosis.SealedKey{
			RecipientAccount: s.RecipientAccount, EphPubB64: s.EphPubB64,
			SealedB64: s.SealedB64, SealedNonceB64: s.SealedNonceB64,
		}
		keys = append(keys, k)
		keyByAccount[s.RecipientAccount] = k
	}
	if len(keys) == 0 {
		http.Error(w, "no valid recipients", http.StatusBadRequest)
		return
	}

	msg, err := gnosis.InsertSealedMessage(h.db, req.C, user.ID, env.BodyCtB64, env.BodyNonceB64, keys)
	if err != nil {
		http.Error(w, "send failed", http.StatusInternalServerError)
		return
	}
	// A message written to this person BEFORE the conversation existed could not
	// have been sealed — sealing needs their key bundle, and that bundle is what
	// contact permission grants. The server held it and rendered it back into
	// this composer rather than writing plaintext into a sealed thread. Whatever
	// was just sealed is what the sender meant to send, so the held copy is
	// released here and cannot resurface.
	if err := gnosis.DeletePendingForConversationPeer(h.db, req.C, user.ID); err != nil {
		log.Printf("[gnosis] releasing a held message for conversation %s: %v", req.C, err)
	}

	// Fan out: each member gets a bubble carrying THEIR sealed key. The sender's own
	// bubble (sealed to self) is returned in the HTTP response for HTMX append.
	//
	// The twin is per-recipient for the same reason the bubble is: a sealed
	// message is a different object to every member, because each holds a
	// different wrapped key. One twin for all of them would hand everybody the
	// same key and decrypt for nobody.
	for _, m := range members {
		k := keyByAccount[m.AccountID]
		bubble := msg
		bubble.EphPubB64 = k.EphPubB64
		bubble.SealedB64 = k.SealedB64
		bubble.SealedNonceB64 = k.SealedNonceB64
		variant := "in"
		if m.AccountID == user.ID {
			variant = "out"
		}
		if m.AccountID == user.ID {
			// The sender's own copy, in the shape they asked for. The web asks
			// for HTML and gets exactly the bubble it has always got.
			if apiWantsJSON(r) {
				who := gnosis.Participant{Handle: user.Handle, Display: user.DisplayName, Avatar: user.AvatarURL}
				apiJSON(w, http.StatusCreated, messageDTO(bubble, user.ID, who))
				continue
			}
			var buf bytes.Buffer
			if err := h.partial.ExecuteTemplate(&buf, "gnosis_bubble", map[string]interface{}{"M": bubble, "Variant": variant}); err != nil {
				continue
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(buf.Bytes())
			continue
		}
		var buf bytes.Buffer
		if err := h.partial.ExecuteTemplate(&buf, "gnosis_bubble", map[string]interface{}{"M": bubble, "Variant": variant}); err != nil {
			continue
		}
		PublishToAccount(m.AccountID, SSEEvent{
			Type: "gnosis_message",
			Data: buf.String(),
			JSON: messagePushJSON(bubble, user.Handle),
		})
	}
}
