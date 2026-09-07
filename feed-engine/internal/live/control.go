package live

// control.go — the media server's control plane inside the application server.
//
// Five things happen here and nothing else:
//
//   POST /live/auth            the media server asks whether a publish may proceed
//   POST /live/hook/ready      the ladder has probed the source and asks for its plan
//   POST /live/hook/playable   the ladder has written the first segment to disk
//   POST /live/hook/pressure   the ladder cannot hold realtime
//   POST /live/hook/notready   the publisher stopped
//
// ready and playable are two hooks and not one because they are two moments.
// The ladder probes a source for twelve to twenty-five seconds before it can
// ask for a plan, and ffmpeg writes the first segment a few seconds after the
// plan arrives. A row that says live at the first moment is a row that says
// live over a playlist that does not exist: every viewer's decoder was fetching
// a 404 for the whole of that gap, and the one that gave up in it stayed dark
// for the rest of the broadcast. The status moves at the second moment only.
//
// This listener is bound to its own address on the internal network and is not
// published to the host. It is deliberately not mounted on the public router:
// the media server's callbacks have no session, and routing them through the
// viewer-facing middleware would mean either weakening that middleware or
// carving an exception through it.

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// authRequest is the payload MediaMTX posts to its external authentication URL.
type authRequest struct {
	User     string `json:"user"`
	Password string `json:"password"`
	Token    string `json:"token"`
	IP       string `json:"ip"`
	Action   string `json:"action"`   // publish | read | playback | api | metrics | pprof
	Path     string `json:"path"`     // the stream id
	Protocol string `json:"protocol"` // rtmp | srt | webrtc | rtsp | hls
	ID       string `json:"id"`
	Query    string `json:"query"`
}

