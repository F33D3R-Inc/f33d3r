import SwiftUI
import F33D3RKit

/// The strip above the tab bar while something plays: artwork, title, artist,
/// play/pause and skip. Tapping anywhere else on it opens the full player.
///
/// It is chrome — a control floating over whatever tab the reader is on — so
/// it takes glass. On iOS 26 the system draws the glass for a tab bar
/// accessory and this draws only its contents; everywhere else the bar paints
/// its own, elevated, because it is genuinely floating over content.
///
/// The bar draws what `MusicPlayer` says and nothing more. It appears when a
/// track is loaded and leaves when there is none; it has no state of its own
/// beyond whether the sheet is open.
struct NowPlayingBar: View {
    enum Style {
        /// The system supplies the surface (iOS 26 `tabViewBottomAccessory`).
        case accessory
        /// This view supplies it.
        case floating
    }

    var style: Style = .floating
    /// The accessory's inline form, when the tab bar has minimised on scroll
    /// and the bar sits in line with it: no artwork, no skip.
    var isCompact = false

    @Environment(AppModel.self) private var model
    @State private var isShowingPlayer = false

    var body: some View {
        let player = model.musicPlayer()
        if let work = player.nowPlaying {
            content(for: model.current(work), player: player)
                .transition(.move(edge: .bottom).combined(with: .opacity))
                .sheet(isPresented: $isShowingPlayer) {
                    MusicPlayerSheet()
                }
                .onAppear {
                    #if DEBUG
                    if MusicDebug.openPlayer { isShowingPlayer = true }
                    #endif
                }
        }
    }

    private func content(for work: Work, player: MusicPlayer) -> some View {
        HStack(spacing: F33Spacing.md) {
            Button {
                isShowingPlayer = true
            } label: {
                HStack(spacing: F33Spacing.md) {
                    if !isCompact {
                        MusicArtwork(work: work, size: 40, cornerRadius: F33Radius.xs)
                    }
                    VStack(alignment: .leading, spacing: 1) {
                        Text(work.trackTitle)
                            .font(.system(size: 14, weight: .semibold))
                            .foregroundStyle(F33Color.ink)
                        if !isCompact {
                            Text("@\(work.author.handle)")
                                .font(.system(size: 12))
                                .foregroundStyle(F33Color.ink3)
                        }
                    }
                    .lineLimit(1)
                    Spacer(minLength: 0)
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Now playing: \(work.trackTitle) by @\(work.author.handle)")
            .accessibilityHint("Opens the player")

            Button {
                player.togglePlayPause()
            } label: {
                Image(systemName: player.isPlaying ? "pause.fill" : "play.fill")
                    .font(.system(size: 18, weight: .semibold))
                    .foregroundStyle(F33Color.ink)
                    .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
                    .contentTransition(.symbolEffect(.replace))
            }
            .buttonStyle(.plain)
            .accessibilityLabel(player.isPlaying ? "Pause" : "Play")

            if !isCompact {
                Button {
                    player.next()
                } label: {
                    Image(systemName: "forward.fill")
                        .font(.system(size: 17, weight: .semibold))
                        .foregroundStyle(player.hasNext ? F33Color.ink : F33Color.ink5)
                        .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
                }
                .buttonStyle(.plain)
                .disabled(!player.hasNext)
                .accessibilityLabel("Next track")
            }
        }
        .padding(.leading, isCompact ? F33Spacing.lg : F33Spacing.sm)
        .padding(.trailing, F33Spacing.xs)
        .padding(.vertical, isCompact ? 0 : F33Spacing.xs)
        .frame(minHeight: F33Layout.minTouchTarget)
        .modifier(BarSurface(style: style))
    }
}

/// Glass under the bar where the system does not draw it.
private struct BarSurface: ViewModifier {
    let style: NowPlayingBar.Style

    func body(content: Content) -> some View {
        switch style {
        case .accessory:
            content
        case .floating:
            content
                .f33Glass(in: RoundedRectangle(cornerRadius: F33Radius.md, style: .continuous), elevation: .floating)
                .padding(.horizontal, F33Spacing.sm)
                .padding(.bottom, F33Spacing.xs)
        }
    }
}

/// The bar as an iOS 26 tab bar accessory, in whichever form the tab bar is
/// in: expanded above it, or inline with it once the bar has minimised.
@available(iOS 26.0, *)
struct NowPlayingAccessory: View {
    @Environment(\.tabViewBottomAccessoryPlacement) private var placement

    var body: some View {
        NowPlayingBar(style: .accessory, isCompact: placement == .inline)
    }
}

/// Puts the now-playing bar on the shell.
///
/// iOS 26 gets the system's accessory slot, which floats the bar above the
/// tab bar and folds it in when the bar minimises. The slot is only claimed
/// while a track is loaded — from 26.1, through `isEnabled`, so an empty
/// accessory never reserves the space; on 26.0 the view is simply empty and
/// the slot collapses with it. Older systems inset the bar into the bottom of
/// every tab's content, which is where a mini player has always gone.
struct NowPlayingShell: ViewModifier {
    @Environment(AppModel.self) private var model

    func body(content: Content) -> some View {
        if #available(iOS 26.1, *) {
            content.tabViewBottomAccessory(isEnabled: model.musicPlayer().nowPlaying != nil) {
                NowPlayingAccessory()
            }
        } else if #available(iOS 26.0, *) {
            content.tabViewBottomAccessory {
                NowPlayingAccessory()
            }
        } else {
            content
        }
    }
}

/// The pre-26 half: the floating bar inset above the tab bar, inside a tab's
/// stack, so it sits over the content and under nothing.
struct LegacyNowPlayingInset: ViewModifier {
    func body(content: Content) -> some View {
        if #available(iOS 26.0, *) {
            content
        } else {
            content.safeAreaInset(edge: .bottom, spacing: 0) {
                NowPlayingBar(style: .floating)
            }
        }
    }
}

extension View {
    /// The now-playing bar on the tab shell. See `NowPlayingShell`.
    func nowPlayingShell() -> some View { modifier(NowPlayingShell()) }

    /// The now-playing bar inside one tab's stack, for systems before 26.
    func legacyNowPlayingInset() -> some View { modifier(LegacyNowPlayingInset()) }
}
