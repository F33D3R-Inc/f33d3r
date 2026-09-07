import SwiftUI
import F33D3RKit

/// The blur-and-reveal gate over flagged media.
///
/// A port of `.nsfw-gate`. The reveal is per-work and per-session, held by the
/// caller, which matches the web's session-persisted behaviour: agreeing to see
/// one work does not silently agree to the next.
///
/// The gate covers the media only. Body text, the author row and the action bar
/// stay legible, because the point is to stop an image appearing unasked, not to
/// hide that the work exists.
///
/// The panel is glass. It is the one place in a card where the treatment is
/// unarguably right: it is floating over an image, the image is already blurred
/// to nothing legible, and the reader can still see there is a picture under
/// there without seeing the picture.
struct ContentGate<Content: View>: View {
    let isNSFW: Bool
    let isGore: Bool
    @Binding var isRevealed: Bool
    @ViewBuilder let content: () -> Content

    var body: some View {
        ZStack {
            content()
                .blur(radius: isRevealed ? 0 : 28)
                // Clipped so the blur cannot bleed past the media's corners.
                .clipShape(RoundedRectangle(cornerRadius: F33Card.mediaCornerRadius))
                .allowsHitTesting(isRevealed)

            if !isRevealed {
                gate
            }
        }
        .animation(F33Motion.easeOut, value: isRevealed)
    }

    private var gate: some View {
        VStack(spacing: F33Spacing.sm) {
            Image(systemName: "eye.slash.fill")
                .font(.system(size: 26))
                .foregroundStyle(F33Color.accent)

            Text(title)
                .font(.system(size: 15, weight: .bold))
                .foregroundStyle(F33Color.ink)

            Text(subtitle)
                .font(.system(size: 12))
                .foregroundStyle(F33Color.ink3)
                .multilineTextAlignment(.center)

            Button("View") { isRevealed = true }
                .font(.system(size: 12, weight: .semibold))
                .foregroundStyle(F33Color.accent)
                .padding(.horizontal, 14)
                .padding(.vertical, 6)
                .overlay(Capsule().strokeBorder(F33Color.accent, lineWidth: 1.5))
                .frame(minHeight: F33Layout.minTouchTarget)
        }
        .padding(.vertical, F33Spacing.xxl)
        .padding(.horizontal, F33Spacing.lg)
        .frame(maxWidth: .infinity)
        .f33Glass(in: RoundedRectangle(cornerRadius: F33Radius.md), tint: F33Color.accent.opacity(0.06))
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(title). \(subtitle)")
        .accessibilityAddTraits(.isButton)
        .accessibilityAction { isRevealed = true }
    }

    private var title: String { isNSFW ? "18+ content" : "Sensitive content" }

    private var subtitle: String {
        isNSFW
            ? "This work is marked adult. Tap to view."
            : "This work may be distressing. Tap to view."
    }
}

/// The paywall that replaces a subscriber-only work's content.
///
/// The card still shows who posted and when — a locked work is visible, just not
/// readable, which is what makes subscribing legible as an offer rather than a
/// dead end.
struct SubscriberGate: View {
    let handle: String

    var body: some View {
        HStack(spacing: F33Spacing.sm) {
            Image(systemName: "lock.fill")
                .font(.system(size: 13))
                .foregroundStyle(F33Color.accent)

            Text("Subscribers only")
                .font(.system(size: 13, weight: .semibold))
                .foregroundStyle(F33Color.ink2)

            Spacer(minLength: F33Spacing.sm)

            NavigationLink(value: Route.profile(handle: handle)) {
                Text("Subscribe")
                    .font(.system(size: 12, weight: .semibold))
                    .foregroundStyle(F33Color.accentInk)
                    .padding(.horizontal, 12)
                    .padding(.vertical, 6)
                    .background(F33Color.accent, in: Capsule())
            }
        }
        .padding(F33Spacing.md)
        .frame(maxWidth: .infinity)
        .background(F33Color.bgSunken, in: RoundedRectangle(cornerRadius: F33Radius.md))
        .overlay(
            RoundedRectangle(cornerRadius: F33Radius.md)
                .strokeBorder(F33Color.hairline, lineWidth: 1)
        )
    }
}

/// The tombstone that stands in for a work moderation has blocked.
///
/// Drawn rather than omitted: a reply thread with a hole in it reads as a bug,
/// and a blocked work still has replies hanging off it.
struct BlockedWorkNotice: View {
    var body: some View {
        HStack(spacing: F33Spacing.sm) {
            Image(systemName: "exclamationmark.octagon.fill")
                .foregroundStyle(F33Color.ink4)
            Text("This work was removed for breaking the rules.")
                .font(.footnote)
                .foregroundStyle(F33Color.ink3)
            Spacer(minLength: 0)
        }
        .padding(F33Spacing.md)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(F33Color.bgSunken, in: RoundedRectangle(cornerRadius: F33Radius.md))
    }
}