// hookRequest is what the transcoder posts on a stream lifecycle edge.
//
// Everything past Protocol is measurement, not request. The ladder reports what
// the source actually is — its codec, the rate it declared, the keyframe
// interval the publisher chose — and the server decides what to do about it.
// The ladder asks for nothing and is told everything.
type hookRequest struct {
	StreamID string `json:"stream_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Protocol string `json:"protocol"`
	Codec    string `json:"codec"`
	FPS      int    `json:"fps"`
	// FPSDeclared distinguishes a rate the source stated from the one the
	// ladder fell back to. Only the former can carry a passed-through rung.
	FPSDeclared bool    `json:"fps_declared"`
	GOPSeconds  float64 `json:"gop_seconds"`
	// NVENCAvailable is the ladder's answer to whether it can actually open a
	// hardware encode session, proved by opening one.
	NVENCAvailable bool `json:"nvenc_available"`
	// Speed is the realtime ratio a ladder reports when it cannot keep up. It
	// is only ever set on the pressure hook.
	Speed float64 `json:"speed"`
}

func (m *Manager) controlRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /live/auth", m.handleAuth)
	mux.HandleFunc("POST /live/hook/ready", m.handleReady)
	mux.HandleFunc("POST /live/hook/playable", m.handlePlayable)
	mux.HandleFunc("POST /live/hook/notready", m.handleNotReady)
	mux.HandleFunc("POST /live/hook/pressure", m.handlePressure)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	return internalOnly(mux)
}

// internalOnly enforces in code what this file's contract has only asserted in
// prose: the control plane is reachable from the container network and nowhere
// else.
//
// The lifecycle hooks carry X-Internal-Key, but /live/auth cannot: MediaMTX's
// external authentication offers authHTTPAddress, authHTTPExclude and
// authHTTPFingerprint and no way to add a request header, so the media server
// has no channel through which to present a shared secret. That left the one
// endpoint that decides who may publish as the only unauthenticated one — and
// the listener binds every interface, so the only thing standing between it and
// an outstanding publish ticket was the fact that nobody had published the port.
//
// The peer address is a control MediaMTX can satisfy without changing anything:
// it always calls from the container network, and so do the ladder's lifecycle
// hooks. This is the connecting peer, not the publisher address the media
// server reports in the body — that one is attacker-supplied and is only ever
// used to decide where a READ came from.
func internalOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isInternalIP(r.RemoteAddr) {
			log.Printf("[live-control] %s %s refused from %s: off-network caller",
				r.Method, r.URL.Path, r.RemoteAddr)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleAuth is the only place that decides who may publish.
//
// A publish is permitted when the path names a stream that exists, is not
// ended, is not blocked, and the presented password matches that stream's
// ingest key hash. Every other action is permitted only from the internal
// network, because the delivery surface is the edge, not the media server.
func (m *Manager) handleAuth(w http.ResponseWriter, r *http.Request) {
	var req authRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	streamID := strings.TrimPrefix(req.Path, "live/")

	switch req.Action {
	case "publish":
		// Credentials arrive either as JSON fields (SRT streamid, WHIP basic
		// auth) or in the RTMP query string, which is the only channel some
		// encoders offer. Both land on the same check.
		user, pass := req.User, req.Password
		if pass == "" && req.Query != "" {
			if q, err := url.ParseQuery(req.Query); err == nil {
				if user == "" {
					user = q.Get("user")
				}
				pass = q.Get("pass")
			}
		}
		if user != "" && user != streamID {
			log.Printf("[live-auth] publish denied for %s: user %q does not name the stream", streamID, user)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		// A browser broadcast never holds the ingest key. It redeems a
		// single-use ticket the WHIP proxy minted for its owner instead.
		if ConsumePublishTicket(streamID, pass) {
			// A browser broadcast costs the box exactly as much as a desktop
			// one — more, in fact, because VP8 from a browser can never have a
			// rung passed through. It passes the same door.
			if !m.admit(streamID, "", req.Protocol, req.IP) {
				http.Error(w, "at capacity", http.StatusServiceUnavailable)
				return
			}
			log.Printf("[live-auth] publish allowed by ticket: stream=%s proto=%s ip=%s",
				streamID, req.Protocol, req.IP)
			w.WriteHeader(http.StatusOK)
			return
		}

		stream, err := dbpkg.AuthorizeIngest(m.db, streamID, pass)
		if err != nil {
			if errors.Is(err, dbpkg.ErrLiveStreamNotFound) || errors.Is(err, dbpkg.ErrLiveStreamEnded) {
				log.Printf("[live-auth] publish denied for %s from %s: %v", streamID, req.IP, err)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			// A database fault is not an authorisation. Deny and say so.
			log.Printf("[live-auth] publish denied for %s: %v", streamID, err)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !m.admit(streamID, stream.AuthorHandle, req.Protocol, req.IP) {
			http.Error(w, "at capacity", http.StatusServiceUnavailable)
			return
		}
		log.Printf("[live-auth] publish allowed: stream=%s author=%s proto=%s ip=%s",
			stream.ID, stream.AuthorHandle, req.Protocol, req.IP)
		w.WriteHeader(http.StatusOK)

	case "read", "playback", "api", "metrics", "pprof":
		// The ladder pulls the source back out of the media server over the
		// internal network, and the control API is internal. Viewers never
		// reach the media server at all: they reach the live edge.
		if !isInternalIP(req.IP) {
			log.Printf("[live-auth] %s denied from %s", req.Action, req.IP)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)

	default:
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

// admit is the door. It is the last moment at which a broadcast can be turned
// away for free: past here the source is inside the media server and something
// has to encode it.
//
// A refusal is not a failure of this platform, it is this platform declining to
// break three working broadcasts to half-serve a fourth. The reason is written
// to the row and pushed to the broadcaster's surfaces, because RTMP gives an
// encoder no channel to carry one — OBS shows "failed to connect" and the
// broadcaster has no way to tell a full platform from a wrong key.
func (m *Manager) admit(streamID, authorHandle, protocol, ip string) bool {
	if m.capacity == nil {
		return true
	}
	result := m.capacity.Reserve(streamID)
	if result.Admitted {
		admissionsTotal.WithLabelValues("admitted").Inc()
		return true
	}
	admissionsTotal.WithLabelValues("refused").Inc()
	log.Printf("[live-auth] publish refused: stream=%s author=%s proto=%s ip=%s: %s",
		streamID, authorHandle, protocol, ip, result.Reason)

	note := "This broadcast could not start: " + result.Reason + ". " +
		"Nothing you did is wrong — try again in a few minutes."
	if err := dbpkg.RecordStreamCapacityRefusal(m.db, streamID, note); err != nil {
		log.Printf("[live-hook] recording capacity refusal for %s: %v", streamID, err)
	}
	m.publishLifecycle("live.capacity_refused", streamID, result.Reason)
	m.renderState(streamID)
	return false
}

// handleReady records what the ladder measured and issues its encode plan. It
// does not move the stream's status: see handlePlayable.
//
// It fires when the transcoder has probed the source and knows its real
// geometry, codec and keyframe cadence — the one instant at which every fact
// needed to size a ladder is known and no frame has yet been encoded. So the
// answer to this hook is the ladder itself: which rungs, on which encoder, at
// what segment duration, with what thread ceiling.
//
// The ladder used to decide all of that for itself, from a rung table copied by
// hand out of ladder.go, on a box where the resource it was spending is shared.
// Every broadcast independently concluded it could afford three 1080p encodes,
// and on the fourth they destroyed each other. Deciding it here is the only way
// the decision can account for the broadcasts already running, and it is what
// makes "existing streams win" enforceable rather than aspirational.
func (m *Manager) handleReady(w http.ResponseWriter, r *http.Request) {
	req, ok := m.decodeHook(w, r)
	if !ok {
		return
	}
	if err := dbpkg.RecordStreamSource(m.db, req.StreamID, req.Width, req.Height, req.Protocol); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, dbpkg.ErrLiveStreamNotFound) || errors.Is(err, dbpkg.ErrLiveStreamEnded) {
			status = http.StatusConflict
		}
		log.Printf("[live-hook] ready %s: %v", req.StreamID, err)
		http.Error(w, err.Error(), status)
		return
	}

	plan, err := m.capacity.Plan(req.StreamID, SourceReport{
		Width:          req.Width,
		Height:         req.Height,
		FPS:            req.FPS,
		Codec:          req.Codec,
		GOPSeconds:     req.GOPSeconds,
		FPSDeclared:    req.FPSDeclared,
		NVENCAvailable: req.NVENCAvailable,
	})
	if err != nil {
		// No plan means no ladder. Ending the stream here is the honest answer:
		// the alternative is a row that says live over a source nothing is
		// packaging, which is the exact failure this lane is being cured of.
		log.Printf("[live-hook] no plan for %s: %v", req.StreamID, err)
		m.capacity.Release(req.StreamID)
		if endErr := dbpkg.EndStream(m.db, req.StreamID); endErr != nil {
			log.Printf("[live-hook] ending unplannable stream %s: %v", req.StreamID, endErr)
		}
		m.renderState(req.StreamID)
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}

	note := ""
	if plan.Degraded {
		admissionsTotal.WithLabelValues("degraded").Inc()
		note = "You are on air, but the platform is busy: " + plan.Reason + "."
	}
	if err := dbpkg.RecordStreamLadder(m.db, req.StreamID, plan.Summary(), plan.Encoder, plan.Degraded, note); err != nil {
		log.Printf("[live-hook] recording ladder for %s: %v", req.StreamID, err)
	}

	log.Printf("[live-hook] stream %s planned: %dx%d %s %s, plan %s%s",
		req.StreamID, req.Width, req.Height, req.Codec, req.Protocol,
		plan.Summary(), map[bool]string{true: " (DEGRADED)", false: ""}[plan.Degraded])
	m.publishLifecycle("live.planned", req.StreamID, plan.Summary())
	// The surfaces are not rendered here. The row is still idle and nothing a
	// viewer can see has changed; the ladder that was just recorded reaches
	// them with the player, at the moment there is a playlist for it to name.

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(plan); err != nil {
		log.Printf("[live-hook] writing plan for %s: %v", req.StreamID, err)
	}
}

// handlePlayable moves a stream idle -> live. It is the one place that does.
//
// The ladder calls it after ffmpeg has written the master playlist and the
// first segment of at least one rung — after it has confirmed, by reading the
// files back, that the playlist URL the player Facet carries now answers with
// video. That is the moment the row is allowed to say live, because it is the
// first moment at which saying so is true for a viewer.
//
// A ladder restarted by the media server mid-broadcast calls this again once
// it is writing again; StartStream is idempotent under an already-live row, so
// the second call refreshes the geometry and changes nothing else.
func (m *Manager) handlePlayable(w http.ResponseWriter, r *http.Request) {
	req, ok := m.decodeHook(w, r)
	if !ok {
		return
	}
	if err := dbpkg.StartStream(m.db, req.StreamID, req.Width, req.Height, req.Protocol); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, dbpkg.ErrLiveStreamNotFound) || errors.Is(err, dbpkg.ErrLiveStreamEnded) {
			status = http.StatusConflict
		}
		log.Printf("[live-hook] playable %s: %v", req.StreamID, err)
		http.Error(w, err.Error(), status)
		return
	}
	log.Printf("[live-hook] stream %s live: %dx%d %s, first segment on disk",
		req.StreamID, req.Width, req.Height, req.Protocol)
	m.publishLifecycle("live.started", req.StreamID, m.capacity.PlanSummary(req.StreamID))
	m.renderState(req.StreamID)
	w.WriteHeader(http.StatusOK)
}

// handlePressure answers a ladder that has reported it cannot hold realtime.
//
// This is the early warning that was being thrown away. The media server logs
// "reader is too slow, discarding N frames" a few seconds before an encoder
// dies, and by then the broadcast is already over; the encoder itself knows
// first, and says so once a second, in progress output nothing was reading. The
// ladder reads it now and reports here.
//
// The decision is taken here rather than in the ladder because only this process
// knows whether there is a smaller ladder to fall back to, and because a ladder
// that reduced itself would be taking capacity decisions one broadcast at a
// time — which is how the box came to be oversubscribed in the first place.
func (m *Manager) handlePressure(w http.ResponseWriter, r *http.Request) {
	req, ok := m.decodeHook(w, r)
	if !ok {
		return
	}
	pressureEventsTotal.Inc()
	verdict := m.capacity.ReportPressure(req.StreamID, req.Speed)
	log.Printf("[live-hook] pressure on %s at %.2fx realtime: %s (restart=%v)",
		req.StreamID, req.Speed, verdict.Reason, verdict.Restart)

	note := "The platform is under load: " + verdict.Reason + "."
	if err := dbpkg.RecordStreamLadder(m.db, req.StreamID,
		m.capacity.PlanSummary(req.StreamID), m.capacity.EncoderFor(req.StreamID), true, note); err != nil {
		log.Printf("[live-hook] recording pressure for %s: %v", req.StreamID, err)
	}
	m.publishLifecycle("live.degraded", req.StreamID, verdict.Reason)
	m.renderState(req.StreamID)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(verdict); err != nil {
		log.Printf("[live-hook] writing pressure verdict for %s: %v", req.StreamID, err)
	}
}

// handleNotReady moves a stream to ended. Ended is terminal.
func (m *Manager) handleNotReady(w http.ResponseWriter, r *http.Request) {
	req, ok := m.decodeHook(w, r)
	if !ok {
		return
	}
	if err := dbpkg.EndStream(m.db, req.StreamID); err != nil {
		if errors.Is(err, dbpkg.ErrLiveStreamNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		log.Printf("[live-hook] notready %s: %v", req.StreamID, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	m.capacity.Release(req.StreamID)
	log.Printf("[live-hook] stream %s ended", req.StreamID)
	m.publishLifecycle("live.ended", req.StreamID, "publisher stopped")
	m.renderState(req.StreamID)
	w.WriteHeader(http.StatusOK)
}

func (m *Manager) decodeHook(w http.ResponseWriter, r *http.Request) (hookRequest, bool) {
	var req hookRequest
	// Constant-time so a peer on the container network cannot recover the
	// shared secret byte by byte from response timing. Start() already refuses
	// to bring the control plane up with an empty key.
	if m.internalKey == "" ||
		subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Internal-Key")), []byte(m.internalKey)) != 1 {
		// A mismatch here means the media server's container is running with a
		// stale key (e.g. recreated before an INTERNAL_API_KEY rotation reached
		// it) — the ladder sees a bare curl failure with no other clue, so the
		// only place this is diagnosable at all is this log line.
		log.Printf("[live-hook] rejected %s %s from %s: internal key mismatch", r.Method, r.URL.Path, r.RemoteAddr)
		http.Error(w, "forbidden", http.StatusForbidden)
		return req, false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return req, false
	}
	if req.StreamID == "" {
		http.Error(w, "stream_id required", http.StatusBadRequest)
		return req, false
	}
	return req, true
}

// isInternalIP reports whether an address belongs to the container network or
// the loopback interface. The media server's own callers are always one of the
// two; anything else reached it from outside and gets nothing.
func isInternalIP(raw string) bool {
	host := raw
	if h, _, err := net.SplitHostPort(raw); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}
