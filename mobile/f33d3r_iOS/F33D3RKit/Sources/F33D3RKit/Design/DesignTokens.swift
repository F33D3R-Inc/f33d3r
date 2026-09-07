#if canImport(SwiftUI)
import SwiftUI

/// F33D3R Design System v4, ported from `web/static/css/styles.css`.
///
/// The web is the source of truth for these values. Ported rather than
/// approximated so the app and the site are recognisably the same product, and
/// so a brand change on the web has one obvious place to land here.
///
/// The web defines accents in oklch, which SwiftUI has no native equivalent for,
/// so those are converted to sRGB once here rather than at every use site. The
/// neutrals are hex on both sides and carry over exactly.
public enum F33Color {

    // MARK: Surfaces

    /// Page background. `--bg`. In dark mode the web shades it toward the
    /// profile's accent theme, so it is read from ``F33Theme/current``.
    public static var bg: Color { F33Theme.current.bg }
    /// Raised surface — cards, sheets. `--bg-elev`. Themed in dark mode like `bg`.
    public static var bgElevated: Color { F33Theme.current.bgElevated }
    /// Recessed surface — wells, inputs. `--bg-sunk`
    public static let bgSunken = adaptive(light: 0xF3F2EE, dark: 0x050507)

    // MARK: Ink

    /// Primary text. `--ink`
    public static let ink = adaptive(light: 0x14120C, dark: 0xF3F2EE)
    /// `--ink-2`
    public static let ink2 = adaptive(light: 0x3A3730, dark: 0xC8C5BE)
    /// Secondary text. `--ink-3`
    public static let ink3 = adaptive(light: 0x6B6760, dark: 0x8A8780)
    /// Muted text and disabled glyphs. `--ink-4`
    public static let ink4 = adaptive(light: 0x9B9790, dark: 0x5D5B56)
    /// `--ink-5`
    public static let ink5 = adaptive(light: 0xC4C0B8, dark: 0x3A3833)

    // MARK: Lines

    /// Hairline separator. `--hairline`
    public static let hairline = Color(
        light: Color(white: 0.08, opacity: 0.09),
        dark: Color(white: 1.0, opacity: 0.07)
    )
    /// `--hairline-strong`
    public static let hairlineStrong = Color(
        light: Color(white: 0.08, opacity: 0.14),
        dark: Color(white: 1.0, opacity: 0.12)
    )

    // MARK: Accent

    /// The profile's accent theme — violet by default. The six palettes and
    /// the theme in force live in ``F33Theme``; these read whichever it is.
    public static var accent: Color { F33Theme.current.accent }
    /// `--accent-2`
    public static var accent2: Color { F33Theme.current.accent2 }
    /// Tinted accent background. `--accent-soft`
    public static var accentSoft: Color { F33Theme.current.accentSoft }
    /// Foreground on an accent fill. `--accent-ink`
    public static let accentInk = Color.white

    // MARK: Semantic

    /// `--ok`
    public static let ok = adaptive(light: 0x2E9E63, dark: 0x3FBF7C)
    /// `--warn`
    public static let warn = adaptive(light: 0xC98A16, dark: 0xE0A22C)
    /// `--danger`
    public static let danger = adaptive(light: 0xE0453A, dark: 0xF05A4E)
    /// `--gold`
    public static let gold = adaptive(light: 0xE0AE3C, dark: 0xEDC260)
    /// AET currency accent. `--aet`
    public static let aet = adaptive(light: 0xD9962B, dark: 0xE8AC49)

    /// The visions lane — the 24-hour row under the header, and anywhere else a
    /// vision is named.
    ///
    /// A fixed violet rather than ``accent``, which follows the reader's chosen
    /// theme. The row reads by colour: violet is a vision, ``danger`` red is a
    /// creator who is live, and that pairing stops meaning anything the moment
    /// somebody picks the moss or dusk theme and every vision turns green or
    /// gold. The value is the default `void` accent, so for most readers it is
    /// the purple they already know.
    public static let vision = adaptive(light: 0x7C5CFF, dark: 0x9B85FF)

    // MARK: Brand

