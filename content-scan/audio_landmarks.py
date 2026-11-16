"""
Audio landmark fingerprints: classic (exact-copy) and robust (pitch- and
tempo-invariant) hashes over spectral peaks, computed across the WHOLE file
with bounded memory.

Pipeline
────────
  ffmpeg ─► mono 8 kHz s16 PCM, read 30 s at a time
        ─► STFT (1024-sample periodic Hann, hop 128) → log power in dBFS, 94-3805 Hz
        ─► 2-D local maxima, gated by a silence floor and a spectral-prominence floor
        ─► density control: the strongest 20 peaks per second (by prominence)
        ─► parabolic refinement of each peak's time and frequency (sub-bin, sub-frame)
        ─► CLASSIC landmarks: anchor + up to 5 targets, hash(f1, f2, Δt)
        ─► ROBUST landmarks: anchor + its 4 nearest-in-time neighbours within ±1 octave,
           every ordered pair (b, c) → hash(log2 f_b/f_a, log2 f_c/f_a, log2 Δt_c/Δt_b)

Nothing about the file's length is bounded: the decode, the spectrogram, the
peak picker and the pairing all stream. What stays resident is one 30 s PCM
chunk, its spectrogram (~4 MB), a few seconds of peak context, and the output
hash arrays themselves (16 bytes per landmark).

Why two hash families
─────────────────────
Classic landmarks are the Wang (2003) construction. They are near-certain on
re-encodes, trims, excerpts, added noise and moderate EQ, and they carry 24
bits each, so a handful of time-consistent hits identifies a work. They do not
survive a pitch shift (every absolute frequency moves) or a tempo change
(every Δt moves).

Robust landmarks hash only quantities that are invariant under
  * a uniform pitch shift   f → p·f       (frequency RATIOS are unchanged)
  * a uniform tempo change  t → s·t       (time-difference RATIOS are unchanged)
  * a speed change (both)   resampling    (both of the above at once)
They carry 18 bits each — the price of throwing away absolute frequency and
absolute time — so a match on them must be confirmed by time consistency in
the matcher (hits that line up on one offset and one scale), never by count
alone. The `t` of every landmark is its anchor time in the source, which is
exactly what that verification needs.

Hash layout (int64, always positive)
────────────────────────────────────
  bits 60-62  family tag: 1 = classic, 2 = robust. The two families never
              collide with each other, whatever their payload.
  classic:    [23:15] anchor FFT bin (9 bits, 12..486)
              [14:6]  target FFT bin (9 bits, 12..486)
              [5:0]   Δt in 32 ms units (6 bits, 1..63 → 32 ms .. 2.02 s)
  robust:     [17:12] q(log2 f_b/f_a)  (6 bits, 1/32 octave steps over ±1 octave)
              [11:6]  q(log2 f_c/f_a)  (6 bits, same)
              [5:0]   q(log2 (t_c−t_a)/(t_b−t_a)) (6 bits, 1/12 octave steps)

Any change to a parameter below changes the hashes. LANDMARK_VERSION names the
parameter set and is returned with every fingerprint, so stored landmarks are
only ever compared against landmarks of the same version.
"""

import array
import math
from typing import Iterable, List, Optional

import numpy as np
from numpy.lib.stride_tricks import sliding_window_view

import media
from errors import MediaError

LANDMARK_VERSION = "lm1"

# ── signal ────────────────────────────────────────────────────────────────────

# 8 kHz mono. Everything a landmark is built from lives below 4 kHz: the
# fundamentals and low harmonics of voice and instruments, which are also what
# survives the destructive transforms that matter here — 8-16 kbps codecs,
# telephone-band rendering, phone-speaker re-recording. Content above 4 kHz is
# exactly what those transforms discard, so peaks there are peaks a copy would
# not reproduce. 11.025 kHz would add 1.5 kHz of band that is mostly that same
# fragile content, at 38 % more compute per second; 8 kHz is the better trade.
SAMPLE_RATE = 8000

# 1024-sample window = 128 ms, 7.8125 Hz per bin. Fine frequency resolution is
# what the robust hashes need: a frequency ratio quantised at 1/32 octave (2.2 %)
# is only meaningful if the peak frequency is known to better than that. At the
# lowest bin used (12, 94 Hz) one whole bin is 8.3 %, and the parabolic
# refinement below brings the estimate to roughly a tenth of a bin.
N_FFT = 1024

