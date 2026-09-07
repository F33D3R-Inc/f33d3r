import Foundation

/// The enrolment secret for an authenticator app.
///
/// Mirrors `GET /api/v1/2fa/setup`. Two spellings of the same secret: `uri` is
/// what a QR code encodes and `secret` is what someone types in by hand. The
/// app draws the QR itself from `uri` rather than fetching an image — a
/// server-rendered QR is a second copy of the secret crossing the wire, and
/// there is nothing in the string a phone cannot draw.
///
/// Neither value is stored. It is live only while the sheet is open; once the
/// code is confirmed the server holds the secret and the client holds nothing.
public struct TwoFactorSetup: Codable, Hashable, Sendable {
    /// `otpauth://totp/F33D3R:handle?secret=…&issuer=F33D3R`
    public let uri: String
    /// Base32, for manual entry.
    public let secret: String

    public init(uri: String, secret: String) {
        self.uri = uri
        self.secret = secret
    }

    /// The secret in the four-character groups every authenticator app prints
    /// it in, so a person copying it by hand does not lose their place.
    public var groupedSecret: String {
        stride(from: 0, to: secret.count, by: 4).map { offset in
            let start = secret.index(secret.startIndex, offsetBy: offset)
            let end = secret.index(start, offsetBy: min(4, secret.count - offset))
            return String(secret[start..<end])
        }.joined(separator: " ")
    }
}
