import Foundation
import Observation

/// The owner's Numbers, and the knocks waiting on them.
///
/// Two reads that belong on one screen, so one store holds both and one refresh
/// brings both back. Every write here is answered by re-reading rather than by
/// editing the list in place: a Number, its policy and the account policy are
/// the server's, and a row that showed a policy the server did not accept would
/// be a lie the owner then acts on.
@MainActor
@Observable
public final class NumbersStore {

    public enum Phase: Equatable, Sendable {
        case idle
        case loading
        case loaded
        case failed(String)
    }

    public private(set) var phase: Phase = .idle
    public private(set) var page = NumbersPage(numbers: [], contactPolicy: ContactPolicy.open, policies: [])
    public private(set) var requests: [ContactRequest] = []
    /// What is in flight, keyed by the thing being changed, so one row can
    /// spin without the whole screen going quiet.
    public private(set) var busy: Set<String> = []
    /// The last write that did not go through. Reads do not set it — a failed
    /// read is ``Phase/failed(_:)``.
    public var problem: String?

    private let client: APIClient

    public init(client: APIClient) {
        self.client = client
    }

    public func loadIfNeeded() async {
        guard phase == .idle else { return }
        await reload()
    }

    public func reload() async {
        if page.numbers.isEmpty { phase = .loading }
        do {
            async let numbers = client.contactNumbers()
            async let waiting = client.contactRequests()
            page = try await numbers
            requests = try await waiting.requests
            phase = .loaded
        } catch let error as APIError {
            // A failed refresh does not take the Numbers away. They were true a
            // moment ago and are still the best answer — and a Number the owner
            // is halfway through reading out must not vanish.
            if page.numbers.isEmpty { phase = .failed(error.userMessage) }
        } catch let error as MessagingError {
            if page.numbers.isEmpty { phase = .failed(error.userMessage) }
        } catch {
            if page.numbers.isEmpty { phase = .failed(error.localizedDescription) }
        }
    }

    // MARK: Writes

    /// Mints a Number, and returns it so the screen can point at the new row.
    @discardableResult
    public func mint() async -> ContactNumber? {
        guard !busy.contains(Self.mintKey) else { return nil }
        busy.insert(Self.mintKey)
        defer { busy.remove(Self.mintKey) }
        do {
            let minted = try await client.mintNumber()
            await reload()
            return minted
        } catch {
            problem = Self.message(error)
            return nil
        }
    }

    public func revoke(_ number: ContactNumber) async {
        await write(number.id) { try await self.client.revokeNumber(id: number.id) }
    }

    public func setPolicy(_ policy: String, on number: ContactNumber) async {
        guard policy != number.policy else { return }
        await write(number.id) { try await self.client.setNumberPolicy(id: number.id, policy: policy) }
    }

    public func setContactPolicy(_ policy: String) async {
        guard policy != page.contactPolicy else { return }
        await write(Self.contactPolicyKey) { try await self.client.setContactPolicy(policy) }
    }

    public func decide(_ request: ContactRequest, accept: Bool) async {
        await write(request.id) {
            try await self.client.decideContactRequest(id: request.id, accept: accept)
        }
    }

    public func isBusy(_ key: String) -> Bool { busy.contains(key) }

    /// The key the mint button spins on.
    public static let mintKey = "mint"
    /// The key the account-wide picker spins on.
    public static let contactPolicyKey = "contact_policy"

    private func write(_ key: String, _ operation: @escaping () async throws -> Void) async {
        guard !busy.contains(key) else { return }
        busy.insert(key)
        defer { busy.remove(key) }
        do {
            try await operation()
            await reload()
        } catch {
            problem = Self.message(error)
            // Whatever the server actually holds is what the screen should
            // show, including after a refusal.
            await reload()
        }
    }

    private static func message(_ error: any Error) -> String {
        if let error = error as? MessagingError { return error.userMessage }
        if let error = error as? APIError { return error.userMessage }
        return error.localizedDescription
    }
}
