import SwiftUI
import F33D3RKit

/// One image from the media origin, with the three states it actually has.
///
/// It fetches through ``MediaLoader`` rather than `AsyncImage`, because F33D3R's
/// post media is walled: `/static/media/posts/…` is authorised against the work
/// that owns it, and `AsyncImage` has no way to carry a session. Every post
/// image came back `403` while avatars, which no work owns, loaded fine — the
/// exact split this replaces.
///
/// The three states are drawn deliberately. `AsyncImage` gives a blank rectangle
/// while loading and a broken glyph on failure, which reads as a bug in a feed.
/// This draws a settled placeholder for both, so a card's geometry never shifts
/// as images arrive and a dead URL looks deliberate rather than broken.
struct RemoteImage: View {
    let path: String?
    /// Seeds the placeholder gradient so a given image always fails to the same
    /// colour rather than flickering between renders.
    var seed: String = ""
    var contentMode: ContentMode = .fill

    @Environment(\.mediaOrigin) private var origin
    @Environment(\.mediaLoader) private var loader

    @State private var image: UIImage?

    private var url: URL? { .media(path, origin: origin) }

    var body: some View {
        content
            // Keyed on the URL: a recycled row pointing at a different image
            // reloads, and one pointing at the same image does not.
            .task(id: url) { await load() }
    }

    @ViewBuilder
    private var content: some View {
        if let image {
            let picture = Image(uiImage: image).resizable()
            if contentMode == .fill {
                // Laid out as the space it is given, not the size it is. A
                // filling image drawn directly reports its own pixel width as
                // its ideal, and a grid of them pushes the card wider than the
                // screen. Painted as an overlay on a clear box the image has no
                // say in layout at all, which is what "fill" means.
                Color.clear
                    .overlay { picture.aspectRatio(contentMode: .fill) }
                    .clipped()
            } else {
                picture.aspectRatio(contentMode: .fit)
            }
        } else {
            placeholder
        }
    }

    private func load() async {
        image = nil
        guard let url else { return }
        // A failure of any kind draws the placeholder. The distinction between
        // "forbidden" and "gone" matters to the wall, not to a feed row: both
        // mean this reader is not seeing this picture, and a row that explained
        // which would be explaining the wall to the wrong person.
        guard let data = try? await loader.data(for: url),
              let decoded = UIImage(data: data)
        else { return }
        guard !Task.isCancelled else { return }
        withAnimation(F33Motion.easeOut) { image = decoded }
    }

    /// A muted version of the avatar gradient. Recognisable as "an image goes
    /// here" without competing with the images that did load.
    private var placeholder: some View {
        let (start, end) = AvatarPalette.colors(for: seed.isEmpty ? (path ?? "") : seed)
        return LinearGradient(
            colors: [start.opacity(0.28), end.opacity(0.28)],
            startPoint: .topLeading,
            endPoint: .bottomTrailing
        )
        .overlay(F33Color.bgSunken.opacity(0.35))
    }
}
