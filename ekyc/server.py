"""
F33D3R eKYC — port 8099.

Measures ONE identity-verification attempt and decides pass/fail. Stateless
apart from active-liveness challenge sessions. Fails closed: a missing model
stops the service at start-up, and any unreadable input, failed check or
exception yields decision "fail".

Routes
  POST /v1/liveness/challenge   issue a single-use, expiring challenge session
  POST /v1/verify               the one verification path (purpose age|creator)
  GET  /health                  liveness of the service (models are loaded at start)
  GET  /metrics                 Prometheus

Images, names, dates of birth and document numbers are never logged. The
document number never leaves this process; only doc_hmac does.
"""
import base64
import binascii
import hashlib
import hmac
import io
import logging
import os
import threading
import time
import uuid
from datetime import date, datetime, timezone
from typing import Optional

import numpy as np
import prometheus_client as prom
import pytesseract
from flask import Flask, Response, jsonify, request
from PIL import Image, ImageOps, UnidentifiedImageError
from werkzeug.exceptions import HTTPException

import faces
import liveness
from aamva import BarcodeResult, read_barcode
from frontocr import cross_check
from mrz import MRZResult, read_mrz

logging.basicConfig(level=logging.INFO, format="%(asctime)s [ekyc] %(levelname)s %(name)s %(message)s")
log = logging.getLogger("ekyc")

# ── Decision thresholds ───────────────────────────────────────────────────────
# Document portrait ↔ selfie, cosine of L2-normalised SFace embeddings.
# OpenCV's published "same person" cut for SFace is 0.363 (its LFW operating
# point). KYC raises it to 0.42 because a false accept binds a stranger's
# identity to an account, whereas a false reject only costs a retake. On the
# six different people of InsightFace's t1.jpg sample the highest cross-person
# cosine was 0.31, and the same face re-detected at 0.6× scale with blur
# scored ≈ 0.90. Genuine ID-photo ↔ live-selfie pairs (older photo, laminate,
# different camera) score lower than that; this value is reasoned, not fitted,
# and is to be tuned on real captures.
FACE_MATCH_THRESHOLD = 0.42
# PAD: mean "live" probability of the MiniFASNet pair. The models are 3-class
# (2D attack / live / 3D attack); requiring ≥ 0.5 makes "live" the majority
# outcome, stricter than the upstream argmax rule. Applied to the selfie AND
# every challenge frame; the reported score is the minimum.
PAD_LIVE_MIN = 0.50
# Face sizes below which the crop carries too little detail to compare.
SELFIE_FACE_MIN_PX = 80
DOC_FACE_MIN_PX = 40
ADULT_AGE = 18

MAX_FRAMES = 8
MAX_PIXELS = 25_000_000           # explicit per-image bound, checked before decoding pixels
Image.MAX_IMAGE_PIXELS = 2 * MAX_PIXELS
DOC_MAX_SIDE = 3000
FACE_MAX_SIDE = 1600

PURPOSES = ("age", "creator")

app = Flask(__name__)
app.config["MAX_CONTENT_LENGTH"] = 48 * 1024 * 1024

# Bounded concurrency: each attempt runs OCR and several ONNX passes.
_verify_slots = threading.BoundedSemaphore(int(os.environ.get("EKYC_MAX_CONCURRENT", "2")))

_DOC_HMAC_KEY: bytes = b""

# ── Metrics (no PII in any label) ─────────────────────────────────────────────
_m_decisions = prom.Counter("ekyc_verify_decisions_total", "Verification decisions", ["decision", "purpose"])
_m_reasons = prom.Counter("ekyc_verify_fail_reasons_total", "Failure reasons", ["reason"])
_m_duration = prom.Histogram("ekyc_verify_duration_seconds", "Verification time",
                             buckets=[0.5, 1, 2, 5, 10, 20, 40, 80])
_m_challenges = prom.Counter("ekyc_liveness_challenges_issued_total", "Liveness sessions issued")
_m_face_cos = prom.Histogram("ekyc_face_match_cosine", "Document↔selfie cosine",
                             buckets=[0.0, 0.1, 0.2, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 1.0])
_m_pad = prom.Histogram("ekyc_pad_live_probability", "Minimum PAD live probability per attempt",
                        buckets=[0.0, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0])
_m_doc_source = prom.Counter("ekyc_document_source_total", "Machine-readable source per attempt",
                             ["source", "status"])


