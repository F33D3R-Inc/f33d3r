import SwiftUI
import F33D3RKit

/// The "there is nothing here" surface.
///
/// Every list gets one. A blank screen is indistinguishable from a screen that
/// failed to load, and the difference is exactly what the user needs to know.
struct EmptyStateView: View {
    let icon: String
    let title: String
    let message: String
    var actionTitle: String?
    var action: (() -> Void)?

    var body: some View {
        VStack(spacing: F33Spacing.md) {
            Image(systemName: icon)
                .font(.system(size: 34))
                .foregroundStyle(F33Color.ink5)

            Text(title)
                .font(.headline)
                .foregroundStyle(F33Color.ink2)

            Text(message)
                .font(.subheadline)
                .foregroundStyle(F33Color.ink4)
                .multilineTextAlignment(.center)

            if let actionTitle, let action {
                Button(actionTitle, action: action)
                    .font(.subheadline.weight(.semibold))
                    .foregroundStyle(F33Color.accent)
                    .frame(minHeight: F33Layout.minTouchTarget)
            }
        }
        .padding(F33Spacing.xl)
        .frame(maxWidth: .infinity)
        .padding(.top, F33Spacing.xxl)
    }
}

/// The failure surface, with the retry that re-fires the request that failed.
///
/// Mirrors the web's `inline_error`: a failed list is replaced by something you
/// can act on, never by a full-page error and never by a silent blank.
struct ErrorStateView: View {
    let error: APIError
    let retry: () -> Void

    var body: some View {
        VStack(spacing: F33Spacing.md) {
            Image(systemName: iconName)
                .font(.system(size: 34))
                .foregroundStyle(F33Color.ink5)

            Text(error.userMessage)
                .font(.subheadline)
                .foregroundStyle(F33Color.ink3)
                .multilineTextAlignment(.center)

            if error.isRateLimited {
                // Nantar answers 429 with Retry-After: 60. Offering an immediate
                // retry would just spend the next slot too.
                Text("Try again in a minute.")
                    .font(.footnote)
                    .foregroundStyle(F33Color.ink4)
            } else {
                Button("Try again", action: retry)
                    .font(.subheadline.weight(.semibold))
                    .foregroundStyle(F33Color.accent)
                    .frame(minHeight: F33Layout.minTouchTarget)
            }
        }
        .padding(F33Spacing.xl)
        .frame(maxWidth: .infinity)
        .padding(.top, F33Spacing.xxl)
    }

    private var iconName: String {
        switch error {
        case .transport: return "wifi.slash"
        case _ where error.isRateLimited: return "hourglass"
        default: return "exclamationmark.triangle"
        }
    }
}

/// The card-shaped placeholder shown while the first page loads.
///
/// Same geometry as a real card, so the list does not jump when content
/// replaces it. This is the `post-card--skeleton` modifier: the same facet in a
/// different state, not a different thing.
struct WorkCardSkeleton: View {
    @State private var isPulsing = false

    var body: some View {
        HStack(alignment: .top, spacing: F33Card.columnGap) {
            Circle()
                .fill(F33Color.ink5.opacity(0.3))
                .frame(width: F33Card.avatarColumnWidth, height: F33Card.avatarColumnWidth)

            VStack(alignment: .leading, spacing: F33Spacing.sm) {
                bar(width: 140, height: 13)
                bar(width: nil, height: 11)
                bar(width: 220, height: 11)
                RoundedRectangle(cornerRadius: F33Card.mediaCornerRadius)
                    .fill(F33Color.ink5.opacity(0.22))
                    .frame(height: 160)
            }
        }
        .padding(.top, F33Card.paddingTop)
        .padding(.horizontal, F33Card.paddingHorizontal)
        .padding(.bottom, F33Card.paddingBottom)
        .opacity(isPulsing ? 0.55 : 1)
        .animation(.easeInOut(duration: 0.9).repeatForever(autoreverses: true), value: isPulsing)
        .onAppear { isPulsing = true }
        .accessibilityHidden(true)
    }

    private func bar(width: CGFloat?, height: CGFloat) -> some View {
        RoundedRectangle(cornerRadius: 3)
            .fill(F33Color.ink5.opacity(0.3))
            .frame(width: width, height: height)
            .frame(maxWidth: width == nil ? .infinity : width, alignment: .leading)
    }
}

/// The hairline between cards. `.post-card { border-bottom: 1px solid }`.
struct CardDivider: View {
    var body: some View {
        Rectangle()
            .fill(F33Color.hairline)
            .frame(height: 1)
    }
}
