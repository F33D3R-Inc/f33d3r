import Foundation
import os

/// Owns the signed-in session: the token in the Keychain and the current user in
/// memory.
///
/// An actor because it is the app's single source of truth for "who is signed
/// in" and is touched from every screen. It is also the `TokenProviding` the
/// APIClient asks for credentials, which is what lets a 401 anywhere in the app
/// clear the session exactly once.
public actor SessionStore: TokenProviding {
    /// What the UI switches on.
    public enum State: Equatable, Sendable {
        /// Not yet restored from the Keychain.
        case unknown
        case signedOut
        /// Signed in, but the server requires backup codes before anything else.
        case needsBackupCodes(CurrentUser)
        case signedIn(CurrentUser)

        public var user: CurrentUser? {
            switch self {
            case .signedIn(let u), .needsBackupCodes(let u): return u
            case .unknown, .signedOut: return nil
            }
        }
    }

    private static let log = Logger(subsystem: "com.f33d3r.ios", category: "session")

    private let keychain: KeychainStore
    private var token: String?
    private(set) public var state: State = .unknown

    /// Whether the token actually reached the Keychain.
    ///
    /// False means the session is good for this launch and no longer: the token
    /// is in memory and every request carries it, but it will not survive a
    /// relaunch. It is worth knowing because the Keychain that refused the token
    /// will refuse this device's signing key too, so the next post fails with
    /// the same OSStatus — a connection nobody can make from a swallowed error.
    private(set) public var tokenIsPersisted = false

    /// Notified on every state change, so views can react without polling.
    private var observers: [UUID: @Sendable (State) -> Void] = [:]

    public init(keychain: KeychainStore = KeychainStore()) {
        self.keychain = keychain
    }

    // MARK: - TokenProviding

    public func currentToken() async -> String? { token }

    /// The server rejected our token. Drop it — retrying cannot help, and holding
    /// a dead token makes every later request fail the same way.
    public func tokenRejected() async {
        await signOutLocally()
    }

    // MARK: - Lifecycle

    /// Restores a token from the Keychain at launch and confirms it is still
    /// valid. Call once, before showing UI.
    ///
    /// The token is only trusted once the server has answered — a stored token
    /// can be up to 30 days stale, or revoked from another device.
    public func restore(using client: APIClient) async {
        do {
            token = try keychain.get()
            tokenIsPersisted = token != nil
        } catch {
            // A stored token that cannot be read is not the same as no stored
            // token, and the difference decides whether "signed out again"
            // is the user's doing or this device's.
            Self.log.error("stored session unreadable: \(String(describing: error), privacy: .public)")
            token = nil
            tokenIsPersisted = false
        }
        guard token != nil else {
            setState(.signedOut)
            return
        }
        do {
            let user = try await client.me()
            setState(.signedIn(user))
        } catch let error as APIError where error.isUnauthenticated {
            await signOutLocally()
        } catch {
            // Offline or the server is down. The token may well still be good, so
            // keep it and let the UI decide how to present a degraded launch
            // rather than silently signing the user out over a dropped network.
            setState(.signedOut)
        }
    }

    /// Re-reads `/me` and publishes the server's current profile and settings.
    /// Called after any account write, so what the app shows is what the
    /// server now holds and never a locally patched copy.
    public func refreshUser(using client: APIClient) async throws {
        guard token != nil else { return }
        let user = try await client.me()
        switch state {
        case .signedIn, .needsBackupCodes:
            setState(.signedIn(user))
        case .signedOut, .unknown:
            break
        }
    }

    public func signIn(_ session: Session) async {
        token = session.token
        do {
            try keychain.set(session.token)
            tokenIsPersisted = true
        } catch {
            tokenIsPersisted = false
            Self.log.error("session token not persisted: \(String(describing: error), privacy: .public)")
        }
        setState(session.needsBackupCodes ? .needsBackupCodes(session.user) : .signedIn(session.user))
    }

    /// Signs out, telling the server to revoke the token first so it dies with
    /// the session rather than living until expiry. Local state is cleared even
    /// if that call fails — the user asked to sign out.
    public func signOut(using client: APIClient) async {
        try? await client.logout()
        await signOutLocally()
    }

    private func signOutLocally() async {
        token = nil
        tokenIsPersisted = false
        do {
            try keychain.delete()
        } catch {
            // The one failure that leaves a bearer credential on the device
            // after the user asked to be rid of it. Never silent.
            Self.log.error("session token not removed from the Keychain: \(String(describing: error), privacy: .public)")
        }
        setState(.signedOut)
    }

    // MARK: - Observation

    /// Registers `handler` for state changes. Returns a token; drop it or pass it
    /// to `removeObserver` to stop.
    @discardableResult
    public func observe(_ handler: @escaping @Sendable (State) -> Void) -> UUID {
        let id = UUID()
        observers[id] = handler
        handler(state)
        return id
    }

    public func removeObserver(_ id: UUID) {
        observers[id] = nil
    }

    private func setState(_ new: State) {
        state = new
        for handler in observers.values { handler(new) }
    }
}
