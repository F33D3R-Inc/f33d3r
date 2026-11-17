"""
faces.py — face detection, recognition, head-pose measurement and
presentation-attack detection. Every model is loaded once at start-up from
files baked into the image; a model that fails to load stops the service.

Every model here is licensed for commercial use:
  YuNet 2026may  (OpenCV Zoo, MIT): face detector with 5 landmarks, run through
                 OpenCV's FaceDetectorYN. Dynamic input shape, as OpenCV 5's
                 ONNX Runtime engine requires.
  SFace 2021dec  (OpenCV Zoo, Apache-2.0): face recognition, 128-d embedding
                 from a landmark-aligned 112×112 crop (FaceRecognizerSF).
  MiniFASNetV2 + MiniFASNetV1SE (Silent-Face-Anti-Spoofing, Minivision,
                 Apache-2.0; ONNX export by yakhyo/face-anti-spoofing,
                 Apache-2.0): passive PAD on an 80×80 crop, three classes
                 (0 = 2D attack, 1 = live, 2 = 3D attack).
"""
import logging
import os
import threading
from dataclasses import dataclass
from typing import Optional

import cv2
import numpy as np
import onnxruntime as ort

log = logging.getLogger("ekyc.faces")

MODEL_DIR = os.environ.get("EKYC_MODEL_DIR", "/opt/ekyc-models")
FACE_MODEL_NAME = "YuNet-2026may+SFace-2021dec"
PAD_MODEL_NAME = "MiniFASNetV2+MiniFASNetV1SE"

_YUNET_FILE = "face_detection_yunet_2026may.onnx"
_SFACE_FILE = "face_recognition_sface_2021dec.onnx"

# Minimum detector confidence for any face used in a decision. Clear frontal
# faces score above 0.9 on YuNet; 0.7 keeps turned heads in the liveness frames
# while dropping the low-confidence hits YuNet gives document artwork.
DET_SCORE_MIN = 0.7
_DET_NMS = 0.3
# YuNet is trained on faces of roughly 10–300 px. A close selfie face is larger
# than that at camera resolution, so detection runs on the image scaled so its
# longer side is 640 px, then 320 px if that found nothing; boxes and landmarks
# are mapped back to the original image, so every size check and the aligned
# recognition crop use full-resolution pixels.
_DET_LONG_SIDES = (640, 320)

# (file, crop scale around the face box) — the scales the models were trained with.
_PAD_MODELS = (("MiniFASNetV2.onnx", 2.7), ("MiniFASNetV1SE.onnx", 4.0))
_PAD_INPUT = (80, 80)

_detector = None
_recognizer = None
# FaceDetectorYN keeps its input size as object state and FaceRecognizerSF
# holds one network; neither is safe to share across request threads.
_cv_lock = threading.Lock()
_pad_sessions: list[tuple[ort.InferenceSession, str, float]] = []


def load_models() -> None:
    """Load every model and run each once on a synthetic image. Raises on failure."""
    global _detector, _recognizer
    face_dir = os.path.join(MODEL_DIR, "face")
    _detector = cv2.FaceDetectorYN.create(os.path.join(face_dir, _YUNET_FILE), "",
                                          (640, 480), DET_SCORE_MIN, _DET_NMS, 5000)
    _recognizer = cv2.FaceRecognizerSF.create(os.path.join(face_dir, _SFACE_FILE), "")
    blank = np.zeros((480, 640, 3), dtype=np.uint8)
    if largest_face(blank) is not None:
        raise RuntimeError("face detector found a face in a blank image")
    emb = _recognizer.feature(np.zeros((112, 112, 3), dtype=np.uint8))
    if emb is None or emb.size != 128:
        raise RuntimeError("face recognizer produced an unexpected embedding")

    sessions = []
    for fname, scale in _PAD_MODELS:
        sess = ort.InferenceSession(os.path.join(MODEL_DIR, "pad", fname),
                                    providers=["CPUExecutionProvider"])
        inp = sess.get_inputs()[0]
        out = sess.run(None, {inp.name: np.zeros((1, 3, *_PAD_INPUT), dtype=np.float32)})[0]
        if out.shape != (1, 3):
            raise RuntimeError(f"{fname} produced an unexpected output shape")
        sessions.append((sess, inp.name, scale))
    _pad_sessions[:] = sessions
    log.info("models loaded: %s, %s", FACE_MODEL_NAME, PAD_MODEL_NAME)


@dataclass
class DetectedFace:
    bbox: np.ndarray        # x1, y1, x2, y2
    kps: np.ndarray         # 5×2: left eye, right eye, nose, left mouth, right mouth (image coords)
    det_score: float
    embedding: np.ndarray   # L2-normalised

    @property
    def width(self) -> float:
        return float(self.bbox[2] - self.bbox[0])


