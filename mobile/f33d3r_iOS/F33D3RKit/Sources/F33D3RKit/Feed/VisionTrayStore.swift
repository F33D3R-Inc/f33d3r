import Foundation
import Observation

/// The tray: the reader's own ring and the rings of the accounts they follow.
///
/// Seen state is the server's. Opening a vision sends `vision_seen`; the ring
/// only flips once the server has said 204, and the flip is applied to the
/// copy held here so the tray does not need a second fetch to catch up.
@MainActor
@Observable
public final class VisionTrayStore {

    public enum Phase: Equatable, Sendable {
        case idle
        case loading
        case loaded
        case failed(APIError)
    }

    public private(set) var own: VisionRing?
    public private(set) var rings: [VisionRing] = []
    public private(set) var phase: Phase = .idle

    private let client: APIClient

    public init(client: APIClient) {
        self.client = client
    }

    public var isEmpty: Bool { own == nil && rings.isEmpty }

    public func loadIfNeeded() async {
        guard phase == .idle else { return }
        await reload()
    }

    public func reload() async {
        if phase == .idle { phase = .loading }
        do {
            let tray = try await client.visions()
            own = tray.own
            rings = tray.rings
            phase = .loaded
        } catch let error as APIError {
            if rings.isEmpty, own == nil { phase = .failed(error) }
        } catch {
            if rings.isEmpty, own == nil { phase = .failed(.transport(error.localizedDescription)) }
        }
    }

    /// Records a view and mirrors the server's answer into the ring.
    public func markSeen(_ vision: Vision) async {
        guard !vision.seen else { return }
        do {
            try await client.markVisionSeen(id: vision.id)
        } catch {
            return
        }
        rings = rings.map { $0.author.handle == vision.author.handle ? $0.marking(seen: vision.id) : $0 }
    }

    /// Hides a creator's visions. Their ring leaves the tray when the server
    /// has recorded the mute.
    public func muteVisions(from handle: String) async throws {
        try await client.setVisionsMuted(true, handle: handle)
        rings.removeAll { $0.author.handle == handle }
    }

    /// Deletes one of the reader's own visions.
    public func deleteOwn(_ vision: Vision) async throws {
        try await client.deleteVision(id: vision.id)
        await reload()
    }

    /// Votes on a vision's poll, then re-reads the tray so the results drawn
    /// are the server's tally.
    public func vote(_ vision: Vision, option: Int) async throws {
        try await client.voteVisionPoll(id: vision.id, optionIndex: option)
        await reload()
    }

    /// The ring after `handle`, for swiping between creators. Nil at the end.
    public func ring(after handle: String) -> VisionRing? {
        guard let index = rings.firstIndex(where: { $0.author.handle == handle }), index + 1 < rings.count else { return nil }
        return rings[index + 1]
    }

    public func ring(before handle: String) -> VisionRing? {
        guard let index = rings.firstIndex(where: { $0.author.handle == handle }), index > 0 else { return nil }
        return rings[index - 1]
    }

    /// The freshest copy of a ring, by author.
    public func ring(for handle: String) -> VisionRing? {
        if own?.author.handle == handle { return own }
        return rings.first { $0.author.handle == handle }
    }
}
