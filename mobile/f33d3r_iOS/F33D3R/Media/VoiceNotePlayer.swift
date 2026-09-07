import SwiftUI
import AVFoundation
import Observation
import F33D3RKit

/// A voice note on a card, played where it sits.
///
/// The web's `compose-voice-player`: a play button, a bar that fills as the
/// note plays and can be tapped or dragged to move through it, and the time.
/// The note is the work's media, so it is behind the same wall its images are
/// — the item is built with the reader's session (``AuthenticatedAsset``) and
/// never without it.
///
/// One note plays at a time. Starting one pauses whichever other note was
/// playing and the music player too: a phone has one audio output, and two
/// voices on it is not a feature. The player is torn down when the card
/// leaves the screen, for the same reason ``WorkVideoView`` tears its down.
struct VoiceNoteView: View {
    let voice: WorkVoice
    /// The work's id, so a card re-used for another work starts over.
    let seed: String

    @Environment(\.mediaOrigin) private var origin
    @Environment(\.mediaClient) private var client
    @Environment(AppModel.self) private var model

    @State private var player = VoiceNotePlayer()

    #if DEBUG
    /// `F33D3R_VOICE_AUTOPLAY=1` starts the first voice note that appears,
    /// for a run on a Simulator nobody can tap: the only way to see the
    /// credentialed playback path work is to watch the bar move.
    private static var debugAutoplayPending = ProcessInfo.processInfo.environment["F33D3R_VOICE_AUTOPLAY"] == "1"
    #endif

