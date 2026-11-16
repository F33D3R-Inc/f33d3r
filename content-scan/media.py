"""
Media access for the content-scan worker: path confinement, probing, and
streaming decodes through ffmpeg.

Every decoder invocation here is bounded three ways:
  * protocol: inputs are opened with `-protocol_whitelist file`, so a playlist
    or concat list inside the media volume cannot make ffmpeg fetch a URL;
  * memory: decoded output is read from a pipe in fixed-size chunks and never
    held whole;
  * time: each run has a deadline derived from the media duration, enforced by
    killing the process, and reported as DeadlineExceeded.

A decoder that exits non-zero raises MediaError carrying the tail of its
stderr. Nothing is retried quietly and nothing is replaced by a fallback value.
"""

import hashlib
import json
import os
import re
import subprocess
import threading
from typing import Iterator, Optional

from errors import (BadRequest, DeadlineExceeded, MediaError, MediaNotFound,
                    PathRefused)

# ── path confinement ──────────────────────────────────────────────────────────


# The read-only volumes the worker may read, by the environment variable that
# names each mount point. abraxas-shield mounts the same volumes at the same
# paths and hands over absolute paths inside them:
#   MEDIA_PRIVATE_DIR  f33d3r_media_private  caeor's private store (clean sources)
#   LIVE_SCAN_DIR      f33d3r_live_scan      sampled live keyframes
_ROOT_VARS = (("MEDIA_PRIVATE_DIR", "f33d3r_media_private"),
              ("LIVE_SCAN_DIR", "f33d3r_live_scan"))


def media_roots_from_env() -> tuple:
    """
    Every permitted root, resolved. Raises RuntimeError at start-up when one is
    unset or absent, so a worker missing a volume never accepts a request.
    """
    roots = []
    for var, volume in _ROOT_VARS:
        raw = os.environ.get(var, "").strip()
        if not raw:
            raise RuntimeError(f"{var} is not set — the worker cannot confine paths "
                               f"without it and refuses to start")
        if not os.path.isabs(raw):
            raise RuntimeError(f"{var}={raw!r} is not an absolute path")
        root = os.path.realpath(raw)
        if not os.path.isdir(root):
            raise RuntimeError(f"{var}={raw!r} does not exist in this container — "
                               f"the {volume} volume is not mounted")
        roots.append(root)
    return tuple(roots)


def resolve_media_path(raw, roots: tuple) -> str:
    """
    Resolve a caller-supplied path and confine it to one of `roots`.

    The path is resolved with realpath BEFORE the containment check, so `..`
    segments and symlinks that point outside a root are judged by where they
    actually lead. The volumes are mounted read-only in this container, so
    nothing running here can swap a checked file for a link afterwards.
    """
    if not isinstance(raw, str) or not raw:
        raise BadRequest("path must be a non-empty string")
    if "\x00" in raw:
        raise BadRequest("path contains a NUL byte")
    if not os.path.isabs(raw):
        raise BadRequest("path must be absolute")
    real = os.path.realpath(raw)
    if not any(real != root and os.path.commonpath([root, real]) == root for root in roots):
        raise PathRefused(f"{raw!r} resolves outside every permitted media root")
    if not os.path.exists(real):
        raise MediaNotFound(f"{raw!r} does not exist")
    if not os.path.isfile(real):
        raise BadRequest(f"{raw!r} is not a regular file")
    return real


def ffmpeg_input(path: str) -> str:
    """The input URL handed to ffmpeg/ffprobe: an explicit file: protocol."""
    return "file:" + path


# ── hashing ───────────────────────────────────────────────────────────────────


def sha256_file(path: str) -> str:
    """SHA-256 of the file's bytes, read in 1 MiB blocks."""
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()


# ── time budgets ──────────────────────────────────────────────────────────────

