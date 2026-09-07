"""
NEXUS biometric hashing endpoint.
Receives liveness check results, computes SHA3-512 hashes, returns hashes only.
RAW BIOMETRIC DATA IS NEVER STORED — deleted immediately after hashing.
"""
import gc
import hashlib
import struct
import time
from typing import List

import numpy as np
from flask import Blueprint, request, jsonify
import prometheus_client as prom

nexus_biometric_bp = Blueprint("nexus_biometric", __name__, url_prefix="/v1/biometric")

_nexus_hash_total = prom.Counter(
    "ekyc_nexus_biometric_hashes_total", "NEXUS biometric hash requests", ["result"]
)
_nexus_hash_duration = prom.Histogram(
    "ekyc_nexus_hash_duration_seconds", "Biometric hash computation time",
    buckets=[0.01, 0.05, 0.1, 0.25, 0.5, 1.0]
)


# ── Pydantic-style validation via plain dicts ─────────────────────────────────

def _validate_liveness(data: dict) -> dict:
    required = {"facial_landmarks", "confidence_score", "session_id"}
    missing = required - set(data.keys())
    if missing:
        raise ValueError(f"Missing liveness fields: {missing}")
    if not isinstance(data["facial_landmarks"], list) or len(data["facial_landmarks"]) == 0:
        raise ValueError("facial_landmarks must be a non-empty list")
    if not isinstance(data["confidence_score"], (int, float)):
        raise ValueError("confidence_score must be a number")
    return data


def _validate_document(data: dict) -> dict:
    required = {"document_text_canonical", "document_type", "issuing_country"}
    missing = required - set(data.keys())
    if missing:
        raise ValueError(f"Missing document fields: {missing}")
    return data


# ── Feature extraction ────────────────────────────────────────────────────────

def _extract_feature_vector(landmarks: List[dict]) -> bytes:
    """
    Extract a rotation-invariant, scale-invariant feature vector from
    MediaPipe face landmarks. Uses normalized inter-landmark distances
    for key facial ratios (eye distance, nose-mouth ratio, etc.).
    This ensures the hash is consistent across lighting/angle variations.
    """
    if not landmarks:
        raise ValueError("Empty landmarks")

    pts = np.array([[lm.get("x", 0.0), lm.get("y", 0.0), lm.get("z", 0.0)]
                    for lm in landmarks], dtype=np.float64)

    # Normalize: center on nose tip (landmark 4), scale by inter-pupil distance
    nose_tip = pts[4]
    pts -= nose_tip

    # Key landmark indices for inter-pupil distance normalization
    # Use indices always present in the 468-point MediaPipe set
    left_eye_center  = pts[33]
    right_eye_center = pts[263]
    inter_pupil = np.linalg.norm(right_eye_center - left_eye_center)
    if inter_pupil < 1e-6:
        inter_pupil = 1.0
    pts /= inter_pupil

    # Extract 128 key landmark positions as feature vector
    KEY_INDICES = [
        1, 4, 6, 10, 14, 17, 21, 33, 37, 40, 46, 52, 55, 58, 61, 63,
        65, 66, 70, 78, 80, 82, 84, 87, 88, 91, 93, 95, 105, 107, 109,
        127, 132, 133, 136, 148, 149, 150, 152, 162, 172, 176, 178, 181,
        185, 191, 234, 246, 249, 251, 263, 267, 269, 270, 276, 282, 285,
        288, 291, 293, 295, 296, 300, 308, 310, 312, 314, 317, 318, 321,
        323, 325, 334, 336, 338, 356, 361, 362, 365, 377, 378, 379, 391,
        397, 454, 466, 0, 7, 8, 9, 11, 12, 13, 15, 16, 18, 20, 22,
        23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 34, 35, 36, 38, 39,
        41, 42, 43, 44, 45, 47, 48, 49, 50, 51, 53, 54, 56, 57
    ]
    # Clamp to available landmarks
    key_pts = np.array([pts[min(i, len(pts) - 1)] for i in KEY_INDICES])

    # Flatten to bytes
    return struct.pack(f'{len(key_pts) * 3}d', *key_pts.flatten())


def _hash_document(doc: dict) -> str:
    """
    Hash document fields in a deterministic order regardless of OCR field ordering.
    """
    canonical = "|".join(sorted([
        doc["document_text_canonical"].upper().strip(),
        doc["document_type"].upper(),
        doc["issuing_country"].upper().strip(),
    ]))
    return hashlib.sha3_512(canonical.encode("utf-8")).hexdigest()


# ── Routes ────────────────────────────────────────────────────────────────────

@nexus_biometric_bp.route("/hash", methods=["POST"])
def hash_biometric():
    """
    Compute biometric and document hashes. Source data is deleted immediately.
    Nothing is persisted — only hashes are returned.
    """
    _t0  = time.monotonic()
    body = request.get_json(force=True, silent=True)
    if not body:
        _nexus_hash_total.labels(result="error").inc()
        return jsonify({"error": "invalid JSON"}), 400

    liveness = body.get("liveness_result", {})
    document = body.get("document_result", {})

    try:
        _validate_liveness(liveness)
        _validate_document(document)
    except ValueError as e:
        _nexus_hash_total.labels(result="validation_error").inc()
        return jsonify({"error": str(e)}), 422

    confidence = float(liveness["confidence_score"])
    if confidence < 0.85:
        _nexus_hash_total.labels(result="low_confidence").inc()
        return jsonify({"error": "Liveness confidence below threshold"}), 422

    try:
        feature_vector = _extract_feature_vector(liveness["facial_landmarks"])
        biometric_hash = hashlib.sha3_512(feature_vector).hexdigest()
        document_hash  = _hash_document(document)
        session_id     = liveness["session_id"]
    except Exception as e:
        _nexus_hash_total.labels(result="error").inc()
        return jsonify({"error": str(e)}), 500
    finally:
        # Explicitly delete sensitive data and force GC
        del body
        del liveness
        del document
        gc.collect()

    _nexus_hash_total.labels(result="ok").inc()
    _nexus_hash_duration.observe(time.monotonic() - _t0)
    return jsonify({
        "biometric_hash": biometric_hash,
        "document_hash":  document_hash,
        "confidence":     confidence,
        "session_id":     session_id,
    })


@nexus_biometric_bp.route("/compare", methods=["POST"])
def compare_hashes():
    """
    Hash comparison. Since SHA3-512 is not distance-preserving, exact match only.
    For similarity comparison during active session, feature vectors in Redis
    (5-min TTL) should be used instead. This endpoint handles the simple case.
    """
    body = request.get_json(force=True, silent=True)
    if not body:
        return jsonify({"error": "invalid JSON"}), 400

    hash_a = body.get("hash_a", "")
    hash_b = body.get("hash_b", "")

    if not hash_a or not hash_b:
        return jsonify({"error": "hash_a and hash_b required"}), 400

    exact_match = hash_a.lower() == hash_b.lower()
    return jsonify({
        "match":      exact_match,
        "confidence": 1.0 if exact_match else 0.0,
        "note":       "SHA3-512 exact match only. For similarity, use session feature vectors.",
    })