# 128-sample hop = 16 ms, 62.5 frames/s (8× overlap). Fine time resolution is
# what the robust TIME ratio needs: Δt_b is at least 128 ms (8 frames), and with
# sub-frame refinement its error stays a few percent — inside the 5.9 % step.
HOP = 128
FRAME_RATE = SAMPLE_RATE / HOP

# Periodic Hann: the window whose spectral leakage the parabolic interpolation
# on log magnitude is accurate for.
_WINDOW = np.hanning(N_FFT + 1)[:-1].astype(np.float32)

# A full-scale sine lands at 0 dBFS in its bin.
_REF_POWER = float((_WINDOW.sum() / 2.0) ** 2)

# Usable band: bins 12..486 = 93.75 Hz .. 3797 Hz. Below ~90 Hz is mains hum
# (50/60 Hz and its first harmonic), rumble and DC offset — energy present in a
# copy because of the room or the electronics, not the work. Above ~3.8 kHz the
# 8 kHz resampler's anti-alias filter is rolling off, so peak magnitudes there
# depend on which resampler made the copy.
BIN_LO = 12
BIN_HI = 487
N_BINS = BIN_HI - BIN_LO

# ── peak picking ──────────────────────────────────────────────────────────────

# A peak must be the maximum of a (2·12+1) bins × (2·8+1) frames neighbourhood:
# ±94 Hz × ±128 ms. Narrow enough in frequency that the separate partials of a
# chord each yield a peak; wide enough in time that a sustained note yields one
# peak per ~quarter second rather than a ridge of them.
PEAK_FREQ_RADIUS = 12
PEAK_TIME_RADIUS = 8

# Prominence is the peak's level above the mean log level of its own frame
# within ±32 bins (±250 Hz). It ranks tonal, structured energy above broadband
# energy: noise mixed into a copy raises the local mean, so a noise peak has low
# prominence and a melody peak keeps its rank. 6 dB is the floor below which a
# "peak" is indistinguishable from the ripple of a noise floor.
BACKGROUND_FREQ_RADIUS = 32
MIN_PROMINENCE_DB = 6.0

# Absolute floor: nothing quieter than -72 dBFS in its bin is a peak. That is
# well below any mastered or spoken programme material and well above the
# dither and codec noise of digital silence, so silent passages emit nothing
# instead of hashing noise.
SILENCE_FLOOR_DBFS = -72.0

# Density target: the strongest 20 peaks in every second, by prominence. The
# per-second quota is what keeps quiet passages fingerprinted — a global top-N
# would spend every peak on the loud chorus and leave a quiet verse, which is
# exactly the excerpt someone lifts, with nothing. 20/s is the density audfprint
# and the Wang paper settle on: enough that a 5 s excerpt carries ~100 anchors
# and several hundred landmarks, few enough that the target zones stay sparse
# and the hashes discriminative.
PEAKS_PER_SECOND = 20

# ── classic landmarks ─────────────────────────────────────────────────────────

# Each anchor pairs with its first 5 peaks (in time order) inside the target
# zone. Fan-out is redundancy: under heavy degradation a given peak survives
# with some probability p, a landmark needs both ends, and five chances per
# anchor keep the expected survivors per second high even when p is ~0.5.
CLASSIC_FANOUT = 5

# Target zone: 32 ms .. 2.02 s after the anchor, within ±96 bins (±750 Hz).
# The lower bound skips the anchor's own time-smear; the upper bound keeps pairs
# local, so a 5 s excerpt still contains whole landmarks; the frequency bound
# keeps the pair inside one musical gesture, which is what makes it repeatable.
CLASSIC_DT_MIN_FRAMES = 2
CLASSIC_DT_MAX_FRAMES = 126
CLASSIC_DF_MAX_BINS = 96

# Δt is hashed in 2-frame (32 ms) units. A peak's refined time moves by a
# fraction of a frame across re-encodes; a 1-frame unit would split the same
# landmark across two hashes far more often.
CLASSIC_DT_UNIT_FRAMES = 2

# ── robust landmarks ──────────────────────────────────────────────────────────

