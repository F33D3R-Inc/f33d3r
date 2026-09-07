import AVFoundation
import Foundation
import MediaPlayer
import Observation
import UIKit
import F33D3RKit

/// The app's one music player.
///
/// One instance, held by `AppModel`, because a phone has one audio output and a
/// second player would mean two tracks fighting for it. Every surface that
/// plays or shows music — the lane, the now-playing bar on every tab, the full
/// player, the lock screen — reads this object, so they cannot disagree about
/// what is playing.
///
/// What it owns is the one piece of state the server does not: where the
/// playhead is. The tracks themselves are `Work`s the server sent, drawn as it
/// sent them; the player never rewrites one. Like, save and tip on the track
/// playing go through `AppModel` and come back as the server's row, same as
/// on a card.
///
/// `@MainActor` because everything it publishes drives SwiftUI. AVFoundation's
/// callbacks are asked to arrive on the main queue and are hopped onto the
/// actor from there rather than trusted by assertion.
@MainActor
@Observable
final class MusicPlayer {

    /// The tracks in play order. The lane hands in its list when a track is
    /// tapped, so next and previous move through what the reader was looking
    /// at rather than through some other idea of the surface.
    private(set) var queue: [Work] = []
    /// The track loaded into the player, playing or paused. Nil until
    /// something has been tapped; the now-playing bar draws nothing then.
    private(set) var nowPlaying: Work?
    private(set) var isPlaying = false
    /// Seconds into the track.
    private(set) var progress: TimeInterval = 0
    /// Seconds long. Seeded from what the server said the track runs so the
    /// scrubber has a range before the asset has been read, then corrected
    /// from the asset once AVFoundation knows.
    private(set) var duration: TimeInterval = 0
    /// Why the last track would not play, when it would not.
    private(set) var problem: String?

    /// Where `/media/...` paths resolve. Same origin the images use.
    let mediaOrigin: URL?
    /// Supplies the session a walled track needs. Nil in previews.
    @ObservationIgnored let client: APIClient?
    /// Fetches artwork with that session attached.
    @ObservationIgnored let loader: MediaLoader

    @ObservationIgnored private let player = AVPlayer()
    @ObservationIgnored private var timeObserver: Any?
    @ObservationIgnored private var endObserver: NSObjectProtocol?
    @ObservationIgnored private var failObserver: NSObjectProtocol?
    @ObservationIgnored private var interruptionObserver: NSObjectProtocol?
    @ObservationIgnored private var artworkTask: Task<Void, Never>?
    /// Builds the current item once its credential is in hand. Cancelled when
    /// a new track is loaded, so a slow lookup cannot land on a later track.
    @ObservationIgnored private var loadTask: Task<Void, Never>?
    @ObservationIgnored private var hasRemoteCommands = false

    init(mediaOrigin: URL?, client: APIClient? = nil, loader: MediaLoader = .unauthenticated) {
        self.mediaOrigin = mediaOrigin
        self.client = client
        self.loader = loader
        // Audio-only playback: the player does not need to keep a display
        // awake, and telling it so is what lets it keep going with the screen
        // locked.
        player.preventsDisplaySleepDuringVideoPlayback = false
    }

    // MARK: - Queue

    var hasNext: Bool {
        guard let index = currentIndex else { return false }
        return index + 1 < queue.count
    }

    var hasPrevious: Bool {
        guard let index = currentIndex else { return false }
        return index > 0
    }

    private var currentIndex: Int? {
        guard let nowPlaying else { return nil }
        return queue.firstIndex { $0.id == nowPlaying.id }
    }

    /// Whether this work is the one loaded, playing or paused.
    func isCurrent(_ work: Work) -> Bool { nowPlaying?.id == work.id }

