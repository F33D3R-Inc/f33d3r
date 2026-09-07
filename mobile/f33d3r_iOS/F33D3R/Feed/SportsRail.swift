import SwiftUI
import F33D3RKit
import Observation

/// The scoreboard above the Sports lane.
///
/// Same games the web's strip and rail draw, from the same cache, asked for as
/// data instead of as rendered Facets. Every label on a card was written by the
/// server — the status, the period, the kickoff time in the league's own zone —
/// because what a game is doing is the platform's statement and a phone forming
/// its own opinion from a clock and a period number would be a second one.
@MainActor
@Observable
final class SportsBoardStore {
    private(set) var board: SportsBoard?
    private(set) var problem: String?
    private(set) var isLoading = false
    /// True when this deployment has no scores provider at all, which is a
    /// different thing from a day with no games.
    private(set) var unavailable = false

    private let client: APIClient
    private var ticker: Task<Void, Never>?

    init(client: APIClient) { self.client = client }

    func load() async {
        guard !isLoading else { return }
        isLoading = true
        defer { isLoading = false }
        do {
            board = try await client.sports()
            problem = nil
            unavailable = false
        } catch let error as APIError where error.code == "sports_unavailable" {
            unavailable = true
        } catch {
            // A board already on screen is kept: a failed refresh is not a
            // reason to take the scores away.
            if board == nil { problem = (error as? APIError)?.userMessage ?? "Scores could not be loaded." }
        }
    }

    /// Refreshes while a game is being played. The web pushes score changes as
    /// Facets down a stream the app has no reader for, so this asks — slowly,
    /// and only while there is something to ask about.
    func startRefreshing() {
        guard ticker == nil else { return }
        ticker = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(30))
                guard let self, !Task.isCancelled else { return }
                guard self.board?.live.isEmpty == false else { continue }
                await self.load()
            }
        }
    }

    func stopRefreshing() {
        ticker?.cancel()
        ticker = nil
    }
}

/// The rail: one horizontal run of game cards per league.
struct SportsRail: View {
    let board: SportsBoard

    var body: some View {
        VStack(alignment: .leading, spacing: F33Spacing.md) {
            ForEach(board.leagues) { league in
                if !league.games.isEmpty {
                    VStack(alignment: .leading, spacing: F33Spacing.xs) {
                        HStack(spacing: 6) {
                            Text(league.label)
                                .font(.system(size: 13, weight: .semibold))
                                .foregroundStyle(F33Color.ink3)
                            if league.stale {
                                // The card is not pretending this is current.
                                Text("may be out of date")
                                    .font(.system(size: 11))
                                    .foregroundStyle(F33Color.ink4)
                            }
                        }
                        .padding(.horizontal, F33Card.paddingHorizontal)

                        ScrollView(.horizontal, showsIndicators: false) {
                            HStack(spacing: F33Spacing.sm) {
                                ForEach(league.games) { game in
                                    SportsGameCard(game: game)
                                }
                            }
                            .padding(.horizontal, F33Card.paddingHorizontal)
                        }
                        // Margins are inherited by nested scroll views; this
                        // row has none of the lane's own. It clips at its own
                        // bounds — the default, and it has to stay the default:
                        // a run of cards drawn outside its frame lands on
                        // whatever the lane floats above it, which is how the
                        // scoreboard ended up smeared sideways under the strip.
                        .contentMargins(.top, 0, for: .scrollContent)
                    }
                }
            }
        }
        .padding(.vertical, F33Spacing.sm)
        .overlay(alignment: .bottom) { CardDivider() }
    }
}

/// One game.
struct SportsGameCard: View {
    let game: SportsGame

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 6) {
                if game.isLive {
                    Circle()
                        .fill(F33Color.danger)
                        .frame(width: 6, height: 6)
                }
                Text(game.detailLine)
                    .font(.system(size: 11, weight: game.isLive ? .semibold : .regular))
                    .foregroundStyle(game.isLive ? F33Color.danger : F33Color.ink4)
                    .lineLimit(1)
                Spacer(minLength: 0)
                if let broadcast = game.broadcast, !broadcast.isEmpty, !game.isFinal {
                    Text(broadcast)
                        .font(.system(size: 10, weight: .medium))
                        .foregroundStyle(F33Color.ink4)
                }
            }

            side(game.away)
            side(game.home)

            if let situation = game.downDistance, !situation.isEmpty {
                Text([situation, game.possessionText].compactMap { $0 }.joined(separator: " · "))
                    .font(.system(size: 10))
                    .foregroundStyle(game.redZone ? F33Color.danger : F33Color.ink4)
                    .lineLimit(1)
            }
        }
        .padding(10)
        .frame(width: 176, alignment: .leading)
        .background(F33Color.bgElevated, in: RoundedRectangle(cornerRadius: 14, style: .continuous))
        .overlay(
            RoundedRectangle(cornerRadius: 14, style: .continuous)
                .stroke(F33Color.hairline, lineWidth: 1)
        )
        .accessibilityElement(children: .combine)
        .accessibilityLabel(spoken)
    }

    private func side(_ team: SportsTeam) -> some View {
        // The leader carries the weight, so a glance finds the score before it
        // finds the names.
        let leads = game.leader == team.abbr
        return HStack(spacing: 8) {
            RemoteImage(path: team.logoURL, seed: team.abbr)
                .frame(width: 18, height: 18)
                .clipShape(Circle())

            Text(team.abbr)
                .font(.system(size: 14, weight: leads ? .bold : .medium))
                .foregroundStyle(leads ? F33Color.ink : F33Color.ink2)

            if team.hasBall {
                Image(systemName: "football.fill")
                    .font(.system(size: 8))
                    .foregroundStyle(F33Color.ink4)
            }

            Spacer(minLength: 0)

            if game.hasScore {
                Text("\(team.score)")
                    .font(.system(size: 15, weight: leads ? .bold : .medium).monospacedDigit())
                    .foregroundStyle(leads ? F33Color.ink : F33Color.ink2)
            } else if let record = team.record, !record.isEmpty {
                Text(record)
                    .font(.system(size: 11).monospacedDigit())
                    .foregroundStyle(F33Color.ink4)
            }
        }
    }

    private var spoken: String {
        guard game.hasScore else {
            return "\(game.away.name) at \(game.home.name), \(game.detailLine)"
        }
        return "\(game.away.name) \(game.away.score), \(game.home.name) \(game.home.score), \(game.detailLine)"
    }
}
