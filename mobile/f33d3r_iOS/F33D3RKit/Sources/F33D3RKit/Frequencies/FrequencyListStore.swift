import Foundation
import Observation

/// The three lanes of the discover screen, and the reader's own open room.
///
/// Three separate reads because the server keeps three separate lanes; the
/// client does not filter one list into three, because "live" is the brain's
/// judgement about presence and heartbeats and not a predicate over a state
/// string. `own` is whichever lane answered with one — the server puts the
/// caller's currently open room there so the UI can offer to return to it.
@MainActor
@Observable
public final class FrequencyListStore {

    public private(set) var live: [Frequency] = []
    public private(set) var scheduled: [Frequency] = []
    public private(set) var mine: [Frequency] = []
    public private(set) var own: Frequency?
    public private(set) var isLoading = false
    /// Set when a refresh fails. The lists keep what they last held.
    public private(set) var lastError: String?
    /// False until the first refresh has answered, so an empty screen can tell
    /// "nothing yet" from "nothing at all".
    public private(set) var hasLoaded = false

    private let client: APIClient

    public init(client: APIClient) {
        self.client = client
    }

    public func refresh() async {
        isLoading = true
        defer { isLoading = false }
        do {
            async let liveLane = client.frequencies(lane: "live")
            async let scheduledLane = client.frequencies(lane: "scheduled")
            async let mineLane = client.frequencies(lane: "mine")
            let (l, s, m) = try await (liveLane, scheduledLane, mineLane)
            live = l.frequencies
            scheduled = s.frequencies
            mine = m.frequencies
            own = l.own ?? m.own ?? s.own
            lastError = nil
            hasLoaded = true
        } catch {
            lastError = Self.message(error)
        }
    }

    private static func message(_ error: Error) -> String {
        switch error {
        case let e as APIError: return e.userMessage
        case let e as MalkuthError: return e.description
        default: return error.localizedDescription
        }
    }
}
