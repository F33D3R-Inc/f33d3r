import SwiftUI
import F33D3RKit

/// A tag's timeline: every live work carrying it, newest first.
///
/// `WorkList` over the tag surface, like every other list. The header says
/// what the list is and how it is ordered, because a tag page that looks like
/// a feed invites the assumption that it is ranked, and it is not.
struct TagView: View {
    let tag: String

    @Environment(AppModel.self) private var model

    var body: some View {
        WorkList(
            feed: model.tagFeed(tag: tag),
            currentUser: model.state.user,
            emptyState: EmptyStateView(
                icon: "number",
                title: "Nothing tagged #\(tag) yet",
                message: "Works that carry this tag collect here as they are posted."
            )
        ) {
            header
        }
        .navigationTitle("#\(tag)")
        .navigationBarTitleDisplayMode(.inline)
    }

    private var header: some View {
        HStack(alignment: .top, spacing: F33Spacing.sm) {
            Image(systemName: "number")
                .font(.system(size: 11, weight: .semibold))
                .foregroundStyle(F33Color.accent)
                .padding(.top, 1)
            Text("Every work tagged #\(tag), newest first. Nothing here is ranked.")
                .font(.system(size: 12))
                .foregroundStyle(F33Color.ink4)
                .fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 0)
        }
        .padding(.horizontal, F33Card.paddingHorizontal)
        .padding(.vertical, F33Spacing.sm)
        .background(F33Color.bg)
        .overlay(alignment: .bottom) { CardDivider() }
        .accessibilityElement(children: .combine)
    }
}
