import Foundation

/// The authenticated user together with the PIAL a signature has to name.
///
/// `author_pial` is a **signed** field, so the client cannot leave it to the
/// server: whatever is in the payload is what the author put their name to, and
/// the server compares the signature against the key registered for the
/// *session's* PIAL. Sign with `""` and both checks still pass — `verifyCID`
/// recomputes the hash of what was sent, and `verifyMalkuthSig` never looks at
/// the field — and the result is a Work permanently stored with an author field
/// that names nobody. Nothing downstream would report it.
///
/// So the PIAL has to come from the server, and `MeDTO` is only now growing a
/// `pial_id`. This type reads it out of `/api/v1/me` without `CurrentUser`
/// having to carry it yet — decoding the same response twice rather than
/// spending a second request — and reports its absence as
/// ``MalkuthError/pialUnknown`` rather than substituting a placeholder.
///
/// When `pial_id` lands on `MeDTO` and on `CurrentUser`, `pialID` here becomes a
/// forward of that and this type stops needing its own decode. Until then it is
/// the seam, and it is deliberately the only place in the Kit that knows a PIAL
/// might be missing.
///
/// Note what is *not* here: no PIAL is ever put in a URL, a query item, a header
/// this client chooses, or any user-visible surface. It travels from `/me` into
/// a signed payload and nowhere else.
public struct AuthenticatedIdentity: Sendable, Equatable {
    public let user: CurrentUser
    /// `nil` when the deployment's `MeDTO` does not carry `pial_id` yet.
    public let pialID: String?

    public init(user: CurrentUser, pialID: String?) {
        self.user = user
        self.pialID = pialID
    }

    /// The PIAL, or a typed refusal to sign without one.
    public func requirePIAL() throws(MalkuthError) -> String {
        guard let pialID, !pialID.trimmingCharacters(in: .whitespaces).isEmpty else {
            throw .pialUnknown
        }
        return pialID
    }

    /// Whether this device can sign at all. Worth checking before a compose
    /// screen opens, so the refusal arrives before the writing rather than
    /// after it.
    public var canSign: Bool {
        (try? requirePIAL()) != nil
    }
}

/// Reads just the identity spine out of a `/me` response.
private struct MePIAL: Decodable {
    let pialID: String?
    enum CodingKeys: String, CodingKey { case pialID = "pial_id" }

    init(from decoder: any Decoder) throws {
        // `decodeIfPresent` on a container that may not have the key at all:
        // an older deployment simply omits it, which is not a decode failure.
        let container = try decoder.container(keyedBy: CodingKeys.self)
        pialID = try container.decodeIfPresent(String.self, forKey: .pialID)
    }
}

public extension APIClient {
    /// `GET /api/v1/me`, returning the profile and the PIAL in one request.
    ///
    /// Use this rather than ``me()`` wherever the answer will be used to sign.
    func meIdentity() async throws -> AuthenticatedIdentity {
        let (data, status) = try await sendRaw(.me, body: nil, contentType: nil)
        guard (200..<300).contains(status) else {
            throw MalkuthError.rejected(
                status: status,
                reason: String(decoding: data.prefix(500), as: UTF8.self)
            )
        }
        let decoder = APIClient.makeDecoder()
        let user = try decoder.decode(CurrentUser.self, from: data)
        let pial = (try? decoder.decode(MePIAL.self, from: data))?.pialID
        return AuthenticatedIdentity(
            user: user,
            pialID: (pial?.isEmpty ?? true) ? nil : pial
        )
    }
}

public extension WorkPayload {
    /// Milliseconds since the epoch, the way `Date.now()` gives them to
    /// `malkuth.js`.
    ///
    /// Stamped once when a work is composed and then never again: this value is
    /// inside the signature, so re-stamping it on a retry makes a different CID
    /// and therefore a second post. See ``WorkSubmission``.
    static func nowMS() -> Int64 {
        Int64((Date().timeIntervalSince1970 * 1000).rounded())
    }
}
