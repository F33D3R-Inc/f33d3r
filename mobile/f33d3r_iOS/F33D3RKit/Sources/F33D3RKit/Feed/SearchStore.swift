import Foundation
import Observation

/// Search: the query, the answer, and the recent queries kept on the device.
///
/// Recents are a reading preference like the lane, so they live in
/// `UserDefaults`, not on the server. Results are always the server's.
@MainActor
@Observable
public final class SearchStore {

    public enum Phase: Equatable, Sendable {
        case idle
        case searching
        case results
        case empty
        case failed(APIError)
    }

    public var query = ""
    public private(set) var results = SearchResults()
    public private(set) var phase: Phase = .idle
    public private(set) var recents: [String]
    public private(set) var trending: [TagCount] = []

    private let client: APIClient
    private let defaults: UserDefaults
    private static let recentsKey = "f33d3r.search.recents"
    private static let maxRecents = 8

    public init(client: APIClient, defaults: UserDefaults = .standard) {
        self.client = client
        self.defaults = defaults
        recents = defaults.stringArray(forKey: Self.recentsKey) ?? []
    }

    /// Runs the current query. Debouncing is the caller's: the view runs this
    /// in a `.task(id: query)` so a keystroke cancels the request in flight.
    public func search() async {
        let q = query.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !q.isEmpty else {
            results = SearchResults()
            phase = .idle
            return
        }
        phase = .searching
        try? await Task.sleep(for: .milliseconds(220))
        if Task.isCancelled { return }
        do {
            let found = try await client.search(q)
            if Task.isCancelled { return }
            results = found
            phase = found.isEmpty ? .empty : .results
        } catch is CancellationError {
        } catch let error as APIError {
            phase = .failed(error)
        } catch {
            phase = .failed(.transport(error.localizedDescription))
        }
    }

    /// Records a query the reader committed to (submitted, or tapped a result).
    public func remember(_ q: String) {
        let trimmed = q.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return }
        recents.removeAll { $0.caseInsensitiveCompare(trimmed) == .orderedSame }
        recents.insert(trimmed, at: 0)
        if recents.count > Self.maxRecents { recents.removeLast(recents.count - Self.maxRecents) }
        defaults.set(recents, forKey: Self.recentsKey)
    }

    public func forget(_ q: String) {
        recents.removeAll { $0 == q }
        defaults.set(recents, forKey: Self.recentsKey)
    }

    public func clearRecents() {
        recents.removeAll()
        defaults.removeObject(forKey: Self.recentsKey)
    }

    public func loadTrending() async {
        trending = (try? await client.trendingTags()) ?? []
    }
}
