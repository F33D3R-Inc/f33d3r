"""
mrz.py — ICAO 9303 machine-readable zone reading for passports (TD3) and
ID cards (TD1).

The MRZ is located and OCR'd by PassportEye (MRZ box detection + Tesseract),
its OCR text is normalised by PassportEye's positional character cleaner
(digits only where the format allows digits, letters only where it allows
letters), and then parsed HERE by a strict parser that verifies every check
digit, including the composite. A result is only "ok" when every check digit
holds; anything else is "invalid". TD2 and visas are not accepted.
"""
import io
import logging
from dataclasses import dataclass
from datetime import date
from typing import Optional

from PIL import Image

log = logging.getLogger("ekyc.mrz")

_CHAR_VAL = {c: i for i, c in enumerate("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ")}
_CHAR_VAL["<"] = 0
_WEIGHTS = (7, 3, 1)


def check_digit(field: str) -> str:
    """ICAO 9303 §4.9 check digit of a field, as a single character."""
    total = 0
    for i, ch in enumerate(field):
        if ch not in _CHAR_VAL:
            raise ValueError("character outside the MRZ alphabet")
        total += _CHAR_VAL[ch] * _WEIGHTS[i % 3]
    return str(total % 10)


def _digit_ok(field: str, digit: str) -> bool:
    try:
        return check_digit(field) == digit
    except ValueError:
        return False


def _optional_digit_ok(field: str, digit: str) -> bool:
    """An optional-data check digit may be '<' when the field is all fillers."""
    if digit == "<":
        return set(field) <= {"<"}
    return _digit_ok(field, digit)


def _parse_yymmdd(s: str, *, past: bool) -> Optional[date]:
    """
    YYMMDD → date. Birth dates (`past=True`) take the latest century that is
    not in the future; expiry dates take 20YY (no current document expires
    before 2000 or after 2099).
    """
    if len(s) != 6 or not s.isdigit():
        return None
    yy, mm, dd = int(s[0:2]), int(s[2:4]), int(s[4:6])
    try:
        if past:
            today = date.today()
            d = date(2000 + yy, mm, dd)
            return d if d <= today else date(1900 + yy, mm, dd)
        return date(2000 + yy, mm, dd)
    except ValueError:
        return None


def _names(field: str) -> tuple[str, str]:
    surname, _, given = field.partition("<<")
    return (surname.replace("<", " ").strip(),
            " ".join(given.replace("<", " ").split()))


@dataclass
class MRZData:
    doc_type: str          # "passport" | "id_card"
    issuing_country: str   # ICAO 3-letter code, fillers stripped
    number: str            # document number — never leaves the process
    surname: str
    given_names: str
    dob: date
    expiry: Optional[date]


@dataclass
class MRZResult:
    status: str            # "ok" | "absent" | "invalid"
    data: Optional[MRZData] = None
    detail: str = ""       # machine-readable failure code, no field values


def _doc_type(code: str, fmt: str) -> Optional[str]:
    first = code[0]
    if fmt == "TD3" and first == "P":
        return "passport"
    if fmt == "TD1" and first in ("I", "A", "C"):
        return "id_card"
    return None


def _parse_td3(l1: str, l2: str) -> MRZResult:
    doc_type = _doc_type(l1[0:2], "TD3")
    if doc_type is None:
        return MRZResult("invalid", detail="unsupported_document_code")
    number, number_cd = l2[0:9], l2[9]
    dob_s, dob_cd = l2[13:19], l2[19]
    exp_s, exp_cd = l2[21:27], l2[27]
    opt, opt_cd = l2[28:42], l2[42]
    composite = l2[0:10] + l2[13:20] + l2[21:43]
    checks = (
        _digit_ok(number, number_cd),
        _digit_ok(dob_s, dob_cd),
        _digit_ok(exp_s, exp_cd),
        _optional_digit_ok(opt, opt_cd),
        _digit_ok(composite, l2[43]),
    )
    if not all(checks):
        return MRZResult("invalid", detail="checksum_failed")
    dob = _parse_yymmdd(dob_s, past=True)
    if dob is None:
        return MRZResult("invalid", detail="dob_unparseable")
    surname, given = _names(l1[5:44])
    return MRZResult("ok", MRZData(
        doc_type=doc_type,
        issuing_country=l1[2:5].replace("<", ""),
        number=number.replace("<", ""),
        surname=surname, given_names=given,
        dob=dob, expiry=_parse_yymmdd(exp_s, past=False),
    ))


