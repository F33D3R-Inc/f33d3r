import Foundation

/// A poll's ballot state, computed server-side.
///
/// Mirrors `PollDTO`, which projects `model.Poll`. Tallies, percentages, the
/// winner and the closed/time-left labels all arrive precomputed: the server
/// owns poll state, and a client that re-derived it would drift the moment a
/// tie-break or rounding rule changed. The app draws what it is given.
public struct Poll: Codable, Hashable, Sendable {
    public let results: [PollResult]
    public let totalVotes: Int
    /// The option index the viewer chose, or nil if they have not voted.
    public let viewerVote: Int?
    public let endsAt: Date?
    /// True when the ballot no longer accepts votes. Read this rather than
    /// comparing `endsAt` to now — the server decides when a poll is shut.
    public let closed: Bool
    /// Pre-formatted: "3 hours left", "2 days left", "Final results".
    public let timeLeft: String

    enum CodingKeys: String, CodingKey {
        case results, closed
        case totalVotes = "total_votes"
        case viewerVote = "viewer_vote"
        case endsAt = "ends_at"
        case timeLeft = "time_left"
    }

    public init(
        results: [PollResult],
        totalVotes: Int,
        viewerVote: Int? = nil,
        endsAt: Date? = nil,
        closed: Bool = false,
        timeLeft: String = ""
    ) {
        self.results = results
        self.totalVotes = totalVotes
        self.viewerVote = viewerVote
        self.endsAt = endsAt
        self.closed = closed
        self.timeLeft = timeLeft
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        results = try c.decodeIfPresent([PollResult].self, forKey: .results) ?? []
        totalVotes = try c.decodeIfPresent(Int.self, forKey: .totalVotes) ?? 0
        // The Go side sends -1 for "has not voted", which is a sentinel the
        // Swift side should not carry any further than this line.
        let raw = try c.decodeIfPresent(Int.self, forKey: .viewerVote) ?? -1
        viewerVote = raw >= 0 ? raw : nil
        endsAt = try c.decodeIfPresent(Date.self, forKey: .endsAt)
        closed = try c.decodeIfPresent(Bool.self, forKey: .closed) ?? false
        timeLeft = try c.decodeIfPresent(String.self, forKey: .timeLeft) ?? ""
    }

    /// Results are only revealed once the viewer has voted or the poll has shut,
    /// matching the web ballot — showing the tally first would bias the vote.
    public var showsResults: Bool { closed || viewerVote != nil }

    public var hasVoted: Bool { viewerVote != nil }
}

/// One option with its precomputed display values.
public struct PollResult: Codable, Hashable, Sendable, Identifiable {
    /// Position in the ballot. Stable for the life of the poll, and what a vote
    /// is cast against.
    public let index: Int
    public let label: String
    public let votes: Int
    /// 0–100, rounded server-side.
    public let pct: Int
    /// True when this is the viewer's own choice.
    public let voted: Bool
    /// True for the leading option — and for every option in a tie.
    public let isWinner: Bool

    public var id: Int { index }

    enum CodingKeys: String, CodingKey {
        case index, label, votes, pct, voted
        case isWinner = "is_winner"
    }

    public init(index: Int, label: String, votes: Int, pct: Int, voted: Bool = false, isWinner: Bool = false) {
        self.index = index
        self.label = label
        self.votes = votes
        self.pct = pct
        self.voted = voted
        self.isWinner = isWinner
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        index = try c.decodeIfPresent(Int.self, forKey: .index) ?? 0
        label = try c.decodeIfPresent(String.self, forKey: .label) ?? ""
        votes = try c.decodeIfPresent(Int.self, forKey: .votes) ?? 0
        pct = try c.decodeIfPresent(Int.self, forKey: .pct) ?? 0
        voted = try c.decodeIfPresent(Bool.self, forKey: .voted) ?? false
        isWinner = try c.decodeIfPresent(Bool.self, forKey: .isWinner) ?? false
    }
}
