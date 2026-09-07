"""
mrz.py — MRZ (Machine Readable Zone) detection and validation for F33D3R eKYC.

Lane 1 age verification: image in → age_verified out. Image never persists.

Supports ICAO 9303 document types:
  TD1  — ID cards (3-line × 30 chars)
  TD2  — 2-line ID cards (2-line × 36 chars)
  TD3  — Passports (2-line × 44 chars)
  MRVA — Visa A (2-line × 44 chars)
  MRVB — Visa B (2-line × 36 chars)
"""
import io
import logging
from datetime import date
from typing import Optional

from PIL import Image

log = logging.getLogger("ekyc.mrz")

# ── Checksum constants (ICAO 9303 §4.9) ───────────────────────────────────────
_WEIGHTS  = [7, 3, 1]
_CHAR_VAL = {c: i for i, c in enumerate("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ<")}


def mrz_checksum(field: str) -> int:
    """Compute the ICAO 9303 check digit for a MRZ field string."""
    total = 0
    for i, ch in enumerate(field):
        val = _CHAR_VAL.get(ch, 0)
        total += val * _WEIGHTS[i % 3]
    return total % 10


# ── Century resolution ────────────────────────────────────────────────────────
def _resolve_century(yy: int) -> int:
    """
    Convert a 2-digit MRZ year to a 4-digit year.
    Threshold 30: YY ≤ 30 → 20YY, YY > 30 → 19YY.
    Safe until ~2030 when the youngest verifiable adult (born 2012) turns 18.
    """
    return 2000 + yy if yy <= 30 else 1900 + yy


def _parse_dob(yymmdd: str) -> Optional[date]:
    """Parse a 6-char YYMMDD MRZ date string into a date object."""
    try:
        yy  = int(yymmdd[0:2])
        mm  = int(yymmdd[2:4])
        dd  = int(yymmdd[4:6])
        return date(_resolve_century(yy), mm, dd)
    except (ValueError, IndexError):
        return None


def _calc_age(dob: date) -> int:
    today = date.today()
    return today.year - dob.year - (
        (today.month, today.day) < (dob.month, dob.day)
    )


# ── Text-only MRZ parser / validator ─────────────────────────────────────────
def parse_mrz_lines(mrz_lines: list[str]) -> dict:
    """
    Parse and validate a list of MRZ text lines using passporteye's MRZ class.

    Returns a result dict with keys: valid, valid_score, doc_type, country,
    doc_number, dob, expiry, age, is_18_plus.  On failure: valid=False + error.
    """
    try:
        from passporteye.mrz.text import MRZ as _TextMRZ
    except ImportError as exc:
        log.error("passporteye not available: %s", exc)
        return {"valid": False, "error": "passporteye_unavailable"}

    if not mrz_lines:
        return {"valid": False, "error": "no_mrz_lines"}

    mrz = _TextMRZ(mrz_lines)

    if mrz.mrz_type is None:
        return {"valid": False, "error": "unrecognised_mrz_format"}

    # Require all check digits to pass — this is the anti-forgery guarantee.
    if not mrz.valid:
        return {
            "valid":       False,
            "error":       "checksum_failed",
            "valid_score": mrz.valid_score,
            "mrz_type":    mrz.mrz_type,
        }

    dob = _parse_dob(mrz.date_of_birth)
    if dob is None:
        return {"valid": False, "error": "dob_parse_failed"}

    age       = _calc_age(dob)
    is_18     = age >= 18

    # Expiry in YYMMDD form — same century heuristic (docs issued now expire ≤ 10 years)
    expiry_str = None
    if mrz.expiration_date:
        exp = _parse_dob(mrz.expiration_date)
        expiry_str = exp.isoformat() if exp else None

    return {
        "valid":       True,
        "valid_score": mrz.valid_score,
        "mrz_type":    mrz.mrz_type,
        "doc_type":    (mrz.type or "").replace("<", "").strip() or mrz.mrz_type[:2],
        "country":     mrz.country,
        "doc_number":  mrz.number,
        "dob":         dob.isoformat(),   # kept internally; not forwarded to API callers
        "age":         age,
        "is_18_plus":  is_18,
        "expiry":      expiry_str,
    }


