import Foundation

/// One row of the Notifications.
///
/// **No handler serves this.** There is no `/api/v1/notifications` today, and
/// the Notifications screen says so rather than showing anything. This type exists for
/// two reasons: it is the shape the screen is built against, and it is a
/// proposal the Go side can be written to satisfy.
///
/// It is a projection of the `notifications` table — `type`, `actor_id`,
/// `target_id`, `target_type`, `is_read`, `created_at` — rather than an invented
/// object, with two deliberate differences:
///
/// - `actors` is plural. The grouping rule is the server's (like, repost and
///   follow group within an hour against the same target; everything else does
///   not), and a client that grouped rows itself would have to hold a page of
///   ungrouped notifications to do it — and would group differently from the
///   web, which groups in SQL.
/// - `preview` and `amountUAET` are carried on the row. A notification the
///   reader cannot understand without a second request is a notification that
///   renders as a spinner.
///
/// Actor identity travels as `WorkAuthor` — handle, display name, avatar,
/// badges — because that is exactly what a row draws, and because it already
/// withholds the PIAL the way every other projection does.
public struct NotificationItem: Codable, Hashable, Sendable, Identifiable {
    public let id: String

    /// like | repost | follow | reply | quote | mention | tip | subscribe |
    /// thread_reply. Open at the edges: an unknown kind still renders, with the
    /// neutral icon and the actor's own words, so a kind added server-side does
    /// not need a client release to stop being a blank row.
    public let kind: String

    /// Who did it, newest first. Capped server-side at the three a row can draw;
    /// `actorCount` carries the real total.
    public let actors: [WorkAuthor]
    /// How many people this row is about, including the ones not in `actors`.
    public let actorCount: Int

    /// The work this is about, when there is one, so tapping the row can push
    /// straight to it. Absent on a follow, which is about a person.
    public let targetID: String?
    /// work | profile
    public let targetType: String

    /// The first line of whatever was said — the reply, the quote, the mention.
    /// Absent on kinds that have no words of their own.
    public let preview: String?

    /// Set on a tip, in micro-AET. Integers all the way down, like every other
    /// amount on the wire.
    public let amountUAET: Int64?

    public let isRead: Bool
    public let createdAt: Date

    enum CodingKeys: String, CodingKey {
        case id, kind, actors, preview
        case actorCount = "actor_count"
        case targetID = "target_id"
        case targetType = "target_type"
        case amountUAET = "amount_uaet"
        case isRead = "is_read"
        case createdAt = "created_at"
    }

    public init(
        id: String,
        kind: String,
        actors: [WorkAuthor],
        actorCount: Int? = nil,
        targetID: String? = nil,
        targetType: String = "work",
        preview: String? = nil,
        amountUAET: Int64? = nil,
        isRead: Bool = false,
        createdAt: Date
    ) {
        self.id = id
        self.kind = kind
        self.actors = actors
        self.actorCount = actorCount ?? actors.count
        self.targetID = targetID
        self.targetType = targetType
        self.preview = preview
        self.amountUAET = amountUAET
        self.isRead = isRead
        self.createdAt = createdAt
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        kind = try c.decode(String.self, forKey: .kind)
        actors = try c.decodeIfPresent([WorkAuthor].self, forKey: .actors) ?? []
        // A server that groups but forgets to count still draws a correct row.
        actorCount = try c.decodeIfPresent(Int.self, forKey: .actorCount) ?? actors.count
        targetID = try c.decodeIfPresent(String.self, forKey: .targetID)
        targetType = try c.decodeIfPresent(String.self, forKey: .targetType) ?? "work"
        preview = try c.decodeIfPresent(String.self, forKey: .preview)
        amountUAET = try c.decodeIfPresent(Int64.self, forKey: .amountUAET)
        isRead = try c.decodeIfPresent(Bool.self, forKey: .isRead) ?? false
        createdAt = try c.decode(Date.self, forKey: .createdAt)
    }
}

// MARK: - Display

public extension NotificationItem {

    /// Which kinds the server groups. Straight from the notification table in
    /// CLAUDE.md: like, repost and follow group within an hour against the same
    /// target; a reply, a quote, a mention, a tip or a subscription is a single
    /// event about a single person and reads wrong pooled with others.
    ///
    /// The client does not do the grouping. This is here so a row can tell the
    /// difference between "one person" and "one person, so far" — a grouped row
    /// with a single actor still says "@dev liked your work", but it is the kind
    /// of row that will say "and 4 others" an hour from now.
    static func groups(kind: String) -> Bool {
        switch kind {
        case "like", "repost", "follow": return true
        default: return false
        }
    }

    /// The sentence a row leads with: "@dev and 4 others liked your work".
    ///
    /// Composed on device, unlike a work's provenance, and the difference is
    /// worth stating. Provenance is an account of a decision the ranking engine
    /// made and the client has no view into, so it arrives as prose. This is a
    /// verb applied to a list of names — no judgement, nothing the server knows
    /// that the row does not already carry — and a native client that asked the
    /// server for a rendered sentence per row would be asking for HTML it cannot
    /// use anyway.
    var summary: String {
        let names = actorNames
        guard !names.isEmpty else { return verb.capitalizedFirst }

        let extra = max(0, actorCount - names.count)
        let subject: String
        switch (names.count, extra) {
        case (_, let n) where n > 0:
            subject = "\(names[0]) and \(Counts.exact(n + names.count - 1)) others"
        case (1, _):
            subject = names[0]
        case (2, _):
            subject = "\(names[0]) and \(names[1])"
        default:
            subject = "\(names[0]), \(names[1]) and \(names[2])"
        }

        if kind == "tip", let amountUAET {
            return "\(subject) tipped \(AET.label(uAET: amountUAET))"
        }
        return "\(subject) \(verb)"
    }