# ── Helpers ───────────────────────────────────────────────────────────────────
def doc_hmac(doc_type: str, country: str, number: str) -> str:
    msg = f"{doc_type}|{country}|{number.upper()}".encode()
    return hmac.new(_DOC_HMAC_KEY, msg, hashlib.sha256).hexdigest()


def calc_age(dob: date, today: date) -> int:
    return today.year - dob.year - ((today.month, today.day) < (dob.month, dob.day))


def decode_image(b64: str, max_side: int) -> Optional[Image.Image]:
    """Decode a base64 (or data-URL) image. None when it is not a valid image."""
    if b64.startswith("data:"):
        b64 = b64.partition(",")[2]
    try:
        raw = base64.b64decode(b64, validate=True)
    except (binascii.Error, ValueError):
        return None
    try:
        img = Image.open(io.BytesIO(raw))
        if img.width * img.height > MAX_PIXELS:
            return None
        img.load()
        img = ImageOps.exif_transpose(img).convert("RGB")
    except (UnidentifiedImageError, OSError, ValueError, Image.DecompressionBombError):
        return None
    if max(img.size) > max_side:
        img.thumbnail((max_side, max_side), Image.Resampling.LANCZOS)
    return img


def to_bgr(img: Image.Image) -> np.ndarray:
    return np.ascontiguousarray(np.asarray(img)[:, :, ::-1])


def _empty_response() -> dict:
    return {
        "decision": "fail",
        "reasons": [],
        "document": {
            "type": "", "issuing_country": "", "issuing_region": "", "doc_hmac": "",
            "given_names": "", "surname": "", "dob": "", "expiry": "",
            "checks": {"barcode": "absent", "mrz_checksums": "absent",
                       "fields_consistent": False, "expired": True, "tamper": "ok"},
        },
        "age": None,
        "face": {"match": False, "cosine": 0.0, "model": faces.FACE_MODEL_NAME},
        "pad": {"live": False, "score": 0.0, "model": faces.PAD_MODEL_NAME},
        "liveness": {"passed": False, "score": 0.0},
        "evidence_types": [],
    }


def _fail(status: int, reasons: list[str]):
    out = _empty_response()
    out["reasons"] = reasons
    for r in reasons:
        _m_reasons.labels(reason=r).inc()
    return jsonify(out), status


# ── Routes ────────────────────────────────────────────────────────────────────
@app.route("/v1/liveness/challenge", methods=["POST"])
def liveness_challenge():
    try:
        session = liveness.issue()
    except OverflowError:
        log.error("liveness challenge refused: session store full")
        return jsonify({"error": "liveness_unavailable"}), 503
    _m_challenges.inc()
    return jsonify(session)


@app.route("/v1/verify", methods=["POST"])
def verify():
    rid = uuid.uuid4().hex[:12]
    body = request.get_json(silent=True)
    if not isinstance(body, dict):
        return _fail(400, ["invalid_request"])

    purpose = body.get("purpose")
    front_b64 = body.get("document_front")
    back_b64 = body.get("document_back", "")
    face_b64 = body.get("face_image")
    session_id = body.get("liveness_session_id")
    frames_b64 = body.get("liveness_challenge_frames")
    if (purpose not in PURPOSES
            or not isinstance(front_b64, str) or not front_b64
            or not isinstance(back_b64, str)
            or not isinstance(face_b64, str) or not face_b64
            or not isinstance(session_id, str) or not session_id
            or not isinstance(frames_b64, list) or not frames_b64
            or len(frames_b64) > MAX_FRAMES
            or not all(isinstance(f, str) and f for f in frames_b64)):
        log.info("verify rid=%s rejected: invalid_request", rid)
        return _fail(400, ["invalid_request"])

    # The session is consumed before anything else so it can never be replayed,
    # whatever the outcome of this attempt.
    challenges, session_reason = liveness.consume(session_id)

    if not _verify_slots.acquire(timeout=60):
        log.error("verify rid=%s refused: all verification slots busy", rid)
        return _fail(503, ["busy"] + ([session_reason] if session_reason else []))
    t0 = time.monotonic()
    try:
        out = _run_verification(rid, purpose, front_b64, back_b64, face_b64,
                                challenges, session_reason, frames_b64)
    finally:
        _verify_slots.release()
        _m_duration.observe(time.monotonic() - t0)

    _m_decisions.labels(decision=out["decision"], purpose=purpose).inc()
    for r in out["reasons"]:
        _m_reasons.labels(reason=r).inc()
    log.info("verify rid=%s purpose=%s decision=%s doc_type=%s country=%s reasons=%s elapsed=%.2fs",
             rid, purpose, out["decision"], out["document"]["type"] or "-",
             out["document"]["issuing_country"] or "-", ",".join(out["reasons"]) or "-",
             time.monotonic() - t0)
    return jsonify(out)


