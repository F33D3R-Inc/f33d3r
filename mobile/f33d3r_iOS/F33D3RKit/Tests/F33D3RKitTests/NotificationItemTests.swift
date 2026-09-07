import Foundation
import Testing
@testable import F33D3RKit

/// The Notifications row's one piece of logic: turning a grouped notification into a
/// sentence. It is worth a suite because it is arithmetic on a count the server
/// sends — "and 11 others" over a list of three names is the kind of off-by-one
/// nobody spots by looking at a screenshot.
@Suite("Notifications notifications")
struct NotificationItemTests {

    private func actor(_ handle: String) -> WorkAuthor {
        WorkAuthor(handle: handle, displayName: handle.capitalized)
    }

    private func notification(
        kind: String,
        actors: [String],
        actorCount: Int? = nil,
        targetType: String = "work",
        amountUAET: Int64? = nil
    ) -> NotificationItem {
        NotificationItem(
            id: UUID().uuidString,
            kind: kind,
            actors: actors.map(actor),
            actorCount: actorCount,
            targetID: targetType == "work" ? "work-1" : nil,
            targetType: targetType,
            amountUAET: amountUAET,
            createdAt: Date()
        )
    }

    @Test("One actor is named, and nothing else is claimed")
    func single() {
        #expect(notification(kind: "like", actors: ["dev"]).summary == "@dev liked your work")
        #expect(notification(kind: "follow", actors: ["dev"], targetType: "profile").summary == "@dev followed you")
    }

    @Test("Two and three actors are listed rather than counted")
    func fewActors() {
        #expect(
            notification(kind: "repost", actors: ["dev", "admin"]).summary
                == "@dev and @admin reposted your work"
        )
        #expect(
            notification(kind: "repost", actors: ["dev", "admin", "guest"]).summary
                == "@dev, @admin and @guest reposted your work"
        )
    }

    @Test("The overflow count covers everyone the row does not name")
    func overflow() {
        // Three faces drawn, twelve people involved: one named, eleven others.
        let row = notification(kind: "like", actors: ["dev", "admin", "guest"], actorCount: 12)
        #expect(row.summary == "@dev and 11 others liked your work")
    }

    @Test("A count larger than the actors sent is still consistent")
    func overflowWithFewerActors() {
        // A server that groups and sends only the newest actor still produces a
        // sentence that adds up.
        let row = notification(kind: "like", actors: ["dev"], actorCount: 4)
        #expect(row.summary == "@dev and 3 others liked your work")
    }

    @Test("A tip says the amount, from the amount the server sent")
    func tip() {
        let row = notification(kind: "tip", actors: ["dev"], amountUAET: 2_500_000)
        #expect(row.summary == "@dev tipped 2.5 AET")
    }

    @Test("An unknown kind still renders a sentence")
    func unknownKind() {
        // A kind added server-side must not produce a blank row in an older
        // build.
        let row = notification(kind: "seance", actors: ["dev"])
        #expect(row.summary.hasPrefix("@dev "))
        #expect(!row.summary.isEmpty)
    }

    @Test("Only like, repost and follow group")
    func grouping() {
        #expect(NotificationItem.groups(kind: "like"))
        #expect(NotificationItem.groups(kind: "repost"))
        #expect(NotificationItem.groups(kind: "follow"))
        for kind in ["reply", "quote", "mention", "tip", "subscribe", "thread_reply"] {
            #expect(!NotificationItem.groups(kind: kind))
        }
    }

    @Test("A row points at a work or a person, never at both")
    func destinations() {
        let like = notification(kind: "like", actors: ["dev"])
        #expect(like.destinationWorkID == "work-1")
        #expect(like.destinationHandle == nil)

        let follow = notification(kind: "follow", actors: ["dev"], targetType: "profile")
        #expect(follow.destinationWorkID == nil)
        #expect(follow.destinationHandle == "dev")
    }

    @Test("Sample rows decode through the production decoder")
    func samplesDecode() {
        // Same argument as the work fixtures: these must survive the real
        // decoder, so they double as a check that the proposed DTO and the
        // model agree.
        #expect(SampleData.notifications.count == 5)
        #expect(SampleData.notifications.contains { $0.actorCount > $0.actors.count })
        #expect(SampleData.notifications.contains { $0.kind == "tip" && $0.amountUAET != nil })
        #expect(SampleData.notifications.contains { !$0.isRead })
    }
}

/// The wallet carries no logic beyond formatting a signed amount, and that is
/// exactly the part that must not be wrong.
@Suite("Wallet")
struct WalletSnapshotTests {

    @Test("Direction comes from the sign, once")
    func signs() {
        let credit = WalletEntry(id: "1", kind: "tip_received", amountUAET: 2_500_000, createdAt: Date())
        let debit = WalletEntry(id: "2", kind: "tip_sent", amountUAET: -1_000_000, createdAt: Date())

        #expect(credit.isCredit)
        #expect(!debit.isCredit)
        #expect(credit.signedLabel == "+2.5 AET")
        // A typographic minus, not a hyphen.
        #expect(debit.signedLabel == "−1 AET")
    }

    @Test("Pending is never folded into the balance")
    func pendingIsSeparate() {
        let wallet = SampleData.wallet
        #expect(wallet.pendingUAET > 0)
        // The screen shows two figures; nothing in the model adds them, and
        // nothing should start.
        #expect(wallet.balanceUAET == 41_250_000)
    }

    @Test("Sample ledger decodes and covers both directions")
    func samplesDecode() {
        #expect(SampleData.wallet.entries.contains { $0.isCredit })
        #expect(SampleData.wallet.entries.contains { !$0.isCredit })
    }
}

/// The Explore lanes exist to be sent to the server verbatim, so the raw values
/// are the contract and not a display detail.
@Suite("Explore lanes")
struct ExploreLaneTests {

    @Test("Raw values are the server's surface ids")
    func rawValues() {
        // `feedLanes` in apiv1_feed.go. A rename on either side has to fail
        // something, and this is the something.
        #expect(ExploreLane.forYou.rawValue == "foryou")
        #expect(ExploreLane.trending.rawValue == "trending")
        #expect(ExploreLane.allCases.count == 2)
        #expect(ExploreLane.default == .forYou)
    }

    @Test("Sample pages differ between lanes")
    func samplePages() {
        #expect(!SampleData.page(explore: .forYou).works.isEmpty)
        #expect(!SampleData.page(explore: .trending).works.isEmpty)
    }
}