# ── Image-based MRZ detection ─────────────────────────────────────────────────
def detect_mrz(image_bytes: bytes) -> dict:
    """
    Detect and parse the MRZ from a raw image.

    The image is consumed entirely in-memory and never written to disk.

    Args:
        image_bytes: Raw bytes of any image format PIL can open (JPEG, PNG, etc.).

    Returns:
        On success: {
            "valid": True, "doc_type": "P", "country": "USA",
            "dob": "1985-01-01", "age": 40, "is_18_plus": True,
            "doc_number": "L898902C3", "expiry": "2028-12-31",
            "mrz_type": "TD3", "valid_score": 100
        }
        On failure: {"valid": False, "error": "<reason>"}
    """
    try:
        from passporteye.mrz.image import read_mrz as _read_mrz
    except ImportError as exc:
        log.error("passporteye not available: %s", exc)
        return {"valid": False, "error": "passporteye_unavailable"}

    # Validate the image with PIL before handing the bytes to passporteye.
    try:
        pil_img = Image.open(io.BytesIO(image_bytes)).convert("RGB")
        pil_img.verify()  # checks for truncation / corruption
    except Exception as exc:
        log.warning("image decode failed: %s", exc)
        return {"valid": False, "error": "invalid_image"}

    # passporteye.read_mrz accepts a file-like stream directly.
    stream = io.BytesIO(image_bytes)
    try:
        mrz_obj = _read_mrz(stream)
    except Exception as exc:
        log.warning("passporteye pipeline error: %s", exc)
        return {"valid": False, "error": "mrz_pipeline_error"}

    if mrz_obj is None:
        return {"valid": False, "error": "no_mrz_detected"}

    # mrz_obj is a passporteye MRZ instance — delegate to the text parser
    # by extracting the raw lines it detected.
    try:
        raw_text: str = mrz_obj.aux.get("raw_text", "")
        lines = [l for l in raw_text.splitlines() if l.strip()]
    except Exception:
        lines = []

    if not lines:
        # Fallback: reconstruct lines from the structured fields passporteye
        # already parsed (it uses the same text MRZ class internally).
        from passporteye.mrz.text import MRZ as _TextMRZ
        mrz_text = _TextMRZ.__new__(_TextMRZ)
        mrz_text.__dict__.update(mrz_obj.__dict__)
        # mrz_obj is already a fully parsed MRZ — re-parse its fields via
        # our own wrapper to get uniform output.
        return _from_passporteye_mrz(mrz_obj)

    return parse_mrz_lines(lines)


def _from_passporteye_mrz(mrz_obj) -> dict:
    """
    Convert a passporteye MRZ object (returned by read_mrz) into our result dict
    without re-running OCR.  Used when raw_text lines are not available.
    """
    if mrz_obj.mrz_type is None:
        return {"valid": False, "error": "unrecognised_mrz_format"}

    if not mrz_obj.valid:
        return {
            "valid":       False,
            "error":       "checksum_failed",
            "valid_score": mrz_obj.valid_score,
            "mrz_type":    mrz_obj.mrz_type,
        }

    dob = _parse_dob(mrz_obj.date_of_birth)
    if dob is None:
        return {"valid": False, "error": "dob_parse_failed"}

    age     = _calc_age(dob)
    expiry_str = None
    if mrz_obj.expiration_date:
        exp = _parse_dob(mrz_obj.expiration_date)
        expiry_str = exp.isoformat() if exp else None

    return {
        "valid":       True,
        "valid_score": mrz_obj.valid_score,
        "mrz_type":    mrz_obj.mrz_type,
        "doc_type":    (mrz_obj.type or "").replace("<", "").strip() or mrz_obj.mrz_type[:2],
        "country":     mrz_obj.country,
        "doc_number":  mrz_obj.number,
        "dob":         dob.isoformat(),
        "age":         age,
        "is_18_plus":  age >= 18,
        "expiry":      expiry_str,
    }
