import Foundation

/// The authenticated user together with the account a signature has to name.
///
/// `author_account` is a **signed** field: the server refuses a work whose
/// `author_account` is not the session's own account, and verifies the
/// signature against the key registered for the session's identity itself. The
/// account id comes from `GET /api/v1/me`, which answers `account_id` to its
/// owner and to nobody else; this type reads it in the same request that reads
/// the profile, and reports its absence as ``MalkuthError/accountUnknown``
/// rather than substituting a placeholder.
///
/// The account id also names this device's signing key for that account
/// (``KeychainMalkuthKeyStore``). It never appears in a URL, a query item, a
/// log line or any user-visible surface.
public struct AuthenticatedIdentity: Sendable, Equatable {
    public let user: CurrentUser
    /// `nil` when the deployment's `MeDTO` did not carry `account_id`.
    public let accountID: String?

    public init(user: CurrentUser, accountID: String?) {
        self.user = user
        self.accountID = accountID
    }

    /// The account id, or a typed refusal to sign without one.
    public func requireAccount() throws(MalkuthError) -> String {
        guard let accountID, !accountID.trimmingCharacters(in: .whitespaces).isEmpty else {
            throw .accountUnknown
        }
        return accountID
    }

    /// Whether this device can sign at all. Worth checking before a compose
    /// screen opens, so the refusal arrives before the writing rather than
    /// after it.
    public var canSign: Bool {
        (try? requireAccount()) != nil
    }
}

/// Reads just the account id out of a `/me` response.
private struct MeAccount: Decodable {
    let accountID: String?
    enum CodingKeys: String, CodingKey { case accountID = "account_id" }

    init(from decoder: any Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        accountID = try container.decodeIfPresent(String.self, forKey: .accountID)
    }
}

public extension APIClient {
    /// `GET /api/v1/me`, returning the profile and the account id in one
    /// request.
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
        let account = try decoder.decode(MeAccount.self, from: data).accountID
        return AuthenticatedIdentity(
            user: user,
            accountID: (account?.isEmpty ?? true) ? nil : account
        )
    }
}

public extension WorkPayload {
    /// Milliseconds since the epoch, the way `Date.now()` gives them to
    /// `malkuth.js`.
    ///
    /// Stamped when a work is signed, which is at send time: the server accepts
    /// a work signed within the last hour. A retry of the same signed bytes
    /// keeps its stamp (the server answers a re-delivered CID with the work it
    /// already created); see ``WorkSubmission``.
    static func nowMS() -> Int64 {
        Int64((Date().timeIntervalSince1970 * 1000).rounded())
    }
}
