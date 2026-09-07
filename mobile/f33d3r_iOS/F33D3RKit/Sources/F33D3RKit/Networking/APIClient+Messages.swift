import Foundation

/// The messaging calls.
///
/// Reads go through the JSON surface like every other read. Writes are split,
/// and not arbitrarily: a plain message is an ordinary mutation and goes down
/// the `POST /events` lane with everything else, while a sealed one carries a
/// structured envelope of ciphertext and per-recipient wrapped keys that the
/// flat event shape cannot express, so it goes to the same `/api/gnosis`
/// endpoint the web client posts to.
///
/// That endpoint answers the web with a rendered fragment. Asking for JSON is
/// what gets the stored message back instead — see ``sendSealedMessage``.
public extension APIClient {

    // MARK: Reads

    /// The reader's conversations.
    func conversations() async throws -> ConversationPage {
        try await send(.conversations, body: Optional<Never>.none, as: ConversationPage.self)
    }

    /// One conversation with a page of its messages, oldest first.
    ///
    /// `before` pages backwards through the history: pass the cursor a previous
    /// page returned to get what came before it.
    func messageThread(id: String, before: String? = nil, limit: Int = 50) async throws -> MessageThread {
        try await send(
            .messageThread(id: id, before: before, limit: limit),
            body: Optional<Never>.none,
            as: MessageThread.self
        )
    }

    // MARK: Writes

    /// Says something in a plain conversation.
    ///
    /// A sealed conversation refuses this with 409 rather than quietly writing
    /// plaintext into a thread both people believe is encrypted. That refusal
    /// is the server's whole downgrade defence, so it is surfaced as its own
    /// error rather than folded into a generic failure.
    ///
    /// A 2xx does not always mean the message arrived. A thread can outlive the
    /// permission that opened it — the other person retires the Number it came
    /// through, or changes who may reach them — and when that happens the
    /// server stores the message and answers the same three states the first
    /// message to somebody gets, rather than an error. That is not an edge
    /// case to paper over: it is the state this app has to draw, so it is a
    /// return value rather than a decode failure.
    func sendMessage(conversation: String, body text: String) async throws -> MessageDelivery {
        let payload = CanonicalJSON.object([
            ("event_type", .string("message_send")),
            ("c", .string(conversation)),
            ("body", .string(text)),
        ])
        let (data, status) = try await sendRaw(.events, body: payload, contentType: "application/json")
        guard (200..<300).contains(status) else {
            throw MessagingError.from(status: status, data: data)
        }
        let decoder = APIClient.makeDecoder()
        if let stored = try? decoder.decode(ChatMessage.self, from: data) {
            return .stored(stored)
        }
        // Only a body that actually carries a state is read as one. The
        // three-state shape decodes leniently by design, so trying it first
        // would read every stored message as an undelivered one.
        if let report = try? decoder.decode(DeliveryReport.self, from: data),
           report.state != .opened {
            return .notArrived(report.state)
        }
        throw MessagingError.storedButUnreadable
    }

    /// Marks a conversation read up to now.
    ///
    /// The server tracks one timestamp per member rather than a state per
    /// message, so this is the whole of read tracking: there is nothing finer
    /// to send and no receipt to expect back.
    func markConversationRead(_ conversation: String) async throws {
        let payload = CanonicalJSON.object([
            ("event_type", .string("message_read")),
            ("c", .string(conversation)),
        ])
        let (data, status) = try await sendRaw(.events, body: payload, contentType: "application/json")
        guard (200..<300).contains(status) else {
            throw MessagingError.from(status: status, data: data)
        }
    }

    /// Writes to somebody by handle, opening a conversation if there is not
    /// one already.
    ///
    /// The message is written before the question of whether it may be
    /// delivered is asked, which is the server's design and not an accident:
    /// it means a message is never lost to a refusal, and can be released later
    /// if the person allows it.
    ///
    /// Three outcomes, and deliberately only three. Every reason a message
    /// might not get through — no such person, a retired contact address, an
    /// account that takes no messages, a block — collapses into one answer
    /// after a fixed minimum delay, so that trying and watching the result
    /// cannot be used to learn who exists or what their settings are. Asking
    /// the app to distinguish them would recreate exactly the oracle the
    /// server refuses to be.
    func startConversation(handle: String, body text: String) async throws -> ConversationStart {
        let payload = CanonicalJSON.object([
            ("event_type", .string("message_start")),
            ("handle", .string(handle)),
            ("body", .string(text)),
        ])
        let (data, status) = try await sendRaw(.events, body: payload, contentType: "application/json")
        guard (200..<300).contains(status) else {
            throw MessagingError.from(status: status, data: data)
        }
        do {
            return try APIClient.makeDecoder().decode(ConversationStart.self, from: data)
        } catch {
            throw MessagingError.malformed("the answer was not understood")
        }
    }

    // MARK: Sealed mode