def _detect(bgr: np.ndarray) -> Optional[np.ndarray]:
    """YuNet rows (x, y, w, h, 5×landmark x/y, score) in original-image pixels."""
    h, w = bgr.shape[:2]
    for long_side in _DET_LONG_SIDES:
        scale = min(1.0, long_side / max(h, w))
        img = bgr if scale == 1.0 else cv2.resize(bgr, (max(1, round(w * scale)), max(1, round(h * scale))),
                                                  interpolation=cv2.INTER_AREA)
        _detector.setInputSize((img.shape[1], img.shape[0]))
        _, rows = _detector.detect(img)
        if rows is not None and len(rows):
            rows = rows.astype(np.float32).copy()
            rows[:, :14] /= scale
            return rows
    return None


def largest_face(bgr: np.ndarray) -> Optional[DetectedFace]:
    with _cv_lock:
        rows = _detect(bgr)
        if rows is None:
            return None
        rows = rows[rows[:, 14] >= DET_SCORE_MIN]
        if not len(rows):
            return None
        row = rows[np.argmax(rows[:, 2] * rows[:, 3])]
        emb = _recognizer.feature(_recognizer.alignCrop(bgr, row)).flatten().astype(np.float32)
    x, y, bw, bh = (float(v) for v in row[:4])
    # YuNet's landmark order is right eye, left eye, nose, right mouth corner,
    # left mouth corner, from the subject's view — image-left first, the order
    # Pose expects (left eye, right eye, nose, left mouth, right mouth in image).
    return DetectedFace(bbox=np.array([x, y, x + bw, y + bh], dtype=np.float32),
                        kps=row[4:14].reshape(5, 2).copy(),
                        det_score=float(row[14]),
                        embedding=emb / (np.linalg.norm(emb) + 1e-12))


def cosine(a: DetectedFace, b: DetectedFace) -> float:
    return float(np.dot(a.embedding, b.embedding))


# ── Head pose from the 5 landmarks ────────────────────────────────────────────
@dataclass
class Pose:
    yaw: float     # nose offset from the eye midpoint along the eye line, in inter-ocular units (+ = image right)
    pitch: float   # nose depth below the eye line as a fraction of eye-line→mouth distance
    mouth: float   # mouth-corner distance in inter-ocular units


def pose(face: DetectedFace) -> Pose:
    le, re_, nose, lm, rm = face.kps
    eye_vec = re_ - le
    iod = float(np.linalg.norm(eye_vec)) + 1e-6
    ux = eye_vec / iod                    # unit vector along the eye line (roll-corrected x)
    uy = np.array([-ux[1], ux[0]])        # perpendicular, pointing down the face
    eye_mid = (le + re_) / 2.0
    mouth_mid = (lm + rm) / 2.0
    n = nose - eye_mid
    m = mouth_mid - eye_mid
    return Pose(
        yaw=float(np.dot(n, ux) / iod),
        pitch=float(np.dot(n, uy) / (np.dot(m, uy) + 1e-6)),
        mouth=float(np.linalg.norm(rm - lm) / iod),
    )


# ── Presentation-attack detection ─────────────────────────────────────────────
def _pad_crop(bgr: np.ndarray, bbox: np.ndarray, scale: float) -> np.ndarray:
    src_h, src_w = bgr.shape[:2]
    x1, y1, x2, y2 = (float(v) for v in bbox)
    box_w, box_h = max(x2 - x1, 1.0), max(y2 - y1, 1.0)
    scale = min((src_h - 1) / box_h, (src_w - 1) / box_w, scale)
    new_w, new_h = box_w * scale, box_h * scale
    cx, cy = x1 + box_w / 2, y1 + box_h / 2
    left = max(0, int(cx - new_w / 2))
    top = max(0, int(cy - new_h / 2))
    right = min(src_w - 1, int(cx + new_w / 2))
    bottom = min(src_h - 1, int(cy + new_h / 2))
    crop = bgr[top:bottom + 1, left:right + 1]
    return cv2.resize(crop, _PAD_INPUT[::-1])


def pad_live_probability(bgr: np.ndarray, face: DetectedFace) -> float:
    """
    Mean probability of the "live" class across both MiniFASNet models, using
    the original Silent-Face preprocessing: BGR, 0-255 float, no normalisation.
    """
    probs = []
    for sess, name, scale in _pad_sessions:
        crop = _pad_crop(bgr, face.bbox, scale).astype(np.float32)
        x = np.transpose(crop, (2, 0, 1))[None, ...]
        logits = sess.run(None, {name: x})[0][0].astype(np.float64)
        e = np.exp(logits - logits.max())
        probs.append(float(e[1] / e.sum()))
    return float(np.mean(probs))
