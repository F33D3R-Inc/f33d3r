import SwiftUI
import F33D3RKit

/// The strip above the tab bar while the reader is tuned into a frequency and
/// looking at something else.
///
/// It is the same promise the now-playing bar makes: you are still in the room,
/// here is the way back, and here is the way out. Chrome floating over whatever
/// tab the reader wandered to, so it takes glass.
///
/// Nothing on it is decided here. It appears when ``FrequencySession`` says the
/// server counts this reader as present in a room, and it goes when the server
/// stops saying so — the X calls `frequency_leave` and waits, rather than
/// hiding the bar and hoping.
struct FrequencyMiniBar: View {
    /// Pushes the room the reader is listening to. The bar lives outside the
    /// navigation stack, so the stack hands it the way in.
    let onOpen: (String) -> Void

    @Environment(FrequencySession.self) private var session
    @State private var isLeaving = false

    var body: some View {
        if session.isMiniBarVisible, let store = session.listening, let frequency = store.frequency {
            content(store: store, frequency: frequency)
                .transition(.move(edge: .bottom).combined(with: .opacity))
        }
    }

    private func content(store: FrequencyRoomStore, frequency: Frequency) -> some View {
        HStack(spacing: F33Spacing.md) {
            Button {
                onOpen(frequency.id)
            } label: {
                HStack(spacing: F33Spacing.md) {
                    FrequencyAudioGlyph(tint: F33Color.accent, size: 18)
                        .frame(width: 24)

                    VStack(alignment: .leading, spacing: 1) {
                        Text(hostName(frequency))
                            .font(.system(size: 14, weight: .semibold))
                            .foregroundStyle(F33Color.accent)
                        Text(secondLine(frequency))
                            .font(.system(size: 12))
                            .foregroundStyle(F33Color.ink3)
                    }
                    .lineLimit(1)

                    Spacer(minLength: 0)
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Listening to \(frequency.title), hosted by \(hostName(frequency))")
            .accessibilityHint("Opens the frequency")

            Button {
                guard !isLeaving else { return }
                isLeaving = true
                Task {
                    await session.leaveListening()
                    isLeaving = false
                }
            } label: {
                Group {
                    if isLeaving {
                        ProgressView().controlSize(.small)
                    } else {
                        Image(systemName: "xmark")
                            .font(.system(size: 15, weight: .semibold))
                            .foregroundStyle(F33Color.ink2)
                    }
                }
                .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .disabled(isLeaving)
            .accessibilityLabel("Leave frequency")
        }
        .padding(.leading, F33Spacing.md)
        .padding(.trailing, F33Spacing.xs)
        .frame(minHeight: F33Layout.minTouchTarget)
        .f33Glass(in: RoundedRectangle(cornerRadius: F33Radius.md, style: .continuous), elevation: .floating)
        .padding(.horizontal, F33Spacing.sm)
        .padding(.bottom, F33Spacing.xs)
    }

    private func hostName(_ frequency: Frequency) -> String {
        frequency.host.displayName.isEmpty ? "@\(frequency.host.handle)" : frequency.host.displayName
    }

    /// "+56 others · Barometric pressure". Everyone but the host, then what the
    /// room is called — in that order, because the reader already knows the
    /// title and is checking whether the room is still worth being in.
    private func secondLine(_ frequency: Frequency) -> String {
        let others = max(0, frequency.participantCount - 1)
        let title = frequency.title.isEmpty ? "Untitled" : frequency.title
        guard others > 0 else { return title }
        return "+\(Counts.label(others) ?? "\(others)") others · \(title)"
    }
}
