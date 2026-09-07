import Foundation
import Observation

/// What to offer somebody half way through an `@`, `#` or `$` in the composer.
///
/// The composer hands over the body and where the caret is; this decides
/// whether that is a token worth searching for, waits long enough that a fast
/// typist spends one request rather than eight, and holds the answer. Nothing
/// about how the list looks is in here.
///
/// Mentions and hashtags come out of the app's own `/search`, which already
/// answers people and tags together — a `#` query throws the people away and an
/// `@` query throws the tags away rather than asking for a second endpoint that
/// would return the same rows. Tickers come from `/cashtag/search`, which is
/// allowed to say it has no provider; that is silence, not an error, so a
/// failure here never surfaces anything for the reader to fix.
@MainActor
@Observable
public final class ComposeSuggestions {

    /// What is being offered. Never empty — nothing to offer is `nil`.
    public enum Results: Equatable, Sendable {
        case people([User])
        case tags([TagCount])
        case cashtags([CashtagSuggestion])

        public var count: Int {
            switch self {
            case .people(let p): return p.count
            case .tags(let t): return t.count
            case .cashtags(let c): return c.count
            }
        }
    }

    /// The token the caret is in, once it is one worth searching for.
    public private(set) var token: ComposeToken?
    public private(set) var results: Results?

    /// Long enough that a run of keystrokes is one request, short enough that a
    /// reader who stops typing does not notice waiting. The web uses 180–300ms
    /// across its three dropdowns; one number for all three is simpler and sits
    /// in the middle of them.
    static let debounce = Duration.milliseconds(220)

    /// A dropdown taller than this is a dropdown covering what is being
    /// written. The rest of an answer is reached by typing another letter.
    static let maxRows = 6

    private let client: APIClient
    /// Never offered to somebody as a mention of themselves — the web filters
    /// its own handle out too.
    private let ownHandle: String?
    private var inFlight: Task<Void, Never>?
    /// The body as it stood when a completion was chosen.
    private var settled: String?

    public init(client: APIClient, ownHandle: String? = nil) {
        self.client = client
        self.ownHandle = ownHandle?.lowercased()
    }

    /// Reads the caret and starts, keeps, or abandons a search.
    ///
    /// - Parameter caret: a UTF-16 offset, or `nil` when there is a selection
    ///   rather than a caret — completing into a selection would replace text
    ///   the reader deliberately highlighted.
    public func update(body: String, caret: Int?) {
        // The list stays shut over a word the reader has just finished. The
        // caret is still inside the completed token, so without this the
        // dropdown would reopen offering the name it has just inserted.
        if let settled {
            if body == settled { return }
            self.settled = nil
        }
        guard let caret,
              let found = ComposeToken.detect(in: body, caret: caret),
              !found.query.isEmpty
        else {
            clear()
            return
        }
        // The same token as last time is the caret moving inside a word that
        // has already been asked about; the answer on screen is still its
        // answer.
        guard found != token else { return }

        token = found
        inFlight?.cancel()
        inFlight = Task { [weak self] in
            await self?.load(found)
        }
    }

    /// Drops the token and whatever was being offered for it.
    public func clear() {
        inFlight?.cancel()
        inFlight = nil
        token = nil
        results = nil
        settled = nil
    }

    /// A completion has been inserted, leaving the body as `body`. Nothing is
    /// offered again until the reader types.
    public func settle(on body: String) {
        clear()
        settled = body
    }

    private func load(_ token: ComposeToken) async {
        try? await Task.sleep(for: Self.debounce)
        if Task.isCancelled { return }

        let found = await fetch(token)
        // The caret may have moved on while the request was out. Only the
        // answer to the question still being asked is shown.
        guard !Task.isCancelled, self.token == token else { return }
        results = (found?.count ?? 0) > 0 ? found : nil
    }

    private func fetch(_ token: ComposeToken) async -> Results? {
        switch token.trigger {
        case .mention:
            guard let answer = try? await client.search(token.query) else { return nil }
            let people = answer.people
                .filter { ownHandle == nil || $0.handle.lowercased() != ownHandle }
                .prefix(Self.maxRows)
            return .people(Array(people))

        case .hashtag:
            guard let answer = try? await client.search(token.query) else { return nil }
            return .tags(Array(answer.tags.prefix(Self.maxRows)))

        case .cashtag:
            // A deployment with no quotes provider answers 503. That is a
            // "nobody can say", and the dropdown simply does not appear.
            guard let answer = try? await client.cashtagSearch(token.query) else { return nil }
            return .cashtags(Array(answer.prefix(Self.maxRows)))
        }
    }
}
