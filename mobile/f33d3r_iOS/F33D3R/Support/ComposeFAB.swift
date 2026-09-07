import SwiftUI
import F33D3RKit

/// The compose button, floating over the feed.
///
/// 52pt, bottom-trailing, above the tab bar's safe area rather than inside it —
/// a compose control that overlaps navigation is a control people hit by
/// accident on the way to another tab.
///
/// It takes the glass treatment over an accent tint: the button has to stay
/// legible over whatever is scrolling beneath it, and a flat fill against a
/// bright photo is the case where it stops being. It is the one control in the
/// app that is unambiguously floating, so it is the one that takes the full
/// treatment — tinted, elevated and interactive.
struct ComposeFAB: View {
    let action: () -> Void

    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        Button(action: action) {
            Image(systemName: "plus")
                .font(.system(size: 22, weight: .semibold))
                .foregroundStyle(F33Color.accentInk)
                .frame(width: 52, height: 52)
                // The glyph carries its own shadow because the tint under glass
                // is translucent by design: over a bright photo the plus would
                // otherwise be white on near-white.
                .shadow(color: .black.opacity(0.28), radius: 3, y: 1)
                // The whole circle is the button, not just the glyph — see the
                // hit-testing note in `GlassSurface`, which is where that is
                // now guaranteed for every interactive glass control.
                .f33Glass(
                    in: Circle(),
                    tint: F33Color.accent,
                    elevation: .floating,
                    interactive: true
                )
        }
        .buttonStyle(FABPressStyle(reduceMotion: reduceMotion))
        .padding(.trailing, F33Spacing.lg)
        .padding(.bottom, F33Spacing.lg)
        .accessibilityLabel("Compose a work")
    }
}

/// A press that scales rather than dims, and does neither when the reader has
/// asked for less movement.
private struct FABPressStyle: ButtonStyle {
    let reduceMotion: Bool

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .scaleEffect(configuration.isPressed && !reduceMotion ? 0.92 : 1)
            .opacity(configuration.isPressed && reduceMotion ? 0.7 : 1)
            .animation(F33Motion.spring, value: configuration.isPressed)
    }
}