    /// This account's wrapped private key, or a first run.
    func gnosisIdentity() async throws -> GnosisIdentityBlob {
        let (data, status) = try await sendRaw(.gnosisBootstrap, body: nil, contentType: nil)
        guard (200..<300).contains(status) else {
            throw MessagingError.from(status: status, data: data)
        }
        do {
            return try APIClient.makeDecoder().decode(GnosisIdentityBlob.self, from: data)
        } catch {
            throw MessagingError.malformed("the key store's answer was not understood")
        }
    }

    /// Files this account's public key in the directory and hands the server
    /// the wrapped private key to hold.
    ///
    /// Overwriting a different key is a rotation, and the server records it as
    /// a security event: a key that changes silently is exactly what a key-swap
    /// interception looks like.
    func provisionGnosisIdentity(pubB64: String, wrappedPriv: String, wrapNonce: String) async throws {
        struct Body: Encodable {
            let pub_b64: String
            let wrapped_priv: String
            let wrap_nonce: String
        }
        let payload = try JSONEncoder().encode(
            Body(pub_b64: pubB64, wrapped_priv: wrappedPriv, wrap_nonce: wrapNonce)
        )
        let (data, status) = try await sendRaw(
            .gnosisProvision, body: payload, contentType: "application/json"
        )
        guard (200..<300).contains(status) else {
            throw MessagingError.from(status: status, data: data)
        }
    }

    /// The public keys of one conversation's members.
    ///
    /// A member who has never provisioned an identity is simply absent, which
    /// is why the caller checks the set rather than assuming one key per
    /// member: sealing to nobody would produce a message no one can open.
    func gnosisDirectory(conversation: String) async throws -> [GnosisDirectoryEntry] {
        let (data, status) = try await sendRaw(
            .gnosisDirectory(conversation: conversation), body: nil, contentType: nil
        )
        guard (200..<300).contains(status) else {
            throw MessagingError.from(status: status, data: data)
        }
        do {
            return try APIClient.makeDecoder().decode([GnosisDirectoryEntry].self, from: data)
        } catch {
            throw MessagingError.malformed("the key directory's answer was not understood")
        }
    }

    /// Stores one sealed envelope and answers the stored message.
    ///
    /// The web's call to this endpoint gets a rendered bubble back. What gets
    /// the stored message instead is the `Accept: application/json` this client
    /// sets on every request, so the JSON answer is had for free — but it is
    /// the reason the header is not optional here.
    func sendSealedMessage(conversation: String, envelope: SealedEnvelope) async throws -> ChatMessage {
        struct Body: Encodable {
            let c: String
            let envelope: SealedEnvelope
        }
        let payload = try JSONEncoder().encode(Body(c: conversation, envelope: envelope))
        let (data, status) = try await sendRaw(
            .gnosisSendSealed, body: payload, contentType: "application/json"
        )
        guard (200..<300).contains(status) else {
            throw MessagingError.from(status: status, data: data)
        }
        return try decodeMessage(data, status: status)
    }

    private func decodeMessage(_ data: Data, status: Int) throws -> ChatMessage {
        do {
            return try APIClient.makeDecoder().decode(ChatMessage.self, from: data)
        } catch {
            // A 2xx means it was stored. Saying that, rather than reporting a
            // decode failure, keeps a composer from telling somebody their
            // message did not send when it did.
            throw MessagingError.storedButUnreadable
        }
    }
}

/// What became of something said in a thread that already exists.
public enum MessageDelivery: Hashable, Sendable {
    /// Stored, and in the conversation.
    case stored(ChatMessage)
    /// Stored, and not in the conversation. The state is carried so a thread
    /// can tell a message waiting on somebody's answer from one that reached
    /// nobody — but never *why* it reached nobody, which the server does not
    /// say and this app must not appear to know.
    case notArrived(ConversationStart.State)
}

/// A 2xx that reports a state instead of a stored message.
///
/// `state` is decoded rather than defaulted: that is what makes this shape
/// distinguishable from a message, and it is the whole reason the type exists
/// separately from ``ConversationStart``, which reads leniently on purpose.
private struct DeliveryReport: Decodable {
    let state: ConversationStart.State
}

/// What happened when somebody wrote to a person for the first time.
public struct ConversationStart: Codable, Hashable, Sendable {
    public enum State: String, Codable, Hashable, Sendable {
        /// The conversation is open and the message is in it.
        case opened
        /// Held: the message is stored and waiting on the other person's
        /// answer to a contact request. Nothing was lost.
        case held
        /// It did not get through, and that is all anyone is told.
        ///
        /// The wire word is `not_delivered`. Spelled out rather than left to the
        /// lenient fallback below: a case that only ever decodes because
        /// everything unknown decodes to it is a case that would keep working
        /// if the server renamed it, and stop meaning anything.
        case undelivered = "not_delivered"

        public init(from decoder: any Decoder) throws {
            let raw = try decoder.singleValueContainer().decode(String.self)
            self = State(rawValue: raw) ?? .undelivered
        }
    }

    public let state: State
    /// Present only when the conversation is open.
    public let conversation: Conversation?

