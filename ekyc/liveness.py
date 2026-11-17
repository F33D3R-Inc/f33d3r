"""
liveness.py — server-issued active-liveness challenges and server-side
verification of the frames captured against them.

A session is issued by POST /v1/liveness/challenge with a random ordered
subset of head-pose / expression challenges. /v1/verify consumes the session
atomically (single use, whether the attempt passes or fails); an expired or
unknown session fails.

Frame protocol: exactly one frame per challenge, in challenge order, captured
while the person holds the action, from the unmirrored camera image (what the
camera sees, not the mirrored preview). The selfie (`face_image`) is the
neutral, frontal baseline every frame is measured against.

Every challenge is verifiable from a single still frame using the detector's
five landmarks; a blink is not (it lasts ~150 ms and is not resolvable from
five points), so blink is not offered.
"""
import secrets
import threading
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone

from faces import DetectedFace, cosine, pose

CHALLENGE_TTL_SECONDS = 300      # covers document capture → selfie → challenges → consent step
MAX_OPEN_SESSIONS = 10_000       # bounds memory; issuing beyond this fails closed
CHALLENGES = ("turn_left", "turn_right", "look_up", "smile")
CHALLENGES_PER_SESSION = 3       # 4P3 = 24 possible ordered sequences

# Thresholds, measured relative to the selfie baseline (see faces.Pose).
# turn_left/turn_right: the nose must move ≥ 0.18 inter-ocular distances
#   along the eye line — about a 20° head turn. The person's LEFT is the
#   camera image's RIGHT (+yaw) in an unmirrored frame.
YAW_DELTA_MIN = 0.18
# look_up: tilting the head back moves the nose tip toward the eye line; the
#   nose's share of the eye→mouth distance must fall by ≥ 0.08 (≈ 10-12°).
PITCH_DELTA_MIN = 0.08
# smile: the mouth corners must spread by ≥ 10 % relative to the neutral selfie.
SMILE_RATIO_MIN = 1.10
# The selfie must be frontal to serve as the baseline.
SELFIE_YAW_MAX = 0.15
# Frame ↔ selfie identity: same person, same session, same camera, minutes
# apart. With SFace, the same face re-detected at a different scale scores
# ≈ 0.90 and different people stay near or below the 0.363 published cut;
# 0.50 leaves margin for a ~20° pose change while rejecting a different person.
FRAME_IDENTITY_MIN = 0.50


@dataclass
class _Session:
    challenges: list[str]
    expires_at: float
    issued_at: float = field(default_factory=time.time)


_lock = threading.Lock()
_sessions: dict[str, _Session] = {}


def issue() -> dict:
    now = time.time()
    with _lock:
        for sid in [k for k, s in _sessions.items() if s.expires_at < now]:
            del _sessions[sid]
        if len(_sessions) >= MAX_OPEN_SESSIONS:
            raise OverflowError("liveness session store is full")
        sid = secrets.token_urlsafe(32)
        rng = secrets.SystemRandom()
        challenges = rng.sample(CHALLENGES, CHALLENGES_PER_SESSION)
        expires_at = now + CHALLENGE_TTL_SECONDS
        _sessions[sid] = _Session(challenges=challenges, expires_at=expires_at)
    return {
        "session_id": sid,
        "challenges": challenges,
        "expires_at": datetime.fromtimestamp(expires_at, tz=timezone.utc)
                              .isoformat(timespec="seconds").replace("+00:00", "Z"),
    }


def consume(session_id: str) -> tuple[list[str] | None, str]:
    """Remove the session and return its challenges, or (None, reason)."""
    with _lock:
        s = _sessions.pop(session_id, None) if session_id else None
    if s is None:
        return None, "liveness_session_unknown"
    if time.time() > s.expires_at:
        return None, "liveness_session_expired"
    return s.challenges, ""


def _performed(challenge: str, base, frame) -> bool:
    if challenge == "turn_left":
        return frame.yaw - base.yaw >= YAW_DELTA_MIN
    if challenge == "turn_right":
        return base.yaw - frame.yaw >= YAW_DELTA_MIN
    if challenge == "look_up":
        return base.pitch - frame.pitch >= PITCH_DELTA_MIN
    if challenge == "smile":
        return (frame.mouth / max(base.mouth, 1e-6) >= SMILE_RATIO_MIN
                and abs(frame.yaw - base.yaw) < YAW_DELTA_MIN)
    return False


def selfie_is_frontal(selfie: DetectedFace) -> bool:
    return abs(pose(selfie).yaw) <= SELFIE_YAW_MAX


def validate_liveness_frames(challenges: list[str], selfie: DetectedFace,
                  frames: list[DetectedFace | None]) -> tuple[bool, float, list[str]]:
    """
    Check each frame against its challenge. Returns (passed, score, reasons),
    score being the fraction of challenges performed by the selfie's person.
    """
    reasons: list[str] = []
    if len(frames) != len(challenges):
        return False, 0.0, ["liveness_frame_count_mismatch"]
    base = pose(selfie)
    done = 0
    for challenge, face in zip(challenges, frames):
        if face is None:
            reasons.append("liveness_no_face_in_frame")
            continue
        if cosine(selfie, face) < FRAME_IDENTITY_MIN:
            reasons.append("liveness_identity_mismatch")
            continue
        if not _performed(challenge, base, pose(face)):
            reasons.append(f"liveness_challenge_not_performed:{challenge}")
            continue
        done += 1
    score = round(done / len(challenges), 4)
    return done == len(challenges), score, sorted(set(reasons))