    var body: some View {
        HStack(spacing: F33Spacing.md) {
            Button(action: toggle) {
                ZStack {
                    Circle().fill(F33Color.accent)
                    if player.isLoading {
                        ProgressView().tint(F33Color.accentInk).scaleEffect(0.7)
                    } else {
                        Image(systemName: player.isPlaying ? "pause.fill" : "play.fill")
                            .font(.system(size: 13, weight: .semibold))
                            .foregroundStyle(F33Color.accentInk)
                            // The play glyph sits a hair right of centre by eye.
                            .offset(x: player.isPlaying ? 0 : 1)
                    }
                }
                .frame(width: 32, height: 32)
                .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
                .contentShape(Circle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel(player.isPlaying ? "Pause voice note" : "Play voice note")

            scrubber

            Text(timeLabel)
                .font(.system(size: 12).monospacedDigit())
                .foregroundStyle(F33Color.ink3)
                .accessibilityHidden(true)
        }
        .padding(.vertical, F33Spacing.xs)
        .padding(.horizontal, F33Spacing.sm)
        .background(F33Color.bgSunken, in: RoundedRectangle(cornerRadius: F33Radius.md))
        .padding(.top, F33Card.mediaTopInset)
        .frame(minHeight: F33Layout.minTouchTarget)
        .overlay(alignment: .bottomLeading) {
            if let problem = player.problem {
                Text(problem)
                    .font(.system(size: 11))
                    .foregroundStyle(F33Color.danger)
                    .lineLimit(1)
                    .padding(.horizontal, F33Spacing.sm)
                    .offset(y: 14)
            }
        }
        .animation(F33Motion.easeOut, value: player.isPlaying)
        .onChange(of: seed) { _, _ in player.stop() }
        .onDisappear { player.stop() }
        #if DEBUG
        .onAppear {
            if Self.debugAutoplayPending {
                Self.debugAutoplayPending = false
                toggle()
            }
        }
        #endif
        .accessibilityElement(children: .contain)
        .accessibilityLabel("Voice note, \(DurationClock.label(Int(total.rounded())))")
        .accessibilityValue(player.progress > 0 ? "\(DurationClock.label(Int(player.progress.rounded()))) elapsed" : "")
    }

    /// The bar: the played part in the accent, the rest faint. A tap moves
    /// there; a drag scrubs. The drag asks for a little movement first so a
    /// vertical scroll that starts on the bar is still a scroll.
    private var scrubber: some View {
        GeometryReader { geo in
            let width = max(1, geo.size.width)
            ZStack(alignment: .leading) {
                Capsule()
                    .fill(F33Color.ink5.opacity(0.5))
                    .frame(height: 3)
                Capsule()
                    .fill(F33Color.accent)
                    .frame(width: width * fraction, height: 3)
                Circle()
                    .fill(F33Color.accent)
                    .frame(width: 10, height: 10)
                    .offset(x: width * fraction - 5)
                    .opacity(player.progress > 0 || player.isPlaying ? 1 : 0)
            }
            .frame(maxHeight: .infinity, alignment: .center)
            .contentShape(Rectangle())
            .onTapGesture { location in
                seek(to: location.x / width)
            }
            .gesture(
                DragGesture(minimumDistance: 8, coordinateSpace: .local)
                    .onChanged { value in seek(to: value.location.x / width) }
            )
        }
        .frame(height: F33Layout.minTouchTarget - 12)
        .accessibilityElement()
        .accessibilityLabel("Position")
        .accessibilityValue(DurationClock.label(Int(player.progress.rounded())))
        .accessibilityAdjustableAction { direction in
            let step = max(1, total / 20)
            switch direction {
            case .increment: player.seek(to: player.progress + step)
            case .decrement: player.seek(to: player.progress - step)
            @unknown default: break
            }
        }
    }

    private var total: Double {
        player.duration > 0 ? player.duration : voice.durationSecs
    }

    private var fraction: CGFloat {
        guard total > 0 else { return 0 }
        return CGFloat(min(1, max(0, player.progress / total)))
    }

    /// The whole length at rest; elapsed over whole once it has moved, the way
    /// Voice Memos does it.
    private var timeLabel: String {
        let whole = DurationClock.label(Int(total.rounded()))
        guard player.isPlaying || player.progress > 0 else { return whole }
        return "\(DurationClock.label(Int(player.progress.rounded()))) / \(whole)"
    }

    private func toggle() {
        if player.isPlaying {
            player.pause()
            return
        }
        guard let url = URL.media(voice.url, origin: origin) else {
            player.report("This voice note has no address.")
            return
        }
        model.pauseMusic()
        player.play(url: url, client: client, durationHint: voice.durationSecs)
    }

    private func seek(to fraction: CGFloat) {
        guard total > 0 else { return }
        let seconds = Double(min(1, max(0, fraction))) * total
        if player.hasItem {
            player.seek(to: seconds)
        } else if let url = URL.media(voice.url, origin: origin) {
            // Scrubbing a note that has not started loads it there, paused.
            player.load(url: url, client: client, durationHint: voice.durationSecs, startingAt: seconds)
        }
    }
}

/// The player behind one voice note.
///
/// A class the view observes, because the `AVPlayer` and its observers have
/// to outlive any one body evaluation. `@MainActor` because everything it
/// publishes drives the card; AVFoundation's callbacks are asked for on the
/// main queue and hopped onto the actor from there.
@MainActor
@Observable
final class VoiceNotePlayer {
    private(set) var isPlaying = false
    private(set) var isLoading = false
    /// Seconds into the note.
    private(set) var progress: Double = 0
    /// Seconds long, once the item says; the work's number until then.
    private(set) var duration: Double = 0
    private(set) var problem: String?

    @ObservationIgnored private let player = AVPlayer()
    @ObservationIgnored private var loadedURL: URL?
    @ObservationIgnored private var timeObserver: Any?
    @ObservationIgnored private var endObserver: (any NSObjectProtocol)?
    @ObservationIgnored private var failObserver: (any NSObjectProtocol)?
    @ObservationIgnored private var loadTask: Task<Void, Never>?

    /// The note that is playing, so starting another can pause it. One
    /// output, one voice on it.
    private static weak var current: VoiceNotePlayer?

    init() {
        player.preventsDisplaySleepDuringVideoPlayback = false
    }

    var hasItem: Bool { loadedURL != nil }

    func report(_ message: String) {
        problem = message
    }

    /// Plays the note, loading it first when it is not the one loaded.
    func play(url: URL, client: APIClient?, durationHint: Double) {
        if loadedURL == url {
            resume()
            return
        }
        load(url: url, client: client, durationHint: durationHint, startingAt: 0, thenPlay: true)
    }

    /// Loads the note and leaves it paused at `startingAt`, or playing when
    /// `thenPlay`. The credential is fetched first and the item built once:
    /// an item built without the session would be refused before it played a
    /// note, and the failure would be reported for a request that was never
    /// the real one.
    func load(url: URL, client: APIClient?, durationHint: Double, startingAt: Double, thenPlay: Bool = false) {
        loadTask?.cancel()
        detachItemObservers()
        problem = nil
        isLoading = true
        isPlaying = false
        loadedURL = url
        duration = durationHint
        progress = max(0, startingAt)
        loadTask = Task { [weak self] in
            guard let self else { return }
            let credential = await client?.mediaCredential(for: url)
            guard !Task.isCancelled, self.loadedURL == url else { return }
            let item = AuthenticatedAsset.playerItem(url: url, credential: credential)
            self.player.replaceCurrentItem(with: item)
            self.attachItemObservers(item)
            self.installTimeObserverIfNeeded()
            if startingAt > 0 {
                await self.player.seek(to: CMTime(seconds: startingAt, preferredTimescale: 600), toleranceBefore: .zero, toleranceAfter: .zero)
            }
            guard !Task.isCancelled, self.loadedURL == url else { return }
            self.isLoading = false
            if thenPlay { self.resume() }
        }
    }

    func resume() {
        guard loadedURL != nil else { return }
        if let other = Self.current, other !== self { other.pause() }
        Self.current = self
        AudioSession.activateForPlayback()
        // A note that ran to its end starts over on the next play.
        if duration > 0, progress >= duration - 0.05 {
            seek(to: 0)
        }
        player.play()
        isPlaying = true
    }

    func pause() {
        player.pause()
        isPlaying = false
    }

    func seek(to seconds: Double) {
        let clamped = max(0, min(seconds, duration > 0 ? duration : seconds))
        progress = clamped
        player.seek(to: CMTime(seconds: clamped, preferredTimescale: 600), toleranceBefore: .zero, toleranceAfter: .zero)
    }

    /// Lets go of the item and the observers. The card is gone.
    func stop() {
        loadTask?.cancel()
        loadTask = nil
        player.pause()
        detachItemObservers()
        if let timeObserver {
            player.removeTimeObserver(timeObserver)
            self.timeObserver = nil
        }
        player.replaceCurrentItem(with: nil)
        loadedURL = nil
        isPlaying = false
        isLoading = false
        progress = 0
        if Self.current === self { Self.current = nil }
    }

    private func attachItemObservers(_ item: AVPlayerItem) {
        let center = NotificationCenter.default
        endObserver = center.addObserver(forName: .AVPlayerItemDidPlayToEndTime, object: item, queue: .main) { _ in
            MainActor.assumeIsolated { [weak self] in
                guard let self else { return }
                self.isPlaying = false
                self.progress = self.duration
            }
        }
        failObserver = center.addObserver(forName: .AVPlayerItemFailedToPlayToEndTime, object: item, queue: .main) { note in
            let reason = (note.userInfo?[AVPlayerItemFailedToPlayToEndTimeErrorKey] as? NSError)?.localizedDescription
            MainActor.assumeIsolated { [weak self] in
                self?.isPlaying = false
                self?.problem = reason ?? "The voice note stopped playing."
            }
        }
    }

    private func detachItemObservers() {
        if let endObserver { NotificationCenter.default.removeObserver(endObserver) }
        if let failObserver { NotificationCenter.default.removeObserver(failObserver) }
        endObserver = nil
        failObserver = nil
    }

    private func installTimeObserverIfNeeded() {
        guard timeObserver == nil else { return }
        let interval = CMTime(seconds: 0.25, preferredTimescale: 600)
        timeObserver = player.addPeriodicTimeObserver(forInterval: interval, queue: .main) { [weak self] time in
            MainActor.assumeIsolated {
                guard let self else { return }
                if time.seconds.isFinite { self.progress = time.seconds }
                if let itemDuration = self.player.currentItem?.duration, itemDuration.isNumeric {
                    let d = itemDuration.seconds
                    if d.isFinite, d > 0, abs(d - self.duration) > 0.5 { self.duration = d }
                }
                // A stall or a route change stops sound without a note; the
                // rate is the truth.
                let playing = self.player.timeControlStatus == .playing
                if playing != self.isPlaying, self.player.timeControlStatus != .waitingToPlayAtSpecifiedRate {
                    self.isPlaying = playing
                }
                if let error = self.player.currentItem?.error, self.problem == nil {
                    self.isPlaying = false
                    self.problem = error.localizedDescription
                }
            }
        }
    }
}
