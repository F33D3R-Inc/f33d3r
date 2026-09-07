import Foundation

/// F33D3R Numbers, as the app receives them.
///
/// A Number is a contact address: twelve digits somebody can be reached at
/// without knowing their handle, and without the caller learning anything about
/// them by trying. An account can hold several, each with its own policy, and
/// can retire any of them without touching the others — which is the whole
/// point of having more than one. A Number given to a venue and a Number given
/// to a friend are revoked separately.
///
/// The policy on a Number, and the account-wide one under it, are the owner's
/// business alone. Nothing here is ever shown to a caller: a refusal names no
/// policy, and this file has no path that could.
public struct ContactNumber: Codable, Hashable, Sendable, Identifiable {
    public let id: String
    /// The Number as the server holds it. Read through ``display`` rather than
    /// directly, so one grouping reaches the screen.
    public let number: String
    public let policy: String
    public let createdAt: Date
    public let revoked: Bool

    enum CodingKeys: String, CodingKey {
        case id, number, policy, revoked
        case createdAt = "created_at"
    }

    public init(
        id: String,
        number: String,
        policy: String = ContactPolicy.open,
        createdAt: Date,
        revoked: Bool = false
    ) {
        self.id = id
        self.number = number
        self.policy = policy
        self.createdAt = createdAt
        self.revoked = revoked
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        number = try c.decodeIfPresent(String.self, forKey: .number) ?? ""
        policy = try c.decodeIfPresent(String.self, forKey: .policy) ?? ContactPolicy.open
        createdAt = try c.decode(Date.self, forKey: .createdAt)
        revoked = try c.decodeIfPresent(Bool.self, forKey: .revoked) ?? false
    }

    /// The parsed Number, when this build understands its shape.
    ///
    /// Nil is not an error. A Number minted in a format added after this build
    /// still belongs to the person looking at it, so ``display`` shows the
    /// server's own string rather than hiding a row the owner needs to read out.
    public var parsed: F33Number? { F33Number(number) }

    /// What goes on screen and gets read aloud: groups separated by spaces.
    public var display: String { parsed?.spoken ?? number }

    /// What lands on the pasteboard. The canonical hyphenated form, because
    /// that is what survives being pasted into a field somewhere else.
    public var copyable: String { parsed?.canonical ?? number }

    /// Whether this is one of the Numbers minted before the digit format.
    /// Owner-facing only — it is an offer to mint a fresh one, never a fact
    /// about anybody else's Number.
    public var isLegacy: Bool { parsed?.isLegacy ?? false }
}

/// Who may reach an account, and by which door.
///
/// The names are the server's. The list of them is the server's too — it
/// arrives with the Numbers and the picker is built from it, so a policy added
/// on the far side appears here without a new build, and one removed stops
/// being offered. Only the wording is ours.
public enum ContactPolicy {
    public static let open = "open"

    /// The label for a policy id, including one this build has never seen.
    public static func title(_ id: String) -> String {
        switch id {
        case "open": return "Anyone"
        case "followers": return "People who follow you"
        case "mutuals": return "People you follow back"
        case "number_only": return "Only with a Number"
        case "capability_only": return "Only with a contact link"
        case "closed": return "Nobody"
        default:
            // A policy from a newer server, named as readably as its id allows.
            // Showing the raw id is better than dropping the row: the owner
            // would otherwise have a setting they cannot see or change.
            return id.replacingOccurrences(of: "_", with: " ").capitalized
        }
    }

    /// The sentence under the picker. Says what the choice does, never what
    /// somebody turned away would be told.
    public static func explanation(_ id: String) -> String {
        switch id {
        case "open": return "Anyone can write to you."
        case "followers": return "People who follow you can write to you. Anyone else has to ask."
        case "mutuals": return "People you follow back can write to you. Anyone else has to ask."
        case "number_only": return "Only somebody who knows one of your Numbers can write to you."
        case "capability_only": return "Only somebody holding a contact link you handed out."
        case "closed": return "Nobody can start a conversation with you."
        default: return "Set on the server."
        }
    }
}

/// The account's Numbers, its contact policy, and the policies it may choose
/// from.
public struct NumbersPage: Codable, Hashable, Sendable {
    public let numbers: [ContactNumber]
    /// The account-wide policy, which applies where a Number does not say
    /// otherwise.
    public let contactPolicy: String
    /// Every policy this server offers, in the order it offers them. The picker
    /// is built from this and nothing else — a hardcoded list here would either
    /// offer a setting the server rejects or hide one it supports.
    public let policies: [String]

    enum CodingKeys: String, CodingKey {
        case numbers, policies
        case contactPolicy = "contact_policy"
    }

    public init(numbers: [ContactNumber], contactPolicy: String, policies: [String]) {
        self.numbers = numbers
        self.contactPolicy = contactPolicy
        self.policies = policies
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        numbers = try c.decodeIfPresent([ContactNumber].self, forKey: .numbers) ?? []
        contactPolicy = try c.decodeIfPresent(String.self, forKey: .contactPolicy) ?? ContactPolicy.open
        policies = try c.decodeIfPresent([String].self, forKey: .policies) ?? []
    }

    /// The live Numbers, newest first. A revoked one is kept on the wire so the
    /// owner can see that it is gone rather than wonder where it went.
    public var active: [ContactNumber] {
        numbers.filter { !$0.revoked }.sorted { $0.createdAt > $1.createdAt }
    }

    public var revoked: [ContactNumber] {
        numbers.filter(\.revoked).sorted { $0.createdAt > $1.createdAt }
    }
}

/// Somebody asking to be let through.
///
/// A knock, not a rejection and not a contacts list — there is no such list on
/// F33D3R, and no surface may imply one. The note is what the decision rests
/// on, so it is carried even when the requester has no account this brain can
/// name.
public struct ContactRequest: Codable, Hashable, Sendable, Identifiable {
    public let id: String
    public let handle: String
    public let display: String
    public let avatar: String?
    public let note: String
    public let createdAt: Date

    enum CodingKeys: String, CodingKey {
        case id, handle, display, avatar, note
        case createdAt = "created_at"
    }

    public init(
        id: String,
        handle: String = "",
        display: String = "",
        avatar: String? = nil,
        note: String = "",
        createdAt: Date
    ) {
        self.id = id
        self.handle = handle
        self.display = display
        self.avatar = avatar
        self.note = note
        self.createdAt = createdAt
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        handle = try c.decodeIfPresent(String.self, forKey: .handle) ?? ""
        display = try c.decodeIfPresent(String.self, forKey: .display) ?? ""
        let raw = try c.decodeIfPresent(String.self, forKey: .avatar)
        avatar = (raw?.isEmpty ?? true) ? nil : raw
        note = try c.decodeIfPresent(String.self, forKey: .note) ?? ""
        createdAt = try c.decode(Date.self, forKey: .createdAt)
    }

    /// Who is asking. A requester with no account here stays anonymous rather
    /// than being dropped: somebody is asking either way, and a row the
    /// recipient cannot see is a decision they cannot make.
    public var name: String {
        if !display.isEmpty { return display }
        if !handle.isEmpty { return "@" + handle }
        return "Someone"
    }
}

/// The requests waiting on the reader.
public struct ContactRequestPage: Codable, Hashable, Sendable {
    public let requests: [ContactRequest]

    public init(requests: [ContactRequest]) {
        self.requests = requests
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        requests = try c.decodeIfPresent([ContactRequest].self, forKey: .requests) ?? []
    }
}
