import Foundation
import Observation

/// The conversation list.
///
/// Short by nature — a person has tens of conversations, not thousands — so
/// this loads the lot rather than paging, and the server orders them by last
/// activity. What it does carry is the unread total, because the tab badge and
/// the list must never disagree about how much is waiting.
@MainActor
@Observable
public final class ConversationsFeed {

    public enum Phase: Equatable, Sendable {
        case idle
        case loading
        case loaded
        case empty
        case failed(APIError)
    }

    public private(set) var conversations: [Conversation] = []
    public private(set) var phase: Phase = .idle
    /// Nil until a list has arrived, so a badge can tell "none" from "not asked".
    public private(set) var unreadTotal: Int?

    private let client: APIClient

    public init(client: APIClient) {
        self.client = client
    }

    public func loadIfNeeded() async {
        guard phase == .idle else { return }
        await reload()
    }

    public func reload() async {
        if conversations.isEmpty { phase = .loading }
        do {
            let page = try await client.conversations()
            conversations = page.conversations
            unreadTotal = page.unreadTotal
            phase = page.conversations.isEmpty ? .empty : .loaded
        } catch let error as APIError {
            // A failed refresh does not take an existing list away: the rows on
            // screen were true a moment ago and are still the best answer.
            if conversations.isEmpty { phase = .failed(error) }
        } catch {
            if conversations.isEmpty { phase = .failed(.transport(error.localizedDescription)) }
        }
    }

    /// Clears one conversation's unread count locally.
    ///
    /// Called when its thread is opened, because the thread is what tells the
    /// server. Doing it here as well keeps the list from showing a badge for
    /// something the reader is looking at, without waiting for a refetch.
    public func markReadLocally(_ id: String) {
        guard let index = conversations.firstIndex(where: { $0.id == id }),
              conversations[index].unread > 0 else { return }
        let was = conversations[index].unread
        conversations[index] = conversations[index].withUnread(0)
        if let total = unreadTotal { unreadTotal = max(0, total - was) }
    }

    /// Moves a conversation to the top with a new preview, the way an arriving
    /// message does. Used by the live stream so a push does not need a refetch
    /// to be visible.
    public func noteActivity(conversationID: String, preview: String?, at date: Date, isMine: Bool) {
        guard let index = conversations.firstIndex(where: { $0.id == conversationID }) else {
            // A conversation nobody has seen before: only a full reload can name
            // it, so ask for one rather than inventing a row.
            Task { await reload() }
            return
        }
        var row = conversations[index]
        row = row.withActivity(preview: preview ?? row.preview, at: date,
                               unread: isMine ? row.unread : row.unread + 1)
        conversations.remove(at: index)
        conversations.insert(row, at: 0)
        if !isMine, let total = unreadTotal { unreadTotal = total + 1 }
    }
}

private extension Conversation {
    func withUnread(_ count: Int) -> Conversation {
        Conversation(
            id: id, mode: mode, isGroup: isGroup, title: title,
            otherHandle: otherHandle, otherDisplay: otherDisplay, otherAvatar: otherAvatar,
            preview: preview, lastAt: lastAt, unread: count
        )
    }

    func withActivity(preview: String, at date: Date, unread: Int) -> Conversation {
        Conversation(
            id: id, mode: mode, isGroup: isGroup, title: title,
            otherHandle: otherHandle, otherDisplay: otherDisplay, otherAvatar: otherAvatar,
            preview: preview, lastAt: date, unread: unread
        )
    }
}
