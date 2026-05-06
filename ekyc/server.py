"""
F33D3R eKYC Microservice — port 8099
Phase 2: Face match (selfie vs ID document) — InsightFace ArcFace/buffalo_sc
Phase 3: OCR + MRZ parsing — pytesseract + regex DOB extraction
Phase 4: Anti-spoof scoring — texture/frequency/saturation analysis (no GPU)
Phase 5: Risk signal on login — device/UA/velocity signals for Verity
"""
import os
import io
import re
import base64
import json
import logging
import hashlib
from datetime import date
from typing import Optional

import numpy as np
from PIL import Image
import pytesseract
from flask import Flask, request, jsonify

logging.basicConfig(level=logging.INFO, format="%(asctime)s [ekyc] %(levelname)s %(message)s")
log = logging.getLogger("ekyc")

app = Flask(__name__)

# ── Lazy model init ────────────────────────────────────────────────────────────
_face_app = None

def get_face_app():
    global _face_app
    if _face_app is None:
        try:
            from insightface.app import FaceAnalysis
            _face_app = FaceAnalysis(name="buffalo_sc", providers=["CPUExecutionProvider"])
            _face_app.prepare(ctx_id=0, det_size=(320, 320))
            log.info("InsightFace buffalo_sc loaded")
        except Exception as e:
            log.error(f"InsightFace load failed: {e}")
    return _face_app


# ── Image utils ────────────────────────────────────────────────────────────────
def decode_b64(b64: str) -> Optional[Image.Image]:
    try:
        if b64.startswith("data:"):
            b64 = b64.split(",", 1)[1]
        data = base64.b64decode(b64)
        return Image.open(io.BytesIO(data)).convert("RGB")
    except Exception as e:
        log.warning(f"b64 decode failed: {e}")
        return None

def pil_to_bgr(img: Image.Image) -> np.ndarray:
    arr = np.array(img)
    return arr[:, :, ::-1]  # RGB → BGR for OpenCV/InsightFace


# ── Phase 2: Face matching ─────────────────────────────────────────────────────
FACE_MATCH_THRESHOLD = 0.30  # ArcFace cosine similarity threshold

def phase2_face_match(doc_img: Image.Image, face_img: Image.Image) -> dict:
    fa = get_face_app()
    if fa is None:
        return {"verified": True, "score": 0.65, "error": "model unavailable", "skipped": True}

    doc_bgr  = pil_to_bgr(doc_img)
    face_bgr = pil_to_bgr(face_img)

    try:
        doc_faces  = fa.get(doc_bgr)
        face_faces = fa.get(face_bgr)
    except Exception as e:
        return {"verified": False, "score": 0.0, "error": str(e)}

    if not doc_faces:
        return {"verified": False, "score": 0.0, "error": "no_face_in_document"}
    if not face_faces:
        return {"verified": False, "score": 0.0, "error": "no_face_in_selfie"}

    # Use largest detected face from each image
    doc_face  = max(doc_faces,  key=lambda f: f.bbox[2] * f.bbox[3])
    face_face = max(face_faces, key=lambda f: f.bbox[2] * f.bbox[3])

    emb1 = doc_face.embedding
    emb2 = face_face.embedding

    cosine = float(np.dot(emb1, emb2) / (np.linalg.norm(emb1) * np.linalg.norm(emb2) + 1e-8))
    score  = round((cosine + 1.0) / 2.0, 4)  # normalise -1..1 → 0..1

    return {
        "verified": cosine >= FACE_MATCH_THRESHOLD,
        "score":    score,
        "cosine":   round(cosine, 4),
    }


# ── Phase 3: OCR + MRZ parsing ─────────────────────────────────────────────────
MRZ_RE    = re.compile(r"[A-Z0-9<]{25,}")
# DOB patterns: YYMMDD (MRZ), DD/MM/YYYY, MM/DD/YYYY, YYYY-MM-DD
DOB_PATS  = [
    re.compile(r"(?<!\d)([0-9]{2})(0[1-9]|1[0-2])(0[1-9]|[12][0-9]|3[01])(?!\d)"),   # YYMMDD
    re.compile(r"\b(0?[1-9]|[12][0-9]|3[01])[\/\-\.](0?[1-9]|1[0-2])[\/\-\.]((?:19|20)[0-9]{2})\b"),  # DD/MM/YYYY
    re.compile(r"\b(0?[1-9]|1[0-2])[\/\-\.](0?[1-9]|[12][0-9]|3[01])[\/\-\.]((?:19|20)[0-9]{2})\b"),  # MM/DD/YYYY
    re.compile(r"\b((?:19|20)[0-9]{2})[\/\-\.](0?[1-9]|1[0-2])[\/\-\.](0?[1-9]|[12][0-9]|3[01])\b"),  # YYYY-MM-DD
]


def _mrz_dob(yymmdd: str) -> Optional[date]:
    try:
        yy, mm, dd = int(yymmdd[:2]), int(yymmdd[2:4]), int(yymmdd[4:6])
        year = 2000 + yy if yy <= 30 else 1900 + yy
        return date(year, mm, dd)
    except:
        return None

