import SwiftUI
import F33D3RKit

/// One conversation.
///
/// Bubbles oldest at the top, newest at the bottom, and the view opens at the
/// bottom because that is where the conversation is. Older messages load when
/// the reader scrolls up into them.
///
/// The composer sends and then waits. Nothing appears until the server says it
/// was stored, and a failure leaves the text where it was typed — a message
/// that looked sent and was not is worse than one that visibly took a moment.
struct ConversationView: View {
    let conversationID: String

    @Environment(AppModel.self) private var model
    @State private var draft = ""
    @State private var isEnteringNumber = false
    @FocusState private var composerFocused: Bool

    var body: some View {
        let store = model.threadStore(conversationID)

        VStack(spacing: 0) {
            messages(store)
            composer(store)
        }
        .background(F33Color.bg)
        // No compose button here — this screen has a composer of its own, at the bottom, and two would be one too many.
        .hidesComposeFAB()
        .navigationTitle(store.conversation?.name ?? "Message")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            if store.conversation?.mode == .sealed {
                ToolbarItem(placement: .topBarTrailing) {
                    // Said once, at the top, rather than on every bubble. Every
                    // message in this room is encrypted; repeating it per line
                    // would make the fact into decoration.
                    Label("Encrypted", systemImage: "lock.fill")
                        .labelStyle(.iconOnly)
                        .font(.system(size: 14))
                        .foregroundStyle(F33Color.ink3)
                        .accessibilityLabel("This conversation is end-to-end encrypted")
                }
            }
        }
        .sheet(isPresented: $isEnteringNumber) {
            NumberPadSheet(handle: store.conversation?.otherHandle) { _ in
                // A Number that opened the way makes the note above untrue.
                store.clearUndelivered()
                Task { await store.reload() }
            }
        }
        .task { await store.loadIfNeeded() }
    }

    // MARK: The thread

    @ViewBuilder
    private func messages(_ store: MessageThreadStore) -> some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: F33Spacing.sm) {
                    switch store.phase {
                    case .idle, .loading:
                        ForEach(0..<6, id: \.self) { index in
                            BubbleSkeleton(mine: index.isMultiple(of: 3))
                        }

                    case .empty:
                        EmptyStateView(
                            icon: "bubble.left",
                            title: "Nothing here yet",
                            message: "Say something. This is the beginning of the conversation."
                        )
                        .padding(.top, F33Spacing.xxl)

                    case .failed(let error):
                        ErrorStateView(error: error) {
                            Task { await store.reload() }
                        }
                        .padding(.top, F33Spacing.xxl)

                    case .loaded:
                        if store.hasOlder {
                            olderLoader(store)
                        }
                        if store.isLocked {
                            lockedNote
                        }
                        ForEach(store.messages) { message in
                            MessageBubble(
                                message: message,
                                text: store.text(for: message),
                                isUnopenable: store.unopenable.contains(message.id),
                                showsSender: store.conversation?.isGroup == true
                            )
                            .id(message.id)
                        }
                        if let state = store.undelivered, store.conversation?.isGroup != true {
                            UnreachableNote(state: state, handle: store.conversation?.otherHandle ?? "") {
                                isEnteringNumber = true
                            }
                        }
                    }
                }
                .padding(.horizontal, F33Card.paddingHorizontal)
                .padding(.vertical, F33Spacing.md)
            }
            .scrollDismissesKeyboard(.interactively)
            .onChange(of: store.messages.last?.id) { _, last in
                guard let last else { return }
                withAnimation(F33Motion.easeOut) { proxy.scrollTo(last, anchor: .bottom) }
            }
            .onChange(of: store.phase) { _, phase in
                guard phase == .loaded, let last = store.messages.last?.id else { return }
                // The first paint lands at the bottom without animating: a
                // thread that scrolls itself on open reads as a glitch.
                proxy.scrollTo(last, anchor: .bottom)
            }
        }
    }

    private func olderLoader(_ store: MessageThreadStore) -> some View {
        HStack {
            Spacer()
            if store.isLoadingOlder {
                ProgressView()
            } else {
                Button("Load earlier messages") {
                    Task { await store.loadOlder() }
                }
                .font(.system(size: 13, weight: .medium))
                .foregroundStyle(F33Color.accent)
            }
            Spacer()
        }
        .frame(minHeight: F33Layout.minTouchTarget)
        .onAppear {
            guard !store.isLoadingOlder else { return }
            Task { await store.loadOlder() }
        }
    }

    /// What a sealed thread says when this device has no key.
    ///
    /// The key is unwrapped with something derived from the password at
    /// sign-in, so signing in again is genuinely the fix rather than a shrug.
    private var lockedNote: some View {
        VStack(spacing: F33Spacing.sm) {
            Image(systemName: "lock.trianglebadge.exclamationmark")
                .font(.system(size: 26))
                .foregroundStyle(F33Color.ink5)
            Text("This conversation is encrypted")
                .font(.system(size: 15, weight: .semibold))
                .foregroundStyle(F33Color.ink2)
            Text("Sign in again to unlock it on this device. The key never leaves your phone, so nothing else can open it for you.")
                .font(.system(size: 13))
                .foregroundStyle(F33Color.ink4)
                .multilineTextAlignment(.center)
        }
        .frame(maxWidth: .infinity)
        .padding(F33Spacing.lg)
        .background(F33Color.bgElevated, in: RoundedRectangle(cornerRadius: F33Radius.md, style: .continuous))
    }

    // MARK: The composer

    private func composer(_ store: MessageThreadStore) -> some View {
        VStack(spacing: 0) {
            if let error = store.sendError {
                HStack(spacing: F33Spacing.sm) {
                    Text(error.userMessage)
                        .font(.system(size: 12))
                        .foregroundStyle(F33Color.danger)
                    Spacer(minLength: 0)
                    Button("Dismiss") { store.clearSendError() }
                        .font(.system(size: 12, weight: .medium))
                        .foregroundStyle(F33Color.ink3)
                }
                .padding(.horizontal, F33Card.paddingHorizontal)
                .padding(.top, F33Spacing.xs)
            }

            HStack(alignment: .bottom, spacing: F33Spacing.sm) {
                TextField("Message", text: $draft, axis: .vertical)
                    .lineLimit(1...5)
                    .font(.system(size: 15))
                    .foregroundStyle(F33Color.ink)
                    .focused($composerFocused)
                    .f33Field()
                    .disabled(store.isLocked)

                Button {
                    send(store)
                } label: {
                    if store.isSending {
                        ProgressView()
                            .frame(width: 34, height: 34)
                    } else {
                        Image(systemName: "arrow.up")
                            .font(.system(size: 15, weight: .bold))
                            .foregroundStyle(F33Color.accentInk)
                            .frame(width: 34, height: 34)
                            .background(canSend(store) ? F33Color.accent : F33Color.ink5, in: Circle())
                    }
                }
                .disabled(!canSend(store))
                .accessibilityLabel("Send")
            }
            .padding(.horizontal, F33Card.paddingHorizontal)
            .padding(.vertical, F33Spacing.sm)
        }
        // Chrome sitting over the thread, so it is glass like every other bar
        // in the app rather than a slab the messages disappear under.
        .f33GlassBar()
    }

    private func canSend(_ store: MessageThreadStore) -> Bool {
        !draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
            && !store.isSending
            && !store.isLocked
    }

    private func send(_ store: MessageThreadStore) {
        let text = draft
        Task {
            if await store.send(text) {
                draft = ""
                model.conversationsFeed().markReadLocally(conversationID)
            }
        }
    }
}