# The anchor's 4 nearest later peaks (in time order) within the robust zone give
# C(4,2) = 6 triples. "Nearest in time order" is itself invariant to a tempo
# change — the order of events does not change when they are played faster — so
# the same neighbours are chosen in the original and in a 1.1× copy.
ROBUST_NEIGHBOURS = 4

# Robust zone: 128 ms .. 3.0 s after the anchor and within ±1 octave of it.
# The 128 ms floor keeps the denominator of the time ratio large enough that a
# sub-frame timing error stays a few percent of it. The octave bound is a
# RATIO, so a pitch shift moves no peak in or out of it except at the very edge.
ROBUST_DT_MIN_FRAMES = 8
ROBUST_DT_MAX_FRAMES = 188
ROBUST_OCTAVES = 1.0

# Frequency ratios are hashed in 1/32-octave (37.5 cent, 2.2 %) steps over the
# ±1 octave span: 64 levels, 6 bits. A ±2 semitone shift moves every ratio by
# exactly zero; what moves a ratio is the peak-frequency estimate itself, and
# after refinement that error is well inside one step.
FREQ_RATIO_STEPS_PER_OCTAVE = 32

# The time ratio (t_c−t_a)/(t_b−t_a) lies in (1, 23.5]; its log2 is hashed in
# 1/12-octave (5.9 %) steps: up to 55 levels, 6 bits.
TIME_RATIO_STEPS_PER_OCTAVE = 12

_TAG_CLASSIC = 1 << 60
_TAG_ROBUST = 2 << 60
_MAX_DT_FRAMES = max(CLASSIC_DT_MAX_FRAMES, ROBUST_DT_MAX_FRAMES)

# ── streaming ─────────────────────────────────────────────────────────────────

# PCM is pulled from ffmpeg 30 s at a time: 480 KB of s16, ~4 MB of
# spectrogram. Independent of the file's length.
DECODE_CHUNK_SECONDS = 30.0

# Decode budget: 8 kHz mono decode runs at hundreds of times real time; half a
# second per media second only trips on a decoder that has stopped making
# progress.
DECODE_SECONDS_PER_MEDIA_SECOND = 0.5


class LandmarkSeries:
    """Parallel arrays of int64 hashes and anchor times (seconds)."""

    __slots__ = ("hashes", "times")

    def __init__(self):
        self.hashes = array.array("q")
        self.times = array.array("d")

    def __len__(self):
        return len(self.hashes)


class AudioLandmarks:
    """Result of one extraction over a whole file."""

    __slots__ = ("classic", "robust", "peaks", "samples")

    def __init__(self):
        self.classic = LandmarkSeries()
        self.robust = LandmarkSeries()
        self.peaks = 0
        self.samples = 0

    @property
    def duration_s(self) -> float:
        return self.samples / SAMPLE_RATE


# ── STFT ──────────────────────────────────────────────────────────────────────


