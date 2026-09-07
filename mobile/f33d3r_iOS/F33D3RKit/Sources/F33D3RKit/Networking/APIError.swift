import Foundation

/// The single error shape the api/v1 surface returns, mirroring `apiErrorBody`.
public struct APIErrorBody: Codable, Hashable, Sendable {
    public let code: String
    public let message: String
}

/// Everything that can go wrong talking to Nantar.
public enum APIError: Error, Hashable, Sendable {
    /// The server answered with a structured error. `code` is stable and safe to
    /// branch on; `message` is human-readable and safe to show.
    case api(status: Int, body: APIErrorBody)
    /// A non-2xx response that was not a structured API error — a proxy error
    /// page, an HTML redirect, a gateway timeout.
    case unexpectedStatus(Int)
    /// The response body did not match the expected shape.
    case decoding(String)
    /// The request never completed: offline, DNS, TLS, cancellation.
    case transport(String)
    /// The request never completed, and we know which origin it was aimed at.
    ///
    /// Separate from ``transport`` because the origin is the whole diagnosis
    /// more often than the reason is. A DEBUG build launched from the home
    /// screen rather than from Xcode gets no `F33D3R_BASE_URL`, falls back to
    /// `http://127.0.0.1:8081`, and fails against a server that is not there —
    /// which reads as "check your connection" unless the message says where it
    /// looked.
    case unreachable(origin: String, reason: String)

    /// True when the session is gone and the user must sign in again.
    ///
    /// Sessions last 30 days and can also be revoked server-side, so any request
    /// can return this — callers should treat it as a signal to clear the stored
    /// token, not as a transient failure to retry.
    public var isUnauthenticated: Bool {
        if case .api(let status, _) = self { return status == 401 }
        if case .unexpectedStatus(401) = self { return true }
        return false
    }

    /// True when the caller is being rate limited. Nantar allows 300 reads and
    /// 60 writes per minute per IP and answers 429 with `Retry-After: 60`.
    public var isRateLimited: Bool {
        if case .api(let status, _) = self { return status == 429 }
        if case .unexpectedStatus(429) = self { return true }
        return false
    }

    /// The server's stable error code, when it sent one. Safe to branch on;
    /// `message` is for the reader, this is for the app.
    public var code: String? {
        if case .api(_, let body) = self { return body.code }
        return nil
    }

    /// True when the server has no such surface — a lane this build knows about
    /// and that deployment does not serve yet.
    ///
    /// Narrow on purpose. `unknown_surface` is what Nantar answers for a lane
    /// with no implementation behind it, and 404/501 are the two statuses that
    /// mean the same thing without a code. A 500 is a surface that exists and
    /// broke, and reporting that as an empty lane would hide a real outage
    /// behind a tidy screen.
    public var isUnservedSurface: Bool {
        if code == "unknown_surface" { return true }
        switch self {
        case .api(let status, _), .unexpectedStatus(let status):
            return status == 404 || status == 501
        default:
            return false
        }
    }

    /// Why a sign-in with the right password was still refused.
    ///
    /// Two situations that look identical as an HTTP 401 and are not: one is
    /// the server asking for the second factor, the other is the second factor
    /// being wrong. A sign-in screen has to open a code field for the first and
    /// leave it open with an error for the second, so the difference is a type
    /// here rather than a string comparison at the call site.
    public enum TwoFactorFailure: String, Sendable {
        case required = "two_fa_required"
        case invalid = "two_fa_invalid"
    }

    /// The two-factor state of a refused login, or nil when the refusal was
    /// about something else.
    public var twoFactor: TwoFactorFailure? {
        code.flatMap(TwoFactorFailure.init(rawValue:))
    }

    /// True when the viewer is asking for someone else's likes. There is no
    /// public likes surface on this platform, so this is a rule rather than a
    /// failure, and it gets a state of its own rather than an error screen.
    public var isPrivateLikes: Bool { code == "likes_private" }

    /// A message safe to put in front of a user.
    public var userMessage: String {
        switch self {
        case .api(_, let body):
            return body.message
        case .unexpectedStatus(let code) where code >= 500:
            return "F33D3R is having trouble right now. Try again in a moment."
        case .unexpectedStatus:
            return "Something went wrong. Try again."
        case .decoding:
            return "F33D3R sent something this version of the app doesn't understand."
        case .transport:
            return "Can't reach F33D3R. Check your connection."
        case .unreachable(let origin, _):
            return "Can't reach F33D3R at \(origin). Check your connection."
        }
    }
}
