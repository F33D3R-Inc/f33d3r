import Foundation

/// Messaging, as the app receives it.
///
/// Two kinds of conversation, and the difference is not a setting anybody
/// chose. A conversation is **sealed** — end-to-end encrypted, ciphertext only,
/// the server unable to read a word of it — when every participant is an 18+
/// verified adult, and **plain** when any one of them is not. Encryption needs
/// both sides to hold key material, an unverified account has none, and so the
/// lowest-capability participant sets the floor. The mode is fixed when the
/// conversation is created and there is no setter for it anywhere, on any
/// surface, which is what stops a downgrade being an attack.
///
/// The app has to honour both. A plain message arrives as text. A sealed one
/// arrives as an envelope this device opens itself, and the mode is what tells
/// the two apart, so it is on the wire rather than inferred from which fields
/// happen to be populated.
public enum MessageMode: String, Codable, Hashable, Sendable {
    case plain
    case sealed

    /// Read leniently: a mode this build has never heard of is treated as
    /// sealed, because the safe failure is a message this device cannot show
    /// rather than one it shows in the clear when it should not have.
    public init(from decoder: any Decoder) throws {
        let raw = try decoder.singleValueContainer().decode(String.self)
        self = MessageMode(rawValue: raw) ?? .sealed
    }
}

/// One row of the conversation list.
public struct Conversation: Codable, Hashable, Sendable, Identifiable {
    public let id: String
    public let mode: MessageMode
    public let isGroup: Bool
    /// A group's name. Empty for a direct message, which is named by whoever is
    /// on the other end of it.
    public let title: String
    public let otherHandle: String
    public let otherDisplay: String
    public let otherAvatar: String?
    /// The last thing said, when the server can read it. A sealed conversation
    /// sends nothing here, because the server genuinely does not know — see
    /// ``previewText``.
    public let preview: String
    public let lastAt: Date
    public let unread: Int

    enum CodingKeys: String, CodingKey {
        case id, mode, title, preview, unread
        case isGroup = "is_group"
        case otherHandle = "other_handle"
        case otherDisplay = "other_display"
        case otherAvatar = "other_avatar"
        case lastAt = "last_at"
    }

    public init(
        id: String,
        mode: MessageMode = .plain,
        isGroup: Bool = false,
        title: String = "",
        otherHandle: String = "",
        otherDisplay: String = "",
        otherAvatar: String? = nil,
        preview: String = "",
        lastAt: Date,
        unread: Int = 0
    ) {
        self.id = id
        self.mode = mode
        self.isGroup = isGroup
        self.title = title
        self.otherHandle = otherHandle
        self.otherDisplay = otherDisplay
        self.otherAvatar = otherAvatar
        self.preview = preview
        self.lastAt = lastAt
        self.unread = unread
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        mode = try c.decodeIfPresent(MessageMode.self, forKey: .mode) ?? .plain
        isGroup = try c.decodeIfPresent(Bool.self, forKey: .isGroup) ?? false
        title = try c.decodeIfPresent(String.self, forKey: .title) ?? ""
        otherHandle = try c.decodeIfPresent(String.self, forKey: .otherHandle) ?? ""
        otherDisplay = try c.decodeIfPresent(String.self, forKey: .otherDisplay) ?? ""
        let avatar = try c.decodeIfPresent(String.self, forKey: .otherAvatar)
        otherAvatar = (avatar?.isEmpty ?? true) ? nil : avatar
        preview = try c.decodeIfPresent(String.self, forKey: .preview) ?? ""
        lastAt = try c.decode(Date.self, forKey: .lastAt)
        unread = try c.decodeIfPresent(Int.self, forKey: .unread) ?? 0
    }

    /// What the row is called. A group wears its title; a direct message wears
    /// the other person, and falls back to their handle when they have set no
    /// display name.
    public var name: String {
        if isGroup { return title.isEmpty ? "Group" : title }
        if !otherDisplay.isEmpty { return otherDisplay }
        return otherHandle.isEmpty ? "Conversation" : "@" + otherHandle
    }

    /// The line under the name.
    ///
    /// A sealed conversation has none, and says so rather than showing a blank
    /// row: the absence is the server being unable to read the message, which
    /// is the feature, and a row that looked empty would read as a bug.
    public var previewText: String {
        if mode == .sealed { return "Encrypted message" }
        return preview
    }

    public var isUnread: Bool { unread > 0 }
}

