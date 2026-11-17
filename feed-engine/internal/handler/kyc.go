package handler

// kyc.go — identity verification, one fail-closed flow.
//
// In Facet Architecture order:
//
//	triggering events  GET /kyc (the page), POST /kyc/consent, POST /kyc/liveness,
//	                   POST /kyc/submit, POST /kyc/creator/accept
//	handlers           below; each decides on the server and answers with exactly
//	                   one Fragment
//	Facet renderer     kyc_flow and kyc_liveness (partials/_kyc.html)
//	Fragments          facet:f33d3r:kyc:<account>:flow      — the whole flow surface
//	                   facet:f33d3r:kyc:<account>:liveness  — the face-scan challenge
//	stream mutation    the htmx answer to the person's own request replaces the
//	                   Facet it targets; nothing else is told
//
// The flow, with one owner per fact:
//
//  1. consent   — a Fragment says exactly what is collected, why, how long it is
//                 kept, and that no biometric template outlives the decision.
//                 Nothing is captured without it. The consent (version, time,
//                 IP, user agent) is recorded as a kyc_attempts row.
//  2. capture   — the document front, the back of a licence or ID card, a selfie
//                 and the frames answering a liveness challenge ekyc issued.
//                 kyc.js only drives the camera and posts; it decides nothing.
//  3. ekyc      — POST /v1/verify decides pass or fail. A "pass" that lacks any
//                 fact a pass requires is not believed (ekyc.Verdict.Passed).
//  4. Verity    — POST /v1/kyc/submissions records the evidence under the
//                 attempt id, with the consent, and the ID copy only for a
//                 creator pass. 409 document_in_use is a refusal.
//  5. identity  — only on Verity's 201: kyc_tier full, age_verified, the
//                 document's date of birth, locked; source ekyc:<attempt_id>.
//  6. notice    — a herald notification and an email to the account's address.
//
// Fail closed everywhere: an unset EKYC_URL or VERITY_URL, an unreachable
// service, a non-2xx answer, an unreadable field — each means not verified, is
// logged with the attempt, and is told to the person as a result Fragment with
// a reason. There is no development grant and no client-claimed liveness.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/ekyc"
	"github.com/f33d3r/feed-engine/internal/identity"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/verity"
)

// kycConsentVersion names the consent text in kycConsentText. Change the
// version whenever the text changes: Verity records which text was agreed to.
const kycConsentVersion = "2026-11-16.1"

// kycStatementVersion names the 18 U.S.C. § 2257 statement in kyc2257Statement.
const kycStatementVersion = "2026-11-16.1"

// kycMaxBody bounds a submission: two document images, a selfie and a few
// liveness frames, base64-encoded.
const kycMaxBody = 24 << 20

// kycMaxImage bounds one decoded image.
const kycMaxImage = 8 << 20

// kycMinFrames is the fewest liveness frames a submission may carry; ekyc
// decides whether they answer the challenge.
const kycMinFrames = 1

// kyc2257Statement is the record-keeping statement a creator accepts.
const kyc2257Statement = "In accordance with 18 U.S.C. § 2257, all models, actors, actresses and other persons who appear in any visual depiction of actual or simulated sexually explicit conduct appearing on this platform were 18 years of age or older at the time of the creation of such depictions. Records required to be maintained pursuant to 18 U.S.C. § 2257 are kept by F33D3R Platform as custodian of records. F33D3R Platform, online at f33d3r.com. By accepting, you certify that every person who appears in your 18+ work is 18 or older, that you hold the records 18 U.S.C. § 2257 requires for every such person, and that F33D3R keeps the copy of your own ID taken during your creator verification as the record for you."

// kycDocumentTypes is the closed vocabulary of documents a person may present,
// in the order they are offered. Mirrored by the kyc_attempts CHECK (0047).
var kycDocumentTypes = []kycOption{
	{Value: "passport", Label: "Passport"},
	{Value: "drivers_license", Label: "Driving licence"},
	{Value: "id_card", Label: "National ID card"},
}

type kycOption struct{ Value, Label string }

func kycDocumentLabel(t string) string {
	for _, o := range kycDocumentTypes {
		if o.Value == t {
			return o.Label
		}
	}
	return ""
}

// kycNeedsBack reports whether a document type has a back that is captured: a
// licence or ID card carries its barcode or MRZ there.
func kycNeedsBack(docType string) bool { return docType != "passport" }

// kycConsentText is what the person agrees to, for one purpose. It describes
// what really happens; kycConsentVersion changes whenever it does.
func kycConsentText(purpose string) []string {
	text := []string{
		"What we collect: photographs of your government ID (the front, and the back of a driving licence or ID card), a selfie, and a few frames of a short live face check that our server asks for. We also record that you agreed to this, which version of this text you agreed to, the time, your IP address and your browser's user agent.",
		"Why: to confirm that the document is genuine and not expired, that you are 18 or older, that your face matches the document's portrait, and that the face check was done live by a real person. The check runs on F33D3R's own servers; no outside verification company sees your documents.",
		"What is kept from the check: the result and its reasons, the face-match score, the presentation-attack score and the liveness score, the document's type and issuing country, and a keyed hash of the document number so that one document can verify only one identity. The document number itself is never stored. No face template or other biometric template is kept after the decision.",
		"If the check fails: the images are not kept, and the personal details are deleted after 30 days. The record that an attempt was made and why it failed is kept.",
	}
	if purpose == ekyc.PurposeCreator {
		text = append(text,
			"Creator verification: 18 U.S.C. § 2257 requires F33D3R to keep a copy of your ID. If you pass, the document images and your selfie are kept, encrypted, together with the name and date of birth on the document. They are kept for as long as you have content on F33D3R and for 7 years after your last content is removed. Deleting your account alone does not end this.")
	} else {
		text = append(text,
			"Age verification: if you pass, the name and date of birth on the document are kept as the record of your age. No images are kept.")
	}
	return text
}

