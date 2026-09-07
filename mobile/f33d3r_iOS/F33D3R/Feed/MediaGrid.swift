import SwiftUI
import F33D3RKit

/// The image grid on a work card.
///
/// Layouts are ported from `.media-img-group[data-count="N"]` in styles.css:
/// one image is centred and contained so a portrait shot stays narrow rather
/// than being cropped square; two sit side by side at 4:3; three are a tall
/// 4:5 on the left with two 4:3 stacked beside it; four are a 1:1 grid. Beyond
/// four the last tile carries an overflow count, which is the mobile rule from
/// CLAUDE.md — the web stops at four because it never has to fit five in 375pt.
struct MediaGrid: View {
    let mediaURLs: [String]
    /// Seeds each tile's placeholder so a failed load is stable, not random.
    let seed: String
    /// The ceiling on a lone image, which the card density decides — the same
    /// photo is allowed less of a compact screen than of a full one.
    var singleImageMaxHeight: CGFloat = F33Card.singleImageMaxHeight
    var onTap: ((Int) -> Void)?
    /// A double tap anywhere on the media. The card uses it to like.
    var onDoubleTap: (() -> Void)?
    /// Pairs each tile with the viewer it opens, so the picture zooms out of
    /// the tile and back into it. The card owns the namespace and hands the
    /// same one to `MediaViewerView`.
    var transitionNamespace: Namespace.ID?

    /// How many tiles the grid draws. Everything past this shares the last
    /// tile, which carries the overflow count — and which the viewer zooms
    /// back into for any page beyond the fourth.
    static let maxTiles = 4

    private var visible: [String] { Array(mediaURLs.prefix(Self.maxTiles)) }
    private var overflow: Int { max(0, mediaURLs.count - Self.maxTiles) }

    var body: some View {
        switch visible.count {
        case 0:
            EmptyView()
        case 1:
            single
        case 2:
            pair
        case 3:
            triple
        default:
            quad
        }
    }

    // A lone image keeps its own proportions — `object-fit: contain` on the web —
    // so a screenshot is not cropped to a shape it was never composed for.
    private var single: some View {
        tile(0, contentMode: .fit)
            .frame(maxWidth: .infinity)
            // A floor as well as a ceiling. A lone image is the one layout with
            // no aspect ratio imposed on it, so before it loads — or when it
            // never does — it has no height at all and the card collapses to a
            // coloured line where a photograph should be.
            .frame(minHeight: 200, maxHeight: singleImageMaxHeight)
            .clipShape(RoundedRectangle(cornerRadius: F33Card.mediaCornerRadius))
    }

    private var pair: some View {
        HStack(spacing: F33Card.mediaTileGap) {
            tile(0).aspectRatio(4.0 / 3.0, contentMode: .fill)
            tile(1).aspectRatio(4.0 / 3.0, contentMode: .fill)
        }
        .clipped()
        .clipShape(RoundedRectangle(cornerRadius: F33Card.mediaCornerRadius))
    }

    private var triple: some View {
        HStack(spacing: F33Card.mediaTileGap) {
            tile(0).aspectRatio(4.0 / 5.0, contentMode: .fill)
            VStack(spacing: F33Card.mediaTileGap) {
                tile(1)
                tile(2)
            }
        }
        .aspectRatio(8.0 / 5.0, contentMode: .fit)
        .clipShape(RoundedRectangle(cornerRadius: F33Card.mediaCornerRadius))
    }

    private var quad: some View {
        VStack(spacing: F33Card.mediaTileGap) {
            HStack(spacing: F33Card.mediaTileGap) {
                tile(0)
                tile(1)
            }
            HStack(spacing: F33Card.mediaTileGap) {
                tile(2)
                overflow > 0 ? AnyView(overflowTile) : AnyView(tile(3))
            }
        }
        .aspectRatio(1, contentMode: .fit)
        .clipShape(RoundedRectangle(cornerRadius: F33Card.mediaCornerRadius))
    }

    private var overflowTile: some View {
        tile(3)
            .overlay(
                ZStack {
                    Color.black.opacity(0.55)
                    Text("+\(overflow)")
                        .font(.title2.weight(.semibold))
                        .foregroundStyle(.white)
                }
            )
    }

    private func tile(_ index: Int, contentMode: ContentMode = .fill) -> some View {
        RemoteImage(path: visible[index], seed: "\(seed)-\(index)", contentMode: contentMode)
            .frame(maxWidth: .infinity, maxHeight: .infinity)
            .clipped()
            .modifier(ZoomTransitionSource(id: index, namespace: transitionNamespace))
            .contentShape(Rectangle())
            // Double first, so a single tap waits for the second before it opens
            // the viewer, and a double never opens it at all.
            .onTapGesture(count: 2) { onDoubleTap?() }
            .onTapGesture { onTap?(index) }
            .accessibilityLabel(accessibilityLabel(index))
            .accessibilityAddTraits(.isImage)
    }

    /// The API carries no alt text yet, so the label says position rather than
    /// pretending to describe the picture. When `alt` lands on the DTO it
    /// replaces this — silence would be worse, an invented description worse
    /// still.
    private func accessibilityLabel(_ index: Int) -> String {
        mediaURLs.count == 1
            ? "Image"
            : "Image \(index + 1) of \(mediaURLs.count)"
    }
}
