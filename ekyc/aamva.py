"""
aamva.py — PDF417 barcode reading and AAMVA DL/ID card design standard
parsing for US and Canadian driver's licences and identification cards.

The PDF417 symbol is decoded by zxing-cpp. The payload is parsed here:

  "@" LF RS CR "ANSI " IIN(6) AAMVAVersion(2) [JurisdictionVersion(2)] Entries(2)
  then Entries × (SubfileType(2) Offset(4) Length(4)), then the subfiles,
  each starting with its type ("DL" / "ID") followed by LF-separated data
  elements of the form <3-letter element ID><value>.

Version 1 (AAMVA 2000) has no jurisdiction version field and carries the
name in DAA; versions 2-3 carry the given names in DCT; version 4+ in DAC/DAD.

Dates: version 1 uses CCYYMMDD. From version 2 on, US documents use
MMDDCCYY and Canadian documents use CCYYMMDD. The two layouts cannot be
confused for any real date (a CCYYMMDD string read as MMDDCCYY has month
19/20; an MMDDCCYY string read as CCYYMMDD has a year below 1900), so the
version-prescribed order is tried first and the other order is accepted only
when the prescribed one does not form a valid date.
"""
import logging
import re
from dataclasses import dataclass
from datetime import date
from typing import Optional

import numpy as np
from PIL import Image

log = logging.getLogger("ekyc.aamva")

_CA_JURISDICTIONS = {"AB", "BC", "MB", "NB", "NL", "NS", "NT", "NU", "ON", "PE", "QC", "SK", "YT"}
_HEADER_RE = re.compile(r"ANSI ?(\d{6})(\d{2})")
_ELEMENT_RE = re.compile(r"^[A-Z]{3}$")


@dataclass
class AAMVAData:
    doc_type: str           # "drivers_license" | "id_card"
    issuing_country: str    # "USA" | "CAN"
    issuing_region: str     # jurisdiction code, e.g. "TX"
    number: str             # DAQ — never leaves the process
    surname: str
    given_names: str
    dob: date
    expiry: date
    version: int


@dataclass
class BarcodeResult:
    status: str             # "ok" | "absent" | "invalid"
    data: Optional[AAMVAData] = None
    detail: str = ""        # failure code, never a field value


def _date(value: str, order: str) -> Optional[date]:
    if len(value) != 8 or not value.isdigit():
        return None
    try:
        if order == "ymd":
            d = date(int(value[0:4]), int(value[4:6]), int(value[6:8]))
        else:
            d = date(int(value[4:8]), int(value[0:2]), int(value[2:4]))
    except ValueError:
        return None
    return d if 1900 <= d.year <= 2100 else None


def _parse_date(value: str, version: int, country: str) -> Optional[date]:
    value = value.strip()
    primary = "ymd" if version <= 1 or country == "CAN" else "mdy"
    secondary = "mdy" if primary == "ymd" else "ymd"
    return _date(value, primary) or _date(value, secondary)


def _clean(value: Optional[str]) -> str:
    if value is None:
        return ""
    v = " ".join(value.replace(",", " ").split()).upper()
    return "" if v in ("NONE", "UNAVL", "UNAVAILABLE") else v


def _elements(body: str) -> dict[str, str]:
    out: dict[str, str] = {}
    for raw in re.split(r"[\n\r\x1e]", body):
        raw = raw.strip("\x1c\x1d\x1e ")
        if len(raw) < 3 or not _ELEMENT_RE.match(raw[:3]):
            continue
        out.setdefault(raw[:3], raw[3:].strip())
    return out


def _subfile(payload: str, header_end: int, entries: int, subfile_type: str) -> Optional[str]:
    """
    Return the element text of the given subfile type. The designators give
    offset/length, but real-world issuers are frequently off by a few bytes,
    so the subfile is located by its "<type><element ID>" start marker after
    the designator table and bounded by the next subfile marker.
    """
    designators = payload[header_end:header_end + 10 * entries]
    types = [designators[i:i + 2] for i in range(0, len(designators), 10)]
    if subfile_type not in types:
        return None
    table_end = header_end + 10 * entries
    m = re.compile(re.escape(subfile_type) + r"(?=[A-Z]{3})").search(payload, table_end)
    if m is None:
        return None
    start = m.end()
    end = len(payload)
    for other in types:
        if other == subfile_type:
            continue
        n = re.compile(r"[\n\r\x1e]" + re.escape(other) + r"(?=[A-Z]{3})").search(payload, start)
        if n is not None:
            end = min(end, n.start())
    return payload[start:end]


