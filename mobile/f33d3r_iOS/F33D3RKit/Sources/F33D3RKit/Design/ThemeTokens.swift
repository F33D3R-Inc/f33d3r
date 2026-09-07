#if canImport(SwiftUI)
import Foundation
import SwiftUI

/// One of the six accent themes a profile can choose on the web.
///
/// The site stores the choice as `user.theme_id` and paints it with
/// `body[data-theme="…"]`: an accent, a second accent, a soft tint, and in
/// dark mode a page and card colour shaded toward the accent. These are those
/// values, converted once from oklch to sRGB. The neutrals — ink, hairlines,
/// the sunken well — do not move with the theme on the web and do not here.
public struct F33Theme: Hashable, Sendable, Identifiable {
    public let id: String
    public let name: String
    let accentLight: UInt32
    let accentDark: UInt32
    let accent2Light: UInt32
    let accent2Dark: UInt32
    let softLight: UInt32
    let softDark: UInt32
    let bgDark: UInt32
    let bgElevatedDark: UInt32

    /// `void` — the default violet. Light `oklch(58% 0.18 295)`, dark
    /// `oklch(70% 0.18 295)`.
    public static let void = F33Theme(
        id: "void", name: "Void",
        accentLight: 0x7C5CFF, accentDark: 0x9B85FF,
        accent2Light: 0x8F76FF, accent2Dark: 0xAF9DFF,
        softLight: 0xF2EFFF, softDark: 0x2A2140,
        bgDark: 0x05050A, bgElevatedDark: 0x0D0D18
    )
    public static let aurora = F33Theme(
        id: "aurora", name: "Aurora",
        accentLight: 0x00A49D, accentDark: 0x00CBC2,
        accent2Light: 0x00B4AD, accent2Dark: 0x00DBD3,
        softLight: 0xDCF9F6, softDark: 0x00322F,
        bgDark: 0x030F14, bgElevatedDark: 0x071520
    )
    public static let sakura = F33Theme(
        id: "sakura", name: "Sakura",
        accentLight: 0xE9609E, accentDark: 0xFF87C4,
        accent2Light: 0xF77CB0, accent2Dark: 0xFFA2D6,
        softLight: 0xFFEDF6, softDark: 0x3F1B2A,
        bgDark: 0x0E060E, bgElevatedDark: 0x160C14
    )
    public static let obsidian = F33Theme(
        id: "obsidian", name: "Obsidian",
        accentLight: 0x5981E0, accentDark: 0x7CA7FF,
        accent2Light: 0x7195E8, accent2Dark: 0x95BCFF,
        softLight: 0xEBF2FF, softDark: 0x1A2846,
        bgDark: 0x07071A, bgElevatedDark: 0x0E0E24
    )
    public static let moss = F33Theme(
        id: "moss", name: "Moss",
        accentLight: 0x399E43, accentDark: 0x61C568,
        accent2Light: 0x5BAE5F, accent2Dark: 0x81D584,
        softLight: 0xE2F9E2, softDark: 0x133015,
        bgDark: 0x040C05, bgElevatedDark: 0x091409
    )
    public static let dusk = F33Theme(
        id: "dusk", name: "Dusk",
        accentLight: 0xD0901E, accentDark: 0xF9B64F,
        accent2Light: 0xDCA744, accent2Dark: 0xFFCE6D,
        softLight: 0xFFF0CC, softDark: 0x362600,
        bgDark: 0x0E0A04, bgElevatedDark: 0x18130A
    )

    /// In the order the web's picker shows them.
    public static let all: [F33Theme] = [void, aurora, sakura, obsidian, moss, dusk]

    /// The theme for a profile's `theme_id`; the default for anything unknown,
    /// so a value from a newer server never leaves the app unpainted.
    public static func named(_ id: String?) -> F33Theme {
        all.first { $0.id == id } ?? .void
    }

    public var accent: Color { Color(light: Color(hex: accentLight), dark: Color(hex: accentDark)) }
    public var accent2: Color { Color(light: Color(hex: accent2Light), dark: Color(hex: accent2Dark)) }
    public var accentSoft: Color { Color(light: Color(hex: softLight), dark: Color(hex: softDark)) }
    /// The theme's accent as a swatch, one colour regardless of appearance.
    public var swatch: Color { Color(hex: accentLight) }
    var bg: Color { Color(light: Color(hex: 0xFAFAF7), dark: Color(hex: bgDark)) }
    var bgElevated: Color { Color(light: Color(hex: 0xFFFFFF), dark: Color(hex: bgElevatedDark)) }

    // MARK: The theme in force

    /// The theme the app is painting with. Set from the signed-in profile's
    /// `theme_id` when `/me` answers, and to the default when nobody is signed
    /// in. Read by every `F33Color` token, so a change here recolours the app
    /// on its next render.
    ///
    /// Held behind a lock rather than on the main actor because colour tokens
    /// are read from static initialisers and nonisolated code as well as from
    /// views; the write is rare and the read is a pointer copy.
    public static var current: F33Theme {
        get { store.withLock { $0 } }
        set { store.withLock { $0 = newValue } }
    }

    private static let store = LockedTheme(.void)
}

/// A theme behind a lock. `NSLock` rather than an actor so the getter stays
/// synchronous, which is what a colour token has to be.
private final class LockedTheme: @unchecked Sendable {
    private var value: F33Theme
    private let lock = NSLock()

    init(_ value: F33Theme) { self.value = value }

    func withLock<T>(_ body: (inout F33Theme) -> T) -> T {
        lock.lock()
        defer { lock.unlock() }
        return body(&value)
    }
}

/// Light, dark, or whatever the device is doing.
///
/// The web's dark/light switch lives in the browser and never reaches the
/// server; this is the same choice kept on the device.
public enum Appearance: String, CaseIterable, Sendable, Identifiable {
    case system
    case light
    case dark

    public var id: String { rawValue }

    public var title: String {
        switch self {
        case .system: return "System"
        case .light: return "Light"
        case .dark: return "Dark"
        }
    }

    public var colorScheme: ColorScheme? {
        switch self {
        case .system: return nil
        case .light: return .light
        case .dark: return .dark
        }
    }
}
#endif