    /// What happened, as the predicate of `summary`.
    ///
    /// An unknown kind falls back to the neutral phrase rather than to nothing:
    /// a row that says only a handle is a row the reader has to tap to
    /// understand.
    private var verb: String {
        switch kind {
        case "like": return targetType == "profile" ? "liked your profile" : "liked your work"
        case "repost": return "reposted your work"
        case "follow": return "followed you"
        case "reply": return "replied to you"
        case "quote": return "quoted your work"
        case "mention": return "mentioned you"
        case "thread_reply": return "replied in your thread"
        case "tip": return "tipped your work"
        case "purchase": return "bought your track"
        case "subscribe": return "subscribed to you"
        default: return "did something on your work"
        }
    }

    private var actorNames: [String] {
        actors.prefix(3).map { "@\($0.handle)" }
    }

    /// Where tapping the row goes, when it goes anywhere.
    ///
    /// A follow is about a person and a like is about a work, and a row that
    /// navigated to the wrong one of the two is worse than a row that does not
    /// navigate at all.
    var destinationWorkID: String? {
        guard targetType == "work", let targetID, !targetID.isEmpty else { return nil }
        return targetID
    }

    var destinationHandle: String? {
        if targetType == "profile" || kind == "follow" || kind == "subscribe" {
            return actors.first?.handle
        }
        return nil
    }
}

private extension String {
    var capitalizedFirst: String {
        guard let first else { return self }
        return String(first).uppercased() + dropFirst()
    }
}

#if DEBUG
public extension SampleData {
    /// Notification rows for looking at the Notifications layout.
    ///
    /// DEBUG only, and reachable only in sample mode. Nothing in the app falls
    /// back to these: with no notifications endpoint, the live Notifications says it has
    /// none rather than showing invented ones.
    static let notifications: [NotificationItem] = decodeNotifications(notificationsJSON)

    private static func decodeNotifications(_ raw: String) -> [NotificationItem] {
        do {
            return try APIClient.makeDecoder().decode([NotificationItem].self, from: Data(raw.utf8))
        } catch {
            fatalError("Sample notifications no longer decode: \(error)")
        }
    }

    private static func minutesAgo(_ minutes: Double) -> String {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        return f.string(from: Date().addingTimeInterval(-minutes * 60))
    }

    private static let notificationsJSON = """
    [
      {
        "id": "b1000000-0001-4a00-9c11-000000000001",
        "kind": "like",
        "actors": [
          { "handle": "miiyazuko", "display_name": "Mii Yazuko", "is_creator": true, "role": "creator", "realm": 4 },
          { "handle": "admin", "display_name": "Admin", "is_verified": true, "role": "admin", "realm": 5 },
          { "handle": "guest", "display_name": "Guest", "role": "user", "realm": 1 }
        ],
        "actor_count": 12,
        "target_id": "0f7d1c2e-0001-4a00-9c11-000000000001",
        "target_type": "work",
        "preview": "Shipped the native client's first screen tonight.",
        "is_read": false,
        "created_at": "\(minutesAgo(4))"
      },
      {
        "id": "b1000000-0002-4a00-9c11-000000000002",
        "kind": "reply",
        "actors": [
          { "handle": "nocturnesignal", "display_name": "Nocturne Signal", "is_creator": true, "is_verified": true, "role": "creator", "realm": 4 }
        ],
        "target_id": "0f7d1c2e-0009-4a00-9c11-000000000009",
        "target_type": "work",
        "preview": "the B-side needs the room mic higher, everything else is right",
        "is_read": false,
        "created_at": "\(minutesAgo(38))"
      },
      {
        "id": "b1000000-0003-4a00-9c11-000000000003",
        "kind": "tip",
        "actors": [
          { "handle": "tehanibentley", "display_name": "Tehani Bentley", "is_verified": true, "is_creator": true, "role": "founder", "realm": 5 }
        ],
        "target_id": "0f7d1c2e-0002-4a00-9c11-000000000002",
        "target_type": "work",
        "amount_uaet": 2500000,
        "is_read": false,
        "created_at": "\(minutesAgo(96))"
      },
      {
        "id": "b1000000-0004-4a00-9c11-000000000004",
        "kind": "follow",
        "actors": [
          { "handle": "guest", "display_name": "Guest", "role": "user", "realm": 1 },
          { "handle": "dev", "display_name": "Engineering", "role": "user", "realm": 3 }
        ],
        "actor_count": 2,
        "target_type": "profile",
        "is_read": true,
        "created_at": "\(minutesAgo(400))"
      },
      {
        "id": "b1000000-0005-4a00-9c11-000000000005",
        "kind": "quote",
        "actors": [
          { "handle": "admin", "display_name": "Admin", "is_verified": true, "role": "admin", "realm": 5 }
        ],
        "target_id": "0f7d1c2e-0003-4a00-9c11-000000000003",
        "target_type": "work",
        "preview": "this is the part everyone gets wrong",
        "is_read": true,
        "created_at": "\(minutesAgo(1500))"
      }
    ]
    """
}
#endif