    /// The gold from the F33D3R mark, sampled from the supplied artwork
    /// (`#C8B086`).
    ///
    /// Kept apart from `gold` and `aet`, which are UI semantics — a realm
    /// milestone and a currency — and are both far more saturated than the mark
    /// is. This one is the identity, and it is the only gold that may appear
    /// under the logo.
    ///
    /// The light value is darkened from the artwork's own. `#C8B086` on the
    /// `#FAFAF7` page reads as a smudge, and a logo nobody can see is worse than
    /// one a shade off brand; dark mode, which is where the artwork was drawn to
    /// live, gets the sampled colour untouched.
    public static let brandGold = adaptive(light: 0x9A7B45, dark: 0xC8B086)

    /// The near-black navy the mark was drawn on (`#00081B`).
    ///
    /// Not the app background: `bg` is a neutral `#0A0A0C` ported from the web,
    /// and swapping the whole product to navy is a brand decision nobody has
    /// made. This exists for surfaces that carry the logo itself.
    public static let brandNavy = Color(hex: 0x00081B)

    private static func adaptive(light: UInt32, dark: UInt32) -> Color {
        Color(light: Color(hex: light), dark: Color(hex: dark))
    }
}

/// Corner radii. `--r-xs` … `--r-xl`
public enum F33Radius {
    public static let xs: CGFloat = 6
    public static let sm: CGFloat = 10
    public static let md: CGFloat = 14
    public static let lg: CGFloat = 20
    public static let xl: CGFloat = 28
    /// `--radius-full`
    public static let full: CGFloat = 9999
}

/// Spacing scale. Not defined as CSS variables on the web, but these are the
/// values the stylesheet uses in practice; centralised so the app stays regular.
public enum F33Spacing {
    public static let xs: CGFloat = 4
    public static let sm: CGFloat = 8
    public static let md: CGFloat = 12
    public static let lg: CGFloat = 16
    public static let xl: CGFloat = 24
    public static let xxl: CGFloat = 32
}

/// Layout constants shared with the web shell.
public enum F33Layout {
    /// `--feed-w`. The feed column never exceeds this, so an iPad does not
    /// stretch post text to an unreadable line length.
    public static let feedMaxWidth: CGFloat = 620
    /// Apple's minimum comfortable touch target; the web enforces the same 44px.
    public static let minTouchTarget: CGFloat = 44
}

/// Motion. `--spring` is a CSS overshoot curve; the closest honest SwiftUI
/// equivalent is a spring with a little bounce rather than a bezier copy.
public enum F33Motion {
    /// `--spring: cubic-bezier(0.34, 1.56, 0.64, 1)` — overshoots, for things
    /// that pop in.
    public static let spring = Animation.spring(response: 0.38, dampingFraction: 0.68)
    /// `--ease-out: cubic-bezier(0.22, 1, 0.36, 1)` — decelerating, for things
    /// that settle.
    public static let easeOut = Animation.timingCurve(0.22, 1, 0.36, 1, duration: 0.28)
}

// MARK: - Helpers

public extension Color {
    /// Builds a colour from a 24-bit RGB literal, so ported hex values read the
    /// same here as in the stylesheet.
    init(hex: UInt32) {
        self.init(
            .sRGB,
            red: Double((hex >> 16) & 0xFF) / 255,
            green: Double((hex >> 8) & 0xFF) / 255,
            blue: Double(hex & 0xFF) / 255,
            opacity: 1
        )
    }

    /// Resolves to `light` or `dark` following the system appearance, matching
    /// the web's `[data-theme="dark"]` switch.
    init(light: Color, dark: Color) {
        #if canImport(UIKit)
        self.init(uiColor: UIColor { traits in
            traits.userInterfaceStyle == .dark ? UIColor(dark) : UIColor(light)
        })
        #elseif canImport(AppKit)
        self.init(nsColor: NSColor(name: nil) { appearance in
            let isDark = appearance.bestMatch(from: [.aqua, .darkAqua]) == .darkAqua
            return isDark ? NSColor(dark) : NSColor(light)
        })
        #else
        self = light
        #endif
    }
}
#endif
