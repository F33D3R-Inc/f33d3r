import SwiftUI
import AVKit
import F33D3RKit

/// The inline HLS player on a work card.
///
/// Given the clean master stream — the moving-watermark rendition is for files
/// that leave the platform, never for the in-app player. Behaviour follows the
/// product rule in CLAUDE.md: autoplay muted when the tile comes into view, tap
/// to unmute, tap the expand control for the system's full-screen player with
/// scrubbing, Picture in Picture and AirPlay. The full-screen player is handed
/// this tile's player, so the picture continues rather than starting over,
/// and the tile has it back when the screen closes.
///
/// Playback is torn down when the tile leaves the screen. A feed that keeps
/// every player it has ever built alive will exhaust the decoder on a real
/// device long before it exhausts memory. The one exception is a player the
/// system is showing — full screen or floating — which the tile leaves alone
/// until it is given back.
struct WorkVideoView: View {
    let video: WorkVideo
    let seed: String
    /// The card density's ceiling on a portrait clip. Landscape video takes the
    /// column it is given; portrait would take the whole screen without this.
    var portraitMaxWidth: CGFloat = F33Card.portraitVideoMaxWidth

    #if DEBUG
    /// Opens the system player as soon as the tile appears, for a run that
    /// cannot tap the expand control.
    var debugOpensFullScreen = false

    /// `F33D3R_VIDEO_URL=<hls url>` stands a video in for a work's images on
    /// its detail page, because no work in this deployment carries one yet and
    /// a player nobody can reach is a player that ships unlooked-at.
    static let debugVideo: WorkVideo? = ProcessInfo.processInfo.environment["F33D3R_VIDEO_URL"]
        .flatMap { $0.isEmpty ? nil : WorkVideo(masterURL: $0) }
    #endif

    @Environment(\.mediaOrigin) private var origin
    @Environment(\.mediaClient) private var client
    @Environment(\.scenePhase) private var scenePhase

    @State private var player: AVPlayer?
    /// The loop observer, kept so it leaves with the player it loops.
    @State private var loopObserver: (any NSObjectProtocol)?
    @State private var isMuted = true
    @State private var isVisible = false
    @State private var isFullScreen = false
    @State private var isInPictureInPicture = false

    /// Whether AVKit, not the tile, is showing the picture right now.
    private var isHandedToSystem: Bool { isFullScreen || isInPictureInPicture }

    var body: some View {
        content
            .aspectRatio(video.aspectRatio, contentMode: .fit)
            .frame(maxWidth: video.isPortrait ? portraitMaxWidth : .infinity)
            // The letterbox and the rounded corner belong to the player, not to
            // the column it is centred in. Backgrounding after the centring
            // frame paints a full-width black plate around a narrow portrait
            // clip, which reads as a video that failed to load.
            .background(Color.black)
            .clipShape(RoundedRectangle(cornerRadius: F33Card.mediaCornerRadius))
            // The presentation is made from here. The view draws nothing and
            // takes no touches; it is a place in the hierarchy, not a control.
            .background {
                SystemVideoPlayer(player: player, isPresented: $isFullScreen, isInPictureInPicture: $isInPictureInPicture)
                    .allowsHitTesting(false)
            }
            .frame(maxWidth: .infinity, alignment: .center)
            // The card sits in a LazyVStack, so appear/disappear track the
            // render window closely enough to drive playback. iOS 18's
            // onScrollVisibilityChange is the precise tool for this and is
            // worth adopting when the deployment target moves off 17.
            .onAppear {
                isVisible = true
                start()
                #if DEBUG
                if debugOpensFullScreen { openFullScreen() }
                #endif
            }
            .onDisappear {
                isVisible = false
                if !isHandedToSystem { stop() }
            }
            .onChange(of: scenePhase) { _, phase in
                // Backgrounding does not fire a disappear, so audio would
                // otherwise keep playing over whatever the user switched to.
                // While the system has the picture — full screen, or floating
                // — AVKit owns that decision.
                guard !isHandedToSystem else { return }
                if phase != .active {
                    player?.pause()
                } else if isVisible {
                    // A call, an alarm or another app deactivates the session
                    // while we are away, and nothing hands it back: it has to
                    // be asked for again or the picture returns without sound.
                    if !isMuted { AudioSession.activateForPlayback() }
                    player?.play()
                }
            }
            .onChange(of: isHandedToSystem) { _, handed in
                // Given back to a tile that has since scrolled away, the player
                // is torn down as it would have been at the time.
                if !handed, !isVisible { stop() }
            }
    }

