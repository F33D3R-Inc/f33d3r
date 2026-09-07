import AVKit
import MediaPlayer
import SwiftUI
import F33D3RKit

/// The full player: big artwork, the title and who made it, the scrubber, the
/// transport, and the work's own actions — like, tip, save, and buy when it is
/// for sale — then the route picker and the volume slider.
///
/// A sheet over whatever the reader was doing, so it takes the glass sheet
/// background and is dismissed by a swipe. Everything on it reads
/// `MusicPlayer` for the playhead and `AppModel.current(_:)` for the work: the
/// heart is red because the server's row says so, the price says "Owned"
/// because the server's row says so.
///
/// The scrubber is the one place the client holds a value the server does not
/// know — where the finger is while it drags. It lives in this view for the
/// length of the drag and is handed to the player on release.
struct MusicPlayerSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    /// Where the finger is while scrubbing. Nil when the slider follows the
    /// player.
    @State private var scrubPosition: TimeInterval?
    @State private var isTipping = false
    @State private var isBusy = false
    @State private var actionError: String?

    var body: some View {
        let player = model.musicPlayer()
        Group {
            if let loaded = player.nowPlaying {
                content(for: model.current(loaded), player: player)
            } else {
                // Only reachable if the track ended and the queue emptied while
                // the sheet was open; there is nothing to show.
                EmptyStateView(icon: "waveform", title: "Nothing playing", message: "Pick a track on Music.")
            }
        }
        .f33GlassSheet()
        .presentationDetents([.large])
        .presentationDragIndicator(.visible)
        .sheet(isPresented: $isTipping) {
            if let work = player.nowPlaying {
                TipSheet(handle: work.author.handle, work: model.current(work))
            }
        }
        .alert("That didn't go through", isPresented: Binding(get: { actionError != nil }, set: { if !$0 { actionError = nil } })) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(actionError ?? "")
        }
    }

    private func content(for work: Work, player: MusicPlayer) -> some View {
        GeometryReader { proxy in
            let artSize = min(proxy.size.width - F33Spacing.xxl * 2, 340)
            VStack(spacing: 0) {
                Spacer(minLength: F33Spacing.lg)

                MusicArtwork(work: work, size: artSize, cornerRadius: F33Radius.lg)
                    .shadow(color: .black.opacity(0.28), radius: 24, y: 12)
                    .scaleEffect(player.isPlaying ? 1 : 0.92)
                    .animation(F33Motion.spring, value: player.isPlaying)

                Spacer(minLength: F33Spacing.lg)

                VStack(spacing: F33Spacing.lg) {
                    titleRow(work)
                    scrubber(player)
                    transport(player)
                    actionRow(work)
                    HStack(spacing: F33Spacing.lg) {
                        VolumeSlider()
                            .frame(height: 32)
                        RoutePicker()
                            .frame(width: 32, height: 32)
                    }
                }
                .padding(.horizontal, F33Spacing.xxl)
                .padding(.bottom, F33Spacing.xl)

                if let problem = player.problem {
                    Text(problem)
                        .font(.footnote)
                        .foregroundStyle(F33Color.danger)
                        .multilineTextAlignment(.center)
                        .padding(.horizontal, F33Spacing.xxl)
                        .padding(.bottom, F33Spacing.lg)
                }
            }
            .frame(width: proxy.size.width)
        }
    }

    private func titleRow(_ work: Work) -> some View {
        HStack(alignment: .center, spacing: F33Spacing.md) {
            VStack(alignment: .leading, spacing: 3) {
                Text(work.trackTitle)
                    .font(.system(size: 20, weight: .bold))
                    .foregroundStyle(F33Color.ink)
                    .lineLimit(2)
                HStack(spacing: F33Spacing.xs) {
                    Text("@\(work.author.handle)")
                        .foregroundStyle(F33Color.accent)
                    if let badge = work.author.badge {
                        Text(badge.label)
                            .font(.system(size: 10, weight: .semibold))
                            .foregroundStyle(badge.tint)
                            .padding(.horizontal, 5)
                            .padding(.vertical, 1)
                            .background(badge.tint.opacity(0.12), in: Capsule())
                    }
                }
                .font(.system(size: 15))
                .lineLimit(1)
            }
            Spacer(minLength: 0)
            if work.isForSale {
                if work.purchasedByViewer {
                    PricePill(work: work)
                } else {
                    HStack(spacing: F33Spacing.sm) {
                        PricePill(work: work)
                        BuyButton(work: work)
                    }
                }
            }
        }
    }

    private func scrubber(_ player: MusicPlayer) -> some View {
        let duration = max(player.duration, 1)
        let shown = scrubPosition ?? player.progress
        return VStack(spacing: F33Spacing.xs) {
            Slider(
                value: Binding(
                    get: { min(shown, duration) },
                    set: { scrubPosition = $0 }
                ),
                in: 0...duration
            ) { editing in
                if !editing, let target = scrubPosition {
                    player.seek(to: target)
                    scrubPosition = nil
                }
            }
            .tint(F33Color.ink2)
            .accessibilityLabel("Playhead")
            .accessibilityValue(MusicTime.label(shown))

            HStack {
                Text(MusicTime.label(shown))
                Spacer()
                Text("-" + MusicTime.label(max(0, duration - shown)))
            }
            .font(.system(size: 12, weight: .medium).monospacedDigit())
            .foregroundStyle(F33Color.ink4)
        }
    }

    private func transport(_ player: MusicPlayer) -> some View {
        HStack(spacing: F33Spacing.xxl) {
            Button { player.previous() } label: {
                Image(systemName: "backward.fill")
                    .font(.system(size: 28, weight: .semibold))
                    .frame(width: 60, height: 60)
            }
            .accessibilityLabel("Previous track")

            Button { player.togglePlayPause() } label: {
                Image(systemName: player.isPlaying ? "pause.fill" : "play.fill")
                    .font(.system(size: 40, weight: .bold))
                    .frame(width: 76, height: 76)
                    .contentTransition(.symbolEffect(.replace))
            }
            .accessibilityLabel(player.isPlaying ? "Pause" : "Play")

            Button { player.next() } label: {
                Image(systemName: "forward.fill")
                    .font(.system(size: 28, weight: .semibold))
                    .frame(width: 60, height: 60)
                    .foregroundStyle(player.hasNext ? F33Color.ink : F33Color.ink5)
            }
            .disabled(!player.hasNext)
            .accessibilityLabel("Next track")
        }
        .buttonStyle(.plain)
        .foregroundStyle(F33Color.ink)
    }

    /// Like, tip and save on the track playing — the card's own controls, on
    /// the server's row, through the same model calls.
    private func actionRow(_ work: Work) -> some View {
        HStack(spacing: 0) {
            action(
                icon: work.likedByViewer ? "heart.fill" : "heart",
                label: Counts.label(work.likeCount) ?? "Like",
                tint: work.likedByViewer ? F33Action.like : F33Color.ink3,
                accessibility: work.likedByViewer ? "Unlike" : "Like"
            ) {
                perform { try await model.toggleLike(work) }
            }
            action(
                icon: "bolt.fill",
                label: work.tipLabel ?? "Tip",
                tint: F33Action.tip,
                accessibility: "Tip @\(work.author.handle)"
            ) {
                isTipping = true
            }
            .disabled(model.state.user?.user.handle == work.author.handle)
            action(
                icon: work.bookmarkedByViewer ? "bookmark.fill" : "bookmark",
                label: work.bookmarkedByViewer ? "Saved" : "Save",
                tint: work.bookmarkedByViewer ? F33Action.bookmark : F33Color.ink3,
                accessibility: work.bookmarkedByViewer ? "Remove from saved" : "Save"
            ) {
                perform { try await model.toggleBookmark(work) }
            }
        }
        .opacity(isBusy ? 0.6 : 1)
        .animation(F33Motion.easeOut, value: isBusy)
        // Felt when the server has answered: the trigger is the row it sent.
        .sensoryFeedback(.impact(weight: .light), trigger: work.likedByViewer)
        .sensoryFeedback(.selection, trigger: work.bookmarkedByViewer)
    }

    private func action(icon: String, label: String, tint: Color, accessibility: String, _ act: @escaping () -> Void) -> some View {
        Button(action: act) {
            VStack(spacing: 4) {
                Image(systemName: icon)
                    .font(.system(size: 20, weight: .medium))
                    .contentTransition(.symbolEffect(.replace))
                Text(label)
                    .font(.system(size: 12, weight: .medium).monospacedDigit())
                    .lineLimit(1)
            }
            .foregroundStyle(tint)
            .frame(maxWidth: .infinity)
            .frame(minHeight: F33Layout.minTouchTarget + 8)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel(accessibility)
    }

    private func perform(_ work: @escaping () async throws -> Void) {
        guard !isBusy else { return }
        isBusy = true
        Task {
            defer { isBusy = false }
            do {
                try await work()
                if let loaded = model.musicPlayer().nowPlaying {
                    model.musicPlayer().refresh(model.current(loaded))
                }
            } catch let apiError as APIError {
                actionError = apiError.userMessage
            } catch let malkuth as MalkuthError {
                actionError = malkuth.description
            } catch {
                actionError = error.localizedDescription
            }
        }
    }
}

// MARK: - System controls

/// AirPlay and Bluetooth routes. The system's picker, because the sheet it
/// shows is the one the reader already knows from every other player.
private struct RoutePicker: UIViewRepresentable {
    func makeUIView(context: Context) -> AVRoutePickerView {
        let view = AVRoutePickerView()
        view.prioritizesVideoDevices = false
        view.tintColor = UIColor(F33Color.ink3)
        view.activeTintColor = UIColor(F33Color.accent)
        return view
    }

    func updateUIView(_ uiView: AVRoutePickerView, context: Context) {}
}

/// The system volume, bound to the hardware level. `MPVolumeView` is the only
/// slider that moves the device's volume rather than a number of our own.
private struct VolumeSlider: UIViewRepresentable {
    func makeUIView(context: Context) -> MPVolumeView {
        let view = MPVolumeView()
        view.tintColor = UIColor(F33Color.ink2)
        return view
    }

    func updateUIView(_ uiView: MPVolumeView, context: Context) {}
}
