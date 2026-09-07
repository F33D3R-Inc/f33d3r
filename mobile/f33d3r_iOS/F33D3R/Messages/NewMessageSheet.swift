import SwiftUI
import F33D3RKit

/// Writing to somebody for the first time.
///
/// The message is written before anyone asks whether it may be delivered, and
/// that order is the server's design rather than an oversight. It means a
/// message is never lost to a refusal: if the person has to accept a contact
/// request first, what was already typed is held and released when they do.
///
/// So this sheet asks for the words, not for permission, and reports one of
/// three things afterwards. Everything that is not "sent" or "saved" is a
/// single sentence, identical whatever the reason — no such person, an account
/// that takes no messages, a block. Telling those apart would let anyone learn
/// who exists and what their settings are by writing to them and watching, and
/// the server refuses to be that oracle. Neither is this screen.
struct NewMessageSheet: View {
    let handle: String
    let displayName: String
    let avatarURL: String?

    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    @State private var draft = ""
    @State private var isSending = false
    @State private var outcome: ConversationStart?
    @State private var problem: String?
    @FocusState private var focused: Bool

    var body: some View {
        NavigationStack {
            VStack(alignment: .leading, spacing: F33Spacing.lg) {
                HStack(spacing: F33Spacing.sm) {
                    F33Avatar(handle: handle, displayName: displayName, avatarURL: avatarURL, size: 36)
                    VStack(alignment: .leading, spacing: 1) {
                        Text(displayName)
                            .font(.system(size: 15, weight: .semibold))
                            .foregroundStyle(F33Color.ink)
                        Text("@" + handle)
                            .font(.system(size: 13))
                            .foregroundStyle(F33Color.ink4)
                    }
                    Spacer(minLength: 0)
                }

                if let outcome {
                    result(outcome)
                } else {
                    TextField("Write something", text: $draft, axis: .vertical)
                        .lineLimit(4...10)
                        .font(.system(size: 16))
                        .foregroundStyle(F33Color.ink)
                        .focused($focused)
                        .f33Field()

                    if let problem {
                        Text(problem)
                            .font(.system(size: 13))
                            .foregroundStyle(F33Color.danger)
                    }
                }

                Spacer(minLength: 0)
            }
            .padding(F33Spacing.lg)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(F33Color.bg)
            .navigationTitle("New message")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button(outcome == nil ? "Cancel" : "Done") { dismiss() }
                }
                if outcome == nil {
                    ToolbarItem(placement: .confirmationAction) {
                        Button {
                            send()
                        } label: {
                            if isSending { ProgressView() } else { Text("Send") }
                        }
                        .disabled(draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || isSending)
                    }
                }
            }
            .onAppear { focused = true }
        }
        .presentationDetents([.medium])
    }

    @ViewBuilder
    private func result(_ outcome: ConversationStart) -> some View {
        VStack(alignment: .leading, spacing: F33Spacing.md) {
            Label(outcome.userMessage, systemImage: icon(for: outcome.state))
                .font(.system(size: 15))
                .foregroundStyle(outcome.state == .undelivered ? F33Color.ink2 : F33Color.ink)

            if let conversation = outcome.conversation {
                NavigationLink(value: Route.conversation(id: conversation.id)) {
                    Text("Open the conversation")
                        .font(.system(size: 14, weight: .semibold))
                        .foregroundStyle(F33Color.accent)
                }
            }
        }
    }

    private func icon(for state: ConversationStart.State) -> String {
        switch state {
        case .opened: return "checkmark.circle"
        case .held: return "clock"
        case .undelivered: return "exclamationmark.circle"
        }
    }

    private func send() {
        let text = draft
        isSending = true
        problem = nil
        Task {
            defer { isSending = false }
            do {
                let answer = try await model.client.startConversation(handle: handle, body: text)
                outcome = answer
                // A conversation that opened belongs in the list straight away,
                // rather than after the reader happens to pull to refresh.
                if answer.conversation != nil {
                    await model.conversationsFeed().reload()
                }
            } catch let error as MessagingError {
                problem = error.userMessage
            } catch {
                problem = "That could not be sent."
            }
        }
    }
}

#Preview("New message") {
    NewMessageSheet(handle: "miiyazuko", displayName: "Miiyazuko", avatarURL: nil)
        .environment(AppModel())
}