/// One message in a thread.
///
/// In plain mode ``body`` carries the text. In sealed mode it is empty and the
/// envelope fields carry the ciphertext, along with *this device's own* wrapped
/// content key — the server hands each member a different one, because a sealed
/// message is sealed separately to every recipient.
public struct ChatMessage: Codable, Hashable, Sendable, Identifiable {
    public let id: String
    public let conversationID: String
    /// The sender's account id. Present because it is what a sealed key is
    /// addressed to, and it is the same id the key directory already returns.
    public let senderAccount: String
    public let senderHandle: String
    public let senderDisplay: String
    public let senderAvatar: String?
    public let mode: MessageMode
    public let body: String
    public let bodyCtB64: String
    public let bodyNonceB64: String
    public let ephPubB64: String
    public let sealedB64: String
    public let sealedNonceB64: String
    public let createdAt: Date
    public let isMine: Bool

    enum CodingKeys: String, CodingKey {
        case id, mode, body
        case conversationID = "conversation_id"
        case senderAccount = "sender_account"
        case senderHandle = "sender_handle"
        case senderDisplay = "sender_display"
        case senderAvatar = "sender_avatar"
        case bodyCtB64 = "body_ct_b64"
        case bodyNonceB64 = "body_nonce_b64"
        case ephPubB64 = "eph_pub_b64"
        case sealedB64 = "sealed_b64"
        case sealedNonceB64 = "sealed_nonce_b64"
        case createdAt = "created_at"
        case isMine = "is_mine"
    }

    public init(
        id: String,
        conversationID: String,
        senderAccount: String = "",
        senderHandle: String = "",
        senderDisplay: String = "",
        senderAvatar: String? = nil,
        mode: MessageMode = .plain,
        body: String = "",
        bodyCtB64: String = "",
        bodyNonceB64: String = "",
        ephPubB64: String = "",
        sealedB64: String = "",
        sealedNonceB64: String = "",
        createdAt: Date,
        isMine: Bool = false
    ) {
        self.id = id
        self.conversationID = conversationID
        self.senderAccount = senderAccount
        self.senderHandle = senderHandle
        self.senderDisplay = senderDisplay
        self.senderAvatar = senderAvatar
        self.mode = mode
        self.body = body
        self.bodyCtB64 = bodyCtB64
        self.bodyNonceB64 = bodyNonceB64
        self.ephPubB64 = ephPubB64
        self.sealedB64 = sealedB64
        self.sealedNonceB64 = sealedNonceB64
        self.createdAt = createdAt
        self.isMine = isMine
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        conversationID = try c.decodeIfPresent(String.self, forKey: .conversationID) ?? ""
        senderAccount = try c.decodeIfPresent(String.self, forKey: .senderAccount) ?? ""
        senderHandle = try c.decodeIfPresent(String.self, forKey: .senderHandle) ?? ""
        senderDisplay = try c.decodeIfPresent(String.self, forKey: .senderDisplay) ?? ""
        let avatar = try c.decodeIfPresent(String.self, forKey: .senderAvatar)
        senderAvatar = (avatar?.isEmpty ?? true) ? nil : avatar
        mode = try c.decodeIfPresent(MessageMode.self, forKey: .mode) ?? .plain
        body = try c.decodeIfPresent(String.self, forKey: .body) ?? ""
        bodyCtB64 = try c.decodeIfPresent(String.self, forKey: .bodyCtB64) ?? ""
        bodyNonceB64 = try c.decodeIfPresent(String.self, forKey: .bodyNonceB64) ?? ""
        ephPubB64 = try c.decodeIfPresent(String.self, forKey: .ephPubB64) ?? ""
        sealedB64 = try c.decodeIfPresent(String.self, forKey: .sealedB64) ?? ""
        sealedNonceB64 = try c.decodeIfPresent(String.self, forKey: .sealedNonceB64) ?? ""
        createdAt = try c.decode(Date.self, forKey: .createdAt)
        isMine = try c.decodeIfPresent(Bool.self, forKey: .isMine) ?? false
    }

    /// Whether this message needs opening before it can be read.
    public var isSealed: Bool { mode == .sealed }

    /// Whether the server sent everything this device needs to open it. A
    /// sealed message with no key for us is one written before this account had
    /// an identity, and no amount of retrying will open it.
    public var isOpenable: Bool {
        !bodyCtB64.isEmpty && !bodyNonceB64.isEmpty
            && !ephPubB64.isEmpty && !sealedB64.isEmpty && !sealedNonceB64.isEmpty
    }

    public var senderName: String {
        if !senderDisplay.isEmpty { return senderDisplay }
        return senderHandle.isEmpty ? "Someone" : senderHandle
    }
}

/// One member of a conversation, for the thread's header and its key set.
public struct ConversationMember: Codable, Hashable, Sendable, Identifiable {
    public let account: String
    public let handle: String
    public let display: String
    public let avatar: String?

    public var id: String { account }

