import Foundation

/// The wallet, as a screen needs it.
///
/// **Nothing serves this.** The wallet belongs to Ain Soph, a different brain
/// with its own database (`f33d3r_wallet`), which is not running in this
/// deployment and has no `/api/v1` surface for a native client to call. Nantar
/// is the only edge brain, so the shape below is a proposal for what Nantar
/// would proxy — not a contract anything satisfies today.
///
/// It exists so the wallet screen has a real layout to be built and reviewed
/// against. The live path never touches it: with no endpoint, the screen says it
/// is not connected. The one thing this platform must never do is put a number
/// next to somebody's name and let them believe it.
///
/// Every amount is micro-AET, like every other amount on the wire. A balance
/// rounded through a `Double` on its way to a screen is a balance that
/// eventually disagrees with the ledger, and the ledger is the one that is right.
public struct WalletSnapshot: Codable, Hashable, Sendable {
    /// Settled, spendable, in micro-AET.
    public let balanceUAET: Int64
    /// Money that has arrived but is not spendable yet — a tip inside its
    /// reversal window, a payout in flight. Shown apart from the balance,
    /// because a figure the reader cannot spend is not their balance.
    public let pendingUAET: Int64
    /// Newest first.
    public let entries: [WalletEntry]

    enum CodingKeys: String, CodingKey {
        case entries
        case balanceUAET = "balance_uaet"
        case pendingUAET = "pending_uaet"
    }

    public init(balanceUAET: Int64, pendingUAET: Int64 = 0, entries: [WalletEntry] = []) {
        self.balanceUAET = balanceUAET
        self.pendingUAET = pendingUAET
        self.entries = entries
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        balanceUAET = try c.decodeIfPresent(Int64.self, forKey: .balanceUAET) ?? 0
        pendingUAET = try c.decodeIfPresent(Int64.self, forKey: .pendingUAET) ?? 0
        entries = try c.decodeIfPresent([WalletEntry].self, forKey: .entries) ?? []
    }
}

/// One movement of money.
public struct WalletEntry: Codable, Hashable, Sendable, Identifiable {
    public let id: String
    /// tip_received | tip_sent | subscription | unlock | payout
    public let kind: String
    /// Signed, in micro-AET: what this did to the balance. Signed rather than a
    /// magnitude plus a direction flag, so a row cannot be drawn with the
    /// direction of one field and the sign of another.
    public let amountUAET: Int64
    /// The other person, when there is one. A payout has none.
    public let counterpartyHandle: String?
    public let createdAt: Date

    enum CodingKeys: String, CodingKey {
        case id, kind
        case amountUAET = "amount_uaet"
        case counterpartyHandle = "counterparty_handle"
        case createdAt = "created_at"
    }

    public init(
        id: String,
        kind: String,
        amountUAET: Int64,
        counterpartyHandle: String? = nil,
        createdAt: Date
    ) {
        self.id = id
        self.kind = kind
        self.amountUAET = amountUAET
        self.counterpartyHandle = counterpartyHandle
        self.createdAt = createdAt
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        kind = try c.decode(String.self, forKey: .kind)
        amountUAET = try c.decode(Int64.self, forKey: .amountUAET)
        counterpartyHandle = try c.decodeIfPresent(String.self, forKey: .counterpartyHandle)
        createdAt = try c.decode(Date.self, forKey: .createdAt)
    }

    public var isCredit: Bool { amountUAET >= 0 }

    /// "+2.5 AET" / "−3 AET". A minus sign, not a hyphen — the row is a number
    /// first and a string second.
    public var signedLabel: String {
        let magnitude = AET.label(uAET: abs(amountUAET))
        return isCredit ? "+\(magnitude)" : "−\(magnitude)"
    }

    /// What happened, for the row's first line.
    public var title: String {
        switch kind {
        case "tip_received": return "Tip received"
        case "tip_sent": return "Tip sent"
        case "subscription": return "Subscription"
        case "unlock": return "Unlocked a work"
        case "payout": return "Payout"
        default: return "Transfer"
        }
    }
}

#if DEBUG
public extension SampleData {
    /// A wallet for looking at the layout. DEBUG only, reachable only in sample
    /// mode, and drawn under a banner that says what it is — an amount on a
    /// screenshot has no environment variable attached to it.
    static let wallet: WalletSnapshot = decodeWallet(walletJSON)

    private static func decodeWallet(_ raw: String) -> WalletSnapshot {
        do {
            return try APIClient.makeDecoder().decode(WalletSnapshot.self, from: Data(raw.utf8))
        } catch {
            fatalError("Sample wallet no longer decodes: \(error)")
        }
    }

    private static func hoursAgo(_ hours: Double) -> String {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        return f.string(from: Date().addingTimeInterval(-hours * 3600))
    }

    private static let walletJSON = """
    {
      "balance_uaet": 41250000,
      "pending_uaet": 2500000,
      "entries": [
        { "id": "c1000000-0001-4a00-9c11-000000000001", "kind": "tip_received",
          "amount_uaet": 2500000, "counterparty_handle": "tehanibentley",
          "created_at": "\(hoursAgo(1.5))" },
        { "id": "c1000000-0002-4a00-9c11-000000000002", "kind": "tip_sent",
          "amount_uaet": -1000000, "counterparty_handle": "nocturnesignal",
          "created_at": "\(hoursAgo(20))" },
        { "id": "c1000000-0003-4a00-9c11-000000000003", "kind": "subscription",
          "amount_uaet": -5000000, "counterparty_handle": "miiyazuko",
          "created_at": "\(hoursAgo(72))" },
        { "id": "c1000000-0004-4a00-9c11-000000000004", "kind": "unlock",
          "amount_uaet": -3000000, "counterparty_handle": "nocturnesignal",
          "created_at": "\(hoursAgo(96))" },
        { "id": "c1000000-0005-4a00-9c11-000000000005", "kind": "payout",
          "amount_uaet": -20000000, "created_at": "\(hoursAgo(240))" }
      ]
    }
    """
}
#endif
