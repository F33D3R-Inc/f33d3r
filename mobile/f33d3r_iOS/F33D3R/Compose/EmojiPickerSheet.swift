import SwiftUI
import F33D3RKit

/// The composer's emoji grid.
///
/// Eight across like the web's popover, the same sixty in the same order, with
/// the ones this device used lately in a row above so the one from a minute
/// ago is one tap away. A tap inserts at the caret the composer had when the
/// sheet opened — the sheet itself takes the keyboard away, so the composer
/// remembers the spot rather than asking for it afterwards — and closes.
struct EmojiPickerSheet: View {
    let recent: RecentEmoji
    let pick: (String) -> Void

    @Environment(\.dismiss) private var dismiss

    private static let columns = Array(repeating: GridItem(.flexible(), spacing: 2), count: 8)

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: F33Spacing.md) {
                if !recent.items.isEmpty {
                    section("Recent", recent.items)
                }
                section("Emoji", ComposeEmoji.palette)
            }
            .padding(.horizontal, F33Spacing.lg)
            .padding(.top, F33Spacing.lg)
            .padding(.bottom, F33Spacing.xl)
        }
        .background(F33Color.bg)
        .presentationDetents([.height(360), .large])
        .presentationDragIndicator(.visible)
    }

    private func section(_ title: String, _ items: [String]) -> some View {
        VStack(alignment: .leading, spacing: F33Spacing.xs) {
            Text(title)
                .font(.system(size: 11, weight: .bold))
                .foregroundStyle(F33Color.ink4)
                .textCase(.uppercase)
                .kerning(0.6)
            LazyVGrid(columns: Self.columns, spacing: 2) {
                ForEach(items, id: \.self) { emoji in
                    Button {
                        pick(emoji)
                        dismiss()
                    } label: {
                        Text(emoji)
                            .font(.system(size: 26))
                            .frame(maxWidth: .infinity)
                            .frame(height: F33Layout.minTouchTarget)
                            .contentShape(RoundedRectangle(cornerRadius: F33Radius.xs))
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel(emoji)
                    .accessibilityHint("Inserts \(emoji)")
                }
            }
        }
    }
}