    enum CodingKeys: String, CodingKey {
        case account, handle, display, avatar
    }

    public init(account: String, handle: String, display: String = "", avatar: String? = nil) {
        self.account = account
        self.handle = handle
        self.display = display
        self.avatar = avatar
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        account = try c.decodeIfPresent(String.self, forKey: .account) ?? ""
        handle = try c.decodeIfPresent(String.self, forKey: .handle) ?? ""
        display = try c.decodeIfPresent(String.self, forKey: .display) ?? ""
        let raw = try c.decodeIfPresent(String.self, forKey: .avatar)
        avatar = (raw?.isEmpty ?? true) ? nil : raw
    }

    public var name: String { display.isEmpty ? handle : display }
}

/// A page of the conversation list.
public struct ConversationPage: Codable, Hashable, Sendable {
    public let conversations: [Conversation]
    /// Every unread message across every conversation, so the tab can badge
    /// itself without adding up rows it may not have all of.
    public let unreadTotal: Int

    enum CodingKeys: String, CodingKey {
        case conversations
        case unreadTotal = "unread_total"
    }

    public init(conversations: [Conversation], unreadTotal: Int = 0) {
        self.conversations = conversations
        self.unreadTotal = unreadTotal
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        conversations = try c.decodeIfPresent([Conversation].self, forKey: .conversations) ?? []
        unreadTotal = try c.decodeIfPresent(Int.self, forKey: .unreadTotal)
            ?? conversations.reduce(0) { $0 + $1.unread }
    }
}

/// One conversation with a page of its messages.
public struct MessageThread: Codable, Hashable, Sendable {
    public let conversation: Conversation
    /// Oldest first, which is the order a thread is read in.
    public let messages: [ChatMessage]
    public let members: [ConversationMember]
    public let nextCursor: String?

    enum CodingKeys: String, CodingKey {
        case conversation, messages, members
        case nextCursor = "next_cursor"
    }

    public init(
        conversation: Conversation,
        messages: [ChatMessage],
        members: [ConversationMember] = [],
        nextCursor: String? = nil
    ) {
        self.conversation = conversation
        self.messages = messages
        self.members = members
        self.nextCursor = nextCursor
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        conversation = try c.decode(Conversation.self, forKey: .conversation)
        messages = try c.decodeIfPresent([ChatMessage].self, forKey: .messages) ?? []
        members = try c.decodeIfPresent([ConversationMember].self, forKey: .members) ?? []
        let cursor = try c.decodeIfPresent(String.self, forKey: .nextCursor)
        nextCursor = (cursor?.isEmpty ?? true) ? nil : cursor
    }

    public var hasMore: Bool { nextCursor != nil }
}

// MARK: The sealed-mode key material

/// The account's own messaging identity, as the server holds it.
///
/// The private key never reaches the server unwrapped: what is stored is the
/// ciphertext of it under a key derived from the password at sign-in, and these
/// two fields are that ciphertext and its nonce. `has` false means this account
/// has never provisioned an identity, which is a first run rather than an error.
public struct GnosisIdentityBlob: Codable, Hashable, Sendable {
    public let has: Bool
    public let wrappedPriv: String
    public let wrapNonce: String

    enum CodingKeys: String, CodingKey {
        case has
        case wrappedPriv = "wrapped_priv"
        case wrapNonce = "wrap_nonce"
    }

    public init(has: Bool, wrappedPriv: String = "", wrapNonce: String = "") {
        self.has = has
        self.wrappedPriv = wrappedPriv
        self.wrapNonce = wrapNonce
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        has = try c.decodeIfPresent(Bool.self, forKey: .has) ?? false
        wrappedPriv = try c.decodeIfPresent(String.self, forKey: .wrappedPriv) ?? ""
        wrapNonce = try c.decodeIfPresent(String.self, forKey: .wrapNonce) ?? ""
    }

    /// True when there is something here to unwrap. `has` alone is not enough:
    /// an identity row with an empty blob would unwrap to nothing.
    public var isUnwrappable: Bool { has && !wrappedPriv.isEmpty && !wrapNonce.isEmpty }
}

/// One entry of the key directory: whose key, and the key.
///
/// The directory only answers for conversations the caller is in. That is
/// deliberate on the server's part — an open directory is a harvesting surface
/// — and it means the app can only ever ask for keys it is about to use.
public struct GnosisDirectoryEntry: Codable, Hashable, Sendable, Identifiable {
    public let account: String
    public let pubB64: String

    public var id: String { account }

    enum CodingKeys: String, CodingKey {
        case account
        case pubB64 = "pub_b64"
    }

    public init(account: String, pubB64: String) {
        self.account = account
        self.pubB64 = pubB64
    }
}