def _run_verification(rid, purpose, front_b64, back_b64, face_b64,
                      challenges, session_reason, frames_b64) -> dict:
    out = _empty_response()
    reasons: list[str] = []
    today = datetime.now(timezone.utc).date()

    front = decode_image(front_b64, DOC_MAX_SIDE)
    back = decode_image(back_b64, DOC_MAX_SIDE) if back_b64 else None
    selfie = decode_image(face_b64, FACE_MAX_SIDE)
    frames = [decode_image(f, FACE_MAX_SIDE) for f in frames_b64]
    if front is None:
        reasons.append("document_front_unreadable_image")
    if back_b64 and back is None:
        reasons.append("document_back_unreadable_image")
    if selfie is None:
        reasons.append("face_image_unreadable")
    if any(f is None for f in frames):
        reasons.append("liveness_frame_unreadable")
    if len({hashlib.sha256(s.encode()).digest() for s in frames_b64 + [face_b64]}) != len(frames_b64) + 1:
        reasons.append("liveness_frames_duplicated")

    # ── Document: machine-readable source ────────────────────────────────────
    barcode = read_barcode(back) if back is not None else BarcodeResult("absent")
    mrz = MRZResult("absent")
    if barcode.status != "ok" and front is not None:
        candidates = [read_mrz(front)]
        if candidates[0].status != "ok" and back is not None:
            candidates.append(read_mrz(back))
        mrz = next((c for c in candidates if c.status == "ok"),
                   next((c for c in candidates if c.status == "invalid"), candidates[0]))
    _m_doc_source.labels(source="pdf417", status=barcode.status).inc()
    _m_doc_source.labels(source="mrz", status=mrz.status).inc()

    doc = out["document"]
    checks = doc["checks"]
    checks["barcode"] = barcode.status
    checks["mrz_checksums"] = mrz.status
    if barcode.status == "invalid":
        reasons.append("barcode_invalid")
        log.info("verify rid=%s barcode invalid detail=%s", rid, barcode.detail)
    if mrz.status == "invalid":
        reasons.append("mrz_checksum_failed")
        log.info("verify rid=%s mrz invalid detail=%s", rid, mrz.detail)

    source = barcode.data if barcode.status == "ok" else (mrz.data if mrz.status == "ok" else None)
    evidence: list[str] = []
    dob: Optional[date] = None
    if source is None:
        reasons.append("no_machine_readable_document")
    else:
        doc["type"] = source.doc_type
        doc["issuing_country"] = source.issuing_country
        doc["issuing_region"] = getattr(source, "issuing_region", "")
        doc["doc_hmac"] = doc_hmac(source.doc_type, source.issuing_country, source.number)
        doc["given_names"] = source.given_names
        doc["surname"] = source.surname
        doc["dob"] = source.dob.isoformat()
        doc["expiry"] = source.expiry.isoformat() if source.expiry else ""
        evidence.append(f"{source.doc_type}_{'pdf417' if barcode.status == 'ok' else 'mrz'}")

        if source.expiry is None:
            reasons.append("expiry_unreadable")
        else:
            checks["expired"] = source.expiry < today
            if checks["expired"]:
                reasons.append("document_expired")

        dob = source.dob
        out["age"] = calc_age(dob, today)
        if out["age"] < ADULT_AGE:
            reasons.append("underage")

        if front is not None:
            consistency = cross_check(front, dob=source.dob, expiry=source.expiry,
                                      surname=source.surname, given_names=source.given_names)
            checks["fields_consistent"] = consistency == "matched"
            if consistency == "contradicted":
                reasons.append("front_back_mismatch")
            elif consistency == "unreadable":
                reasons.append("front_unreadable")

    # ── Faces ────────────────────────────────────────────────────────────────
    selfie_bgr = to_bgr(selfie) if selfie is not None else None
    selfie_face = faces.largest_face(selfie_bgr) if selfie_bgr is not None else None
    doc_face = faces.largest_face(to_bgr(front)) if front is not None else None
    if selfie is not None and selfie_face is None:
        reasons.append("no_face_in_selfie")
    elif selfie_face is not None and selfie_face.width < SELFIE_FACE_MIN_PX:
        reasons.append("selfie_face_too_small")
    if front is not None and doc_face is None:
        reasons.append("no_face_in_document")
    elif doc_face is not None and doc_face.width < DOC_FACE_MIN_PX:
        reasons.append("document_portrait_too_small")

    if selfie_face is not None and doc_face is not None:
        cos = faces.cosine(doc_face, selfie_face)
        _m_face_cos.observe(cos)
        out["face"]["cosine"] = round(cos, 4)
        out["face"]["match"] = (cos >= FACE_MATCH_THRESHOLD
                                and selfie_face.width >= SELFIE_FACE_MIN_PX
                                and doc_face.width >= DOC_FACE_MIN_PX)
        if cos < FACE_MATCH_THRESHOLD:
            reasons.append("face_mismatch")
        if out["face"]["match"]:
            evidence.append("selfie_face_match")

    # ── Liveness frames: detection shared by active liveness and PAD ─────────
    frame_bgrs = [to_bgr(f) if f is not None else None for f in frames]
    frame_faces = [faces.largest_face(b) if b is not None else None for b in frame_bgrs]

    # ── PAD: selfie and every challenge frame must be live ───────────────────
    if selfie_face is not None and all(f is not None for f in frame_faces):
        probs = [faces.pad_live_probability(selfie_bgr, selfie_face)]
        probs += [faces.pad_live_probability(b, f) for b, f in zip(frame_bgrs, frame_faces)]
        pad_score = min(probs)
        _m_pad.observe(pad_score)
        out["pad"]["score"] = round(pad_score, 4)
        out["pad"]["live"] = pad_score >= PAD_LIVE_MIN
        if out["pad"]["live"]:
            evidence.append("pad")
        else:
            reasons.append("presentation_attack_suspected")
    else:
        reasons.append("pad_not_evaluated")

    # ── Active liveness ──────────────────────────────────────────────────────
    if challenges is None:
        reasons.append(session_reason)
    elif selfie_face is None:
        reasons.append("liveness_not_evaluated")
    else:
        if not liveness.selfie_is_frontal(selfie_face):
            reasons.append("selfie_not_frontal")
        else:
            passed, score, live_reasons = liveness.validate_liveness_frames(
                challenges, selfie_face, frame_faces)
            out["liveness"] = {"passed": passed, "score": score}
            reasons.extend(live_reasons)
            if passed:
                evidence.append("active_liveness")

    # ── Decision: every requirement, explicitly ──────────────────────────────
    passed = (
        not reasons
        and source is not None
        and checks["fields_consistent"]
        and checks["expired"] is False
        and dob is not None and out["age"] is not None and out["age"] >= ADULT_AGE
        and out["face"]["match"]
        and out["pad"]["live"]
        and out["liveness"]["passed"]
    )
    out["decision"] = "pass" if passed else "fail"
    out["reasons"] = sorted(set(reasons))
    out["evidence_types"] = evidence
    if not passed:
        # Personal data leaves this service only with a pass.
        doc["given_names"] = doc["surname"] = doc["dob"] = ""
        out["age"] = None
    return out


