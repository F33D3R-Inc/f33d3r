"""
Tests for the banned-content registry's fuzzy audio radius (scanner.py).

Hermetic: fingerprints are built directly as chromaprint frames and encoded with
libchromaprint, so nothing here needs ffmpeg, media files, a network or a
database. The scores are therefore synthetic — the radii themselves were chosen
from measurements on real encodes, recorded above AUDIO_BANNED_BLOCK_SCORE in
scanner.py. What these tests hold in place is the behaviour around them: that a
copy blocks, that a derivation is reported rather than refused, that an
unrelated work passes, and above all that nothing degenerate, undecodable or
missing is ever allowed to read as a clean answer.

Run with:  python3 test_banned_audio.py     (or under pytest)
"""

import numpy as np

import chromaprint
import scanner

_RNG = np.random.default_rng(20260904)


def _encode(frames: "np.ndarray") -> str:
    """The compressed base64 form banned_content_hashes stores."""
    return chromaprint.encode_fingerprint(
        [int(f) for f in frames], 1, base64=True
    ).decode("ascii")


def _work(n_frames: int = 400) -> "np.ndarray":
    """A fingerprint with the bit spread real music measures well above."""
    return _RNG.integers(0, 2 ** 32, n_frames, dtype=np.uint64).astype(np.uint32)


def _derive(frames: "np.ndarray", flip_fraction: float) -> "np.ndarray":
    """The same fingerprint with a fraction of its bits flipped."""
    bits = np.unpackbits(frames.copy().view(np.uint8))
    n = int(len(bits) * flip_fraction)
    idx = _RNG.choice(len(bits), size=n, replace=False)
    bits[idx] ^= 1
    return np.packbits(bits).view(np.uint32)


def _constant(n_frames: int = 400) -> "np.ndarray":
    """What chromaprint emits for silence or a steady tone: one repeated frame."""
    return np.full(n_frames, 0x0F0F0F0F, dtype=np.uint32)


def _rows(*fingerprints, category="test_vector"):
    return [{"hash_value": fp, "category": category} for fp in fingerprints]


# ── the radius ────────────────────────────────────────────────────────────────

def test_a_copy_of_banned_audio_blocks():
    banned = _work()
    copy = _derive(banned, 0.01)  # a re-encode moves a few bits per frame
    scan = scanner.scan_banned_audio(_encode(copy), _rows(_encode(banned)))
    assert scan.verdict == "block", scan.match
    assert scan.match["score"] >= scanner.AUDIO_BANNED_BLOCK_SCORE
    assert scan.match["category"] == "test_vector"


def test_derived_audio_is_reported_not_refused():
    banned = _work()
    derived = _derive(banned, 0.16)  # inside the review band, outside the block cut
    scan = scanner.scan_banned_audio(_encode(derived), _rows(_encode(banned)))
    assert scan.verdict == "review", scan.match
    assert (scanner.AUDIO_BANNED_REVIEW_SCORE
            <= scan.match["score"] < scanner.AUDIO_BANNED_BLOCK_SCORE)


def test_an_unrelated_work_is_not_a_match():
    scan = scanner.scan_banned_audio(_encode(_work()), _rows(_encode(_work())))
    assert scan.verdict == "none"
    assert scan.match is None
    assert scan.compared == 1  # it was compared, not skipped


def test_the_best_match_wins_over_a_whole_registry():
    banned = _work()
    rows = _rows(_encode(_work()), _encode(_work()), _encode(banned), _encode(_work()))
    scan = scanner.scan_banned_audio(_encode(_derive(banned, 0.02)), rows)
    assert scan.verdict == "block"
    assert scan.compared == 4


def test_the_banned_cuts_are_looser_than_the_duplicate_cuts():
    # The banned registry is curated and a miss admits banned material, so its
    # radius is wider than the duplicate scan's. If that ever inverts, the
    # reasoning in scanner.py no longer describes the code.
    assert scanner.AUDIO_BANNED_BLOCK_SCORE < scanner.AUDIO_DUPLICATE_BLOCK_SCORE
    assert scanner.AUDIO_BANNED_REVIEW_SCORE < scanner.AUDIO_DUPLICATE_REVIEW_SCORE
    assert scanner.AUDIO_BANNED_REVIEW_SCORE < scanner.AUDIO_BANNED_BLOCK_SCORE


# ── the degenerate-audio floor ────────────────────────────────────────────────