    public init(state: State, conversation: Conversation? = nil) {
        self.state = state
        self.conversation = conversation
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        state = try c.decodeIfPresent(State.self, forKey: .state) ?? .undelivered
        conversation = try c.decodeIfPresent(Conversation.self, forKey: .conversation)
    }

    enum CodingKeys: String, CodingKey {
        case state, conversation
    }

    /// The one sentence a screen shows. Identical for every reason a message
    /// did not arrive, on purpose.
    public var userMessage: String {
        switch state {
        case .opened: return "Sent."
        case .held: return "Saved. They will see it if they accept your request."
        case .undelivered: return "This could not be delivered."
        }
    }
}

/// The envelope a sealed message travels in.
///
/// The field names are the server's and the crypto core's, which are the same
/// names, and they are not ours to rename: this object is written by one
/// implementation and read by another.
public struct SealedEnvelope: Codable, Hashable, Sendable {
    public let bodyCtB64: String
    public let bodyNonceB64: String
    public let sealed: [SealedKey]

    enum CodingKeys: String, CodingKey {
        case sealed
        case bodyCtB64 = "body_ct_b64"
        case bodyNonceB64 = "body_nonce_b64"
    }

    public init(bodyCtB64: String, bodyNonceB64: String, sealed: [SealedKey]) {
        self.bodyCtB64 = bodyCtB64
        self.bodyNonceB64 = bodyNonceB64
        self.sealed = sealed
    }

    /// One recipient's own wrapped copy of the message's content key.
    public struct SealedKey: Codable, Hashable, Sendable {
        public let recipientAccount: String
        public let ephPubB64: String
        public let sealedB64: String
        public let sealedNonceB64: String

        enum CodingKeys: String, CodingKey {
            case recipientAccount = "recipient_account"
            case ephPubB64 = "eph_pub_b64"
            case sealedB64 = "sealed_b64"
            case sealedNonceB64 = "sealed_nonce_b64"
        }

        public init(recipientAccount: String, ephPubB64: String, sealedB64: String, sealedNonceB64: String) {
            self.recipientAccount = recipientAccount
            self.ephPubB64 = ephPubB64
            self.sealedB64 = sealedB64
            self.sealedNonceB64 = sealedNonceB64
        }
    }
}

/// What can go wrong sending or reading a message.
///
/// Its own type rather than ``APIError`` because these routes are on the write
/// lane and the sealed-key routes, and neither answers the JSON error envelope
/// the read surface does — they answer a bare line of text. Mapping that to a
/// pretend error code would be inventing one.
public enum MessagingError: Error, Hashable, Sendable {
    /// The reader is not signed in, or the session has expired.
    case unauthenticated
    /// Not a member of this conversation.
    case forbidden
    /// The conversation is gone, or never existed.
    case notFound
    /// A plain send was attempted in a sealed conversation. The composer should
    /// have sealed it; reaching this means the mode was read wrong.
    case sealedConversation
    /// Nothing to send to: no member of this conversation has a messaging key.
    case noRecipients
    /// Somebody's key changed since this device last sealed to them. A new
    /// device, or an interception; the reader decides which.
    case keyChanged([String])
    /// Longer than the server will store.
    case tooLong
    /// Sending too fast.
    case rateLimited
    /// The message was stored but the answer could not be read.
    case storedButUnreadable
    /// The server answered something this build does not understand.
    case malformed(String)
    /// Anything else, with whatever the server said.
    case server(status: Int, message: String)

    static func from(status: Int, data: Data) -> MessagingError {
        let text = String(decoding: data.prefix(400), as: UTF8.self)
            .trimmingCharacters(in: .whitespacesAndNewlines)
        switch status {
        case 401: return .unauthenticated
        case 403: return .forbidden
        case 404: return .notFound
        case 409: return .sealedConversation
        case 413: return .tooLong
        case 429: return .rateLimited
        case 400 where text.contains("recipient"): return .noRecipients
        default: return .server(status: status, message: text.isEmpty ? "HTTP \(status)" : text)
        }
    }

    /// The one string a screen may show.
    public var userMessage: String {
        switch self {
        case .unauthenticated: return "Sign in again to send messages."
        case .forbidden: return "You are not in this conversation."
        case .notFound: return "This conversation is no longer here."
        case .sealedConversation: return "This conversation is encrypted. Unlock it to send."
        case .noRecipients: return "Nobody here has set up encryption yet."
        case .keyChanged(let accounts):
            let who = accounts.count == 1 ? "Someone in this conversation" : "\(accounts.count) people here"
            return "\(who) changed encryption keys. That is a new device, or somebody in the middle. Nothing was sent."
        case .tooLong: return "That message is too long."
        case .rateLimited: return "Slow down a moment, then try again."
        case .storedButUnreadable: return "Sent, but the reply could not be read."
        case .malformed(let what): return what.isEmpty ? "Something went wrong." : what
        case .server(_, let message): return message.isEmpty ? "Something went wrong." : message
        }
    }
}
