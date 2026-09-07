import Foundation

/// One team in a game, as the scoreboard shows them.
public struct SportsTeam: Codable, Hashable, Sendable {
    public let abbr: String
    public let name: String
    public let shortName: String
    public let logoURL: String?
    public let record: String?
    public let score: Int
    /// Possession. Only meaningful while a game is being played.
    public let hasBall: Bool

    enum CodingKeys: String, CodingKey {
        case abbr, name, record, score
        case shortName = "short_name"
        case logoURL = "logo_url"
        case hasBall = "has_ball"
    }
}

/// One game.
///
/// Every label it prints was written by the server: the status, the period, the
/// start time in the league's own zone. That is deliberate and it is the same
/// rule the web card follows — what a game is doing is the platform's statement,
/// not something a phone should be forming a second opinion about from a clock
/// and a period number.
public struct SportsGame: Codable, Hashable, Sendable, Identifiable {
    public let id: String
    public let league: String
    public let leagueLabel: String

    /// scheduled | in_progress | halftime | end_period | delayed | postponed |
    /// canceled | final | unknown
    public let state: String
    /// "Final", "Final/OT", "3rd Quarter", "Halftime".
    public let statusLabel: String
    /// The kickoff, in the league's zone. Empty once there is a score to show.
    public let startLabel: String?
    public let start: Date
    public let clock: String?
    public let periodLabel: String?

    public let home: SportsTeam
    public let away: SportsTeam
    /// False before a game starts. A scheduled game showing 0–0 is a lie:
    /// nobody has failed to score yet, so the card shows the time instead.
    public let hasScore: Bool
    public let isLive: Bool
    public let isFinal: Bool

    /// Football only. Absent for a basketball game rather than blank.
    public let downDistance: String?
    public let possessionText: String?
    public let redZone: Bool
    public let lastPlay: String?

    public let broadcast: String?
    public let venue: String?

    enum CodingKeys: String, CodingKey {
        case id, league, state, clock, home, away, start, broadcast, venue
        case leagueLabel = "league_label"
        case statusLabel = "status_label"
        case startLabel = "start_label"
        case periodLabel = "period_label"
        case hasScore = "has_score"
        case isLive = "is_live"
        case isFinal = "is_final"
        case downDistance = "down_distance"
        case possessionText = "possession_text"
        case redZone = "red_zone"
        case lastPlay = "last_play"
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        league = try c.decodeIfPresent(String.self, forKey: .league) ?? ""
        leagueLabel = try c.decodeIfPresent(String.self, forKey: .leagueLabel) ?? ""
        state = try c.decodeIfPresent(String.self, forKey: .state) ?? "unknown"
        statusLabel = try c.decodeIfPresent(String.self, forKey: .statusLabel) ?? ""
        startLabel = try c.decodeIfPresent(String.self, forKey: .startLabel)
        start = try c.decodeIfPresent(Date.self, forKey: .start) ?? Date()
        clock = try c.decodeIfPresent(String.self, forKey: .clock)
        periodLabel = try c.decodeIfPresent(String.self, forKey: .periodLabel)
        home = try c.decode(SportsTeam.self, forKey: .home)
        away = try c.decode(SportsTeam.self, forKey: .away)
        hasScore = try c.decodeIfPresent(Bool.self, forKey: .hasScore) ?? false
        isLive = try c.decodeIfPresent(Bool.self, forKey: .isLive) ?? false
        isFinal = try c.decodeIfPresent(Bool.self, forKey: .isFinal) ?? false
        downDistance = try c.decodeIfPresent(String.self, forKey: .downDistance)
        possessionText = try c.decodeIfPresent(String.self, forKey: .possessionText)
        redZone = try c.decodeIfPresent(Bool.self, forKey: .redZone) ?? false
        lastPlay = try c.decodeIfPresent(String.self, forKey: .lastPlay)
        broadcast = try c.decodeIfPresent(String.self, forKey: .broadcast)
        venue = try c.decodeIfPresent(String.self, forKey: .venue)
    }

    /// What the card prints under the teams: the clock while it is running,
    /// the status otherwise.
    public var detailLine: String {
        if isLive, let clock, !clock.isEmpty {
            return [periodLabel, clock].compactMap { $0 }.filter { !$0.isEmpty }.joined(separator: " · ")
        }
        if !hasScore, let startLabel, !startLabel.isEmpty { return startLabel }
        return statusLabel
    }

    /// The side that is ahead, for the weight the card gives a leader. Nil
    /// while the game is level or has not started.
    public var leader: String? {
        guard hasScore, home.score != away.score else { return nil }
        return home.score > away.score ? home.abbr : away.abbr
    }
}

/// One league's slate.
public struct SportsLeague: Codable, Hashable, Sendable, Identifiable {
    public var id: String { slug }
    public let slug: String
    public let label: String
    public let games: [SportsGame]
    /// The upstream has not answered recently, so a score may be behind. Said
    /// rather than hidden: a frozen score presented as current is worse than
    /// one that admits its age.
    public let stale: Bool
}

/// `GET /api/v1/sports`.
public struct SportsBoard: Codable, Hashable, Sendable {
    public let leagues: [SportsLeague]
    public let count: Int

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        leagues = try c.decodeIfPresent([SportsLeague].self, forKey: .leagues) ?? []
        count = try c.decodeIfPresent(Int.self, forKey: .count) ?? leagues.reduce(0) { $0 + $1.games.count }
    }

    public var isEmpty: Bool { leagues.allSatisfy(\.games.isEmpty) }
    /// Games being played right now, across every league.
    public var live: [SportsGame] { leagues.flatMap(\.games).filter(\.isLive) }
}
