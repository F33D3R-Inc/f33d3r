import SwiftUI
import F33D3RKit

/// The role chip beside a name — Founder, Admin, Government, Business, Creator,
/// Verified.
///
/// Exactly one is drawn, chosen by `F33Badge.resolve` in the web's precedence
/// order. The tinted-fill-plus-hairline treatment is `.role-badge`.
struct BadgePill: View {
    let badge: F33Badge

    var body: some View {
        Text(badge.label)
            .font(.system(size: 9.5, weight: .bold))
            .tracking(0.4)
            .textCase(.uppercase)
            .foregroundStyle(badge.tint)
            .padding(.horizontal, 6)
            .padding(.vertical, 2)
            .background(badge.tint.opacity(0.12), in: RoundedRectangle(cornerRadius: 4))
            .overlay(
                RoundedRectangle(cornerRadius: 4)
                    .strokeBorder(badge.tint.opacity(0.25), lineWidth: 1)
            )
            .fixedSize()
    }
}

/// The "18+" and "Sensitive" labels a work carries under its body.
struct ContentLabel: View {
    enum Kind {
        case nsfw
        case sensitive

        var label: String {
            switch self {
            case .nsfw: return "18+"
            case .sensitive: return "Sensitive"
            }
        }

        var tint: Color {
            switch self {
            case .nsfw: return F33Color.danger
            case .sensitive: return F33Color.warn
            }
        }
    }

    let kind: Kind

    var body: some View {
        Text(kind.label)
            .font(.system(size: 10, weight: .bold))
            .tracking(0.3)
            .foregroundStyle(kind.tint)
            .padding(.horizontal, 7)
            .padding(.vertical, 3)
            .background(kind.tint.opacity(0.12), in: Capsule())
            .fixedSize()
    }
}

/// The pinned-work marker at the top of a card on a profile.
struct PinnedLabel: View {
    var body: some View {
        Label("Pinned", systemImage: "pin.fill")
            .font(.system(size: 11, weight: .semibold))
            .foregroundStyle(F33Color.ink4)
            .padding(.bottom, 2)
    }
}