// kycChallengeView is one liveness challenge as the person reads it.
type kycChallengeView struct{ Name, Label, Hint string }

// kycChallengeWords is what each of ekyc's liveness prompts asks the person to
// do. Directions are the person's own: "your left" is their left.
var kycChallengeWords = map[string][2]string{
	"turn_left":  {"Turn left", "Turn your head to your own left."},
	"turn_right": {"Turn right", "Turn your head to your own right."},
	"look_up":    {"Look up", "Tilt your head back."},
	"smile":      {"Smile", "Give a natural smile."},
}

func kycChallengeViews(names []string) []kycChallengeView {
	out := make([]kycChallengeView, 0, len(names))
	for _, n := range names {
		v := kycChallengeView{Name: n, Label: strings.ReplaceAll(n, "_", " "), Hint: "Follow the instruction."}
		if w, ok := kycChallengeWords[n]; ok {
			v.Label, v.Hint = w[0], w[1]
		}
		out = append(out, v)
	}
	return out
}

// kycReasonTexts says what each of ekyc's reason codes means. The codes are
// ekyc's vocabulary (ekyc/server.py); this handler adds the pass_* codes for a
// "pass" that lacked a fact a pass requires (ekyc.Verdict.Passed).
var kycReasonTextByCode = map[string]string{
	"underage":                        "The document shows you are under 18.",
	"document_expired":                "The document has expired. Use a document that is still valid.",
	"expiry_unreadable":               "The expiry date on the document could not be read. Retake the photo in better light.",
	"no_machine_readable_document":    "The barcode on the back of your licence or ID card, or the two lines of text at the bottom of your passport, could not be read. Retake the photo flat, in better light, with no glare.",
	"barcode_invalid":                 "The barcode on the back of the document is not valid.",
	"mrz_checksum_failed":             "The machine-readable lines on the document did not check out. Retake the photo so both lines are sharp.",
	"front_back_mismatch":             "The front and the back of the document do not belong together.",
	"front_unreadable":                "The front of the document could not be read. Retake it flat, in good light.",
	"document_front_unreadable_image": "The photo of the front of the document could not be opened. Take it again.",
	"document_back_unreadable_image":  "The photo of the back of the document could not be opened. Take it again.",
	"face_image_unreadable":           "Your selfie could not be opened. Run the face scan again.",
	"no_face_in_selfie":               "No face was found in your selfie. Look straight at the camera.",
	"selfie_face_too_small":           "Your face was too small in the selfie. Hold the phone closer.",
	"selfie_not_frontal":              "Your selfie must face the camera straight on with a neutral expression.",
	"no_face_in_document":             "No portrait was found on the document.",
	"document_portrait_too_small":     "The portrait on the document was too small to compare. Fill the frame with the document.",
	"face_mismatch":                   "Your face did not match the portrait on the document.",
	"presentation_attack_suspected":   "The selfie did not look like a live person in front of the camera. Do not use a photo or a screen.",
	"pad_not_evaluated":               "The live-person check could not be run, so the check could not pass.",
	"liveness_not_evaluated":          "The live face check could not be run, so the check could not pass.",
	"liveness_session_expired":        "The face scan took too long. Start the face scan again.",
	"liveness_session_unknown":        "The face scan session was not recognised. Start the face scan again.",
	"liveness_frame_count_mismatch":   "The face scan did not arrive complete. Run it again and follow every prompt.",
	"liveness_frames_duplicated":      "The face scan frames were identical. Run it again and follow every prompt.",
	"liveness_frame_unreadable":       "A face scan frame could not be opened. Run the scan again.",
	"liveness_no_face_in_frame":       "Your face left the frame during the face scan. Keep it in view.",
	"liveness_identity_mismatch":      "The face in the face scan was not the face in the selfie.",
	"request_too_large":               "The images were too large. Try again.",
	"invalid_request":                 "The submission was incomplete. Start again.",
}

// kycReasonText turns one reason code into a sentence. An unknown code is said
// plainly rather than hidden.
func kycReasonText(code string) string {
	c := strings.TrimSpace(code)
	if c == "" {
		return ""
	}
	if t, ok := kycReasonTextByCode[c]; ok {
		return t
	}
	switch {
	case strings.HasPrefix(c, "liveness_"):
		return "The live face check did not pass. Follow each prompt while your face stays in the frame."
	case strings.HasPrefix(c, "pass_"):
		return "The verification service's answer was incomplete, so it could not count as a pass."
	}
	return "The check did not pass (" + code + ")."
}

