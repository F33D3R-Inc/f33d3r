import Foundation
import Testing
@testable import F33D3RKit

/// The preview fixtures are JSON decoded through the production decoder, so they
/// only load if they match the models. That makes them worth asserting on: this
/// suite is what turns "the previews still render" into a checked claim, and it
/// fails the moment a `CodingKeys` entry drifts from the fixture.
@Suite("Sample data")
struct SampleDataTests {

    @Test("Every sample work decodes")
    func worksDecode() {
        // SampleData traps on a decode failure rather than returning empty, so
        // reaching this line at all is most of the assertion.
        #expect(SampleData.works.count == 13)
        #expect(SampleData.works.allSatisfy { !$0.id.isEmpty })
        #expect(SampleData.works.allSatisfy { $0.cid.hasPrefix("sha256:") })
    }

    @Test("Samples cover the card states the UI has to draw")
    func coversCardStates() {
        let works = SampleData.works

        #expect(works.contains { $0.mediaURLs.count == 4 })
        #expect(works.contains { $0.mediaURLs.count == 2 })
        #expect(works.contains { $0.video != nil })
        #expect(works.contains { $0.poll != nil })
        #expect(works.contains { $0.quoted != nil })
        #expect(works.contains { $0.repostedBy != nil })
        #expect(works.contains { $0.isNSFW })
        #expect(works.contains { $0.linkPreview != nil })
        #expect(works.contains { !$0.tags.isEmpty })
        #expect(works.contains { $0.commentGating == "followers" })
    }

    @Test("Samples cover the states 3a added")
    func coversLaneStates() {
        let works = SampleData.works

        #expect(works.contains { $0.visibleProvenance != nil })
        #expect(works.contains { $0.tipLabel != nil })
        #expect(works.contains { $0.gate == .subscriber })
        #expect(works.contains { if case .priced = $0.gate { return true } else { return false } })
        #expect(works.contains { $0.voice != nil })
        #expect(works.contains { $0.expiresAt != nil })
        // Every provenance kind the vocabulary has, so no icon branch ships
        // unlooked-at.
        let kinds = Set(works.compactMap { $0.visibleProvenance?.kind })
        #expect(kinds.isSuperset(of: ["you_follow", "circle", "popular", "surface", "reposted"]))
    }

    @Test("Each lane's sample page differs from the others")
    func lanesFilter() {
        // Not an assertion about what the server means by a lane — it owns
        // that — only that the five are visibly different things to look at.
        #expect(SampleData.works(lane: .following).count == SampleData.works.count)
        #expect(SampleData.works(lane: .visions).allSatisfy { $0.expiresAt != nil })
        #expect(SampleData.works(lane: .live).allSatisfy { $0.video != nil })
        #expect(!SampleData.works(lane: .music).isEmpty)
        #expect(SampleData.page(lane: .live).liveCount == SampleData.liveCount)
    }

    @Test("A poll the viewer has voted in reveals its results")
    func pollState() throws {
        let poll = try #require(SampleData.works.compactMap(\.poll).first)

        #expect(poll.viewerVote == 1)
        #expect(poll.hasVoted)
        #expect(poll.showsResults)
        #expect(poll.results.count == 3)
        #expect(poll.results[0].isWinner)
        #expect(poll.results[1].voted)
        #expect(poll.totalVotes == 875)
    }

    @Test("An unvoted poll hides its results")
    func unvotedPollHidesResults() {
        let poll = Poll(results: [], totalVotes: 0, viewerVote: nil, closed: false)
        #expect(!poll.showsResults)
        #expect(!poll.hasVoted)
    }

    @Test("A closed poll reveals results even without a vote")
    func closedPollShowsResults() {
        let poll = Poll(results: [], totalVotes: 9, viewerVote: nil, closed: true)
        #expect(poll.showsResults)
    }

    @Test("The Go -1 no-vote sentinel does not survive decoding")
    func noVoteSentinelIsNormalised() throws {
        let raw = #"{"results":[],"total_votes":0,"viewer_vote":-1,"closed":false,"time_left":""}"#
        let poll = try APIClient.makeDecoder().decode(Poll.self, from: Data(raw.utf8))

        #expect(poll.viewerVote == nil)
    }

    @Test("Profile and current-user fixtures decode")
    func profileDecodes() {
        #expect(SampleData.profile.user.handle == "miiyazuko")
        #expect(SampleData.profile.followsViewer)
        #expect(!SampleData.profile.viewerFollows)
        #expect(SampleData.currentUser.user.handle == "dev")
        #expect(SampleData.currentUser.unreadCount == 3)
    }
}

/// The web and the app must label the same timestamp identically, so these pin
/// the boundaries of the port of `formatPostTimes()`.
@Suite("Relative time")
struct RelativeTimeTests {
    private let now = Date(timeIntervalSince1970: 1_770_000_000)

    @Test("Under a minute reads as now")
    func underAMinute() {
        #expect(RelativeTime.label(for: now.addingTimeInterval(-30), now: now) == "now")
        #expect(RelativeTime.label(for: now.addingTimeInterval(-59), now: now) == "now")
    }

    @Test("Clock skew from the server does not produce a negative age")
    func futureTimestamp() {
        #expect(RelativeTime.label(for: now.addingTimeInterval(5), now: now) == "now")
    }

    @Test("Under an hour counts minutes")
    func minutes() {
        #expect(RelativeTime.label(for: now.addingTimeInterval(-60), now: now) == "1m")
        #expect(RelativeTime.label(for: now.addingTimeInterval(-42 * 60), now: now) == "42m")
        #expect(RelativeTime.label(for: now.addingTimeInterval(-3599), now: now) == "59m")
    }

    @Test("Past an hour switches to a 24-hour clock time")
    func clockTime() {
        let label = RelativeTime.label(for: now.addingTimeInterval(-7200), now: now)

        #expect(label.count == 5)
        #expect(label.contains(":"))
        // The web pins hour12:false; a locale that would otherwise render
        // "2:00 PM" must not leak through.
        #expect(!label.uppercased().contains("AM"))
        #expect(!label.uppercased().contains("PM"))
    }

    @Test("A vision counts down to its expiry")
    func visionCountdown() {
        #expect(RelativeTime.countdown(to: now.addingTimeInterval(3 * 3600), now: now) == "3h left")
        #expect(RelativeTime.countdown(to: now.addingTimeInterval(30 * 60), now: now) == "30m left")
        // Expiry is query-time filtering server-side, so a permalink to an
        // expired vision still resolves and must read as expired, not negative.
        #expect(RelativeTime.countdown(to: now.addingTimeInterval(-60), now: now) == "expired")
    }
}

/// Counts are the one place the app knowingly diverges from the web, so the
/// divergence is pinned rather than left to drift.
@Suite("Counts")
struct CountsTests {
    @Test("Zero draws no label at all")
    func zeroIsHidden() {
        #expect(Counts.label(0) == nil)
        #expect(Counts.label(-1) == nil)
    }

    @Test("Up to five figures matches the web exactly")
    func rawBelowTenThousand() {
        #expect(Counts.label(1) == "1")
        #expect(Counts.label(999) == "999")
        #expect(Counts.label(9_999) == "9999")
    }

    @Test("Above five figures abbreviates so six controls still fit 375pt")
    func abbreviatesAboveTenThousand() {
        #expect(Counts.label(10_000) == "10K")
        #expect(Counts.label(12_400) == "12.4K")
        #expect(Counts.label(999_000) == "999K")
        #expect(Counts.label(1_200_000) == "1.2M")
    }
}