    /// Plays `work`, with `queue` as what next and previous move through.
    ///
    /// Tapping the track already loaded toggles it instead of restarting it:
    /// a reader who taps the row they are listening to means "pause", and the
    /// row is already drawn as the one playing.
    func play(_ work: Work, queue: [Work]) {
        self.queue = queue.filter(\.isPlayable)
        if isCurrent(work) {
            togglePlayPause()
            return
        }
        load(work)
        resume()
    }

    func togglePlayPause() {
        guard nowPlaying != nil else { return }
        if isPlaying { pause() } else { resume() }
    }

    func pause() {
        player.pause()
        isPlaying = false
        publishPlaybackState()
    }

    func resume() {
        guard nowPlaying != nil else { return }
        activateAudioSession()
        player.play()
        isPlaying = true
        publishPlaybackState()
    }

    func next() {
        guard let index = currentIndex, index + 1 < queue.count else {
            // Off the end: stop where we are, at the start of the last track,
            // the way a record player would.
            pause()
            seek(to: 0)
            return
        }
        load(queue[index + 1])
        resume()
    }

    /// Back to the start of this track, or to the previous track when the
    /// playhead is already near the start — the convention every player
    /// shares.
    func previous() {
        if progress > 3 || currentIndex == 0 || currentIndex == nil {
            seek(to: 0)
            return
        }
        load(queue[currentIndex! - 1])
        resume()
    }

    func seek(to seconds: TimeInterval) {
        let clamped = max(0, min(seconds, duration > 0 ? duration : seconds))
        progress = clamped
        player.seek(to: CMTime(seconds: clamped, preferredTimescale: 600), toleranceBefore: .zero, toleranceAfter: .zero)
        publishPlaybackState()
    }

    /// Replaces the loaded track's row with a fresher one — the server's, after
    /// a like or a purchase — so the player's own copy does not lag the card's.
    /// Identity is by id; a different work is ignored.
    func refresh(_ work: Work) {
        if let index = queue.firstIndex(where: { $0.id == work.id }) { queue[index] = work }
        if isCurrent(work) { nowPlaying = work }
    }

    // MARK: - Loading

    private func load(_ work: Work) {
        problem = nil
        guard let path = work.playbackPath, let url = URL.media(path, origin: mediaOrigin) else {
            problem = "This work has nothing to play."
            return
        }
        detachItemObservers()
        nowPlaying = work
        progress = 0
        duration = work.playbackDurationSecs

        // A track is a work's media, so it is behind the same wall its images
        // are: an item built without the session is refused with 403 before it
        // plays a note. The credential is fetched first and the item is built
        // once — building an unauthorised one first and swapping it would fire
        // the failure observer on the way past and report a problem that was
        // never real.
        loadTask?.cancel()
        loadTask = Task { [weak self] in
            guard let self else { return }
            let credential = await self.client?.mediaCredential(for: url)
            guard !Task.isCancelled, self.nowPlaying?.id == work.id else { return }
            let item = AuthenticatedAsset.playerItem(url: url, credential: credential)
            self.player.replaceCurrentItem(with: item)
            self.attachItemObservers(item)
            // `resume()` may have run while the credential was in flight; the
            // player had no item then, so the play has to be repeated now.
            if self.isPlaying { self.player.play() }
        }
        installTimeObserverIfNeeded()
        installRemoteCommandsIfNeeded()
        publishNowPlayingInfo(for: work)
    }

    private func attachItemObservers(_ item: AVPlayerItem) {
        let center = NotificationCenter.default
        endObserver = center.addObserver(forName: .AVPlayerItemDidPlayToEndTime, object: item, queue: .main) { _ in
            MainActor.assumeIsolated { [weak self] in self?.next() }
        }
        failObserver = center.addObserver(forName: .AVPlayerItemFailedToPlayToEndTime, object: item, queue: .main) { note in
            let reason = (note.userInfo?[AVPlayerItemFailedToPlayToEndTimeErrorKey] as? NSError)?.localizedDescription
            MainActor.assumeIsolated { [weak self] in
                self?.isPlaying = false
                self?.problem = reason ?? "The track stopped playing."
                self?.publishPlaybackState()
            }
        }
    }