def _parse_td1(l1: str, l2: str, l3: str) -> MRZResult:
    doc_type = _doc_type(l1[0:2], "TD1")
    if doc_type is None:
        return MRZResult("invalid", detail="unsupported_document_code")
    number, number_cd = l1[5:14], l1[14]
    if number_cd == "<":
        # Long document number (ICAO 9303-5 §4.2.2 note j): the number
        # continues in optional data 1; its last character before the
        # filler is the check digit of the whole number.
        ext = l1[15:30].split("<", 1)[0]
        if len(ext) < 2:
            return MRZResult("invalid", detail="checksum_failed")
        number, number_cd = number + ext[:-1], ext[-1]
    dob_s, dob_cd = l2[0:6], l2[6]
    exp_s, exp_cd = l2[8:14], l2[14]
    composite = l1[5:30] + l2[0:7] + l2[8:15] + l2[18:29]
    checks = (
        _digit_ok(number, number_cd),
        _digit_ok(dob_s, dob_cd),
        _digit_ok(exp_s, exp_cd),
        _digit_ok(composite, l2[29]),
    )
    if not all(checks):
        return MRZResult("invalid", detail="checksum_failed")
    dob = _parse_yymmdd(dob_s, past=True)
    if dob is None:
        return MRZResult("invalid", detail="dob_unparseable")
    surname, given = _names(l3)
    return MRZResult("ok", MRZData(
        doc_type=doc_type,
        issuing_country=l1[2:5].replace("<", ""),
        number=number.replace("<", ""),
        surname=surname, given_names=given,
        dob=dob, expiry=_parse_yymmdd(exp_s, past=False),
    ))


def parse_lines(lines: list[str]) -> MRZResult:
    """Strictly parse already-cleaned MRZ lines (TD1: 3×30, TD3: 2×44)."""
    lines = [ln.strip().upper() for ln in lines if ln.strip()]
    if len(lines) == 2 and all(len(ln) == 44 for ln in lines):
        return _parse_td3(*lines)
    if len(lines) == 3 and all(len(ln) == 30 for ln in lines):
        return _parse_td1(*lines)
    return MRZResult("invalid", detail="unsupported_mrz_format")


# ── OCR normalisation ─────────────────────────────────────────────────────────
# Per-position character classes (ICAO 9303 layouts):
#   a = letter, n = digit, * = any (no correction).
# OCR confuses O/0, I/1, S/5, B/8, Z/2 and G/6; a confusable character is
# corrected only where the layout allows exactly one class. Corrections can
# never forge a pass: every corrected field is still bound by its check digit.
_TD3_FMT = ("a" + "*" * 43,
            "*" * 9 + "n" + "a" * 3 + "n" * 7 + "*" + "n" * 7 + "*" * 14 + "*" + "n")
_TD1_FMT = ("a" + "*" + "a" * 3 + "*" * 25,
            "n" * 7 + "*" + "n" * 7 + "a" * 3 + "*" * 11 + "n",
            "*" * 30)
_TO_LETTER = {"0": "O", "1": "I", "2": "Z", "5": "S", "6": "G", "8": "B"}
_TO_DIGIT = {"O": "0", "Q": "0", "D": "0", "I": "1", "Z": "2", "S": "5", "G": "6", "B": "8"}
_MRZ_ALPHABET = set("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789<")


def _fix(line: str, fmt: str) -> str:
    out = []
    for ch, cls in zip(line, fmt):
        if cls == "a" and ch != "<":
            ch = _TO_LETTER.get(ch, ch)
        elif cls == "n":
            ch = _TO_DIGIT.get(ch, ch)
        out.append(ch)
    return "".join(out)


def _candidate_lines(raw: str) -> list[str]:
    lines = []
    for ln in raw.upper().splitlines():
        ln = ln.replace(" ", "")
        if len(ln) in (30, 44) and set(ln) <= _MRZ_ALPHABET:
            lines.append(ln)
    return lines


def _from_ocr_text(raw: str) -> MRZResult:
    lines = _candidate_lines(raw)
    td3 = [ln for ln in lines if len(ln) == 44]
    td1 = [ln for ln in lines if len(ln) == 30]
    if len(td3) >= 2:
        l1, l2 = td3[-2], td3[-1]
        return parse_lines([_fix(l1, _TD3_FMT[0]), _fix(l2, _TD3_FMT[1])])
    if len(td1) >= 3:
        l1, l2, l3 = td1[-3], td1[-2], td1[-1]
        return parse_lines([_fix(l1, _TD1_FMT[0]), _fix(l2, _TD1_FMT[1]), l3])
    return MRZResult("invalid", detail="unsupported_mrz_format")


def read_mrz(img: Image.Image) -> MRZResult:
    """
    Locate, OCR and strictly validate the MRZ in a document image.
    "absent" when no MRZ region is found; "invalid" when one is found but it
    does not satisfy the TD1/TD3 layout and every check digit.
    """
    from passporteye.mrz.image import read_mrz as _pe_read_mrz

    buf = io.BytesIO()
    img.save(buf, format="PNG")
    buf.seek(0)
    found = _pe_read_mrz(buf)
    if found is None:
        return MRZResult("absent")
    raw = found.aux.get("raw_text") or ""
    if "<" not in raw:
        return MRZResult("absent")
    return _from_ocr_text(raw)
