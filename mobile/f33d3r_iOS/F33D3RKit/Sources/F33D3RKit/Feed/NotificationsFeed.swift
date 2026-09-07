import Foundation
import Observation

/// The reader's notifications, paginated, with the unread count the server
/// sent alongside.
///
/// The same shape as `WorkFeed` for the same reasons: one store per screen,
/// held on the model so a tab switch does not refetch, phases rather than a
/// bag of booleans, and every state change here is a server answer — a row is
/// marked read *after* the server has said so.
@MainActor
@Observable
public final class NotificationsFeed {

    public enum Phase: Equatable, Sendable {
        case idle
        case loading
        case loaded
        case empty
        case failed(APIError)
    }

    public private(set) var notifications: [NotificationItem] = []
    public private(set) var phase: Phase = .idle
    public private(set) var isLoadingMore = false
    public private(set) var hasMore = false
    public private(set) var loadMoreError: APIError?
    /// The badge number, as of the last page. Nil until a page has arrived.
    public private(set) var unreadCount: Int?

    private var cursor: String?
    private let client: APIClient

    public init(client: APIClient) {
        self.client = client
    }

    public func loadFirstPageIfNeeded() async {
        guard phase == .idle else { return }
        await reload()
    }

    public func reload() async {
        if notifications.isEmpty { phase = .loading }
        loadMoreError = nil
        do {
            let page = try await client.notifications()
            notifications = page.notifications
            cursor = page.nextCursor
            hasMore = page.hasMore
            unreadCount = page.unreadCount
            phase = page.notifications.isEmpty ? .empty : .loaded
        } catch let error as APIError {
            if notifications.isEmpty { phase = .failed(error) } else { loadMoreError = error }
        } catch {
            if notifications.isEmpty { phase = .failed(.transport(error.localizedDescription)) }
        }
    }

    public func loadMore() async {
        guard hasMore, !isLoadingMore, let cursor else { return }
        isLoadingMore = true
        loadMoreError = nil
        defer { isLoadingMore = false }
        do {
            let page = try await client.notifications(cursor: cursor)
            let known = Set(notifications.map(\.id))
            notifications.append(contentsOf: page.notifications.filter { !known.contains($0.id) })
            self.cursor = page.nextCursor
            hasMore = page.hasMore
            unreadCount = page.unreadCount
        } catch let error as APIError {
            loadMoreError = error
        } catch {
            loadMoreError = .transport(error.localizedDescription)
        }
    }

    public func shouldLoadMore(after notification: NotificationItem) -> Bool {
        guard hasMore, !isLoadingMore, loadMoreError == nil else { return false }
        guard let index = notifications.firstIndex(where: { $0.id == notification.id }) else { return false }
        return index >= notifications.count - 4
    }

    /// Tells the server the row was read, then mirrors the answer. A failure
    /// leaves the row unread, because it is.
    public func markRead(_ notification: NotificationItem) async {
        guard !notification.isRead else { return }
        do {
            try await client.markNotificationRead(id: notification.id)
        } catch {
            return
        }
        if let index = notifications.firstIndex(where: { $0.id == notification.id }) {
            notifications[index] = notification.markedRead()
        }
        if let unread = unreadCount {
            unreadCount = max(0, unread - 1)
        }
    }

    public func markAllRead() async {
        do {
            try await client.markAllNotificationsRead()
        } catch {
            return
        }
        notifications = notifications.map { $0.isRead ? $0 : $0.markedRead() }
        unreadCount = 0
    }
}
