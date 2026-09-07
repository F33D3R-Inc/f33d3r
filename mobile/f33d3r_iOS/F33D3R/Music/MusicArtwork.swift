import SwiftUI
import F33D3RKit

/// A track's cover: the first image attached to the work, else the author's
/// palette with a waveform on it.
///
/// Works have no cover field, so the first attached image stands in — the
/// place an author would put one. A track with no image is not drawn as a
/// broken image or a grey square: it takes the same handle-seeded gradient the
/// avatar does, so a creator's untitled tracks look like theirs at a glance
/// and the tile is never blank.
///
/// Content, not chrome. It never takes glass; the now-playing bar the small
/// version sits in is what takes the glass.
struct MusicArtwork: View {
    let work: Work
    var size: CGFloat
    var cornerRadius: CGFloat = F33Radius.sm

    var body: some View {
        ZStack {
            gradient
            if let path = work.artworkPath {
                RemoteImage(path: path, seed: work.author.handle)
            } else {
                Image(systemName: "waveform")
                    .font(.system(size: size * 0.34, weight: .medium))
                    .foregroundStyle(.white.opacity(0.9))
            }
        }
        .frame(width: size, height: size)
        .clipShape(RoundedRectangle(cornerRadius: cornerRadius, style: .continuous))
        .overlay(
            RoundedRectangle(cornerRadius: cornerRadius, style: .continuous)
                .strokeBorder(F33Color.hairline, lineWidth: 0.5)
        )
        .accessibilityHidden(true)
    }

    private var gradient: some View {
        let (start, end) = AvatarPalette.colors(for: work.author.handle)
        return LinearGradient(colors: [start, end], startPoint: .topLeading, endPoint: .bottomTrailing)
    }
}

/// The section title on the Music surface — "New", "Store", "Tracks" — with
/// an optional line under it saying what the section selects.
struct MusicSectionHeader: View {
    let title: String
    var subtitle: String?

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(title)
                .font(.system(size: 22, weight: .bold))
                .foregroundStyle(F33Color.ink)
            if let subtitle {
                Text(subtitle)
                    .font(.system(size: 13))
                    .foregroundStyle(F33Color.ink4)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(.horizontal, F33Card.paddingHorizontal)
        .padding(.top, F33Spacing.xl)
        .padding(.bottom, F33Spacing.md)
        .accessibilityAddTraits(.isHeader)
    }
}

/// The equaliser-style mark beside the track that is playing. Three bars that
/// move while sound is coming out and hold still when it is paused, so the row
/// says both "this one" and "right now".
struct NowPlayingGlyph: View {
    let isPlaying: Bool

    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @State private var phase = false

    var body: some View {
        HStack(alignment: .bottom, spacing: 2) {
            ForEach(0..<3, id: \.self) { index in
                RoundedRectangle(cornerRadius: 1)
                    .fill(F33Color.accent)
                    .frame(width: 3, height: height(index))
            }
        }
        .frame(width: 16, height: 16, alignment: .bottom)
        .onAppear { if isPlaying, !reduceMotion { phase = true } }
        .onChange(of: isPlaying) { _, playing in phase = playing && !reduceMotion }
        .animation(phase ? .easeInOut(duration: 0.5).repeatForever(autoreverses: true) : .default, value: phase)
        .accessibilityLabel(isPlaying ? "Now playing" : "Paused")
    }

    private func height(_ index: Int) -> CGFloat {
        let rest: [CGFloat] = [6, 10, 7]
        let peak: [CGFloat] = [14, 6, 12]
        return phase ? peak[index] : rest[index]
    }
}
