"""
frontocr.py — cross-check of the printed document front against the
machine-readable data (PDF417 or MRZ).

The front is OCR'd with Tesseract. Lines that look like MRZ text are dropped,
so a passport's own MRZ can never confirm itself. The result is:

  "matched"      — the printed front confirms the machine-readable DOB or
                   name, and nothing on it contradicts the machine data;
  "contradicted" — the front was demonstrably legible (the printed expiry
                   date matches the machine expiry, or the DOB matched) yet
                   the DOB or the name is missing from it;
  "unreadable"   — the OCR could not confirm anything.

Only "matched" counts as consistent: an unreadable field is not evidence.
"""
import re
from datetime import date
from typing import Optional

import pytesseract
from PIL import Image, ImageOps

_MONTHS = {
    "JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
    "JUL": 7, "AUG": 8, "SEP": 9, "SEPT": 9, "OCT": 10, "NOV": 11, "DEC": 12,
}
_NUMERIC_DATE = re.compile(r"(?<!\d)(\d{1,4})[/\-. ](\d{1,2})[/\-. ](\d{2,4})(?!\d)")
_NAMED_DATE = re.compile(r"(?<!\d)(\d{1,2})\s*([A-Z]{3,4})(?:\s*/\s*[A-Z]{3,4})?\s*(\d{4})(?!\d)")
_NAMED_DATE_MDY = re.compile(r"\b([A-Z]{3,4})\s*(\d{1,2}),?\s*(\d{4})(?!\d)")


def _safe(y: int, m: int, d: int) -> Optional[date]:
    try:
        return date(y, m, d) if 1900 <= y <= 2100 else None
    except ValueError:
        return None


def _dates(text: str) -> set[date]:
    """Every valid reading of every date-like token (DMY, MDY and YMD)."""
    found: set[date] = set()
    for a, b, c in _NUMERIC_DATE.findall(text):
        if len(a) == 4:
            found.add(_safe(int(a), int(b), int(c)))
        elif len(c) == 4:
            found.add(_safe(int(c), int(a), int(b)))
            found.add(_safe(int(c), int(b), int(a)))
    for d, mon, y in _NAMED_DATE.findall(text):
        if mon in _MONTHS:
            found.add(_safe(int(y), _MONTHS[mon], int(d)))
    for mon, d, y in _NAMED_DATE_MDY.findall(text):
        if mon in _MONTHS:
            found.add(_safe(int(y), _MONTHS[mon], int(d)))
    found.discard(None)
    return found


def _words(text: str) -> list[str]:
    return re.findall(r"[A-Z]{3,}", text)


def _within_one_edit(a: str, b: str) -> bool:
    if a == b:
        return True
    if abs(len(a) - len(b)) > 1:
        return False
    if len(a) > len(b):
        a, b = b, a
    i = j = edits = 0
    while i < len(a) and j < len(b):
        if a[i] == b[j]:
            i += 1
            j += 1
            continue
        edits += 1
        if edits > 1:
            return False
        if len(a) == len(b):
            i += 1
        j += 1
    return edits + (len(b) - j) <= 1


def _name_seen(name: str, words: list[str]) -> bool:
    for token in re.findall(r"[A-Z]{3,}", name.upper()):
        for w in words:
            if (len(token) >= 5 and _within_one_edit(token, w)) or token == w:
                return True
    return False


def ocr_text(img: Image.Image) -> str:
    gray = ImageOps.autocontrast(img.convert("L"))
    if max(gray.size) < 1600:
        scale = 1600 / max(gray.size)
        gray = gray.resize((int(gray.width * scale), int(gray.height * scale)),
                           Image.Resampling.BICUBIC)
    chunks = [pytesseract.image_to_string(gray, config=cfg, timeout=20)
              for cfg in ("--psm 11 -l eng", "--psm 6 -l eng")]
    lines = []
    for ln in "\n".join(chunks).upper().splitlines():
        if "<<" in ln:
            continue  # MRZ text is the machine source, not the printed front
        lines.append(ln)
    return "\n".join(lines)


def cross_check(img: Image.Image, *, dob: date, expiry: Optional[date],
                surname: str, given_names: str) -> str:
    text = ocr_text(img)
    dates = _dates(text)
    words = _words(text)
    dob_seen = dob in dates
    exp_seen = expiry is not None and expiry in dates
    name_seen = _name_seen(surname, words) or _name_seen(given_names, words)

    if exp_seen and not dob_seen:
        return "contradicted"
    if dob_seen and not name_seen:
        return "contradicted"
    if dob_seen or name_seen:
        return "matched"
    return "unreadable"