# Fixed start-up allowance for any decoder run, plus a per-media-second rate.
# The rates are deliberately generous — a deadline exists to catch a wedged
# decoder, not to race a slow one — and are named per operation below.
BUDGET_BASE_S = 120.0


def budget(duration_s: float, seconds_per_media_second: float) -> float:
    """Deadline in seconds for an operation over `duration_s` of media."""
    return BUDGET_BASE_S + max(duration_s, 0.0) * seconds_per_media_second


# ── probing ───────────────────────────────────────────────────────────────────

# Demuxers that read other files or URLs named inside the input. They are
# refused outright: what gets analysed must be the bytes at the confined path,
# not whatever a list inside it points to.
_INDIRECT_FORMATS = {"hls", "applehttp", "concat", "ffconcat", "dash", "webm_dash_manifest"}


class Probe:
    """What ffprobe says about a file: its streams, format and duration."""

    __slots__ = ("format_name", "duration", "has_video", "has_audio", "video_codec")

    def __init__(self, format_name: str, duration: Optional[float],
                 has_video: bool, has_audio: bool, video_codec: str):
        self.format_name = format_name
        self.duration = duration
        self.has_video = has_video
        self.has_audio = has_audio
        self.video_codec = video_codec


def probe(path: str) -> Probe:
    """
    Probe a file. Raises MediaError when ffprobe cannot read it or when it is
    a playlist-style container.
    """
    try:
        out = subprocess.run(
            ["ffprobe", "-v", "error", "-protocol_whitelist", "file",
             "-show_entries", "format=format_name,duration:stream=codec_type,codec_name",
             "-of", "json", ffmpeg_input(path)],
            capture_output=True, timeout=30,
        )
    except subprocess.TimeoutExpired as e:
        raise DeadlineExceeded(f"ffprobe did not answer within {e.timeout}s") from e
    if out.returncode != 0:
        raise MediaError("ffprobe could not read the file: "
                         + _tail(out.stderr), code="media_unreadable")
    try:
        info = json.loads(out.stdout)
    except json.JSONDecodeError as e:
        raise MediaError(f"ffprobe printed output that is not JSON: {e}",
                         code="media_unreadable") from e

    fmt = info.get("format") or {}
    format_name = str(fmt.get("format_name") or "")
    if set(format_name.split(",")) & _INDIRECT_FORMATS:
        raise MediaError(f"container {format_name!r} references other files and is "
                         f"not analysed — pass the media file itself",
                         code="indirect_container")

    duration = None
    raw_duration = fmt.get("duration")
    if raw_duration not in (None, "", "N/A"):
        try:
            duration = float(raw_duration)
        except (TypeError, ValueError) as e:
            raise MediaError(f"ffprobe reported an unreadable duration {raw_duration!r}",
                             code="media_unreadable") from e

    streams = info.get("streams") or []
    video = [s for s in streams if s.get("codec_type") == "video"]
    audio = [s for s in streams if s.get("codec_type") == "audio"]
    return Probe(format_name, duration, bool(video), bool(audio),
                 str(video[0].get("codec_name") or "") if video else "")


def require_duration(p: Probe) -> float:
    """The probed duration, or MediaError when the container does not state one."""
    if p.duration is None or p.duration <= 0:
        raise MediaError("the container does not report a duration — the media cannot "
                         "be sampled across its length", code="duration_unknown")
    return p.duration


# ── streaming decode ──────────────────────────────────────────────────────────

_STDERR_KEEP_BYTES = 4096


def _tail(stderr: bytes) -> str:
    text = (stderr or b"").decode("utf-8", "replace").strip()
    return text[-_STDERR_KEEP_BYTES:] if text else "(no stderr)"


