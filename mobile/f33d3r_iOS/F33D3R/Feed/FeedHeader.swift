import SwiftUI
import F33D3RKit

/// The row above the lanes: wordmark and account.
///
/// It replaces the navigation bar on Home rather than sitting under one. A
/// system bar here would cost 44pt to say "Home" to someone who just opened the
/// app on the home screen, and the wordmark says it better.
///
/// It is glass, and it runs out under the status bar rather than stopping at the
/// safe area — a chrome slab that ends in a straight line halfway up the top
/// inset reads as a mistake, and the status bar is part of the same surface.
/// The hairline that closes the slab belongs to the lane strip below, so the two
/// read as one bar rather than two.
struct FeedHeaderRow: View {
    let account: User?

    var body: some View {
        HStack(spacing: F33Spacing.sm) {
            BrandWordmark()

            Spacer(minLength: F33Spacing.sm)

            accountButton
        }
        .padding(.horizontal, F33Spacing.lg)
        .padding(.vertical, F33Spacing.sm)
        .frame(maxWidth: .infinity, alignment: .leading)
        .f33GlassBar(extending: .top)
    }

    @ViewBuilder
    private var accountButton: some View {
        if let account {
            NavigationLink(value: Route.profile(handle: account.handle)) {
                F33Avatar(user: account, size: 30)
                    .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
                    .contentShape(Circle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Your profile, @\(account.handle)")
        } else {
            // Holds the row's shape while the session restores, so the wordmark
            // does not slide sideways a moment after launch.
            Color.clear
                .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
        }
    }
}

