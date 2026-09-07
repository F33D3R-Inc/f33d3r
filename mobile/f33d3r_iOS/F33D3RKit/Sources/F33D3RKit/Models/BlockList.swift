import Foundation

/// Everyone this account has silenced, in the two ways it can be done.
///
/// Mirrors `GET /api/v1/blocks`. Blocking severs the relationship in both
/// directions; muting only hides what they write. They are listed together
/// because the reader thinks of them together — "people I don't want to hear
/// from" — and separated inside because undoing one is not undoing the other.
public struct BlockList: Codable, Hashable, Sendable {
    public let blocked: [User]
    public let muted: [User]

    public init(blocked: [User] = [], muted: [User] = []) {
        self.blocked = blocked
        self.muted = muted
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        blocked = try c.decodeIfPresent([User].self, forKey: .blocked) ?? []
        muted = try c.decodeIfPresent([User].self, forKey: .muted) ?? []
    }

    enum CodingKeys: String, CodingKey {
        case blocked, muted
    }

    public var isEmpty: Bool { blocked.isEmpty && muted.isEmpty }
}
