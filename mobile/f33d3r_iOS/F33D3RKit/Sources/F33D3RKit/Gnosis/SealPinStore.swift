import Foundation

/// Trust-on-first-use pinning for recipients' public keys.
///
/// This is not an optional nicety bolted onto ``SealCore``; it is the only thing
/// standing between a user and a key swap. The scheme has no forward secrecy and
/// no out-of-band verification, so a server that hands out an attacker's public
/// key for `@someone` gets every subsequent message sealed to the attacker, and
/// nothing in the ciphertext gives it away. Pinning turns that from silent into
/// a question the user gets asked.
///
/// The browser does exactly this in `checkPins` in `gnosis-seal.js`, backed by
/// IndexedDB, and the server logs a `gnosis_key_rotated` security event when
/// `UpsertIdentity` sees a key change. This is the iOS half of the same
/// defence, and a compose path that seals without calling
/// ``check(_:)`` has dropped it.
///
/// ## What a change actually means
///
/// Most of the time: the person signed in on a new device, or recovered their
/// account, and the browser re-provisioned a fresh identity key. That is
/// routine, which is precisely why a warning cannot be fatal — a client that
/// refuses to send would be unusable — and why the decision belongs to the user,
/// not to this type. ``check(_:)`` reports; it does not block.
public protocol SealPinStore: Sendable {
    /// The public key last seen for `account`, or nil if this device has never
    /// sealed to them.
    func pinnedKey(for account: String) -> String?
    /// Records `publicKeyBase64` as the key now trusted for `account`.
    func pin(_ publicKeyBase64: String, for account: String)
}

/// What ``SealPinStore/check(_:)`` found.
public struct SealPinChange: Sendable, Equatable {
    public let account: String
    public let previousPublicKeyBase64: String
    public let currentPublicKeyBase64: String
}

public extension SealPinStore {

    /// Compares each recipient against what this device last saw.
    ///
    /// Returns the accounts whose key changed, **without** pinning the new ones:
    /// accepting a changed key is the user's answer to a question, so it happens
    /// in ``accept(_:)`` after they have given it. First-contact keys are pinned
    /// here, because there is nothing to ask about.
    func check(_ recipients: [SealCore.Recipient]) -> [SealPinChange] {
        var changes: [SealPinChange] = []
        for recipient in recipients {
            guard let previous = pinnedKey(for: recipient.account) else {
                pin(recipient.publicKeyBase64, for: recipient.account)
                continue
            }
            if previous != recipient.publicKeyBase64 {
                changes.append(
                    SealPinChange(
                        account: recipient.account,
                        previousPublicKeyBase64: previous,
                        currentPublicKeyBase64: recipient.publicKeyBase64
                    )
                )
            }
        }
        return changes
    }

    /// Pins the new keys after the user has said to send anyway.
    func accept(_ changes: [SealPinChange]) {
        for change in changes { pin(change.currentPublicKeyBase64, for: change.account) }
    }
}

/// Pins in `UserDefaults`, namespaced per account so two identities on one
/// device do not share a trust store.
///
/// Not the Keychain: a pin is public information — it is the same public key the
/// directory serves — and its integrity, not its secrecy, is what matters. What
/// it must survive is app launches, which `UserDefaults` does.
public struct DefaultsSealPinStore: SealPinStore {
    // `UserDefaults` is thread-safe and documented as such, but predates
    // `Sendable` and has never been annotated. A pin store is consulted from
    // whatever task is about to seal, so it has to cross isolation.
    nonisolated(unsafe) private let defaults: UserDefaults
    private let prefix: String

    /// - Parameter account: the local account these pins belong to. Pins made
    ///   while signed in as one identity say nothing about what another identity
    ///   on the same device has verified.
    public init(account: String, defaults: UserDefaults = .standard) {
        self.defaults = defaults
        self.prefix = "gnosis.pin.\(account)."
    }

    public func pinnedKey(for account: String) -> String? {
        defaults.string(forKey: prefix + account)
    }

    public func pin(_ publicKeyBase64: String, for account: String) {
        defaults.set(publicKeyBase64, forKey: prefix + account)
    }
}

/// A pin store that forgets on relaunch. Tests, and previews.
public final class InMemorySealPinStore: SealPinStore, @unchecked Sendable {
    private let lock = NSLock()
    private var pins: [String: String]

    public init(pins: [String: String] = [:]) { self.pins = pins }

    public func pinnedKey(for account: String) -> String? {
        lock.lock(); defer { lock.unlock() }
        return pins[account]
    }

    public func pin(_ publicKeyBase64: String, for account: String) {
        lock.lock(); defer { lock.unlock() }
        pins[account] = publicKeyBase64
    }
}
