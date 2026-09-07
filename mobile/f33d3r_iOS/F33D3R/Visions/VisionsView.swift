import SwiftUI
import F33D3RKit

/// The lane of works that carry a picture or a video, from the people the
/// reader already chose.
///
/// **This screen shares a name with something else and should not.** The Vision
/// tray under the Home header is the 24-hour lane — `Vision`, `VisionRing`,
/// `VisionTrayStore` — and that is what the platform now means by the word. This
/// screen is a feed of ordinary, permanent Works that happen to carry media: the
/// server's `visions` feed surface, which kept the name from when an ephemeral
/// post was a `kind` on the works table. Nothing here expires. Some works still
/// carry an `expires_at` and the card counts those down where it finds one, but
/// the surface is not built on expiry, so this screen never tells anyone their
/// posts are about to disappear.
///
/// Renaming it is a product decision, not a mechanical one, so it is left as it
/// stands and stated here instead of being guessed at. The lane's wire value is
/// `surface=visions`, which the server still answers to whatever this is called.
///
/// It is a `WorkList` like everything else. A media surface invites a masonry
/// grid, and it is the wrong instinct here twice over: the DTO carries no image
/// dimensions, so a grid would have to impose a ratio on every photo — the
/// forced-9:16 crop the whole card design exists to avoid — and a work is not a
/// tile. A photograph posted with three paragraphs under it is still a work, and
/// cropping it to a square throws away the half the author wrote.
struct VisionsView: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        WorkList(
            feed: model.homeFeed(surface: .visions),
            currentUser: model.state.user,
            emptyState: emptyState,
            // Pinned to the scroll view, like Home's, so the works pass behind
            // the glass instead of stopping at it. The note below scrolls: it is
            // read once, and a sentence that never leaves is a sentence in the
            // way.
            topChrome: AnyView(SurfaceHeaderRow(title: "Visions"))
        ) {
            VisionsSurfaceNote()
        }
        .toolbar(.hidden, for: .navigationBar)
    }

    private var emptyState: EmptyStateView {
        EmptyStateView(
            icon: "circle.dashed",
            title: "Nothing to see yet",
            message: """
                Works with a photo or a video, from you and the accounts you follow, \
                collect here. Follow a few people and this fills up.
                """
        )
    }
}

/// What the Visions surface is, in one line. Same rule as the other surfaces:
/// a list somebody else's definition assembled says whose definition it was.
struct VisionsSurfaceNote: View {
    var body: some View {
        HStack(alignment: .top, spacing: F33Spacing.sm) {
            Image(systemName: "photo.on.rectangle.angled")
                .font(.system(size: 11, weight: .semibold))
                .foregroundStyle(F33Color.accent)
                .padding(.top, 1)

            Text("Photos and video from you and the accounts you follow, newest first.")
                .font(.system(size: 12))
                .foregroundStyle(F33Color.ink4)
                .fixedSize(horizontal: false, vertical: true)

            Spacer(minLength: 0)
        }
        .padding(.horizontal, F33Card.paddingHorizontal)
        .padding(.top, F33Spacing.sm)
        .padding(.bottom, F33Spacing.sm)
        .background(F33Color.bg)
        .overlay(alignment: .bottom) { CardDivider() }
        .accessibilityElement(children: .combine)
    }
}

#Preview("Visions") {
    NavigationStack {
        VisionsView()
    }
    .environment(AppModel())
}