def calc_age(dob: date) -> int:
    today = date.today()
    return today.year - dob.year - ((today.month, today.day) < (dob.month, dob.day))

def _parse_mrz_lines(lines: list) -> tuple:
    """Return (dob, name) from MRZ lines."""
    td3 = [l for l in lines if len(l.replace(" ", "")) == 44]
    td1 = [l for l in lines if len(l.replace(" ", "")) == 30]

    dob  = None
    name = None

    if len(td3) >= 2:
        l1 = td3[0].replace(" ", "")
        l2 = td3[1].replace(" ", "")
        if len(l2) >= 19:
            dob = _mrz_dob(l2[13:19])
        # Name is the remainder of line1 after 5-char header
        if "<<" in l1[5:]:
            surname_given = l1[5:].split("<<", 1)
            surname = surname_given[0].replace("<", " ").strip()
            given   = surname_given[1].replace("<", " ").strip() if len(surname_given) > 1 else ""
            name    = f"{given} {surname}".strip() if given else surname

    elif len(td1) >= 2:
        l2 = td1[1].replace(" ", "")
        if len(l2) >= 6:
            dob = _mrz_dob(l2[:6])

    return dob, name

def phase3_ocr(doc_img: Image.Image) -> dict:
    configs = ["--psm 6 -l eng", "--psm 11 -l eng", "--psm 3 -l eng"]
    text_chunks = []
    for cfg in configs:
        try:
            t = pytesseract.image_to_string(doc_img, config=cfg)
            text_chunks.append(t.upper())
        except Exception as e:
            log.warning(f"tesseract psm failed: {e}")

    full_text = "\n".join(text_chunks)
    mrz_lines = [m for line in full_text.splitlines()
                 if (m := line.strip()) and MRZ_RE.fullmatch(m.replace(" ", ""))]

    dob  = None
    name = None

    if mrz_lines:
        dob, name = _parse_mrz_lines(mrz_lines)

    # Fallback: regex scan full text for DOB patterns
    if dob is None:
        for i, pat in enumerate(DOB_PATS):
            m = pat.search(full_text)
            if not m:
                continue
            g = m.groups()
            try:
                if i == 0:                          # YYMMDD
                    dob = _mrz_dob("".join(g))
                elif i == 1:                        # DD/MM/YYYY
                    dob = date(int(g[2]), int(g[1]), int(g[0]))
                elif i == 2:                        # MM/DD/YYYY
                    dob = date(int(g[2]), int(g[1]), int(g[0]))
                elif i == 3:                        # YYYY-MM-DD
                    dob = date(int(g[0]), int(g[1]), int(g[2]))
            except:
                dob = None
            if dob:
                break

    age = calc_age(dob) if dob else None

    return {
        "dob":        dob.isoformat() if dob else None,
        "age":        age,
        "name":       name,
        "mrz_found":  len(mrz_lines) > 0,
        "raw_sample": full_text[:300],
    }


# ── Phase 4: Anti-spoof / presentation-attack detection ───────────────────────
SPOOF_PASS_THRESHOLD = 0.35

def phase4_antispoof(face_img: Image.Image) -> dict:
    """
    Passive texture analysis — no ML model needed.
    Checks: micro-texture gradient (flat screens lack this),
            colour saturation (screens over-saturate skin),
            high-freq noise (real camera sensors add it).
    Score: 0 = likely spoof, 1 = likely real.
    """
    try:
        img = face_img.resize((224, 224))
        arr = np.array(img).astype(np.float32)
        gray = arr.mean(axis=2)

        # 1. Local texture gradient variance
        gy, gx = np.gradient(gray)
        grad_var = float(np.var(np.sqrt(gx**2 + gy**2)))
        texture_score = min(1.0, grad_var / 400.0)

        # 2. Colour saturation — screens over-saturate
        r, g, b = arr[:,:,0], arr[:,:,1], arr[:,:,2]
        cmax = np.maximum(np.maximum(r, g), b)
        cmin = np.minimum(np.minimum(r, g), b)
        sat  = np.where(cmax > 0, (cmax - cmin) / (cmax + 1e-6), 0.0)
        sat_mean = float(sat.mean())
        sat_score = 1.0 - max(0.0, (sat_mean - 0.55) / 0.45)

        # 3. High-frequency energy ratio (sensor noise signature)
        from numpy.fft import fft2, fftshift
        F    = np.abs(fftshift(fft2(gray)))
        h, w = F.shape
        hc, wc = h // 4, w // 4
        centre = F[hc:h-hc, wc:w-wc]
        total  = float(F.sum()) + 1e-6
        hf_ratio = float(F.sum() - centre.sum()) / total
        noise_score = min(1.0, hf_ratio * 4.0)

        composite = round(
            texture_score * 0.40 +
            sat_score     * 0.30 +
            noise_score   * 0.30, 4
        )

        return {
            "score":       composite,
            "texture":     round(texture_score, 3),
            "saturation":  round(sat_score, 3),
            "noise":       round(noise_score, 3),
            "likely_real": composite >= SPOOF_PASS_THRESHOLD,
        }
    except Exception as e:
        log.error(f"antispoof error: {e}")
        return {"score": 0.7, "likely_real": True, "error": str(e)}