func kycReasonTexts(codes []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range codes {
		t := kycReasonText(c)
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		out = []string{"The check did not pass."}
	}
	return out
}

// kycFlow is the data of the kyc_flow Facet. Step selects what it shows:
//
//	unavailable     verification cannot be done here now (a service is unset)
//	consent         what is collected and why; agree or decline
//	declined        nothing was collected
//	capture         document and face capture for one consented attempt
//	result          the answer to an attempt or a refusal, with reasons
//	verified        the account's identity is verified
//	statement       the 18 U.S.C. § 2257 statement for a creator to accept
//	creator_active  adult creator mode is on
type kycFlow struct {
	Account  string
	Step     string
	Purpose  string
	ReturnTo string
	// Notice is a sentence shown above the step (a reason it is shown again).
	Notice string

	// consent
	ConsentText    []string
	ConsentVersion string
	DocumentTypes  []kycOption

	// capture
	AttemptID     string
	DocumentType  string
	DocumentLabel string
	NeedsBack     bool
	Liveness      kycLiveness

	// result
	Outcome string // pass | fail | refused | error
	Title   string
	Reasons []string
	Ref     string

	// verified / statement / creator_active
	AgeVerified    bool
	CreatorPass    bool
	AdultCreator   bool
	Statement      string
	ShowCreatorCTA bool
}

// kycLiveness is the data of the kyc_liveness Facet. State: idle (not started),
// ready (a challenge was issued), error (it could not be).
type kycLiveness struct {
	Account    string
	State      string
	AttemptID  string
	SessionID  string
	Challenges []kycChallengeView
	Message    string
}

// kycAvailable reports whether every service the flow depends on is configured.
func (h *Handler) kycAvailable() bool {
	return h.db != nil && h.ekyc.Configured() && h.verity.Configured() && h.identity.Client().Configured()
}

// kycUnavailableReason names what is missing, for the log.
func (h *Handler) kycUnavailableReason() string {
	var missing []string
	if h.db == nil {
		missing = append(missing, "database")
	}
	if !h.ekyc.Configured() {
		missing = append(missing, "EKYC_URL")
	}
	if !h.verity.Configured() {
		missing = append(missing, "VERITY_URL or INTERNAL_API_KEY")
	}
	if !h.identity.Client().Configured() {
		missing = append(missing, "ELOHIM_VENI_URL")
	}
	return strings.Join(missing, ", ")
}

func kycPurpose(raw string) string {
	if raw == ekyc.PurposeCreator {
		return ekyc.PurposeCreator
	}
	return ekyc.PurposeAge
}

// kycReturnTo keeps a return path only when it is a path on this site.
func kycReturnTo(raw string) string {
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") && !strings.Contains(raw, `\`) {
		return raw
	}
	return ""
}

