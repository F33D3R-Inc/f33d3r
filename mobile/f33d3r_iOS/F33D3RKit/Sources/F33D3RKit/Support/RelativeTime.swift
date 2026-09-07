import Foundation

/// Post timestamps, formatted exactly as the web formats them.
///
/// A direct port of `formatPostTimes()` in `web/static/js/f33d3r.js`, not an
/// approximation and not `RelativeDateTimeFormatter`. The same work seen in the
/// app and on the site should carry the same label, and Foundation's relative
/// formatter says "3 minutes ago" where F33D3R says "3m" and switches to a clock
/// time after an hour rather than counting hours.
///
/// The server also sends a rendered `TimeAgo` string, which the app deliberately
/// ignores: a cached feed page would otherwise show a label that ages while the
/// screen is open. The timestamp is the truth; the label is derived from it on
/// every render.
public enum RelativeTime {

    /// The short label under an author's name.
    ///
    ///     < 1 minute   now
    ///     < 1 hour     42m
    ///     < 1 day      14:03
    ///     < 1 week     Tue 14:03
    ///     otherwise    12 Mar 14:03
    public static func label(for date: Date, now: Date = Date()) -> String {
        let elapsed = now.timeIntervalSince(date)

        // A clock skew between device and server can put a just-posted work a
        // few seconds in the future. Treat that as "now" rather than rendering a
        // negative age.
        if elapsed < 60 { return "now" }
        if elapsed < 3600 { return "\(Int(elapsed / 60))m" }
        if elapsed < 86_400 { return timeOfDay.string(from: date) }
        if elapsed < 7 * 86_400 { return "\(weekday.string(from: date)) \(timeOfDay.string(from: date))" }
        return "\(monthDay.string(from: date)) \(timeOfDay.string(from: date))"
    }

    /// The age inside a quote embed, where the row is narrow and the label is
    /// one token among five.
    ///
    ///     < 1 minute   now
    ///     < 1 hour     42m
    ///     < 1 day      5h
    ///     < 1 week     3d
    ///     this year    12 Mar
    ///     otherwise    12 Mar 2025
    ///
    /// This is the one place the app departs from the web's label on purpose.
    /// The web's "28 Aug 22:35" is right under a full-width card and wrong in
    /// a quote head, where it was being clipped to "Aug 28 22…" beside a
    /// truncated handle; an embed says how old, not when.
    public static func compactLabel(for date: Date, now: Date = Date()) -> String {
        let elapsed = now.timeIntervalSince(date)
        if elapsed < 60 { return "now" }
        if elapsed < 3600 { return "\(Int(elapsed / 60))m" }
        if elapsed < 86_400 { return "\(Int(elapsed / 3600))h" }
        if elapsed < 7 * 86_400 { return "\(Int(elapsed / 86_400))d" }
        let calendar = Calendar.current
        if calendar.component(.year, from: date) == calendar.component(.year, from: now) {
            return monthDay.string(from: date)
        }
        return monthDayYear.string(from: date)
    }

    /// The full timestamp, for the detail screen and accessibility labels, where
    /// there is room to be unambiguous.
    public static func fullLabel(for date: Date) -> String {
        full.string(from: date)
    }

    /// How long a vision has left before the server expires it.
    ///
    /// Visions carry a 24h TTL set server-side at insert. This counts down to
    /// `expiresAt`; an already-expired vision reads "expired" rather than a
    /// negative interval, because expiry is query-time filtering and a direct
    /// link to an expired vision can still resolve.
    public static func countdown(to expiry: Date, now: Date = Date()) -> String {
        let remaining = expiry.timeIntervalSince(now)
        if remaining <= 0 { return "expired" }
        if remaining < 3600 { return "\(max(1, Int(remaining / 60)))m left" }
        if remaining < 86_400 { return "\(Int(remaining / 3600))h left" }
        return "\(Int(remaining / 86_400))d left"
    }

    // Fixed 24-hour clock, matching the web's `hour12: false`. The formatters
    // are cached because a feed builds hundreds of these per scroll and
    // DateFormatter construction is not cheap.
    private static let timeOfDay: DateFormatter = fixed("HH:mm")
    private static let weekday: DateFormatter = fixed("EEE")
    private static let monthDay: DateFormatter = fixed("d MMM")
    private static let monthDayYear: DateFormatter = fixed("d MMM yyyy")

    private static let full: DateFormatter = {
        let f = DateFormatter()
        f.dateStyle = .medium
        f.timeStyle = .short
        return f
    }()

    private static func fixed(_ template: String) -> DateFormatter {
        let f = DateFormatter()
        // setLocalizedDateFormatFromTemplate reorders components for the user's
        // locale, which is what we want for the date parts, but it also drags in
        // 12-hour time where the locale prefers it. The web pins 24-hour, so
        // pin it here too.
        f.locale = Locale.current
        f.setLocalizedDateFormatFromTemplate(template)
        if template == "HH:mm" { f.dateFormat = "HH:mm" }
        return f
    }
}

public extension Work {
    /// The label the card shows beside the handle.
    var timeLabel: String { RelativeTime.label(for: createdAt) }

    /// Non-nil only for visions, and only while the TTL is meaningful.
    var expiryLabel: String? {
        guard let expiresAt else { return nil }
        return RelativeTime.countdown(to: expiresAt)
    }
}

public extension QuotedWork {
    var timeLabel: String { RelativeTime.label(for: createdAt) }
    /// The label a quote head shows: the embed form, not the card's.
    var compactTimeLabel: String { RelativeTime.compactLabel(for: createdAt) }
}