class _Stft:
    """
    Streaming short-time Fourier transform. Frame k is centred on sample k·HOP:
    N_FFT/2 zeros are prepended, so a landmark's time is the time in the file
    and an excerpt cut at any instant reports times offset by exactly the cut.
    """

    def __init__(self):
        self._carry = np.zeros(N_FFT // 2, dtype=np.float32)
        self.frames_out = 0
        self.samples_in = 0

    def _frames(self, buf: np.ndarray) -> np.ndarray:
        if len(buf) < N_FFT:
            self._carry = buf
            return np.empty((0, N_BINS), dtype=np.float32)
        n = 1 + (len(buf) - N_FFT) // HOP
        windows = sliding_window_view(buf, N_FFT)[::HOP][:n]
        spec = np.fft.rfft(windows * _WINDOW, axis=1)[:, BIN_LO:BIN_HI]
        power = spec.real * spec.real + spec.imag * spec.imag
        db = (10.0 * np.log10(power / _REF_POWER + 1e-12)).astype(np.float32)
        self._carry = buf[n * HOP:].copy()
        self.frames_out += n
        return db

    def push(self, samples: np.ndarray) -> np.ndarray:
        self.samples_in += len(samples)
        return self._frames(np.concatenate([self._carry, samples]))

    def finish(self) -> np.ndarray:
        """Frames centred on the remaining samples, zero-padded on the right."""
        needed = -(-self.samples_in // HOP)
        missing = needed - self.frames_out
        if missing <= 0:
            return np.empty((0, N_BINS), dtype=np.float32)
        tail = np.concatenate([self._carry, np.zeros(missing * HOP + N_FFT, dtype=np.float32)])
        db = self._frames(tail)[:missing]
        self.frames_out = needed
        return db


# ── peak picking ──────────────────────────────────────────────────────────────


def _block_start(block: int) -> int:
    """First frame of one-second block `block` (frames f with f·HOP // SR == block)."""
    return -(-block * SAMPLE_RATE // HOP)


class _Peaks:
    """Peaks finalised in one pass, sorted by refined time."""

    __slots__ = ("frame", "t", "bin", "f")

    def __init__(self, frame, t, fbin, f):
        self.frame = frame
        self.t = t
        self.bin = fbin
        self.f = f


class _PeakPicker:
    """
    Streaming 2-D peak picker. Frames are finalised a whole one-second block at
    a time, once PEAK_TIME_RADIUS frames of right-hand context exist, so the
    per-second density quota is applied to complete seconds and the result does
    not depend on where the decode chunks happened to split.
    """

    def __init__(self):
        self._buf = np.empty((0, N_BINS), dtype=np.float32)
        self._buf_start = 0
        self.final_frame = 0          # every frame below this is finalised
        self._next_block = 0

    def push(self, db: np.ndarray) -> Optional[_Peaks]:
        if len(db):
            self._buf = np.concatenate([self._buf, db])
        buf_end = self._buf_start + len(self._buf)
        block = self._next_block
        while _block_start(block + 1) + PEAK_TIME_RADIUS <= buf_end:
            block += 1
        if block == self._next_block:
            return None
        peaks = self._finalise(self.final_frame, _block_start(block))
        self._next_block = block
        self.final_frame = _block_start(block)
        self._trim()
        return peaks

    def finish(self) -> Optional[_Peaks]:
        buf_end = self._buf_start + len(self._buf)
        if self.final_frame >= buf_end:
            return None
        peaks = self._finalise(self.final_frame, buf_end)
        self.final_frame = buf_end
        return peaks

    def _trim(self):
        keep_from = max(self.final_frame - PEAK_TIME_RADIUS, self._buf_start)
        self._buf = self._buf[keep_from - self._buf_start:].copy()
        self._buf_start = keep_from

    def _finalise(self, f0: int, f1: int) -> _Peaks:
        n = f1 - f0
        lo, hi = f0 - PEAK_TIME_RADIUS, f1 + PEAK_TIME_RADIUS
        buf_end = self._buf_start + len(self._buf)

        # Context outside the file (before frame 0, after the last frame) is
        # -inf: it can never out-rank a real value, so edge frames are judged
        # on the context that exists.
        region = np.full((hi - lo, N_BINS), -np.inf, dtype=np.float32)
        src_lo, src_hi = max(lo, self._buf_start), min(hi, buf_end)
        region[src_lo - lo:src_hi - lo] = self._buf[src_lo - self._buf_start:src_hi - self._buf_start]
        core = region[PEAK_TIME_RADIUS:PEAK_TIME_RADIUS + n]

        # Separable max filter: frequency, then time.
        padded = np.pad(region, ((0, 0), (PEAK_FREQ_RADIUS, PEAK_FREQ_RADIUS)),
                        constant_values=-np.inf)
        fmax = sliding_window_view(padded, 2 * PEAK_FREQ_RADIUS + 1, axis=1).max(axis=-1)
        nmax = sliding_window_view(fmax, 2 * PEAK_TIME_RADIUS + 1, axis=0).max(axis=-1)

        # A plateau (two equal neighbours — synthetic tones, clipped material)
        # yields one peak at its first cell, not one per cell: strictly greater
        # than the previous frame and the lower bin, equal to the neighbourhood max.
        prev_frame = region[PEAK_TIME_RADIUS - 1:PEAK_TIME_RADIUS - 1 + n]
        lower_bin = np.full_like(core, -np.inf)
        lower_bin[:, 1:] = core[:, :-1]
        is_peak = (core >= nmax) & (core > prev_frame) & (core > lower_bin)
        is_peak &= core > SILENCE_FLOOR_DBFS
        # Edge bins have only one neighbour; they cannot be refined or trusted.
        is_peak[:, 0] = False
        is_peak[:, -1] = False

        # Prominence over the frame's own ±BACKGROUND_FREQ_RADIUS bins.
        cs = np.zeros((n, N_BINS + 1), dtype=np.float64)
        np.cumsum(core, axis=1, dtype=np.float64, out=cs[:, 1:])
        idx = np.arange(N_BINS)
        b_lo = np.maximum(idx - BACKGROUND_FREQ_RADIUS, 0)
        b_hi = np.minimum(idx + BACKGROUND_FREQ_RADIUS + 1, N_BINS)
        background = (cs[:, b_hi] - cs[:, b_lo]) / (b_hi - b_lo)
        prominence = core - background
        is_peak &= prominence >= MIN_PROMINENCE_DB

        rows, cols = np.nonzero(is_peak)
        if rows.size == 0:
            empty_i, empty_f = np.empty(0, np.int64), np.empty(0, np.float64)
            return _Peaks(empty_i, empty_f, empty_i, empty_f)

        # Density quota: strongest PEAKS_PER_SECOND per one-second block.
        frames = f0 + rows.astype(np.int64)
        blocks = frames * HOP // SAMPLE_RATE
        prom = prominence[rows, cols]
        order = np.lexsort((-prom, blocks))
        sorted_blocks = blocks[order]
        group_start = np.r_[0, np.flatnonzero(np.diff(sorted_blocks)) + 1]
        group_len = np.diff(np.r_[group_start, sorted_blocks.size])
        rank = np.arange(sorted_blocks.size) - np.repeat(group_start, group_len)
        kept = order[rank < PEAKS_PER_SECOND]
        rows, cols, frames = rows[kept], cols[kept], frames[kept]

        # Parabolic refinement on the log spectrum, along frequency and time.
        with np.errstate(invalid="ignore", divide="ignore"):
            a = core[rows, cols - 1].astype(np.float64)
            b = core[rows, cols].astype(np.float64)
            c = core[rows, cols + 1].astype(np.float64)
            den = a - 2.0 * b + c
            df = np.where(den < 0, 0.5 * (a - c) / den, 0.0)
            tp = region[PEAK_TIME_RADIUS + rows - 1, cols].astype(np.float64)
            tn = region[PEAK_TIME_RADIUS + rows + 1, cols].astype(np.float64)
            den_t = tp - 2.0 * b + tn
            usable_t = np.isfinite(tp) & np.isfinite(tn) & (den_t < 0)
            dt = np.where(usable_t, 0.5 * (tp - tn) / den_t, 0.0)
        df = np.clip(np.nan_to_num(df, nan=0.0), -0.5, 0.5)
        dt = np.clip(np.nan_to_num(dt, nan=0.0), -0.5, 0.5)

        fbin = BIN_LO + cols.astype(np.int64)
        t = frames.astype(np.float64) + dt
        f = fbin.astype(np.float64) + df
        order = np.lexsort((fbin, t))
        return _Peaks(frames[order], t[order], fbin[order], f[order])


# ── pairing ───────────────────────────────────────────────────────────────────


class _Pairer:
    """
    Turns the finalised peak stream into landmarks. An anchor is paired once
    every peak that could fall in its target zones is final, then dropped, so
    only ~3 s of peaks are ever held.
    """

    def __init__(self, out: AudioLandmarks):
        self._out = out
        self._frame: List[int] = []
        self._t: List[float] = []
        self._bin: List[int] = []
        self._f: List[float] = []
        self._head = 0

    def add(self, peaks: Optional[_Peaks]):
        if peaks is None or peaks.frame.size == 0:
            return
        self._frame.extend(peaks.frame.tolist())
        self._t.extend(peaks.t.tolist())
        self._bin.extend(peaks.bin.tolist())
        self._f.extend(peaks.f.tolist())
        self._out.peaks += int(peaks.frame.size)

    def emit(self, final_frame: Optional[int]):
        """Pair every anchor whose zones are complete (all, when final_frame is None)."""
        while self._head < len(self._frame):
            if final_frame is not None and self._frame[self._head] + _MAX_DT_FRAMES >= final_frame:
                break
            self._pair(self._head)
            self._head += 1
        if self._head > 4096:
            del self._frame[:self._head], self._t[:self._head]
            del self._bin[:self._head], self._f[:self._head]
            self._head = 0

    def _pair(self, i: int):
        frame_a, t_a, bin_a, f_a = self._frame[i], self._t[i], self._bin[i], self._f[i]
        anchor_s = t_a / FRAME_RATE
        classic_n = 0
        neighbours = []
        j, n = i + 1, len(self._frame)
        while j < n:
            dframes = self._frame[j] - frame_a
            if dframes > _MAX_DT_FRAMES:
                break
            if (classic_n < CLASSIC_FANOUT
                    and CLASSIC_DT_MIN_FRAMES <= dframes <= CLASSIC_DT_MAX_FRAMES
                    and abs(self._bin[j] - bin_a) <= CLASSIC_DF_MAX_BINS):
                dtq = int(round((self._t[j] - t_a) / CLASSIC_DT_UNIT_FRAMES))
                dtq = 1 if dtq < 1 else (63 if dtq > 63 else dtq)
                self._out.classic.hashes.append(
                    _TAG_CLASSIC | (bin_a << 15) | (self._bin[j] << 6) | dtq)
                self._out.classic.times.append(anchor_s)
                classic_n += 1
            if (len(neighbours) < ROBUST_NEIGHBOURS
                    and ROBUST_DT_MIN_FRAMES <= dframes <= ROBUST_DT_MAX_FRAMES):
                ratio = math.log2(self._f[j] / f_a)
                if -ROBUST_OCTAVES < ratio < ROBUST_OCTAVES:
                    neighbours.append((self._t[j], ratio))
            if classic_n >= CLASSIC_FANOUT and len(neighbours) >= ROBUST_NEIGHBOURS:
                break
            j += 1

        for x in range(len(neighbours)):
            t_b, r_b = neighbours[x]
            span_b = t_b - t_a
            q_b = _quantise_octaves(r_b)
            for y in range(x + 1, len(neighbours)):
                t_c, r_c = neighbours[y]
                if t_c <= t_b:
                    # Simultaneous peaks have no time order to hash; the pair
                    # would be ambiguous between (b, c) and (c, b).
                    continue
                q_t = int(math.log2((t_c - t_a) / span_b) * TIME_RATIO_STEPS_PER_OCTAVE)
                q_t = 63 if q_t > 63 else q_t
                self._out.robust.hashes.append(
                    _TAG_ROBUST | (q_b << 12) | (_quantise_octaves(r_c) << 6) | q_t)
                self._out.robust.times.append(anchor_s)


def _quantise_octaves(ratio_octaves: float) -> int:
    """Map (−1, 1) octave to 0..63 in 1/32-octave steps."""
    q = int(math.floor((ratio_octaves + ROBUST_OCTAVES) * FREQ_RATIO_STEPS_PER_OCTAVE))
    return 0 if q < 0 else (63 if q > 63 else q)


# ── entry point ───────────────────────────────────────────────────────────────


def extract_pcm(chunks: Iterable[bytes]) -> AudioLandmarks:
    """Landmarks from a stream of mono s16le PCM chunks at SAMPLE_RATE."""
    out = AudioLandmarks()
    stft, picker, pairer = _Stft(), _PeakPicker(), _Pairer(out)
    for chunk in chunks:
        if len(chunk) % 2:
            raise MediaError("the audio decoder produced a truncated 16-bit sample",
                             code="decode_failed")
        samples = np.frombuffer(chunk, dtype="<i2").astype(np.float32) / 32768.0
        out.samples += samples.size
        pairer.add(picker.push(stft.push(samples)))
        pairer.emit(picker.final_frame)
    pairer.add(picker.push(stft.finish()))
    pairer.add(picker.finish())
    pairer.emit(None)
    return out


def extract(path: str, duration_s: float) -> AudioLandmarks:
    """
    Landmarks over the whole of the first audio stream of `path`.

    Raises MediaError when the stream will not decode or decodes to nothing,
    DeadlineExceeded when the decoder stalls. An empty landmark list is
    returned only for audio that genuinely has no peak above the silence and
    prominence floors — digital silence, a steady hum — and `peaks` says so.
    """
    deadline = media.budget(duration_s, DECODE_SECONDS_PER_MEDIA_SECOND)
    result = extract_pcm(media.pcm_chunks(path, SAMPLE_RATE, DECODE_CHUNK_SECONDS, deadline))
    if result.samples == 0:
        raise MediaError("the audio stream decoded to zero samples", code="audio_empty")
    return result