def test_degenerate_upload_audio_is_never_fuzzy_matched():
    # Silence measures 0.9375 against a 440 Hz tone. Without the floor one
    # banned near-silent clip would refuse every quiet upload on the platform.
    scan = scanner.scan_banned_audio(_encode(_constant()), _rows(_encode(_work())))
    assert scan.query_usable is False
    assert scan.query_reason
    assert scan.match is None
    assert scan.compared == 0


def test_a_degenerate_registry_entry_is_refused_and_named():
    degenerate = _encode(_constant())
    scan = scanner.scan_banned_audio(_encode(_work()), _rows(degenerate, category="csam"))
    assert scan.match is None
    assert scan.compared == 0
    assert len(scan.unusable) == 1
    assert scan.unusable[0]["hash_value"] == degenerate
    assert scan.unusable[0]["category"] == "csam"
    assert "bit variance" in scan.unusable[0]["reason"]
    assert scan.registry_faults is True


def test_a_degenerate_registry_entry_does_not_disable_the_others():
    banned = _work()
    rows = _rows(_encode(_constant()), _encode(banned))
    scan = scanner.scan_banned_audio(_encode(_derive(banned, 0.01)), rows)
    assert scan.verdict == "block"
    assert len(scan.unusable) == 1


def test_a_registry_entry_too_short_to_judge_is_refused():
    short = _encode(_work(scanner.AUDIO_MIN_OVERLAP_FRAMES - 1))
    scan = scanner.scan_banned_audio(_encode(_work()), _rows(short))
    assert scan.compared == 0
    assert len(scan.unusable) == 1
    assert "frames" in scan.unusable[0]["reason"]


# ── failure must never look like a clean answer ───────────────────────────────

def test_an_undecodable_registry_row_is_reported_not_scored_zero():
    banned = _work()
    rows = ([{"hash_value": "!!!not-a-fingerprint!!!", "category": "corrupt"}]
            + _rows(_encode(banned)))
    scan = scanner.scan_banned_audio(_encode(_derive(banned, 0.01)), rows)
    assert scan.verdict == "block", "one corrupt row must not disable the gate"
    assert len(scan.undecodable) == 1
    assert scan.undecodable[0]["category"] == "corrupt"
    assert scan.undecodable[0]["error"]
    assert scan.registry_faults is True


def test_an_undecodable_query_raises_rather_than_returning_no_match():
    try:
        scanner.scan_banned_audio("###", _rows(_encode(_work())))
    except scanner.AudioFingerprintError:
        return
    raise AssertionError("a fingerprint that will not decode reported 'no match'")


def test_an_empty_registry_is_not_a_verdict_about_anything():
    scan = scanner.scan_banned_audio(_encode(_work()), [])
    assert scan.verdict == "none"
    assert scan.compared == 0
    assert scan.registry_faults is False


def test_blank_registry_values_are_skipped_not_compared():
    scan = scanner.scan_banned_audio(
        _encode(_work()), [{"hash_value": "", "category": "x"}])
    assert scan.compared == 0
    assert scan.undecodable == []


def test_the_verdict_mapping_covers_every_band():
    assert scanner.banned_audio_match_verdict(None) == "none"
    assert scanner.banned_audio_match_verdict(0.0) == "none"
    assert scanner.banned_audio_match_verdict(
        scanner.AUDIO_BANNED_REVIEW_SCORE - 0.001) == "none"
    assert scanner.banned_audio_match_verdict(
        scanner.AUDIO_BANNED_REVIEW_SCORE) == "review"
    assert scanner.banned_audio_match_verdict(
        scanner.AUDIO_BANNED_BLOCK_SCORE - 0.001) == "review"
    assert scanner.banned_audio_match_verdict(
        scanner.AUDIO_BANNED_BLOCK_SCORE) == "block"
    assert scanner.banned_audio_match_verdict(1.0) == "block"


def test_the_entry_cache_answers_for_the_row_it_was_built_from():
    # Cached by the exact stored string, so a cached entry can never answer for
    # a different row.
    a, b = _encode(_work()), _encode(_work())
    assert scanner._banned_audio_entry(a).frames.tobytes() != \
           scanner._banned_audio_entry(b).frames.tobytes()
    assert scanner._banned_audio_entry(a) is scanner._banned_audio_entry(a)


if __name__ == "__main__":
    failures = []
    for name, fn in sorted(globals().items()):
        if not name.startswith("test_") or not callable(fn):
            continue
        try:
            fn()
            print("PASS  " + name)
        except Exception as e:  # noqa: BLE001 - a test harness reports everything
            failures.append(name)
            print("FAIL  %s: %s: %s" % (name, type(e).__name__, e))
    print("\n%d failed" % len(failures))
    raise SystemExit(1 if failures else 0)