/// What the thread says when the last message did not get to the other person.
///
/// It sits under the messages, where the reply would have appeared, because
/// that is where the writer is looking.
///
/// The wording is the hard part. The server refuses to say why a message did
/// not arrive — an account that never existed, a retired contact address, a
/// block and somebody who simply takes no messages are one answer, delivered
/// after a fixed delay, so that writing and watching cannot be used to learn
/// who exists or what their settings are. This app knows exactly as much. So it
/// says that it did not arrive and stops, and offers the one door it can
/// honestly offer: a Number, which its holder chose to give out.
private struct UnreachableNote: View {
    let state: ConversationStart.State
    let handle: String
    let onEnterNumber: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: F33Spacing.sm) {
            Text(sentence)
                .font(.system(size: 13))
                .foregroundStyle(F33Color.ink3)
                .fixedSize(horizontal: false, vertical: true)

            if state != .held {
                Text("If you know their F33D3R Number, you can reach them with it.")
                    .font(.system(size: 13))
                    .foregroundStyle(F33Color.ink4)
                    .fixedSize(horizontal: false, vertical: true)

                Button(action: onEnterNumber) {
                    Text("Enter F33D3R Number")
                        .font(.system(size: 14, weight: .semibold))
                        .foregroundStyle(F33Color.accent)
                        .padding(.horizontal, F33Spacing.lg)
                        .frame(minHeight: F33Layout.minTouchTarget)
                        .background(F33Color.accentSoft, in: Capsule())
                }
                .buttonStyle(.plain)
                .padding(.top, F33Spacing.xs)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(F33Spacing.lg)
        .background(F33Color.bgElevated, in: RoundedRectangle(cornerRadius: F33Radius.md, style: .continuous))
        .padding(.top, F33Spacing.sm)
    }

    private var sentence: String {
        switch state {
        case .held:
            return "Saved. They will see it if they accept your request."
        case .opened, .undelivered:
            let who = handle.isEmpty ? "them" : "@" + handle
            return "This didn't reach \(who), and the server doesn't say why."
        }
    }
}

