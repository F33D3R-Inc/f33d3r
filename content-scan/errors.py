"""
Failure types for the content-scan worker.

Every failure in this service is one of these, raised and carried to the HTTP
layer intact. Nothing here has a "default" form: a detector that did not run,
media that would not decode, or a request that was malformed is an HTTP error
with a code and a detail. It is never a zero score, an empty label list or an
empty fingerprint, because abraxas cannot tell any of those apart from a real
answer.
"""


class WorkerError(Exception):
    """Base failure. `status` is the HTTP status, `code` a stable machine name."""

    status = 500
    code = "internal_error"

    def __init__(self, detail: str, code: str = ""):
        super().__init__(detail)
        self.detail = detail
        if code:
            self.code = code


class BadRequest(WorkerError):
    """The request body is malformed or names an unsupported kind."""

    status = 400
    code = "bad_request"


class Unauthorized(WorkerError):
    """X-Internal-Key is missing or does not match."""

    status = 401
    code = "unauthorized"


class PathRefused(WorkerError):
    """The path resolves outside every permitted media root."""

    status = 403
    code = "path_outside_media_root"


class MediaNotFound(WorkerError):
    """The path is inside the root but nothing is there."""

    status = 404
    code = "media_not_found"


class MediaError(WorkerError):
    """
    The media exists but cannot be processed as the requested kind: it will not
    decode, has no stream of the required type, or a decoder exited non-zero.
    """

    status = 422
    code = "media_unprocessable"


class DetectorUnavailable(WorkerError):
    """
    A model could not be loaded or could not classify. The content was not
    examined, and the answer says so instead of carrying a score.
    """

    status = 503
    code = "detector_unavailable"


class DeadlineExceeded(WorkerError):
    """A decoder or model ran past the budget derived from the media duration."""

    status = 504
    code = "deadline_exceeded"