// writeKYC renders one kyc Facet as the answer.
func (h *Handler) writeKYC(w http.ResponseWriter, name string, data interface{}) {
	var buf bytes.Buffer
	if h.partial == nil {
		log.Printf("[kyc] rendering %s: templates not loaded", name)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	if err := h.partial.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("[kyc] rendering %s: %v", name, err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

func (h *Handler) writeKYCFlow(w http.ResponseWriter, f kycFlow) { h.writeKYC(w, "kyc_flow", f) }

// kycResult is the result step.
func kycResult(user *model.User, purpose, returnTo, outcome, title string, reasons []string, ref string) kycFlow {
	return kycFlow{
		Account: user.ID, Step: "result", Purpose: purpose, ReturnTo: returnTo,
		Outcome: outcome, Title: title, Reasons: reasons, Ref: ref,
	}
}

// kycConsentStep is the consent step for a purpose.
func kycConsentStep(user *model.User, purpose, returnTo, notice string) kycFlow {
	return kycFlow{
		Account: user.ID, Step: "consent", Purpose: purpose, ReturnTo: returnTo, Notice: notice,
		ConsentText: kycConsentText(purpose), ConsentVersion: kycConsentVersion, DocumentTypes: kycDocumentTypes,
	}
}

// kycCaptureStep is the capture step for one consented attempt.
func kycCaptureStep(user *model.User, a *dbpkg.KYCAttempt, returnTo, notice string) kycFlow {
	return kycFlow{
		Account: user.ID, Step: "capture", Purpose: a.Purpose, ReturnTo: returnTo, Notice: notice,
		AttemptID: a.AttemptID, DocumentType: a.DocumentType, DocumentLabel: kycDocumentLabel(a.DocumentType),
		NeedsBack: kycNeedsBack(a.DocumentType),
		Liveness:  kycLiveness{Account: user.ID, State: "idle", AttemptID: a.AttemptID},
	}
}

// kycFlowFor decides what the flow shows an account for a purpose. The tier is
// identity's; whether a creator verification passed is Verity's.
func (h *Handler) kycFlowFor(ctx context.Context, user *model.User, purpose, returnTo string) kycFlow {
	if !h.kycAvailable() {
		log.Printf("[kyc] verification unavailable for @%s: not configured: %s", user.Handle, h.kycUnavailableReason())
		return kycFlow{Account: user.ID, Step: "unavailable", Purpose: purpose, ReturnTo: returnTo}
	}
	if user.PIALID == "" {
		log.Printf("[kyc] @%s (%s) has no identity root; verification cannot be recorded", user.Handle, user.ID)
		return kycResult(user, purpose, returnTo, "error", "Verification is not possible on this account",
			[]string{"This account has no identity to record a verification on. Contact support."}, "")
	}
	if purpose == ekyc.PurposeAge {
		if user.IsIdentityVerified() {
			return kycFlow{Account: user.ID, Step: "verified", Purpose: purpose, ReturnTo: returnTo,
				AgeVerified: user.IsAgeVerified, ShowCreatorCTA: !user.IsAdultCreator}
		}
		return kycConsentStep(user, purpose, returnTo, "")
	}

	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	creatorPass, err := h.verity.Eligible2257(cctx, user.PIALID)
	if err != nil {
		log.Printf("[kyc] creator status of @%s: Verity could not be asked: %v", user.Handle, err)
		return kycResult(user, purpose, returnTo, "error", "We could not check your creator verification",
			[]string{"The verification records could not be reached. Nothing was changed. Try again shortly."}, "")
	}
	switch {
	case !creatorPass || !user.IsIdentityVerified():
		// No creator pass, or one whose identity verification is no longer
		// held: the creator checks are run (again). A document already bound
		// to this identity passes again; Verity is idempotent on attempt_id.
		return kycConsentStep(user, purpose, returnTo, "")
	case user.IsAdultCreator:
		return kycFlow{Account: user.ID, Step: "creator_active", Purpose: purpose, ReturnTo: returnTo,
			AgeVerified: user.IsAgeVerified, CreatorPass: true, AdultCreator: true}
	}
	return kycFlow{Account: user.ID, Step: "statement", Purpose: purpose, ReturnTo: returnTo,
		CreatorPass: true, Statement: kyc2257Statement}
}

// ── GET /kyc ─────────────────────────────────────────────────────────────────

func (h *Handler) kycPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	purpose := kycPurpose(r.URL.Query().Get("mode"))
	flow := h.kycFlowFor(r.Context(), user, purpose, kycReturnTo(r.URL.Query().Get("return_to")))
	title := "Age verification · F33D3R"
	if purpose == ekyc.PurposeCreator {
		title = "Creator verification · F33D3R"
	}
	h.render(w, r, "kyc.html", map[string]interface{}{
		"User":       user,
		"Flow":       flow,
		"Title":      title,
		"SessionID":  uuid.New().String(),
		"ShowScores": h.cfg.ShowScores,
		"Themes":     ThemesWithActive(user.ThemeID),
	})
}

// ── POST /kyc/consent ────────────────────────────────────────────────────────

func (h *Handler) kycConsent(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if err := r.ParseForm(); err != nil {
		log.Printf("[kyc] consent form from @%s: %v", user.Handle, err)
		h.writeKYCFlow(w, kycResult(user, ekyc.PurposeAge, "", "error", "Your answer could not be read",
			[]string{"Nothing was collected. Reload the page and try again."}, ""))
		return
	}
	purpose := kycPurpose(r.FormValue("purpose"))
	returnTo := kycReturnTo(r.FormValue("return_to"))

	if r.FormValue("decision") == "decline" {
		log.Printf("[kyc] @%s declined the %s verification consent (version %s)", user.Handle, purpose, kycConsentVersion)
		h.writeKYCFlow(w, kycFlow{Account: user.ID, Step: "declined", Purpose: purpose, ReturnTo: returnTo})
		return
	}
	// The flow is decided again, so a person already verified is never asked
	// to consent to a collection they do not need.
	if f := h.kycFlowFor(r.Context(), user, purpose, returnTo); f.Step != "consent" {
		h.writeKYCFlow(w, f)
		return
	}
	if r.FormValue("consent") != "yes" {
		h.writeKYCFlow(w, kycConsentStep(user, purpose, returnTo,
			"You did not tick the box, so nothing was collected. Tick it to agree, or choose “Not now”."))
		return
	}
	docType := r.FormValue("document_type")
	if kycDocumentLabel(docType) == "" {
		h.writeKYCFlow(w, kycConsentStep(user, purpose, returnTo, "Choose the document you will use."))
		return
	}
	ip := requestIP(r)
	if net.ParseIP(ip) == nil {
		log.Printf("[kyc] consent from @%s refused: the client address %q is not an IP address", user.Handle, ip)
		h.writeKYCFlow(w, kycResult(user, purpose, returnTo, "error", "Your consent could not be recorded",
			[]string{"Your connection's address could not be read, so your consent could not be recorded. Nothing was collected."}, ""))
		return
	}
	ua := r.UserAgent()
	if len(ua) > 512 {
		ua = ua[:512]
	}
	a := dbpkg.KYCAttempt{
		AttemptID: uuid.NewString(), AccountID: user.ID, PIALID: user.PIALID,
		Purpose: purpose, DocumentType: docType,
		ConsentVersion: kycConsentVersion, ConsentAcceptedAt: time.Now().UTC(),
		ConsentIP: ip, ConsentUserAgent: ua,
	}
	if err := dbpkg.CreateKYCAttempt(h.db, a); err != nil {
		log.Printf("[kyc] @%s: %v", user.Handle, err)
		h.writeKYCFlow(w, kycResult(user, purpose, returnTo, "error", "Your consent could not be recorded",
			[]string{"Nothing was collected. Try again shortly."}, ""))
		return
	}
	log.Printf("[kyc] @%s consented to %s verification, attempt %s, consent version %s", user.Handle, purpose, a.AttemptID, kycConsentVersion)
	h.writeKYCFlow(w, kycCaptureStep(user, &a, returnTo, ""))
}

// ── POST /kyc/liveness ───────────────────────────────────────────────────────

// kycLiveness issues the face-scan challenge for an open attempt. ekyc decides
// the challenges and later verifies the frames against this session.
func (h *Handler) kycLiveness(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	// The button sits in the capture form, which is multipart; only the
	// attempt id is sent (hx-params), in whichever encoding the form uses.
	r.Body = http.MaxBytesReader(w, r.Body, kycMaxBody)
	if err := r.ParseMultipartForm(1 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		log.Printf("[kyc] liveness form from @%s: %v", user.Handle, err)
		h.writeKYC(w, "kyc_liveness", kycLiveness{Account: user.ID, State: "error",
			Message: "The request could not be read. Reload the page and start again."})
		return
	}
	attemptID := r.FormValue("attempt_id")
	view := kycLiveness{Account: user.ID, AttemptID: attemptID}
	if _, err := uuid.Parse(attemptID); err != nil {
		view.State, view.Message = "error", "This verification has no attempt. Reload the page and start again."
		h.writeKYC(w, "kyc_liveness", view)
		return
	}
	if _, err := dbpkg.OpenKYCAttempt(h.db, attemptID, user.ID); err != nil {
		if !errors.Is(err, dbpkg.ErrKYCAttemptUnavailable) {
			log.Printf("[kyc] liveness for @%s: %v", user.Handle, err)
		}
		view.State, view.Message = "error", "Your consent for this attempt has expired or was already used. Reload the page and start again."
		h.writeKYC(w, "kyc_liveness", view)
		return
	}
	if !h.ekyc.Configured() {
		log.Printf("[kyc] liveness for @%s attempt %s: EKYC_URL is not configured", user.Handle, attemptID)
		view.State, view.Message = "error", "Verification is not available right now."
		h.writeKYC(w, "kyc_liveness", view)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	ch, err := h.ekyc.NewChallenge(ctx)
	if err != nil {
		log.Printf("[kyc] liveness challenge for @%s attempt %s: %v", user.Handle, attemptID, err)
		view.State, view.Message = "error", "The face scan could not be started. Try again."
		h.writeKYC(w, "kyc_liveness", view)
		return
	}
	view.State, view.SessionID, view.Challenges = "ready", ch.SessionID, kycChallengeViews(ch.Challenges)
	h.writeKYC(w, "kyc_liveness", view)
}

// ── POST /kyc/submit ─────────────────────────────────────────────────────────

// kycEvidence is one submission's images, base64 without a data: prefix.
type kycEvidence struct {
	front, back, face string
	sessionID         string
	frames            []string
}

// kycImage reads one base64 image field: the data: prefix is dropped, and the
// rest must decode and fit kycMaxImage. "" with no error means it is absent.
func kycImage(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if strings.HasPrefix(raw, "data:") {
		i := strings.Index(raw, ",")
		if i < 0 {
			return "", errors.New("malformed data URL")
		}
		raw = raw[i+1:]
	}
	if base64.StdEncoding.DecodedLen(len(raw)) > kycMaxImage {
		return "", fmt.Errorf("image larger than %d bytes", kycMaxImage)
	}
	if _, err := base64.StdEncoding.DecodeString(raw); err != nil {
		return "", fmt.Errorf("not base64: %w", err)
	}
	return raw, nil
}

// readKYCEvidence reads the submission. A missing or unreadable part is named
// in a sentence the person reads.
func readKYCEvidence(r *http.Request, docType string) (*kycEvidence, string) {
	var ev kycEvidence
	var err error
	if ev.front, err = kycImage(r.FormValue("document_front")); err != nil || ev.front == "" {
		return nil, "The front of your document was not captured. Capture it and try again."
	}
	if kycNeedsBack(docType) {
		if ev.back, err = kycImage(r.FormValue("document_back")); err != nil || ev.back == "" {
			return nil, "The back of your document was not captured. Capture it and try again."
		}
	}
	if ev.face, err = kycImage(r.FormValue("face_image")); err != nil || ev.face == "" {
		return nil, "Your selfie was not captured. Run the face scan and try again."
	}
	ev.sessionID = strings.TrimSpace(r.FormValue("liveness_session_id"))
	var frames []string
	if raw := r.FormValue("liveness_challenge_frames"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &frames); err != nil {
			return nil, "The face scan did not arrive intact. Run it again."
		}
	}
	for _, f := range frames {
		img, err := kycImage(f)
		if err != nil || img == "" {
			return nil, "The face scan did not arrive intact. Run it again."
		}
		ev.frames = append(ev.frames, img)
	}
	if ev.sessionID == "" || len(ev.frames) < kycMinFrames {
		return nil, "The face scan was not completed. Run it and follow each prompt."
	}
	return &ev, ""
}

func (h *Handler) kycSubmit(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	r.Body = http.MaxBytesReader(w, r.Body, kycMaxBody)
	if err := r.ParseMultipartForm(kycMaxBody); err != nil {
		var tooBig *http.MaxBytesError
		reason := "The submission could not be read. Start again."
		if errors.As(err, &tooBig) {
			reason = "The images were too large. Start again; uploaded photos should be under 8 MB each."
		}
		log.Printf("[kyc] submission from @%s unreadable: %v", user.Handle, err)
		h.writeKYCFlow(w, kycResult(user, kycPurpose(r.FormValue("purpose")), "", "error", "Your verification was not submitted", []string{reason}, ""))
		return
	}
	returnTo := kycReturnTo(r.FormValue("return_to"))
	if !h.kycAvailable() {
		log.Printf("[kyc] submission from @%s refused: not configured: %s", user.Handle, h.kycUnavailableReason())
		h.writeKYCFlow(w, kycFlow{Account: user.ID, Step: "unavailable", Purpose: kycPurpose(r.FormValue("purpose")), ReturnTo: returnTo})
		return
	}

	attemptID := r.FormValue("attempt_id")
	if _, err := uuid.Parse(attemptID); err != nil {
		h.writeKYCFlow(w, kycResult(user, kycPurpose(r.FormValue("purpose")), returnTo, "error", "Your verification was not submitted",
			[]string{"This submission names no verification attempt. Start again."}, ""))
		return
	}
	open, err := dbpkg.OpenKYCAttempt(h.db, attemptID, user.ID)
	if err != nil {
		if !errors.Is(err, dbpkg.ErrKYCAttemptUnavailable) {
			log.Printf("[kyc] submission %s from @%s: %v", attemptID, user.Handle, err)
		}
		h.writeKYCFlow(w, kycResult(user, kycPurpose(r.FormValue("purpose")), returnTo, "error", "Your verification was not submitted",
			[]string{"Your consent for this attempt has expired or was already used. Nothing was sent. Start again."}, ""))
		return
	}
	ev, missing := readKYCEvidence(r, open.DocumentType)
	if missing != "" {
		// The attempt stays open: the person captures what is missing.
		h.writeKYCFlow(w, kycCaptureStep(user, open, returnTo, missing))
		return
	}
	a, err := dbpkg.ClaimKYCAttempt(h.db, attemptID, user.ID)
	if err != nil {
		if !errors.Is(err, dbpkg.ErrKYCAttemptUnavailable) {
			log.Printf("[kyc] claiming attempt %s for @%s: %v", attemptID, user.Handle, err)
		}
		h.writeKYCFlow(w, kycResult(user, open.Purpose, returnTo, "error", "Your verification was not submitted",
			[]string{"This attempt was already submitted. Reload the page to see where it stands."}, attemptID))
		return
	}
	// The consent is carried to Verity with the evidence; the local row is
	// orchestration only and goes once the attempt is answered, whatever the answer.
	defer func() {
		if err := dbpkg.DeleteKYCAttempt(h.db, a.AttemptID); err != nil {
			log.Printf("[kyc] %v", err)
		}
	}()

	h.writeKYCFlow(w, h.decideKYCAttempt(r.Context(), user, a, ev, returnTo))
}

// decideKYCAttempt runs steps 3 to 6 for one claimed attempt and returns the
// Fragment that answers it.
func (h *Handler) decideKYCAttempt(ctx context.Context, user *model.User, a *dbpkg.KYCAttempt, ev *kycEvidence, returnTo string) kycFlow {
	purpose := a.Purpose
	notVerified := func(title, reason string) kycFlow {
		return kycResult(user, purpose, returnTo, "error", title, []string{reason, "You have not been verified."}, a.AttemptID)
	}

	// 3. ekyc decides.
	vctx, cancel := context.WithTimeout(ctx, 75*time.Second)
	verdict, err := h.ekyc.Verify(vctx, ekyc.VerifyRequest{
		Purpose: purpose, DocumentFront: ev.front, DocumentBack: ev.back, FaceImage: ev.face,
		LivenessSessionID: ev.sessionID, LivenessChallengeFrames: ev.frames,
	})
	cancel()
	if err != nil {
		log.Printf("[kyc] attempt %s (@%s, %s): ekyc gave no decision: %v", a.AttemptID, user.Handle, purpose, err)
		if errors.Is(err, ekyc.ErrBusy) {
			return notVerified("Verification is busy", "Verification is busy right now. Try again shortly.")
		}
		return notVerified("The check could not be completed", "The verification service did not give an answer, so nothing was decided.")
	}
	passed, unbelieved := verdict.Passed()
	decision, reasons := "fail", append([]string(nil), verdict.Reasons...)
	if passed {
		decision = "pass"
	} else if verdict.Decision == "pass" {
		log.Printf("[kyc] CRITICAL: attempt %s (@%s): ekyc said pass without %v — recorded as a fail", a.AttemptID, user.Handle, unbelieved)
		reasons = append(reasons, unbelieved...)
	}
	if decision == "fail" && len(reasons) == 0 {
		reasons = []string{"unspecified"}
	}

	// 4. Verity records the evidence.
	d := verdict.Document
	sub := verity.Submission{
		AttemptID: a.AttemptID, PIALID: a.PIALID, Purpose: purpose, Decision: decision,
		Reasons: reasons, EvidenceTypes: verdict.EvidenceTypes,
		DocHMAC: d.DocHMAC, DocType: d.Type, IssuingCountry: d.IssuingCountry, IssuingRegion: d.IssuingRegion,
		DocExpiry: d.Expiry,
		Scores:    verity.Scores{FaceCosine: verdict.Face.Cosine, PAD: verdict.PAD.Score, Liveness: verdict.Liveness.Score},
		Consent: verity.Consent{
			Version: a.ConsentVersion, AcceptedAt: a.ConsentAcceptedAt.UTC().Format(time.RFC3339),
			IP: a.ConsentIP, UserAgent: a.ConsentUserAgent,
		},
	}
	if sub.EvidenceTypes == nil {
		sub.EvidenceTypes = []string{}
	}
	if decision == "pass" {
		// A name and a date of birth are recorded only for a pass.
		sub.LegalName, sub.DOB = d.LegalName(), d.DOB
		if purpose == ekyc.PurposeCreator {
			// 18 U.S.C. § 2257: the copy of the performer's ID, and the
			// selfie taken with it as the portrait.
			sub.IDCopy = &verity.IDCopy{Front: ev.front, Back: ev.back, Portrait: ev.face}
		}
	}
	sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	result, err := h.verity.Submit(sctx, sub)
	cancel()
	if err != nil {
		log.Printf("[kyc] attempt %s (@%s, %s, %s): Verity did not record it: %v", a.AttemptID, user.Handle, purpose, decision, err)
		return notVerified("Your result could not be recorded", "The verification records could not be written, so the result cannot count.")
	}
	switch result {
	case verity.SubmissionDocumentInUse:
		log.Printf("[kyc] attempt %s (@%s): refused — the document already verified another identity", a.AttemptID, user.Handle)
		return kycResult(user, purpose, returnTo, "refused", "This document is already in use",
			[]string{"This document is already verified on another account. One document verifies one person's identity.",
				"You have not been verified. If this is your document and you did not verify another account, contact support with the reference below."},
			a.AttemptID)
	case verity.SubmissionRecordedFail:
		if decision == "pass" {
			log.Printf("[kyc] CRITICAL: attempt %s (@%s): Verity recorded a pass as not accepted", a.AttemptID, user.Handle)
			return notVerified("Your result could not be recorded", "The verification records did not accept the result.")
		}
		log.Printf("[kyc] attempt %s (@%s, %s): failed: %v", a.AttemptID, user.Handle, purpose, reasons)
		return kycResult(user, purpose, returnTo, "fail", "Verification did not pass", kycReasonTexts(reasons), a.AttemptID)
	case verity.SubmissionAccepted:
		if decision != "pass" {
			log.Printf("[kyc] CRITICAL: attempt %s (@%s): Verity accepted a failed attempt as a pass", a.AttemptID, user.Handle)
			return notVerified("Your result could not be recorded", "The verification records gave an inconsistent answer.")
		}
	}

	// 5. Identity records the tier, only now.
	pending, err := h.recordKYCPass(ctx, user, a.AttemptID, d.DOB)
	if err != nil {
		log.Printf("[kyc] CRITICAL: attempt %s (@%s): Verity holds a pass, identity refused it: %v", a.AttemptID, user.Handle, err)
		return kycResult(user, purpose, returnTo, "error", "Your verification could not be applied",
			[]string{"Your documents passed, but your account could not be updated: " + adminIdentityRefusal(err),
				"Contact support with the reference below."}, a.AttemptID)
	}
	log.Printf("[kyc] attempt %s (@%s, %s): passed; identity %s", a.AttemptID, user.Handle, purpose,
		map[bool]string{true: "queued for delivery", false: "recorded"}[pending])

	// 6. The notice.
	h.noticeKYCPass(user, a.AttemptID, time.Now())

	if purpose == ekyc.PurposeCreator {
		return kycFlow{Account: user.ID, Step: "statement", Purpose: purpose, ReturnTo: returnTo,
			CreatorPass: true, Statement: kyc2257Statement,
			Notice: "Your creator verification passed. Read and accept the record-keeping statement to turn on adult creator mode."}
	}
	f := kycResult(user, purpose, returnTo, "pass", "You're verified",
		[]string{"Your identity and age are verified on this account."}, a.AttemptID)
	if pending {
		f.Reasons = append(f.Reasons, "It can take a minute to show everywhere.")
	}
	return f
}

// recordKYCPass sets the identity facts of a pass Verity accepted: kyc_tier
// full, age_verified, the document's date of birth, locked, with the attempt
// as provenance. When elohim-veni cannot be reached the write is queued in
// identity_commands and delivered by the relay until it is accepted (pending);
// a refusal is returned.
func (h *Handler) recordKYCPass(ctx context.Context, user *model.User, attemptID, dob string) (pending bool, err error) {
	tier, yes := model.KYCTierFull, true
	update := identity.FactsUpdate{
		KYCTier: &tier, AgeVerified: &yes, DOB: &dob, DOBLocked: &yes,
		Source: "ekyc:" + attemptID,
	}
	err = h.identity.Client().PutFacts(ctx, user.PIALID, update)
	switch {
	case err == nil:
	case identity.IsUnavailable(err):
		log.Printf("[kyc] attempt %s (@%s): elohim-veni unavailable, queueing the identity write: %v", attemptID, user.Handle, err)
		if qerr := dbpkg.WithTx(h.db, func(tx *sql.Tx) error {
			return dbpkg.EnqueueIdentityCommand(tx, identity.CommandFacts, user.PIALID, update)
		}); qerr != nil {
			return false, fmt.Errorf("elohim-veni unavailable (%v) and the write could not be queued: %w", err, qerr)
		}
		pending = true
	default:
		return false, err
	}
	h.evictCachedAccount(user.ID, "")
	return pending, nil
}

// noticeKYCPass tells the account its identity was verified: a herald notice
// and an email to the account's address. Neither undoes the verification when
// it fails; each failure is logged.
func (h *Handler) noticeKYCPass(user *model.User, attemptID string, at time.Time) {
	if err := dbpkg.NotifyVerified(h.db, user.ID, attemptID, at); err != nil {
		log.Printf("[kyc] attempt %s (@%s): verified, but the notification was not written: %v", attemptID, user.Handle, err)
	}
	email, err := dbpkg.GetUserEmail(h.db, user.ID)
	if err != nil {
		log.Printf("[kyc] attempt %s (@%s): verified, but the email address could not be read: %v", attemptID, user.Handle, err)
		return
	}
	if email == "" {
		log.Printf("[kyc] attempt %s (@%s): verified; no email address on the account, so the notice is in-app only", attemptID, user.Handle)
		return
	}
	when := at.UTC().Format("Jan 2, 2006 15:04 UTC")
	body := fmt.Sprintf(`<p>Your identity was verified on f33d3r at %s.</p><p>If this wasn't you, contact support.</p>`, when)
	if err := h.sendEmail(email, "Your identity was verified on f33d3r", body); err != nil {
		log.Printf("[kyc] attempt %s (@%s): verified, but the email notice was not sent: %v", attemptID, user.Handle, err)
	}
}

// ── POST /kyc/creator/accept ─────────────────────────────────────────────────

// kycCreatorAccept turns on adult creator mode once the creator has a passing
// creator verification at Verity and accepts the 2257 statement. An identity
// tier alone — an admin override included — never satisfies it.
func (h *Handler) kycCreatorAccept(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if err := r.ParseForm(); err != nil {
		log.Printf("[kyc] creator accept form from @%s: %v", user.Handle, err)
		h.writeKYCFlow(w, kycResult(user, ekyc.PurposeCreator, "", "error", "Your answer could not be read",
			[]string{"Nothing was changed. Reload the page and try again."}, ""))
		return
	}
	returnTo := kycReturnTo(r.FormValue("return_to"))
	flow := h.kycFlowFor(r.Context(), user, ekyc.PurposeCreator, returnTo)
	if flow.Step != "statement" {
		// Not yet verified, already active, or the records could not be read:
		// the flow says which.
		h.writeKYCFlow(w, flow)
		return
	}
	if user.IsMinor {
		h.writeKYCFlow(w, kycResult(user, ekyc.PurposeCreator, returnTo, "refused", "Adult creator mode is not available",
			[]string{"Adult creator mode cannot be turned on for this account."}, ""))
		return
	}
	if r.FormValue("accept_2257") != "yes" {
		flow.Notice = "Tick the box to accept the statement, or leave this page."
		h.writeKYCFlow(w, flow)
		return
	}
	if _, err := h.db.Exec(`
		UPDATE user_profiles
		   SET is_adult_creator = TRUE, adult_creator_pending = FALSE, content_setting = 'adult_enabled'
		 WHERE user_id = $1`, user.ID); err != nil {
		log.Printf("[kyc] creator accept for @%s: %v", user.Handle, err)
		h.writeKYCFlow(w, kycResult(user, ekyc.PurposeCreator, returnTo, "error", "Adult creator mode was not turned on",
			[]string{"Your acceptance could not be saved. Try again."}, ""))
		return
	}
	dbpkg.LogPIALEvent(h.db, user.PIALID, user.ID, "creator_2257_statement_accepted", map[string]interface{}{
		"statement_version": kycStatementVersion,
	}, "kyc")
	h.evictCachedAccount(user.ID, "")
	log.Printf("[kyc] @%s accepted the 2257 statement (version %s); adult creator mode on", user.Handle, kycStatementVersion)
	h.writeKYCFlow(w, kycFlow{Account: user.ID, Step: "creator_active", Purpose: ekyc.PurposeCreator, ReturnTo: returnTo,
		AgeVerified: true, CreatorPass: true, AdultCreator: true})
}
