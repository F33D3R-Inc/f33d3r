#if canImport(SwiftUI)
import SwiftUI

/// The Liquid Glass recipe, transcribed from the iOS 26 UI Kit device frame the
/// design direction comes from.
///
/// These numbers are not ported from `styles.css` — the web has no equivalent
/// surface, and inventing one there to copy here would be the tail wagging the
/// dog. They come from the kit's own two-layer construction: a blurred,
/// saturated backdrop, then an inset shine and a hairline that together make a
/// floating control read as a raised piece of glass rather than a flat sticker.
///
/// They are tokens rather than literals at the use site for the same reason
/// every other value in this file is: when Apple's material moves under us, or
/// the brand decides glass should be warmer, there is one place to change it.
///
/// `Glass.swift` in the app target is the only thing that should read these.
public enum F33Glass {

    // MARK: Layer 1 — backdrop

    /// The translucent fill over the blur. `rgba(255,255,255,0.5)` light,
    /// `rgba(120,120,128,0.28)` dark.
    ///
    /// Only the iOS 17 fallback paints this by hand, and then at reduced
    /// strength: `.ultraThinMaterial` is UIKit's own implementation of this
    /// whole layer — blur, saturation and a light fill that already adapts —
    /// so stacking the kit's fill on top of it at full opacity produces near
    /// white, not glass.
    public static let fill = Color(
        light: Color(.sRGB, white: 1, opacity: 0.5),
        dark: Color(.sRGB, red: 120 / 255, green: 120 / 255, blue: 128 / 255, opacity: 0.28)
    )

    // MARK: Layer 2 — shine and edge

    /// The bright corner of the inset shine — the kit's
    /// `inset 1.5px 1.5px 1px rgba(255,255,255,0.7)`, top-leading.
    public static let shineLeading = Color(
        light: Color(white: 1, opacity: 0.70),
        dark: Color(white: 1, opacity: 0.15)
    )

    /// The faint corner — `inset -1px -1px 1px rgba(255,255,255,0.4)`,
    /// bottom-trailing. Two opposed inset shadows is how CSS says "lit from the
    /// top left"; a diagonal gradient along the border is how SwiftUI says it.
    public static let shineTrailing = Color(
        light: Color(white: 1, opacity: 0.40),
        dark: Color(white: 1, opacity: 0.08)
    )

    /// The containing hairline: `0.5px rgba(0,0,0,0.06)` light,
    /// `0.5px rgba(255,255,255,0.15)` dark. It is what keeps a pale control from
    /// dissolving into a pale background.
    public static let border = Color(
        light: Color(white: 0, opacity: 0.06),
        dark: Color(white: 1, opacity: 0.15)
    )

    /// A true half-point line, not a hairline approximation — the kit specifies
    /// `0.5px` and a Retina screen can draw it.
    public static let borderWidth: CGFloat = 0.5

    /// The shine is drawn as a stroke rather than as two shadows, so it needs a
    /// width the kit does not state. 1.25pt is the width at which the 1.5px
    /// bright edge and the 1px faint edge average out at 3x.
    public static let shineWidth: CGFloat = 1.25

    // MARK: Elevation

    /// The near shadow. The kit gives light `0 1px 3px rgba(0,0,0,0.07)` and
    /// dark `0 2px 6px rgba(0,0,0,0.35)`.
    ///
    /// Only the colour varies by appearance here, not the geometry. SwiftUI has
    /// no adaptive shadow radius — a radius that changed with the theme would
    /// have to be resolved from the environment at every call site, which is
    /// exactly the repetition these tokens exist to prevent — so the difference
    /// between the two is carried where it is actually visible, in how dark the
    /// shadow is.
    public static let shadowNear = GlassShadow(
        color: Color(light: Color(white: 0, opacity: 0.07), dark: Color(white: 0, opacity: 0.35)),
        radius: 2,
        y: 1.5
    )

    /// The far shadow — light `0 3px 10px rgba(0,0,0,0.06)`,
    /// dark `0 6px 16px rgba(0,0,0,0.2)`. CSS blur radii are roughly twice
    /// SwiftUI's, which is why 10px lands here as 5-ish.
    public static let shadowFar = GlassShadow(
        color: Color(light: Color(white: 0, opacity: 0.06), dark: Color(white: 0, opacity: 0.20)),
        radius: 6.5,
        y: 4.5
    )

    // MARK: Geometry

    /// `height 44, minWidth 44, borderRadius 9999` — the kit's control is the
    /// same 44pt pill Apple and the web both settled on, which is why
    /// `F33Layout.minTouchTarget` is the value here and not a second copy of it.
    public static let controlSize: CGFloat = F33Layout.minTouchTarget
}

/// One layer of a glass surface's drop shadow.
public struct GlassShadow: Hashable, Sendable {
    public let color: Color
    public let radius: CGFloat
    public let y: CGFloat

    public init(color: Color, radius: CGFloat, y: CGFloat) {
        self.color = color
        self.radius = radius
        self.y = y
    }
}
#endif
