import SwiftUI
import F33D3RKit

/// The filled accent button. Mirrors `.btn-primary` on the web: full-width pill,
/// accent fill, white label, and a press state that scales rather than dims.
struct F33PrimaryButtonStyle: ButtonStyle {
    @Environment(\.isEnabled) private var isEnabled

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.headline)
            .foregroundStyle(F33Color.accentInk)
            .frame(maxWidth: .infinity)
            .frame(height: F33Layout.minTouchTarget + 6)
            .background(F33Color.accent, in: Capsule())
            .opacity(isEnabled ? 1 : 0.5)
            .scaleEffect(configuration.isPressed ? 0.97 : 1)
            .animation(F33Motion.spring, value: configuration.isPressed)
    }
}

/// A bordered text field matching the web's input treatment.
///
/// The well stays opaque even where it sits on a glass panel. A blur behind text
/// somebody is in the middle of typing is the exact case where the treatment
/// costs more legibility than it buys look, so the panel is glass and the field
/// on it is not. The hairline is the strong one for the same reason: on glass, a
/// 9%-black edge is not enough to say where the field starts.
struct F33FieldStyle: ViewModifier {
    func body(content: Content) -> some View {
        content
            .padding(.horizontal, F33Spacing.lg)
            .frame(height: F33Layout.minTouchTarget + 6)
            .background(F33Color.bgElevated, in: RoundedRectangle(cornerRadius: F33Radius.md))
            .overlay(
                RoundedRectangle(cornerRadius: F33Radius.md)
                    .strokeBorder(F33Color.hairlineStrong, lineWidth: 1)
            )
            .foregroundStyle(F33Color.ink)
    }
}

extension View {
    func f33Field() -> some View { modifier(F33FieldStyle()) }
}
