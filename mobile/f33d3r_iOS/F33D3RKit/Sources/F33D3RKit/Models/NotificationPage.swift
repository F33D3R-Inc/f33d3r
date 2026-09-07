import Foundation

/// One page of `GET /api/v1/notifications`.
///
/// The unread total rides with the page so the badge and the list come from
/// one answer and cannot disagree.
public struct NotificationPage: Codable, Hashable, Sendable {
    public let notifications: [NotificationItem]
    public let nextCursor: String?
    public let unreadCount: Int

    enum CodingKeys: String, CodingKey {
        case notifications
        case nextCursor = "next_cursor"
        case unreadCount = "unread_count"
    }

    public init(notifications: [NotificationItem], nextCursor: String? = nil, unreadCount: Int = 0) {
        self.notifications = notifications
        self.nextCursor = nextCursor
        self.unreadCount = unreadCount
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        notifications = try c.decodeIfPresent([NotificationItem].self, forKey: .notifications) ?? []
        let cursor = try c.decodeIfPresent(String.self, forKey: .nextCursor)
        nextCursor = (cursor?.isEmpty ?? true) ? nil : cursor
        unreadCount = try c.decodeIfPresent(Int.self, forKey: .unreadCount) ?? 0
    }

    public var hasMore: Bool { nextCursor != nil }
}

/// `GET /api/v1/search`.
public struct SearchResults: Codable, Hashable, Sendable {
    public let works: [Work]
    public let people: [User]
    public let tags: [TagCount]

    public init(works: [Work] = [], people: [User] = [], tags: [TagCount] = []) {
        self.works = works
        self.people = people
        self.tags = tags
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        works = try c.decodeIfPresent([Work].self, forKey: .works) ?? []
        people = try c.decodeIfPresent([User].self, forKey: .people) ?? []
        tags = try c.decodeIfPresent([TagCount].self, forKey: .tags) ?? []
    }

    public var isEmpty: Bool { works.isEmpty && people.isEmpty && tags.isEmpty }
}

public extension NotificationItem {
    /// The same notification with `isRead` flipped, for mirroring a server-
    /// confirmed read into a list without refetching the page.
    func markedRead() -> NotificationItem {
        NotificationItem(
            id: id, kind: kind, actors: actors, actorCount: actorCount,
            targetID: targetID, targetType: targetType, preview: preview,
            amountUAET: amountUAET, isRead: true, createdAt: createdAt
        )
    }
}