/// One message.
private struct MessageBubble: View {
    let message: ChatMessage
    /// The plaintext, once there is one. Nil while a sealed message is still
    /// shut.
    let text: String?
    let isUnopenable: Bool
    let showsSender: Bool

    var body: some View {
        HStack {
            if message.isMine { Spacer(minLength: 48) }

            VStack(alignment: message.isMine ? .trailing : .leading, spacing: 3) {
                if showsSender && !message.isMine {
                    Text(message.senderName)
                        .font(.system(size: 11, weight: .medium))
                        .foregroundStyle(F33Color.ink4)
                }

                body(for: text)
                    .padding(.horizontal, 12)
                    .padding(.vertical, 8)
                    .background(
                        message.isMine ? F33Color.accent : F33Color.bgElevated,
                        in: RoundedRectangle(cornerRadius: F33Radius.md, style: .continuous)
                    )

                Text(RelativeTime.label(for: message.createdAt))
                    .font(.system(size: 10))
                    .foregroundStyle(F33Color.ink5)
            }

            if !message.isMine { Spacer(minLength: 48) }
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel(spoken)
    }

    @ViewBuilder
    private func body(for text: String?) -> some View {
        if let text {
            Text(text)
                .font(.system(size: 15))
                .foregroundStyle(message.isMine ? F33Color.accentInk : F33Color.ink)
                .textSelection(.enabled)
        } else if isUnopenable || !message.isOpenable {
            // A sealed message this device cannot open. Usually one written
            // before this account had a key. It is shown rather than hidden,
            // because a gap in a conversation is worse than a line saying why.
            Label("Cannot be opened on this device", systemImage: "lock.slash")
                .font(.system(size: 13))
                .foregroundStyle(F33Color.ink4)
        } else {
            Label("Encrypted", systemImage: "lock.fill")
                .font(.system(size: 13))
                .foregroundStyle(F33Color.ink4)
        }
    }

    private var spoken: String {
        let who = message.isMine ? "You" : message.senderName
        let what = text ?? "an encrypted message"
        return "\(who): \(what), \(RelativeTime.fullLabel(for: message.createdAt))"
    }
}

private struct BubbleSkeleton: View {
    let mine: Bool
    @State private var pulse = false

    var body: some View {
        HStack {
            if mine { Spacer(minLength: 80) }
            RoundedRectangle(cornerRadius: F33Radius.md, style: .continuous)
                .fill(F33Color.bgSunken)
                .frame(height: 34)
                .frame(maxWidth: 220)
            if !mine { Spacer(minLength: 80) }
        }
        .opacity(pulse ? 0.45 : 0.85)
        .animation(.easeInOut(duration: 1).repeatForever(autoreverses: true), value: pulse)
        .onAppear { pulse = true }
        .accessibilityHidden(true)
    }
}

#Preview("Conversation") {
    NavigationStack {
        ConversationView(conversationID: "preview")
    }
    .environment(AppModel())
}