class FFmpegRun:
    """
    One ffmpeg process whose stdout is consumed as a stream.

    stderr is drained on its own thread — a full stderr pipe would otherwise
    block ffmpeg while this process waits on stdout. Lines matching
    `capture_pattern` are kept in full (showinfo timestamps); everything else is
    kept as a bounded tail for error reports.

    Use as a context manager. Leaving the block early (an exception in the
    consumer) kills the process; leaving it normally waits for exit and raises
    MediaError on a non-zero status and DeadlineExceeded when the deadline
    killed it.
    """

    def __init__(self, args: list, deadline_s: float, what: str,
                 capture_pattern: Optional["re.Pattern"] = None):
        self.what = what
        self.deadline_s = deadline_s
        self.captured: list = []
        self._capture = capture_pattern
        self._stderr_tail = bytearray()
        self._timed_out = False
        self._proc = subprocess.Popen(
            ["ffmpeg", "-nostdin", "-hide_banner", "-nostats"] + args,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        self._stderr_thread = threading.Thread(target=self._drain_stderr, daemon=True)
        self._stderr_thread.start()
        self._timer = threading.Timer(deadline_s, self._kill_for_deadline)
        self._timer.daemon = True
        self._timer.start()

    def _drain_stderr(self):
        for line in self._proc.stderr:
            if self._capture is not None:
                m = self._capture.search(line.decode("utf-8", "replace"))
                if m:
                    self.captured.append(m)
                    continue
            self._stderr_tail.extend(line)
            if len(self._stderr_tail) > _STDERR_KEEP_BYTES * 2:
                del self._stderr_tail[:-_STDERR_KEEP_BYTES]

    def _kill_for_deadline(self):
        self._timed_out = True
        self._proc.kill()

    @property
    def stdout(self):
        return self._proc.stdout

    def read_exact(self, n: int) -> bytes:
        """Read exactly n bytes, or fewer only at end of stream."""
        buf = bytearray()
        while len(buf) < n:
            chunk = self._proc.stdout.read(n - len(buf))
            if not chunk:
                break
            buf.extend(chunk)
        return bytes(buf)

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc, tb):
        if exc_type is not None:
            self._proc.kill()
        self._proc.stdout.close()
        returncode = self._proc.wait()
        self._timer.cancel()
        self._stderr_thread.join()
        if exc_type is not None:
            return False
        if self._timed_out:
            raise DeadlineExceeded(f"{self.what}: ffmpeg was still running after "
                                   f"{self.deadline_s:.0f}s and was stopped")
        if returncode != 0:
            raise MediaError(f"{self.what}: ffmpeg exited {returncode}: "
                             + _tail(bytes(self._stderr_tail)), code="decode_failed")
        return False


def pcm_chunks(path: str, sample_rate: int, chunk_seconds: float, deadline_s: float,
               start_s: float = 0.0, length_s: Optional[float] = None,
               sample_format: str = "s16le") -> Iterator[bytes]:
    """
    Decode the first audio stream to mono PCM at `sample_rate`, yielding raw
    byte chunks of about `chunk_seconds`. Raises MediaError on a decoder
    failure, after the last chunk has been yielded.
    """
    bytes_per_sample = 2 if sample_format == "s16le" else 4
    chunk_bytes = int(sample_rate * chunk_seconds) * bytes_per_sample
    args = []
    if start_s > 0:
        args += ["-ss", f"{start_s:.3f}"]
    args += ["-v", "error", "-protocol_whitelist", "file", "-i", ffmpeg_input(path)]
    if length_s is not None:
        args += ["-t", f"{length_s:.3f}"]
    args += ["-map", "0:a:0", "-vn", "-sn", "-dn", "-ac", "1", "-ar", str(sample_rate),
             "-f", sample_format, "pipe:1"]
    with FFmpegRun(args, deadline_s, "audio decode") as run:
        while True:
            chunk = run.read_exact(chunk_bytes)
            if not chunk:
                break
            yield chunk


SHOWINFO_PTS = re.compile(r"Parsed_showinfo.*?\bn:\s*(\d+)\s+pts:\s*\S+\s+pts_time:\s*([-0-9.eE+]+)")
