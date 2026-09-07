import SwiftUI
import F33D3RKit

/// What the shell knows about Frequencies: the three lanes, and the one room
/// the reader is currently tuned into.
///
/// It is a projection and not a brain. It holds the Kit's stores and the id of
/// the room whose screen is on top, and it decides nothing about roles, queues
/// or moderation — those live on the server and arrive through
/// ``FrequencyRoomStore``. The only reason it exists at all is that a listener
/// who leaves the room screen is still listening: the store has to outlive the
/// view so the mini bar has something to draw and the stream stays open.
///
/// One store per room, cached, so the mini bar and the room screen are the same
/// connection rather than two of them fighting over the same heartbeat.
@MainActor
@Observable
final class FrequencySession {

    /// The three lanes. Nil until ``attach(client:)``, which the root does once
    /// the session is known.
    private(set) var lanes: FrequencyListStore?

    /// The room the reader has tuned into, if any. Set from the server's own
    /// answer — see ``sync(_:)`` — never from the tap that asked for it.
    private(set) var listening: FrequencyRoomStore?

    /// The room whose screen is on top, so the mini bar can stay out of its
    /// way. Nil when the reader is anywhere else.
    private(set) var openRoomID: String?

    private var client: APIClient?
    private var rooms: [String: FrequencyRoomStore] = [:]

    /// Hands the session its client. Idempotent: the root calls it on every
    /// appearance and only the first one counts.
    func attach(client: APIClient) {
        guard self.client == nil else { return }
        self.client = client
        let lanes = FrequencyListStore(client: client)
        self.lanes = lanes
        // The rail on Home has no other chance to ask: it is drawn inside a
        // chrome overlay that is built before this runs, so a `task` on it
        // would find no store and never fire again.
        Task { await lanes.refresh() }
        #if DEBUG
        if let id = FrequencyDebug.backgroundRoomID, let store = room(id) {
            Task { [weak self] in
                await store.connect()
                self?.sync(store)
            }
        }
        #endif
    }

    /// The store for one room, made once and kept.
    func room(_ id: String) -> FrequencyRoomStore? {
        if let existing = rooms[id] { return existing }
        guard let client else { return nil }
        let store = FrequencyRoomStore(id: id, client: client)
        rooms[id] = store
        return store
    }

    // MARK: - Which screen is on top

    func roomAppeared(_ id: String) {
        openRoomID = id
    }

    func roomDisappeared(_ id: String) {
        guard openRoomID == id else { return }
        openRoomID = nil
    }

    /// Whether the mini bar belongs on screen: the reader is tuned in, and is
    /// not looking at the room they are tuned into.
    var isMiniBarVisible: Bool {
        guard let listening, listening.viewer?.isJoined == true else { return false }
        return openRoomID != listening.frequencyID
    }

    // MARK: - Listening

    /// Reconciles "am I listening" with the room the server last sent.
    ///
    /// Called after every frame and every mutation. The client never decides it
    /// has joined; `viewer.present` is the server saying so, and a room that has
    /// ended or that the reader has left drops out of here on the same evidence.
    func sync(_ store: FrequencyRoomStore) {
        let joined = store.viewer?.isJoined == true && store.frequency?.isOver == false
        if joined {
            listening = store
        } else if listening?.frequencyID == store.frequencyID {
            listening = nil
        }
    }

    /// Leaves whatever the reader is listening to, from the mini bar's X.
    func leaveListening() async {
        guard let store = listening else { return }
        await store.leave()
        sync(store)
        // A room left from the bar has no screen holding it open.
        if listening == nil, openRoomID != store.frequencyID {
            store.disconnect()
            rooms[store.frequencyID] = nil
        }
    }

    /// Forgets a room nobody is looking at or listening to, so a long session
    /// does not accumulate one dead store per room visited.
    func releaseIfIdle(_ id: String) {
        guard listening?.frequencyID != id, openRoomID != id else { return }
        rooms[id]?.disconnect()
        rooms[id] = nil
    }
}

/// The server's error codes, in words.
///
/// The brain answers `speakers_full` and feed-engine passes the code through
/// untouched, which is exactly right for a client that wants to branch and
/// exactly wrong for a reader. Anything not in this list is shown as the server
/// wrote it: a message this build has never seen is still the server's best
/// account of what happened, and swallowing it would leave nothing on screen.
enum FrequencyErrorCopy {

    static func sentence(_ raw: String?) -> String? {
        guard let raw, !raw.isEmpty else { return nil }
        let key = raw.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
        if let known = table[key] { return known }
        // The code sometimes arrives inside a longer line ("400 locked").
        for (code, sentence) in table where key.contains(code) {
            return sentence
        }
        return raw
    }

    private static let table: [String: String] = [
        "not_live": "This frequency isn't on air.",
        "locked": "The host has locked this room.",
        "blocked": "You can't join this frequency.",
        "full": "This frequency is full.",
        "speakers_full": "The stage is full — every speaker slot is taken.",
        "not_joined": "Tune in first.",
        "requests_closed": "The host isn't taking requests right now.",
        "verity_tier_too_low": "Your account isn't verified far enough to speak here yet.",
        "own_request": "That's your own hand.",
        "not_a_speaker": "They're not on the stage.",
        "not_a_cohost": "They're not a co-host.",
        "is_host": "You can't do that to the host.",
        "host_already_live": "You already have a frequency on air.",
        "media_unavailable": "Audio isn't available yet.",
        "already_cancelled": "This frequency was already called off.",
        "already_ending": "This frequency is already ending.",
        "over": "This frequency is over.",
        "recording_locked_while_live": "Recording can't be changed once you're on air.",
        "invalid_transition": "That can't be done from here.",
        "version_conflict": "Someone changed this a moment ago. Try again.",
        "rate_limited": "Too fast. Give it a minute.",
        "not_found": "This frequency no longer exists.",
        "forbidden": "You don't have permission for that.",
    ]
}
