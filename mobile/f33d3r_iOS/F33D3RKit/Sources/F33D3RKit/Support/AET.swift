import Foundation

/// AET amounts, formatted for a card.
///
/// Everything that carries money on the wire counts in micro-AET, matching the
/// `amount_uaet` field on the tip and subscribe events — integers all the way
/// down, because a balance rounded through a `Double` on the way to a screen is
/// a balance that eventually disagrees with the ledger. The conversion to a
/// readable string happens here and nowhere else.
public enum AET {
    /// The ledger counts in millionths of an AET.
    public static let microsPerAET: Int64 = 1_000_000

    /// The bare number: "3", "1.25", "12.4K".
    ///
    /// Two decimal places is the most a card ever shows. An amount too small to
    /// survive that reads as "<0.01" rather than as "0", because a tip that
    /// happened should not render as one that did not.
    public static func amount(uAET: Int64) -> String {
        guard uAET > 0 else { return "0" }

        let whole = uAET / microsPerAET
        if whole >= 1_000_000 { return trimmed(Double(whole) / 1_000_000) + "M" }
        if whole >= 10_000 { return trimmed(Double(whole) / 1_000) + "K" }

        let value = Double(uAET) / Double(microsPerAET)
        if value < 0.01 { return "<0.01" }

        let rounded = (value * 100).rounded() / 100
        if rounded == rounded.rounded() { return String(Int64(rounded)) }
        return String(format: "%g", rounded)
    }

    /// The amount with its unit: "3 AET". What a chip or a button shows.
    public static func label(uAET: Int64) -> String {
        "\(amount(uAET: uAET)) AET"
    }

    /// One decimal place, but only when it says something: 1.2K, not 1.0K.
    private static func trimmed(_ value: Double) -> String {
        let rounded = (value * 10).rounded() / 10
        return rounded == rounded.rounded()
            ? String(Int(rounded))
            : String(format: "%.1f", rounded)
    }
}