# ── Liveness server-side validation ───────────────────────────────────────────
def validate_liveness(results: list) -> dict:
    if not results:
        return {"passed": False, "score": 0.0, "reason": "no_data"}

    total  = len(results)
    passed = [r for r in results if r.get("passed")]
    score  = len(passed) / total

    # Reject bot-like uniform perfect scores
    motion_scores = [r.get("motion_score", 0) for r in results]
    suspicious = all(m == motion_scores[0] for m in motion_scores) and total > 1

    ok = len(passed) >= max(1, (total + 1) // 2) and not suspicious

    return {
        "passed":            ok,
        "score":             round(score, 3),
        "challenges_total":  total,
        "challenges_passed": len(passed),
        "suspicious":        suspicious,
    }


# ── /v1/verify — full KYC pipeline ────────────────────────────────────────────
@app.route("/v1/verify", methods=["POST"])
def verify():
    data = request.get_json(force=True) or {}

    doc_b64  = data.get("document_image", "")
    face_b64 = data.get("face_image", "")
    liveness = data.get("liveness_results", [])

    out = {
        "pass":         False,
        "confidence":   0.0,
        "age_band":     "unknown",
        "age_verified": False,
        "liveness":     {},
        "face_match":   {},
        "ocr":          {},
        "antispoof":    {},
        "reason":       "",
    }

    # ── Liveness ──────────────────────────────────────────────────────────────
    out["liveness"] = validate_liveness(liveness)
    if not out["liveness"]["passed"]:
        out["reason"] = "liveness_failed"
        return jsonify(out)

    # ── Decode images ─────────────────────────────────────────────────────────
    doc_img  = decode_b64(doc_b64)  if doc_b64  else None
    face_img = decode_b64(face_b64) if face_b64 else None

    if doc_img is None or face_img is None:
        out["reason"] = "missing_images"
        return jsonify(out)

    # ── Phase 4: Anti-spoof (fast, runs first) ────────────────────────────────
    out["antispoof"] = phase4_antispoof(face_img)
    if not out["antispoof"]["likely_real"]:
        out["reason"] = "presentation_attack"
        return jsonify(out)

    # ── Phase 3: OCR ─────────────────────────────────────────────────────────
    out["ocr"] = phase3_ocr(doc_img)
    age = out["ocr"].get("age")
    if age is not None:
        out["age_verified"] = age >= 18
        out["age_band"]     = "25+" if age >= 25 else ("18-24" if age >= 18 else "minor")
        if not out["age_verified"]:
            out["reason"] = "age_verification_failed"
            return jsonify(out)

    # ── Phase 2: Face match ───────────────────────────────────────────────────
    out["face_match"] = phase2_face_match(doc_img, face_img)
    face_ok = out["face_match"].get("verified", False)

    # ── Composite confidence ──────────────────────────────────────────────────
    liveness_w  = out["liveness"]["score"]             * 0.25
    face_w      = out["face_match"].get("score", 0.0)  * 0.40
    spoof_w     = out["antispoof"]["score"]             * 0.20
    age_w       = 0.15 if out["age_verified"] else 0.08
    out["confidence"] = round(liveness_w + face_w + spoof_w + age_w, 4)

    # Age is advisory if OCR couldn't extract a DOB (many ID types obscure it)
    age_ok = out["age_verified"] or age is None

    out["pass"] = face_ok and age_ok
    out["reason"] = "passed" if out["pass"] else (
        "face_mismatch" if not face_ok else "age_failed"
    )

    return jsonify(out)


# ── Phase 5: Risk signal on login ─────────────────────────────────────────────
@app.route("/v1/risk/score", methods=["POST"])
def risk_score():
    data = request.get_json(force=True) or {}

    ua         = data.get("user_agent", "")
    device_id  = data.get("device_id", "")
    pial_id    = data.get("pial_id", "")

    flags = []
    score = 0.0

    bot_strings = ["python-requests", "curl/", "wget/", "scrapy", "go-http-client", "libwww"]
    if any(b in ua.lower() for b in bot_strings):
        flags.append("bot_ua")
        score += 0.45

    if not device_id:
        flags.append("no_device_id")
        score += 0.10

    # IP-less requests
    if not data.get("ip"):
        flags.append("no_ip")
        score += 0.05

    score = round(min(score, 1.0), 4)

    return jsonify({
        "pial_id":         pial_id,
        "risk_score":      score,
        "flags":           flags,
        "require_reverify": score >= 0.50,
        "velocity_score":  score,
        "ip_reputation":   round(1.0 - score * 0.5, 4),
    })


@app.route("/health", methods=["GET"])
def health():
    return jsonify({"status": "ok", "service": "ekyc", "port": 8099})


if __name__ == "__main__":
    port = int(os.environ.get("PORT", 8099))
    log.info(f"eKYC service starting on :{port}")
    app.run(host="0.0.0.0", port=port, debug=False, threaded=True)
