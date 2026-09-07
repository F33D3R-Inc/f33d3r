import Foundation
import Observation

/// A paginating list of works, and the state a screen needs to draw around it.
///
/// One type serves the home feed, a profile tab and a reply list, because they
/// differ only in which page they ask for. The difference is the `load` closure
/// handed in at construction; everything about cursors, in-flight requests,
/// duplicate suppression and error state is the same and lives here rather than
/// being written three times in three views.
///
/// `@MainActor` because it drives SwiftUI directly. The network work happens in
/// `APIClient`, which is an actor of its own — this only awaits it.
@MainActor
@Observable
public final class WorkFeed {

    /// What the screen shows. A list that has loaded once and come back empty is
    /// a different thing from one that has never loaded, and a different thing
    /// again from one that failed — each gets its own surface, so they are
    /// distinct cases rather than a bag of booleans.
    public enum Phase: Equatable, Sendable {
        case idle
        case loadingFirstPage
        case loaded
        case empty
        case failed(APIError)
    }

    public private(set) var works: [Work] = []
    public private(set) var phase: Phase = .idle
    /// True while a *later* page is in flight. Separate from `phase` because the
    /// list stays on screen during it — the spinner belongs at the bottom, not
    /// in place of the content.
    public private(set) var isLoadingMore = false
    public private(set) var hasMore = false
    /// Set when appending a page fails. The list is still valid, so this drives a
    /// retry footer rather than replacing the screen.
    public private(set) var loadMoreError: APIError?

    /// How many rooms are live, as of the last page that mentioned it.
    ///
    /// Only overwritten when a page actually carries the number, so a later page
    /// from a server that omits it does not blank a badge an earlier one filled.
    public private(set) var liveCount: Int?

    private var cursor: String?
    private let load: @Sendable (String?) async throws -> WorkPage

    /// Ids already in `works`. The ranked feed can hand back a row that has
    /// shifted across a page boundary, and a duplicate id would break a
    /// SwiftUI `ForEach` rather than merely look wrong.
    private var seenIDs: Set<String> = []

    /// - Parameter load: fetches one page. Receives the cursor for the page
    ///   wanted, or nil for the first.
    public init(load: @escaping @Sendable (String?) async throws -> WorkPage) {
        self.load = load
    }

    /// Loads the first page unless it is already loaded or in flight.
    ///
    /// Safe to call from `.task`, which re-runs whenever the view's identity
    /// changes — a tab switch should not refetch a feed the user already has.
    public func loadFirstPageIfNeeded() async {
        guard phase == .idle else { return }
        await reload()
    }

    /// Discards everything and loads the first page again. Backs pull-to-refresh
    /// and the retry button on the error state.
    public func reload() async {
        if works.isEmpty { phase = .loadingFirstPage }
        loadMoreError = nil

        do {
            let page = try await load(nil)
            works = page.works
            seenIDs = Set(page.works.map(\.id))
            cursor = page.nextCursor
            hasMore = page.hasMore
            if let count = page.liveCount { liveCount = count }
            phase = page.works.isEmpty ? .empty : .loaded
        } catch let error as APIError {
            // A refresh that fails on a list the user can already see should not
            // snatch it away; keep the stale page and surface the failure at the
            // bottom instead.
            if works.isEmpty {
                phase = .failed(error)
            } else {
                loadMoreError = error
            }
        } catch {
            if works.isEmpty { phase = .failed(.transport(error.localizedDescription)) }
        }
    }

    /// Appends the next page. Called when the list is scrolled near its end.
    public func loadMore() async {
        guard hasMore, !isLoadingMore, let cursor else { return }
        isLoadingMore = true
        loadMoreError = nil
        defer { isLoadingMore = false }

        do {
            let page = try await load(cursor)
            let fresh = page.works.filter { seenIDs.insert($0.id).inserted }
            works.append(contentsOf: fresh)
            self.cursor = page.nextCursor
            hasMore = page.hasMore
            if let count = page.liveCount { liveCount = count }
            if phase == .empty, !works.isEmpty { phase = .loaded }
        } catch let error as APIError {
            loadMoreError = error
        } catch {
            loadMoreError = .transport(error.localizedDescription)
        }
    }

    /// Whether reaching `work` should trigger the next page.
    ///
    /// Prefetching from the fourth-from-last row rather than the last means the
    /// page is usually already in place by the time the user gets there, which
    /// is what the web's 200px/400px scroll trigger buys on the other side.
    public func shouldLoadMore(after work: Work) -> Bool {
        guard hasMore, !isLoadingMore, loadMoreError == nil else { return false }
        guard let index = works.firstIndex(where: { $0.id == work.id }) else { return false }
        return index >= works.count - 4
    }

    /// Replaces one row in place, for a state change that affects a single work
    /// without disturbing scroll position.
    public func replace(_ work: Work) {
        guard let index = works.firstIndex(where: { $0.id == work.id }) else { return }
        works[index] = work
    }

    /// Places a work the server has just confirmed — the reader's own new
    /// post, fetched back by id — at the top of a loaded list, so it is there
    /// when the compose sheet closes rather than after the next refresh.
    ///
    /// Only into a list that has loaded: an idle feed will fetch the row itself
    /// and a placed copy would then appear twice.
    public func insert(_ work: Work, at index: Int = 0) {
        guard phase == .loaded || phase == .empty else { return }
        guard seenIDs.insert(work.id).inserted else {
            replace(work)
            return
        }
        works.insert(work, at: min(index, works.count))
        phase = .loaded
    }

    /// Drops a work that no longer exists — a delete, or an SSE `post_deleted`.
    public func remove(id: String) {
        works.removeAll { $0.id == id }
        seenIDs.remove(id)
        if works.isEmpty, phase == .loaded { phase = .empty }
    }
}

// MARK: - Constructors

public extension WorkFeed {
    /// The home feed on one lane.
    ///
    /// A lane the deployment does not serve yet comes back as an empty page
    /// rather than an error. The app ships all five lanes; the surfaces behind
    /// them arrive one at a time, and a reader who swipes onto one that has not
    /// landed should be told what that lane is for, not shown a failure they
    /// cannot act on. Only 404 and 501 are treated this way — a real fault still
    /// reaches the error state.
    static func home(client: APIClient, surface: FeedSurface) -> WorkFeed {
        WorkFeed { cursor in
            do {
                return try await client.feed(surface: surface, cursor: cursor)
            } catch let error as APIError where error.isUnservedSurface {
                return WorkPage(works: [])
            }
        }
    }

    /// One tab of a profile.
    ///
    /// A likes tab the viewer is not entitled to comes back empty rather than
    /// failing: the tab is already hidden on other people's profiles, and a
    /// deep link that reaches it anyway should meet the rule stated calmly, not
    /// a red error with a retry button that can never succeed.
    static func profile(client: APIClient, handle: String, tab: Profile.Tab) -> WorkFeed {
        WorkFeed { cursor in
            do {
                return try await client.profileWorks(handle: handle, tab: tab, cursor: cursor)
            } catch let error as APIError where error.isPrivateLikes || error.code == "saves_private" {
                return WorkPage(works: [])
            }
        }
    }

    /// A work's replies.
    /// A tag's timeline.
    static func tag(client: APIClient, tag: String) -> WorkFeed {
        WorkFeed { cursor in
            try await client.tagFeed(tag, cursor: cursor)
        }
    }

    static func replies(client: APIClient, workID: String) -> WorkFeed {
        WorkFeed { cursor in
            try await client.replies(workID: workID, cursor: cursor)
        }
    }
}