def parse_payload(payload: str) -> BarcodeResult:
    """Parse a decoded PDF417 payload. Non-AAMVA payloads are "absent"."""
    if not payload.startswith("@"):
        return BarcodeResult("absent")
    m = _HEADER_RE.search(payload, 0, 64)
    if m is None:
        return BarcodeResult("absent")
    version = int(m.group(2))
    pos = m.end()
    if version >= 2:
        pos += 2  # jurisdiction version number
    entries_s = payload[pos:pos + 2]
    if not entries_s.isdigit() or int(entries_s) < 1:
        return BarcodeResult("invalid", detail="header_malformed")
    entries = int(entries_s)
    header_end = pos + 2

    doc_type = None
    body = _subfile(payload, header_end, entries, "DL")
    if body is not None:
        doc_type = "drivers_license"
    else:
        body = _subfile(payload, header_end, entries, "ID")
        if body is not None:
            doc_type = "id_card"
    if body is None:
        return BarcodeResult("invalid", detail="no_dl_or_id_subfile")

    el = _elements(body)
    number = _clean(el.get("DAQ")).replace(" ", "")
    region = _clean(el.get("DAJ"))
    country = _clean(el.get("DCG"))
    if not country:
        country = "CAN" if region in _CA_JURISDICTIONS else "USA"
    if country not in ("USA", "CAN"):
        return BarcodeResult("invalid", detail="country_unrecognised")

    surname = _clean(el.get("DCS"))
    given = " ".join(p for p in (_clean(el.get("DAC")), _clean(el.get("DAD"))) if p)
    if not given:
        given = _clean(el.get("DCT"))
    if not surname and el.get("DAA"):
        # Version 1 full name: "LAST,FIRST,MIDDLE" (or space separated).
        parts = [p.strip() for p in el["DAA"].upper().replace("$", ",").split(",") if p.strip()]
        if len(parts) == 1:
            parts = parts[0].split()
        if parts:
            surname, given = parts[0], " ".join(parts[1:])

    dob = _parse_date(el.get("DBB", ""), version, country)
    expiry = _parse_date(el.get("DBA", ""), version, country)

    missing = [code for code, ok in (
        ("number", bool(number)), ("region", bool(region)), ("surname", bool(surname)),
        ("dob", dob is not None), ("expiry", expiry is not None),
    ) if not ok]
    if missing:
        return BarcodeResult("invalid", detail="missing_" + "_".join(missing))
    if dob >= date.today():
        return BarcodeResult("invalid", detail="dob_not_in_past")

    return BarcodeResult("ok", AAMVAData(
        doc_type=doc_type, issuing_country=country, issuing_region=region,
        number=number, surname=surname, given_names=given,
        dob=dob, expiry=expiry, version=version,
    ))


def read_barcode(img: Image.Image) -> BarcodeResult:
    """
    Decode the PDF417 symbol on a document back and parse it. zxing-cpp
    searches rotations and inverted symbols itself; a 2× upscale pass is
    tried when the first pass finds nothing, for symbols whose modules are
    below two pixels wide in the photo.
    """
    import zxingcpp

    gray = np.asarray(img.convert("L"))
    attempts = [gray]
    h, w = gray.shape
    if max(h, w) <= 2000:
        attempts.append(np.asarray(img.convert("L").resize((w * 2, h * 2), Image.Resampling.BICUBIC)))

    for arr in attempts:
        results = zxingcpp.read_barcodes(
            arr,
            formats=zxingcpp.BarcodeFormat.PDF417,
            try_rotate=True,
            try_downscale=True,
            try_invert=True,
        )
        for r in results:
            if not r.valid:
                continue
            payload = bytes(r.bytes).decode("latin-1")
            parsed = parse_payload(payload)
            if parsed.status != "absent":
                return parsed
    return BarcodeResult("absent")