    private func detachItemObservers() {
        if let endObserver { NotificationCenter.default.removeObserver(endObserver) }
        if let failObserver { NotificationCenter.default.removeObserver(failObserver) }
        endObserver = nil
        failObserver = nil
    }

    /// One observer for the life of the player: it reports on whatever item is
    /// current, so it is not re-made per track.
    private func installTimeObserverIfNeeded() {
        guard timeObserver == nil else { return }
        let interval = CMTime(seconds: 0.5, preferredTimescale: 600)
        timeObserver = player.addPeriodicTimeObserver(forInterval: interval, queue: .main) { [weak self] time in
            MainActor.assumeIsolated {
                guard let self else { return }
                let seconds = time.seconds
                if seconds.isFinite { self.progress = seconds }
                if let itemDuration = self.player.currentItem?.duration, itemDuration.isNumeric {
                    let d = itemDuration.seconds
                    if d.isFinite, d > 0, abs(d - self.duration) > 0.5 {
                        self.duration = d
                        self.publishPlaybackState()
                    }
                }
                // The player can stall (buffering, a route change) without a
                // notification; the rate is the truth about whether sound is
                // coming out.
                let playing = self.player.timeControlStatus == .playing
                if playing != self.isPlaying, self.player.timeControlStatus != .waitingToPlayAtSpecifiedRate {
                    self.isPlaying = playing
                    self.publishPlaybackState()
                }
            }
        }
    }

    // MARK: - Audio session

    /// `.playback` is what lets the track keep going when the app leaves the
    /// foreground and the screen locks, and what puts it on the lock screen.
    /// Set when playback starts rather than at launch, so an app opened to read
    /// does not claim the audio route from whatever the reader was listening to.
    private func activateAudioSession() {
        let session = AVAudioSession.sharedInstance()
        do {
            try session.setCategory(.playback, mode: .default, policy: .longFormAudio)
            try session.setActive(true)
        } catch {
            problem = "Audio couldn't start: \(error.localizedDescription)"
        }
        guard interruptionObserver == nil else { return }
        interruptionObserver = NotificationCenter.default.addObserver(
            forName: AVAudioSession.interruptionNotification, object: session, queue: .main
        ) { note in
            let raw = note.userInfo?[AVAudioSessionInterruptionTypeKey] as? UInt
            let type = raw.flatMap(AVAudioSession.InterruptionType.init(rawValue:))
            let options = (note.userInfo?[AVAudioSessionInterruptionOptionKey] as? UInt).map(AVAudioSession.InterruptionOptions.init(rawValue:))
            MainActor.assumeIsolated { [weak self] in
                guard let self else { return }
                switch type {
                case .began:
                    // A call or another app took the route. Reflect it; the
                    // player has already stopped.
                    self.isPlaying = false
                    self.publishPlaybackState()
                case .ended:
                    if options?.contains(.shouldResume) == true { self.resume() }
                default:
                    break
                }
            }
        }
    }

    // MARK: - Lock screen and Control Center

    /// Play, pause, next and previous from the lock screen, Control Center,
    /// headphones and CarPlay. Installed once, on the first track.
    private func installRemoteCommandsIfNeeded() {
        guard !hasRemoteCommands else { return }
        hasRemoteCommands = true
        let center = MPRemoteCommandCenter.shared()
        center.playCommand.addTarget { _ in
            MainActor.assumeIsolated { [weak self] in self?.resume() }
            return .success
        }
        center.pauseCommand.addTarget { _ in
            MainActor.assumeIsolated { [weak self] in self?.pause() }
            return .success
        }
        center.togglePlayPauseCommand.addTarget { _ in
            MainActor.assumeIsolated { [weak self] in self?.togglePlayPause() }
            return .success
        }
        center.nextTrackCommand.addTarget { _ in
            MainActor.assumeIsolated { [weak self] in self?.next() }
            return .success
        }
        center.previousTrackCommand.addTarget { _ in
            MainActor.assumeIsolated { [weak self] in self?.previous() }
            return .success
        }
        center.changePlaybackPositionCommand.addTarget { event in
            guard let event = event as? MPChangePlaybackPositionCommandEvent else { return .commandFailed }
            let seconds = event.positionTime
            MainActor.assumeIsolated { [weak self] in self?.seek(to: seconds) }
            return .success
        }
    }