    @ViewBuilder
    private var content: some View {
        ZStack {
            if let player {
                VideoPlayer(player: player)
                    .disabled(true)
            } else {
                RemoteImage(path: video.posterURL, seed: seed)
            }

            controls
        }
        .contentShape(Rectangle())
        .onTapGesture {
            // The first tap on a muted autoplaying video should turn the sound
            // on, which is what the viewer almost always means by tapping it.
            toggleMute()
        }
    }

    /// Pinned to the dark scheme, and deliberately.
    ///
    /// Glass takes its appearance from the environment, not from the pixels
    /// behind it. On a light page these controls would render as pale glass
    /// carrying white glyphs — over a bright frame that is unreadable. Media
    /// controls are dark on every Apple surface for this reason.
    private var controls: some View {
        VStack {
            Spacer()
            HStack(spacing: F33Spacing.sm) {
                pill(systemImage: isMuted ? "speaker.slash.fill" : "speaker.wave.2.fill") {
                    toggleMute()
                }
                .accessibilityLabel(isMuted ? "Unmute" : "Mute")

                if video.durationSecs > 0 {
                    Text(durationLabel)
                        .font(.system(size: 11, weight: .semibold).monospacedDigit())
                        .foregroundStyle(.white)
                        .padding(.horizontal, 7)
                        .padding(.vertical, 4)
                        .f33Glass(in: Capsule(), elevation: .floating)
                }

                Spacer()

                pill(systemImage: "arrow.up.left.and.arrow.down.right") {
                    openFullScreen()
                }
                .accessibilityLabel("Full screen")
            }
            .padding(F33Spacing.sm)
        }
        .environment(\.colorScheme, .dark)
    }

    private func pill(systemImage: String, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            Image(systemName: systemImage)
                .font(.system(size: 12, weight: .semibold))
                .foregroundStyle(.white)
                .frame(width: 28, height: 28)
                .f33Glass(in: Circle(), elevation: .floating, interactive: true)
        }
        // The drawn pill is 28pt for the look the web has; the tappable area is
        // padded out to the 44pt minimum.
        .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
        .contentShape(Rectangle())
    }

    private var durationLabel: String {
        let total = Int(video.durationSecs.rounded())
        return String(format: "%d:%02d", total / 60, total % 60)
    }

    private var streamURL: URL? {
        .media(video.masterURL, origin: origin)
    }

    // MARK: Actions

    private func toggleMute() {
        isMuted.toggle()
        // Sound was asked for: the session has to be one that plays it.
        if !isMuted { AudioSession.activateForPlayback() }
        player?.isMuted = isMuted
    }

    /// Hands the running player to the system's full-screen player. The
    /// reader asked for the picture big; they will want to hear it too.
    private func openFullScreen() {
        if player == nil { start() }
        AudioSession.activateForPlayback()
        isMuted = false
        player?.isMuted = false
        isFullScreen = true
    }

    // MARK: Playback

    private func start() {
        guard player == nil, let streamURL else { return }
        Task { await startPlayer(at: streamURL) }
    }

    /// Built asynchronously because the session has to be read before the first
    /// request goes out: a walled work's playlist is `403` without it, and
    /// AVFoundation gives no second chance to authorise an asset already loading.
    private func startPlayer(at streamURL: URL) async {
        guard player == nil else { return }
        let credential = await client?.mediaCredential(for: streamURL)
        guard player == nil else { return }
        // Whoever is about to make sound says so first. A tile that starts
        // muted does not, and does not take the route from whatever the reader
        // is listening to.
        if !isMuted { AudioSession.activateForPlayback() }
        let new = AVPlayer(playerItem: AuthenticatedAsset.playerItem(url: streamURL, credential: credential))
        new.isMuted = isMuted
        // Feed video loops the way the web's `<video loop>` does, so a short
        // clip does not end on a frozen frame.
        new.actionAtItemEnd = .none
        loopObserver = NotificationCenter.default.addObserver(
            forName: .AVPlayerItemDidPlayToEndTime,
            object: new.currentItem,
            queue: .main
        ) { _ in
            new.seek(to: .zero)
            new.play()
        }
        player = new
        new.play()
    }

    private func stop() {
        player?.pause()
        if let loopObserver {
            NotificationCenter.default.removeObserver(loopObserver)
        }
        loopObserver = nil
        player = nil
    }
}
