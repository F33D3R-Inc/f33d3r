import Foundation
import Testing
@testable import F33D3RKit

/// The pin store is the only thing that notices a key swap, so what it must not
/// do is notice quietly.
struct SealPinStoreTests {

    static func recipient(_ account: String, _ key: String) -> SealCore.Recipient {
        SealCore.Recipient(account: account, publicKeyBase64: key)
    }

    @Test("First contact pins silently — there is nothing to warn about")
    func firstContactPins() {
        let store = InMemorySealPinStore()
        let changes = store.check([Self.recipient("bob", "key-one")])
        #expect(changes.isEmpty)
        #expect(store.pinnedKey(for: "bob") == "key-one")
    }

    @Test("The same key again is not a change")
    func stableKeyIsQuiet() {
        let store = InMemorySealPinStore(pins: ["bob": "key-one"])
        #expect(store.check([Self.recipient("bob", "key-one")]).isEmpty)
    }

    @Test("A changed key is reported and is not pinned until the user accepts it")
    func changedKeyIsReported() {
        let store = InMemorySealPinStore(pins: ["bob": "key-one"])
        let changes = store.check([Self.recipient("bob", "key-two")])

        #expect(changes.count == 1)
        #expect(changes[0].account == "bob")
        #expect(changes[0].previousPublicKeyBase64 == "key-one")
        #expect(changes[0].currentPublicKeyBase64 == "key-two")
        // Still the old key: a warning the user has not answered must not
        // silently update the trust store, or the second send would be quiet.
        #expect(store.pinnedKey(for: "bob") == "key-one")

        store.accept(changes)
        #expect(store.pinnedKey(for: "bob") == "key-two")
    }

    @Test("Only the changed recipients are reported out of a group")
    func reportsOnlyWhatChanged() {
        let store = InMemorySealPinStore(pins: ["alice": "a1", "bob": "b1"])
        let changes = store.check([
            Self.recipient("alice", "a1"),
            Self.recipient("bob", "b2"),
            Self.recipient("carol", "c1"),
        ])
        #expect(changes.map(\.account) == ["bob"])
        // Carol was first contact, so she is pinned as a side effect.
        #expect(store.pinnedKey(for: "carol") == "c1")
    }

    @Test("Pins are namespaced per local account, so two identities do not share trust")
    func pinsAreNamespacedPerAccount() throws {
        let defaults = try #require(UserDefaults(suiteName: "sealcore.tests.\(UUID().uuidString)"))
        defer { defaults.removePersistentDomain(forName: defaults.description) }

        let mine = DefaultsSealPinStore(account: "me", defaults: defaults)
        let theirs = DefaultsSealPinStore(account: "someone-else", defaults: defaults)

        mine.pin("key-one", for: "bob")
        #expect(mine.pinnedKey(for: "bob") == "key-one")
        // What I verified says nothing about what another identity on this
        // device has verified.
        #expect(theirs.pinnedKey(for: "bob") == nil)
        #expect(theirs.check([Self.recipient("bob", "key-two")]).isEmpty)
        #expect(mine.check([Self.recipient("bob", "key-two")]).count == 1)
    }
}
