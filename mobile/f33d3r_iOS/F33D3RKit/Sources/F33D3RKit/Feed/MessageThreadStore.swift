import Foundation
import Observation

/// What can open and close a sealed message on this device.
///
/// A protocol rather than a concrete type because the key custody and the
/// thread are separate concerns and only one of them needs a password. The
/// thread knows a message is sealed and knows when it wants it open; it knows
/// nothing about curves, wrapping, or where a private key is kept.
public protocol SealedMessageOpener: Sendable {
    /// Whether this device currently holds the key. False means the reader has
    /// to sign in again before anything sealed can be read.
    var isUnlocked: Bool { get async }

    /// The plaintext of one sealed message, or nil when it cannot be opened.
    ///
    /// Never throws. A message that will not open is an ordinary outcome —
    /// written before this account had a key, or sealed to a key since rotated
    /// — and the thread shows it as unopenable rather than as a failure.
    func open(_ message: ChatMessage) async -> String?

    /// Seals text to every member of a conversation.
    func seal(_ text: String, conversation: String) async throws -> SealedEnvelope
}

/// One conversation, open.
///
/// Holds the messages, the plaintext of any sealed ones this device could open,
/// and the send. Two things are worth knowing about how it behaves.
///
/// It does not flip anything before the server has. A sent message appears when
/// the server says it was stored, not when the button was pressed — the same
/// rule the rest of this app follows, and the reason a failed send leaves the
/// text in the composer where the writer can still see it.
///
/// And it opens sealed messages once each, caching by message id. Opening is
/// two key agreements and two decryptions, which is nothing on its own and adds
/// up over a screenful when a list re-renders.
@MainActor
@Observable
public final class MessageThreadStore {

    public enum Phase: Equatable, Sendable {
        case idle
        case loading
        case loaded
        case empty
        case failed(APIError)
    }

    public let conversationID: String

    public private(set) var conversation: Conversation?
    public private(set) var messages: [ChatMessage] = []
    public private(set) var members: [ConversationMember] = []
    public private(set) var phase: Phase = .idle

    /// Plaintext by message id, for messages that arrived sealed.
    public private(set) var opened: [String: String] = [:]
    /// Sealed messages this device tried and could not open, so the thread can
    /// say so once instead of retrying on every redraw.
    public private(set) var unopenable: Set<String> = []
    /// True when the conversation is sealed and this device has no key.
    public private(set) var isLocked = false

    public private(set) var isSending = false
    public private(set) var sendError: MessagingError?
    /// Set when the last thing said was stored but did not reach the other
    /// person. Nil when everything sent has arrived.
    ///
    /// Not an error: nothing failed and nothing was lost. A thread can outlive
    /// the permission that opened it, and this is the thread saying so. Why it
    /// happened is not here because the server does not send it.
    public private(set) var undelivered: ConversationStart.State?
    public private(set) var isLoadingOlder = false
    public private(set) var hasOlder = false

    private var cursor: String?
    private let client: APIClient
    private let opener: (any SealedMessageOpener)?

    public init(conversationID: String, client: APIClient, opener: (any SealedMessageOpener)? = nil) {
        self.conversationID = conversationID
        self.client = client
        self.opener = opener
    }

    // MARK: Loading

    public func loadIfNeeded() async {
        guard phase == .idle else { return }
        await reload()
    }

    public func reload() async {
        if messages.isEmpty { phase = .loading }
        do {
            let thread = try await client.messageThread(id: conversationID)
            conversation = thread.conversation
            members = thread.members
            messages = thread.messages
            cursor = thread.nextCursor
            hasOlder = thread.hasMore
            phase = thread.messages.isEmpty ? .empty : .loaded
            await openSealed(thread.messages)
            await markRead()
        } catch let error as APIError {
            if messages.isEmpty { phase = .failed(error) }
        } catch {
            if messages.isEmpty { phase = .failed(.transport(error.localizedDescription)) }
        }
    }

