import SwiftUI
import F33D3RKit

/// The banner over a screen that is drawing fixtures rather than the platform.
///
/// Sample mode exists so a screen can be looked at without a server, and every
/// other surface in the app is safe to look at that way because a fake work
/// looks like a work. A wallet is the exception: an amount on screen is a claim
/// about somebody's money, and a screenshot of one has no environment variable
/// attached to it. So anywhere sample data can reach a figure, this says so, in
/// the colour the product uses for warnings, above the figure and not below it.
struct SampleDataBanner: View {
    var body: some View {
        HStack(spacing: F33Spacing.sm) {
            Image(systemName: "exclamationmark.triangle.fill")
                .font(.system(size: 12, weight: .semibold))

            Text("Sample data. Not a real balance, not a real ledger.")
                .font(.system(size: 12, weight: .semibold))
                .fixedSize(horizontal: false, vertical: true)

            Spacer(minLength: 0)
        }
        .foregroundStyle(F33Color.warn)
        .padding(.horizontal, F33Card.paddingHorizontal)
        .padding(.vertical, F33Spacing.sm)
        .frame(maxWidth: .infinity)
        .background(F33Color.warn.opacity(0.12))
        .accessibilityElement(children: .combine)
    }
}
