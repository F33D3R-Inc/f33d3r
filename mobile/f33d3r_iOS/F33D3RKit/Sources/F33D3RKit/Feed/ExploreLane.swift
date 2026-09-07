import Foundation

/// The two discovery lanes behind the Explore tab.
///
/// A type of its own rather than two more cases on `FeedSurface`, because the
/// two enums answer different questions. `FeedSurface` is *the lanes the home
/// pager draws*, and its order and membership are a reading decision the strip
/// depends on; this is *the surfaces Explore asks the server for*. `trending`
/// has no home lane and `following` has no place in Explore, so folding them
/// into one enum would give both screens a case they must remember to skip.
///
/// The raw values are the server's own surface ids — `foryou` and `trending` in
/// `feedLanes` — and are sent verbatim as `?surface=`. Nantar answers 400
/// `unknown_surface` for anything else, so a typo here is visible at the first
/// request rather than silently serving the wrong rows.
public enum ExploreLane: String, CaseIterable, Sendable, Hashable, Identifiable {
    case forYou = "foryou"
    case trending

    public static let `default`: ExploreLane = .forYou

    public var id: String { rawValue }

    /// The lane label. The server's own names for these surfaces, so the app
    /// and the site call the same thing the same thing.
    public var title: String {
        switch self {
        case .forYou: return "For you"
        case .trending: return "Trending"
        }
    }
}

// MARK: - Feed

public extension WorkFeed {
    /// One Explore lane.
    ///
    /// A lane this build knows about and the deployment does not serve yet
    /// degrades to an empty page, the same way `home` does, so a reader meets
    /// the lane's own empty state instead of a failure they cannot act on. A
    /// real fault still reaches the error state.
    static func explore(client: APIClient, lane: ExploreLane) -> WorkFeed {
        WorkFeed { cursor in
            do {
                return try await client.exploreFeed(lane: lane, cursor: cursor)
            } catch let error as APIError where error.isUnservedSurface {
                return WorkPage(works: [])
            }
        }
    }

    /// The Music surface.
    ///
    /// Not `home(surface: .music)`, for one reason: Music is the only lane whose
    /// definition lives in a database row (`feed_surfaces`) rather than in the
    /// handler, and a deployment whose row is missing or inactive answers 503
    /// `surface_unavailable`. That is the same class of fact as an unserved
    /// surface — this deployment does not have Music — and it belongs in the
    /// empty state with the rest of the lane's explanation, not behind a red
    /// error with a retry button that will keep failing. Every other 503 still
    /// reaches the error state, because a surface that exists and is briefly
    /// down is a different thing and the reader should retry it.
    static func music(client: APIClient) -> WorkFeed {
        WorkFeed { cursor in
            do {
                return try await client.feed(surface: .music, cursor: cursor)
            } catch let error as APIError where error.isUnservedSurface || error.code == "surface_unavailable" {
                return WorkPage(works: [])
            }
        }
    }
}

// MARK: - Client

public extension APIClient {
    /// A page of one Explore lane. Same `/feed` endpoint the home lanes use —
    /// Explore is a different set of surfaces, not a different API.
    func exploreFeed(lane: ExploreLane, cursor: String? = nil, limit: Int = 20) async throws -> WorkPage {
        var query = [
            URLQueryItem(name: "surface", value: lane.rawValue),
            URLQueryItem(name: "limit", value: String(limit)),
        ]
        if let cursor { query.append(URLQueryItem(name: "cursor", value: cursor)) }
        return try await send(
            Endpoint(path: "feed", query: query),
            body: Optional<Never>.none,
            as: WorkPage.self
        )
    }
}

#if DEBUG
public extension SampleData {
    /// One page of an Explore lane, for looking at the screen without a server.
    ///
    /// The split is the app's guess at what the two surfaces mean, not the
    /// server's rule — it owns that — and exists only so the two lanes are
    /// visibly different things while there is no server to ask.
    static func page(explore lane: ExploreLane, from offset: Int = 0) -> WorkPage {
        switch lane {
        case .forYou:
            return page(lane: .forYou, from: offset)
        case .trending:
            // Trending is global discovery, so nothing from the follow graph is
            // guaranteed: the samples with a score band stand in for "the server
            // had an opinion about this one".
            let banded = works.filter { $0.scoreBand != nil }
            return WorkPage(works: banded.isEmpty ? works : banded)
        }
    }
}
#endif