    /// Loads the page before the oldest message on screen.
    public func loadOlder() async {
        guard hasOlder, !isLoadingOlder, let cursor else { return }
        isLoadingOlder = true
        defer { isLoadingOlder = false }
        do {
            let page = try await client.messageThread(id: conversationID, before: cursor)
            let known = Set(messages.map(\.id))
            let older = page.messages.filter { !known.contains($0.id) }
            messages.insert(contentsOf: older, at: 0)
            self.cursor = page.nextCursor
            hasOlder = page.hasMore
            await openSealed(older)
        } catch {
            // Nothing is taken away and nothing is said: the reader asked for
            // history, not for a report, and the gesture can simply be repeated.
            hasOlder = true
        }
    }

    // MARK: Sending

    /// Sends `text`, by whichever path this conversation's mode requires.
    ///
    /// Returns true when the message was stored. A false answer leaves
    /// ``sendError`` set and the caller keeps the text.
    @discardableResult
    public func send(_ text: String) async -> Bool {
        let body = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !body.isEmpty, !isSending else { return false }
        isSending = true
        sendError = nil
        defer { isSending = false }

        do {
            let delivery: MessageDelivery
            if conversation?.mode == .sealed {
                guard let opener else {
                    sendError = .sealedConversation
                    return false
                }
                let envelope = try await opener.seal(body, conversation: conversationID)
                let stored = try await client.sendSealedMessage(conversation: conversationID, envelope: envelope)
                // Our own copy is sealed to us like everyone else's, but we
                // already know what it says, so it is recorded rather than
                // decrypted back.
                opened[stored.id] = body
                delivery = .stored(stored)
            } else {
                delivery = try await client.sendMessage(conversation: conversationID, body: body)
            }
            switch delivery {
            case .stored(let stored):
                undelivered = nil
                append(stored)
            case .notArrived(let state):
                // The message is on the server; it is only not in the thread.
                // So this answers true and the composer clears — the writer
                // does not have to retype something that was taken.
                undelivered = state
            }
            return true
        } catch let error as MessagingError {
            sendError = error
            return false
        } catch {
            // Whatever the crypto core throws lands here. It is reported as it
            // describes itself rather than translated, because a sealing
            // failure is rare and specific and a generic sentence would hide
            // which of several very different things went wrong.
            sendError = .server(status: 0, message: error.localizedDescription)
            return false
        }
    }

    public func clearSendError() { sendError = nil }

    /// Forgets that something did not arrive, once the reader has another way
    /// through. Called when a Number opens the conversation the ordinary send
    /// could not reach.
    public func clearUndelivered() { undelivered = nil }

    // MARK: Live arrivals

    /// Takes a message that arrived on the event stream.
    public func receive(_ message: ChatMessage) async {
        guard message.conversationID == conversationID else { return }
        append(message)
        await openSealed([message])
        await markRead()
    }

    // MARK: Internals

    private func append(_ message: ChatMessage) {
        guard !messages.contains(where: { $0.id == message.id }) else { return }
        messages.append(message)
        if phase == .empty { phase = .loaded }
    }

    private func markRead() async {
        try? await client.markConversationRead(conversationID)
    }

    /// Opens whichever of `batch` are sealed, and works out whether the whole
    /// thread is locked.
    private func openSealed(_ batch: [ChatMessage]) async {
        let sealed = batch.filter { $0.isSealed }
        guard !sealed.isEmpty else { return }
        guard let opener else {
            isLocked = true
            return
        }
        guard await opener.isUnlocked else {
            isLocked = true
            return
        }
        isLocked = false
        for message in sealed where opened[message.id] == nil && !unopenable.contains(message.id) {
            if let text = await opener.open(message) {
                opened[message.id] = text
            } else {
                unopenable.insert(message.id)
            }
        }
    }

    /// What one row shows: the plaintext for a plain message, the opened text
    /// for a sealed one, and nil when it is sealed and shut.
    public func text(for message: ChatMessage) -> String? {
        message.isSealed ? opened[message.id] : message.body
    }
}
