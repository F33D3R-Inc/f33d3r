import Foundation

/// Engagement counts, formatted for a narrow action bar.
public enum Counts {
    /// The web prints these raw and hides the label entirely at zero. This does
    /// the same up to five figures, then abbreviates — six digits fits a desktop
    /// action row but not six controls across a 375pt screen, which is the one
    /// place the app cannot follow the web exactly.
    public static func label(_ value: Int) -> String? {
        guard value > 0 else { return nil }
        if value < 10_000 { return String(value) }
        if value < 1_000_000 { return trimmed(Double(value) / 1_000) + "K" }
        return trimmed(Double(value) / 1_000_000) + "M"
    }

    /// The unabbreviated value, for accessibility labels and the detail screen's
    /// stat row where the exact number is the point.
    public static func exact(_ value: Int) -> String {
        exactFormatter.string(from: NSNumber(value: value)) ?? String(value)
    }

    /// One decimal place, but only when it says something: 1.2K, not 5.0K.
    private static func trimmed(_ value: Double) -> String {
        let rounded = (value * 10).rounded() / 10
        return rounded == rounded.rounded()
            ? String(Int(rounded))
            : String(format: "%.1f", rounded)
    }

    private static let exactFormatter: NumberFormatter = {
        let f = NumberFormatter()
        f.numberStyle = .decimal
        return f
    }()
}
