import SwiftUI
import F33D3RKit

/// The chat overlay in a room: the last few lines as pills, newest at the
/// bottom, each one a handle in bold and the words after it. A tip is the
/// same pill with the amount and a bolt.
struct LiveChatBubbles: View {
    let messages: [LiveChatMessage]
    var maxLines = 6

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            ForEach(messages.suffix(maxLines)) { message in
                bubble(message)
                    .transition(.move(edge: .bottom).combined(with: .opacity))
            }
        }
        .animation(F33Motion.easeOut, value: messages.count)
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    @ViewBuilder
    private func bubble(_ message: LiveChatMessage) -> some View {
        let handle = message.author?.handle ?? "f33d3r"
        Group {
            if message.isTip, let amount = message.amountUAET {
                (Text("@\(handle) ").bold() + Text("tipped \(AET.label(uAET: amount)) ") + Text(Image(systemName: "bolt.fill")))
                    .foregroundStyle(Color(hex: 0x14120C))
                    .padding(.horizontal, 10)
                    .padding(.vertical, 6)
                    .background(Color(hex: 0xFFE27A).opacity(0.95), in: Capsule())
            } else if message.kind == "system" {
                Text(message.body)
                    .italic()
                    .foregroundStyle(.white.opacity(0.75))
                    .padding(.horizontal, 10)
                    .padding(.vertical, 6)
                    .background(Color.black.opacity(0.4), in: Capsule())
            } else {
                (Text("@\(handle) ").bold() + Text(message.body))
                    .foregroundStyle(.white)
                    .padding(.horizontal, 10)
                    .padding(.vertical, 6)
                    .background(Color.black.opacity(0.5), in: RoundedRectangle(cornerRadius: 14))
            }
        }
        .font(.system(size: 13))
        .lineLimit(3)
        .fixedSize(horizontal: false, vertical: true)
        .accessibilityLabel(message.isTip
            ? "@\(handle) tipped \(AET.label(uAET: message.amountUAET ?? 0))"
            : "@\(handle): \(message.body)")
    }
}

/// The "Say something…" field at the foot of a room.
struct LiveChatComposer: View {
    @Binding var text: String
    let onSend: () -> Void
    var placeholder = "Say something…"

    @FocusState private var focused: Bool

    var body: some View {
        HStack(spacing: 8) {
            TextField(placeholder, text: $text)
                .font(.system(size: 15))
                .foregroundStyle(.white)
                .tint(.white)
                .submitLabel(.send)
                .focused($focused)
                .onSubmit(onSend)
            if !text.trimmingCharacters(in: .whitespaces).isEmpty {
                Button(action: onSend) {
                    Image(systemName: "arrow.up.circle.fill")
                        .font(.system(size: 24))
                        .foregroundStyle(.white)
                }
                .buttonStyle(.plain)
                .accessibilityLabel("Send")
            }
        }
        .padding(.horizontal, 16)
        .frame(height: 48)
        .overlay(Capsule().strokeBorder(.white.opacity(0.7), lineWidth: 1.5))
    }
}

/// A 48pt round control in a room's right rail.
struct RailButton: View {
    let icon: String
    let label: String
    var caption: String?
    var tint: Color?
    var filled: Color?
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            VStack(spacing: 3) {
                Image(systemName: icon)
                    .font(.system(size: 18, weight: .semibold))
                    .foregroundStyle(filled == nil ? (tint ?? .white) : Color(hex: 0x14120C))
                    .frame(width: 48, height: 48)
                    .background(filled ?? .clear, in: Circle())
                    .overlay(Circle().strokeBorder(filled == nil ? (tint ?? .white).opacity(0.7) : .clear, lineWidth: 1.5))
                if let caption {
                    Text(caption)
                        .font(.system(size: 10, weight: .semibold).monospacedDigit())
                        .foregroundStyle(.white)
                        .lineLimit(1)
                }
            }
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel(caption.map { "\(label), \($0)" } ?? label)
    }
}

/// The tip goal as the broadcaster sees it: total, goal, and who gave most.
struct TipGoalCard: View {
    let totalUAET: Int64
    let goalUAET: Int64
    let topTippers: [LiveStream.Tipper]
    var newSubs: Int = 0

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Text("Tip goal · \(AET.label(uAET: goalUAET))")
                Spacer()
                Text(AET.label(uAET: totalUAET))
            }
            .font(.system(size: 12, weight: .semibold).monospacedDigit())
            .foregroundStyle(F33Color.ink)

            GeometryReader { geo in
                ZStack(alignment: .leading) {
                    Capsule().fill(Color(hex: 0xDDDDDD))
                    Capsule().fill(F33Color.ink)
                        .frame(width: geo.size.width * CGFloat(min(1, goalUAET > 0 ? Double(totalUAET) / Double(goalUAET) : 0)))
                }
            }
            .frame(height: 6)

            if !topTippers.isEmpty {
                Text(topLine)
                    .font(.system(size: 11))
                    .foregroundStyle(F33Color.ink3)
                    .lineLimit(1)
            }
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
        .background(Color.white.opacity(0.94), in: RoundedRectangle(cornerRadius: 10))
        // A white card over a dark room: its text resolves against light, not
        // against the room's dark scheme.
        .environment(\.colorScheme, .light)
        .accessibilityElement(children: .combine)
    }

    private var topLine: String {
        var parts = topTippers.map { "@\($0.author.handle) \(AET.amount(uAET: $0.amountUAET))" }
        if newSubs > 0 { parts.append("\(newSubs) new sub\(newSubs == 1 ? "" : "s")") }
        return "top: " + parts.joined(separator: " · ")
    }
}
