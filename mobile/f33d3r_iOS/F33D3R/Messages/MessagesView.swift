import SwiftUI
import F33D3RKit

/// Messages: the conversation list.
///
/// A row per conversation, newest activity first, the way the surface has
/// always been ordered. Two things on it are worth a word.
///
/// A sealed conversation shows a lock instead of a preview, and that is not the
/// app being coy. The server cannot read those messages — it stores ciphertext
/// and per-recipient wrapped keys and nothing else — so there is no preview to
/// send. Saying "Encrypted message" states the fact; a blank line would read as
/// a bug.
///
/// And unread is the server's. A row loses its dot when the thread is opened
/// and the server has been told, not when the finger lands, which is the same
/// rule the Notifications tab follows for the same reason.
///
/// There is a second way in, in the toolbar. A F33D3R Number is a contact
/// address somebody hands out so they can be reached without giving away their
/// handle, and the person holding one usually has no conversation to enter it
/// from — often they do not know whose it is. So the keypad is reachable from
/// the list itself, not only from a thread that failed to deliver.
struct MessagesView: View {
    @Environment(AppModel.self) private var model

    @State private var isEnteringNumber = false

    var body: some View {
        let feed = model.conversationsFeed()

        ScrollView {
            LazyVStack(spacing: 0) {
                list(feed)
            }
        }
        .background(F33Color.bg)
        .navigationTitle("Messages")
        .navigationBarTitleDisplayMode(.inline)
        .refreshable { await feed.reload() }
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                Button {
                    isEnteringNumber = true
                } label: {
                    Image(systemName: "number")
                }
                .accessibilityLabel("Enter a F33D3R Number")
            }
        }
        .sheet(isPresented: $isEnteringNumber) {
            // No handle: nobody has been named and nobody will be, unless the
            // person on the other end lets the message through. The sheet
            // reloads this list itself when one opens, so the conversation is
            // the top row by the time the keypad is out of the way.
            NumberPadSheet()
        }
        .task { await feed.loadIfNeeded() }
    }

    @ViewBuilder
    private func list(_ feed: ConversationsFeed) -> some View {
        switch feed.phase {
        case .idle, .loading:
            ForEach(0..<5, id: \.self) { _ in
                ConversationRowSkeleton()
                CardDivider()
            }

        case .empty:
            EmptyStateView(
                icon: "bubble.left.and.bubble.right",
                title: "No messages yet",
                message: "Open someone's profile and write to them. If you were handed a F33D3R Number instead, that works too.",
                actionTitle: "Enter a F33D3R Number",
                action: { isEnteringNumber = true }
            )
            .padding(.top, F33Spacing.xxl)

        case .failed(let error):
            ErrorStateView(error: error) {
                Task { await feed.reload() }
            }
            .padding(.top, F33Spacing.xxl)

        case .loaded:
            ForEach(feed.conversations) { conversation in
                NavigationLink(value: Route.conversation(id: conversation.id)) {
                    ConversationRow(conversation: conversation)
                }
                .buttonStyle(.plain)
                CardDivider()
            }
        }
    }
}

/// One conversation.
private struct ConversationRow: View {
    let conversation: Conversation

    var body: some View {
        HStack(alignment: .top, spacing: F33Card.columnGap) {
            F33Avatar(
                handle: conversation.otherHandle,
                displayName: conversation.name,
                avatarURL: conversation.otherAvatar,
                size: F33Card.avatarColumnWidth
            )

            VStack(alignment: .leading, spacing: 3) {
                HStack(spacing: F33Spacing.sm) {
                    Text(conversation.name)
                        .font(.system(size: 15, weight: conversation.isUnread ? .semibold : .medium))
                        .foregroundStyle(F33Color.ink)
                        .lineLimit(1)

                    if conversation.mode == .sealed {
                        // The one place the app says a conversation is
                        // encrypted without being asked. It is a property of
                        // the room, not a state of this message, so it belongs
                        // beside the name.
                        Image(systemName: "lock.fill")
                            .font(.system(size: 10))
                            .foregroundStyle(F33Color.ink4)
                            .accessibilityLabel("Encrypted")
                    }

                    Spacer(minLength: 0)

                    Text(RelativeTime.label(for: conversation.lastAt))
                        .font(.system(size: 11))
                        .foregroundStyle(F33Color.ink4)
                }

                HStack(alignment: .top, spacing: F33Spacing.sm) {
                    Text(conversation.previewText)
                        .font(.system(size: 14))
                        .foregroundStyle(
                            conversation.mode == .sealed
                                ? F33Color.ink4
                                : (conversation.isUnread ? F33Color.ink2 : F33Color.ink3)
                        )
                        .lineLimit(2)
                        .multilineTextAlignment(.leading)

                    Spacer(minLength: 0)

                    if conversation.isUnread {
                        Text("\(conversation.unread)")
                            .font(.system(size: 11, weight: .semibold).monospacedDigit())
                            .foregroundStyle(F33Color.accentInk)
                            .padding(.horizontal, 6)
                            .padding(.vertical, 2)
                            .background(F33Color.accent, in: Capsule())
                    }
                }
            }
        }
        .padding(.horizontal, F33Card.paddingHorizontal)
        .padding(.vertical, F33Spacing.md)
        .frame(minHeight: F33Layout.minTouchTarget)
        .contentShape(Rectangle())
        .accessibilityElement(children: .combine)
        .accessibilityLabel(spoken)
    }

    private var spoken: String {
        var parts = [conversation.name]
        if conversation.mode == .sealed { parts.append("encrypted") }
        parts.append(conversation.previewText)
        if conversation.isUnread { parts.append("\(conversation.unread) unread") }
        parts.append(RelativeTime.fullLabel(for: conversation.lastAt))
        return parts.joined(separator: ", ")
    }
}

/// The shape of a row, while the rows are on their way.
private struct ConversationRowSkeleton: View {
    @State private var pulse = false

    var body: some View {
        HStack(alignment: .top, spacing: F33Card.columnGap) {
            Circle()
                .fill(F33Color.bgSunken)
                .frame(width: F33Card.avatarColumnWidth, height: F33Card.avatarColumnWidth)

            VStack(alignment: .leading, spacing: 6) {
                RoundedRectangle(cornerRadius: 4).fill(F33Color.bgSunken).frame(width: 120, height: 12)
                RoundedRectangle(cornerRadius: 4).fill(F33Color.bgSunken).frame(height: 12)
                RoundedRectangle(cornerRadius: 4).fill(F33Color.bgSunken).frame(width: 200, height: 12)
            }
        }
        .padding(.horizontal, F33Card.paddingHorizontal)
        .padding(.vertical, F33Spacing.md)
        .opacity(pulse ? 0.45 : 0.85)
        .animation(.easeInOut(duration: 1).repeatForever(autoreverses: true), value: pulse)
        .onAppear { pulse = true }
        .accessibilityHidden(true)
    }
}

#Preview("Messages") {
    NavigationStack {
        MessagesView()
    }
    .environment(AppModel())
}
