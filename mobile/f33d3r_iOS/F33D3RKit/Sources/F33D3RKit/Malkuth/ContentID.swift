import Foundation
import CryptoKit

/// The content address of a Work: `sha256:` followed by 64 lower-case hex
/// digits.
///
/// A CID is not an identifier the client invents — it is a hash of the signed
/// payload, and the server recomputes it (`verifyCID`) rather than trusting it.
/// That makes it the one value in this system with a useful property for a
/// mobile client on an unreliable network: identical content produces an
/// identical CID, `works.cid` is `UNIQUE NOT NULL`, and so a retried post cannot
/// become a second post. ``WorkSubmission`` is built on exactly that.
public enum ContentID {

    public static let prefix = "sha256:"

    /// `sha256:` + lower-case hex of the SHA-256 of `canonical`.
    ///
    /// Go builds the same string with `fmt.Sprintf("sha256:%x", sum)`, and `%x`
    /// on a byte array is lower-case, zero-padded, two digits per byte.
    public static func of(_ canonical: Data) -> String {
        let digest = SHA256.hash(data: canonical)
        var hex = ""
        hex.reserveCapacity(64)
        for byte in digest {
            hex.append(hexDigits[Int(byte >> 4)])
            hex.append(hexDigits[Int(byte & 0x0F)])
        }
        return prefix + hex
    }

    /// Whether `value` has the shape the server requires.
    ///
    /// The server checks only `strings.HasPrefix(cid, "sha256:")` on the
    /// envelope's own CID, and checks nothing at all on `parent_cid` — an
    /// ill-formed parent simply matches no row and the reply is stored as a
    /// top-level work. So this is stricter than the server on purpose: full
    /// length, and lower-case hex only, because `%x` never emits upper-case and
    /// a CID that differs only in case would never match a stored row.
    public static func isWellFormed(_ value: String) -> Bool {
        guard value.hasPrefix(prefix) else { return false }
        let hex = value.dropFirst(prefix.count)
        guard hex.count == 64 else { return false }
        return hex.allSatisfy { $0.isNumber || ("a"..."f").contains($0) }
    }

    private static let hexDigits = Array("0123456789abcdef")
}