@app.route("/health", methods=["GET"])
def health():
    return jsonify({"status": "ok", "service": "ekyc",
                    "models": [faces.FACE_MODEL_NAME, faces.PAD_MODEL_NAME]})


@app.route("/metrics", methods=["GET"])
def metrics():
    return Response(prom.generate_latest(prom.REGISTRY), mimetype=prom.CONTENT_TYPE_LATEST)


@app.errorhandler(HTTPException)
def _http_error(e: HTTPException):
    if request.path == "/v1/verify":
        return _fail(e.code or 400, ["request_too_large" if e.code == 413 else "invalid_request"])
    return jsonify({"error": e.name}), e.code


@app.errorhandler(Exception)
def _internal_error(e: Exception):
    # Logged with its stack; no request content is ever part of the message.
    log.exception("unhandled error on %s %s", request.method, request.path)
    if request.path == "/v1/verify":
        return _fail(500, ["internal_error"])
    return jsonify({"error": "internal_error"}), 500


def startup() -> None:
    """
    Load the key and every model. Raises on any failure; wsgi.py runs it in the
    gunicorn worker, and an exception there stops the server instead of serving
    without a model.
    """
    global _DOC_HMAC_KEY
    key = os.environ.get("EKYC_DOC_HMAC_KEY", "")
    if len(key) < 32:
        raise RuntimeError("EKYC_DOC_HMAC_KEY is missing or shorter than 32 characters")
    _DOC_HMAC_KEY = key.encode()
    log.info("tesseract %s", pytesseract.get_tesseract_version())
    import zxingcpp  # noqa: F401 — fails start-up if the PDF417 decoder is absent
    faces.load_models()
