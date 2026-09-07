import SwiftUI
import F33D3RKit

/// The F33D3R mark and wordmark.
///
/// The supplied artwork comes as a horizontal lockup — mark, rule, wordmark, all
/// gold on a near-black navy plate — and as the mark alone on transparency. The
/// lockup is not what goes in a header: it is an opaque plate, so on the light
/// theme it would land as a black brick beside our own paper background, and at
/// the height a header row allows the wordmark inside it would be roughly six
/// points tall. So the header takes the transparent mark and sets "F33D3R"
/// beside it in our own type, which keeps the letters legible, lets them follow
/// the theme, and lets them scale with Dynamic Type. The lockup keeps its own
/// job: places where a dark plate is the design, which is nowhere in 3a.
///
/// The mark ships as a template image so it can carry `brandGold`, which is the
/// artwork's own gold in dark mode and a darkened one on the light background
/// the sampled colour disappears against.
struct BrandMark: View {
    var size: CGFloat = 26

    var body: some View {
        Image(.brandMark)
            .resizable()
            .renderingMode(.template)
            .aspectRatio(contentMode: .fit)
            .frame(width: size, height: size)
            .foregroundStyle(F33Color.brandGold)
            .accessibilityHidden(true)
    }
}

/// Mark plus wordmark, as the feed header and the sign-in screen show it.
struct BrandWordmark: View {
    var markSize: CGFloat = 26
    var textSize: CGFloat = 19

    var body: some View {
        HStack(spacing: 7) {
            BrandMark(size: markSize)

            Text("F33D3R")
                .font(.system(size: textSize, weight: .bold, design: .rounded))
                // Tracking is what makes a six-character wordmark read as a
                // wordmark rather than as a shouted word.
                .tracking(1.4)
                .foregroundStyle(F33Color.ink)
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("F33D3R")
        .accessibilityAddTraits(.isHeader)
    }
}