    /// The static half of the lock-screen card: title, artist, length,
    /// artwork. Artwork is fetched once per track and attached when it lands.
    private func publishNowPlayingInfo(for work: Work) {
        var info: [String: Any] = [
            MPMediaItemPropertyTitle: work.trackTitle,
            MPMediaItemPropertyArtist: "@\(work.author.handle)",
            MPMediaItemPropertyAlbumTitle: "F33D3R",
            MPMediaItemPropertyPlaybackDuration: duration,
            MPNowPlayingInfoPropertyElapsedPlaybackTime: progress,
            MPNowPlayingInfoPropertyPlaybackRate: isPlaying ? 1.0 : 0.0,
            MPNowPlayingInfoPropertyMediaType: MPNowPlayingInfoMediaType.audio.rawValue,
        ]
        MPNowPlayingInfoCenter.default().nowPlayingInfo = info

        artworkTask?.cancel()
        guard let url = URL.media(work.artworkPath ?? work.author.avatarURL, origin: mediaOrigin) else { return }
        let workID = work.id
        artworkTask = Task { [weak self, loader = self.loader] in
            guard let data = try? await loader.data(for: url),
                  let image = UIImage(data: data) else { return }
            guard let self, !Task.isCancelled, self.nowPlaying?.id == workID else { return }
            let artwork = MPMediaItemArtwork(boundsSize: image.size) { _ in image }
            info = MPNowPlayingInfoCenter.default().nowPlayingInfo ?? info
            info[MPMediaItemPropertyArtwork] = artwork
            MPNowPlayingInfoCenter.default().nowPlayingInfo = info
        }
    }

    /// The moving half: elapsed time and rate. The system extrapolates the
    /// playhead from these between updates, so they are sent on every change
    /// of state rather than on every tick.
    private func publishPlaybackState() {
        guard nowPlaying != nil else { return }
        var info = MPNowPlayingInfoCenter.default().nowPlayingInfo ?? [:]
        info[MPMediaItemPropertyPlaybackDuration] = duration
        info[MPNowPlayingInfoPropertyElapsedPlaybackTime] = progress
        info[MPNowPlayingInfoPropertyPlaybackRate] = isPlaying ? 1.0 : 0.0
        MPNowPlayingInfoCenter.default().nowPlayingInfo = info
    }
}

/// Track lengths and playheads, as "3:34". Hours only when a track has them.
enum MusicTime {
    static func label(_ seconds: TimeInterval) -> String {
        guard seconds.isFinite, seconds >= 0 else { return "0:00" }
        let total = Int(seconds.rounded(.down))
        if total >= 3600 {
            return String(format: "%d:%02d:%02d", total / 3600, (total % 3600) / 60, total % 60)
        }
        return String(format: "%d:%02d", total / 60, total % 60)
    }
}

#if DEBUG
/// Simulator-only ways to reach the player's screens.
///
/// `F33D3R_PLAY=1` starts the first track on the Music surface as soon as it
/// loads, which is the only way to get the now-playing bar on screen in a
/// Simulator nobody can tap; `F33D3R_PLAYER=1` opens the full player over it.
/// Opt-in per run and read once.
enum MusicDebug {
    static let autoPlay = ProcessInfo.processInfo.environment["F33D3R_PLAY"] == "1"
    static let openPlayer = ProcessInfo.processInfo.environment["F33D3R_PLAYER"] == "1"
}
#endif
