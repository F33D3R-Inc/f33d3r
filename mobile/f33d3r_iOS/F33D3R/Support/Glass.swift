import SwiftUI
import F33D3RKit

// MARK: - Where glass goes
//
// Glass is chrome. It belongs on the things that float over content and have to
// stay legible against whatever scrolls beneath them: the compose button, the
// header and lane strip, sheets, and overlays sitting on media.
//
// It does not belong on the work card, on a quoted card, on a poll or a link
// preview. Those are the content — putting a blur between the reader and the
// thing they came to read buys nothing and costs contrast, and a feed of glass
// cards is a feed with no figure and no ground. It does not belong behind
// editable text either, which is why the sign-in fields stay opaque wells even
// though the panel they sit on is glass.
//
// The version question is asked exactly once, here. `glassEffect` is iOS 26 and
// this app deploys to 17, so the real API is used where it exists and the kit's
// two-layer construction is hand-built from a system material everywhere else.
// No call site repeats the check.

/// How far off the surface a glass control sits.
enum GlassElevation {
    /// Flush with the edge it spans — bars, and overlays already lifted by the
    /// thing they sit on.
    case flat
    /// Genuinely floating over content. The compose button.
    case floating
}

/// The Liquid Glass treatment for a control-shaped surface.
///
/// Three paths, in the order they are checked:
///
/// 1. `reduceTransparency` — an opaque surface with a stronger hairline. Not a
///    dimmed version of the glass: someone who has turned transparency off has
///    said that translucency costs them legibility, and answering with a
///    slightly-less-translucent control answers the wrong question.
/// 2. iOS 26 — the system's own Liquid Glass, which carries its own shadow and
///    its own reaction to touch. Nothing hand-built can match it and nothing
///    hand-built should try.
/// 3. Everything else — `.ultraThinMaterial` for the kit's layer 1, and the
///    inset shine and hairline of layer 2 drawn on top of it, because materials
///    have no edge of their own.
private struct GlassSurface<S: InsettableShape>: ViewModifier {
    let shape: S
    /// A control carrying an action rather than a state gets a tint under the
    /// glass, so the accent survives whatever is scrolling behind it.
    var tint: Color?
    var elevation: GlassElevation
    /// Whether the surface should deform under a finger the way an iOS 26
    /// control does. Only meaningful on a button.
    var isInteractive: Bool

    @Environment(\.accessibilityReduceTransparency) private var reduceTransparency
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    @ViewBuilder
    func body(content: Content) -> some View {
        // An interactive glass surface is touchable across its whole shape.
        //
        // This is not a nicety. A SwiftUI view is only hit-tested where it
        // draws, and on iOS 26 the glass is `glassEffect`, which paints behind
        // the label without contributing anything to hit-test against. A
        // control whose label is a 22pt glyph centred in a 52pt circle was
        // therefore live only in the middle: every touch that landed on the
        // glass fell through to whatever was behind it. That was the compose
        // button, and it read as a button that simply did not work.
        //
        // Naming the shape here rather than at each call site is deliberate —
        // the ones that would forget are exactly the small round ones where it
        // matters most.
        // Only the interactive ones. A glass bar or a card has a background of
        // its own and already hit-tests; claiming a shape for it would only
        // change what it swallows.
        if isInteractive {
            surface(content).contentShape(shape)
        } else {
            surface(content)
        }
    }

    @ViewBuilder
    private func surface(_ content: Content) -> some View {
        if reduceTransparency {
            content
                .modifier(OpaqueSurface(shape: shape, tint: tint))
                .modifier(GlassDropShadow(elevation: elevation))
        } else if #available(iOS 26.0, *) {
            // The system draws its own elevation, so ours would double it.
            content.modifier(
                NativeGlass(
                    shape: shape,
                    tint: tint,
                    // The interactive response is motion. A reader who asked for
                    // less of it should not get a control that squashes.
                    isInteractive: isInteractive && !reduceMotion
                )
            )
        } else {
            content
                .modifier(BuiltGlass(shape: shape, tint: tint))
                .modifier(GlassDropShadow(elevation: elevation))
        }
    }
}

/// Path 2 — the real thing.
@available(iOS 26.0, *)
private struct NativeGlass<S: Shape>: ViewModifier {
    let shape: S
    let tint: Color?
    let isInteractive: Bool

    func body(content: Content) -> some View {
        content.glassEffect(glass, in: shape)
    }

    private var glass: Glass {
        var glass = Glass.regular
        if let tint { glass = glass.tint(tint) }
        if isInteractive { glass = glass.interactive() }
        return glass
    }
}

/// Path 3 — the kit's two layers, by hand.
///
/// Layer 1 is `.ultraThinMaterial` rather than a blur plus `F33Glass.fill`:
/// the material already *is* a blur with a saturating, appearance-adaptive
/// translucent fill, and painting the kit's fill over it at full strength gives
/// near-white in light mode. What the material has no equivalent for is layer 2,
/// so that is drawn: the shine as a diagonal border gradient — bright
/// top-leading, faint bottom-trailing, which is what the kit's two opposed inset
/// shadows describe — and the containing hairline over it.
private struct BuiltGlass<S: InsettableShape>: ViewModifier {
    let shape: S
    let tint: Color?

    func body(content: Content) -> some View {
        content
            .background {
                shape
                    .fill(.ultraThinMaterial)
                    .overlay { if let tint { shape.fill(tint.opacity(0.75)) } }
            }
            .overlay {
                shape.strokeBorder(
                    LinearGradient(
                        colors: [F33Glass.shineLeading, F33Glass.shineTrailing],
                        startPoint: .topLeading,
                        endPoint: .bottomTrailing
                    ),
                    lineWidth: F33Glass.shineWidth
                )
            }
            .overlay {
                shape.strokeBorder(F33Glass.border, lineWidth: F33Glass.borderWidth)
            }
            .clipShape(shape)
    }
}

/// Path 1 — what `reduceTransparency` gets.
///
/// A tinted control keeps its tint at full strength, because the tint was the
/// thing carrying the meaning; an untinted one falls back to the elevated
/// surface, so chrome still reads as sitting above the page.
private struct OpaqueSurface<S: InsettableShape>: ViewModifier {
    let shape: S
    let tint: Color?

    func body(content: Content) -> some View {
        content
            .background { shape.fill(tint ?? F33Color.bgElevated) }
            .overlay { shape.strokeBorder(F33Color.hairlineStrong, lineWidth: 1) }
            .clipShape(shape)
    }
}

/// The kit's two-layer drop shadow, for the paths that do not get one for free.
private struct GlassDropShadow: ViewModifier {
    let elevation: GlassElevation

    func body(content: Content) -> some View {
        switch elevation {
        case .flat:
            content
        case .floating:
            content
                // Grouped first: without it each shadow is cast by every leaf
                // inside the control, and the glyph gets its own halo.
                .compositingGroup()
                .shadow(color: F33Glass.shadowNear.color, radius: F33Glass.shadowNear.radius, y: F33Glass.shadowNear.y)
                .shadow(color: F33Glass.shadowFar.color, radius: F33Glass.shadowFar.radius, y: F33Glass.shadowFar.y)
        }
    }
}

// MARK: - Bars

/// A chrome bar: the feed header slab, the lane strip, a tab bar's backing.
///
/// Bars are not controls, and they deliberately do not go through
/// `GlassSurface`. They span an edge rather than floating in the middle of one,
/// they have no corner radius for a shine to catch, and — the deciding reason —
/// two of them stacked have to read as one continuous surface. An inset shine on
/// a full-width rectangle draws a bright line straight across the screen at
/// every seam, which is the opposite of what the treatment is for. So a bar is
/// layer 1 and a single hairline, and nothing else.
private struct GlassBar: ViewModifier {
    /// Whether the bar closes with a hairline along its bottom edge. The last
    /// bar in a stack carries it; the ones above do not, or the stack reads as
    /// separate slabs.
    let hasBottomHairline: Bool
    /// Edges the fill runs out past, so a bar under the status bar paints the
    /// safe area too instead of leaving a differently-coloured strip above it.
    let extending: Edge.Set

    @Environment(\.accessibilityReduceTransparency) private var reduceTransparency

    func body(content: Content) -> some View {
        content
            .background {
                fill.ignoresSafeArea(edges: extending)
            }
            .overlay(alignment: .bottom) {
                if hasBottomHairline { CardDivider() }
            }
    }

    @ViewBuilder
    private var fill: some View {
        if reduceTransparency {
            // The elevated surface rather than the page's own, so chrome is
            // still distinguishable from feed once the blur is gone.
            F33Color.bgElevated
        } else {
            Rectangle().fill(.ultraThinMaterial)
        }
    }
}

// MARK: - Sheets

/// The backing for a sheet presented over content.
///
/// A sheet is chrome by definition — it is a surface that arrived on top of
/// something the reader was already looking at, and the blurred shape of what is
/// underneath is how they keep their place.
private struct GlassSheetBackground: ViewModifier {
    @Environment(\.accessibilityReduceTransparency) private var reduceTransparency

    func body(content: Content) -> some View {
        if reduceTransparency {
            content.presentationBackground(F33Color.bg)
        } else {
            content.presentationBackground(.ultraThinMaterial)
        }
    }
}

// MARK: - Tab bar

/// Glass under the tab bar, for the systems that do not do it themselves.
///
/// iOS 26 already draws a Liquid Glass tab bar and floats it over the content,
/// which is why this does nothing there — calling `toolbarBackground` with a
/// material on 26 would replace the real thing with an approximation of it.
private struct GlassTabBar: ViewModifier {
    func body(content: Content) -> some View {
        if #available(iOS 26.0, *) {
            content
        } else {
            content
                .toolbarBackground(.ultraThinMaterial, for: .tabBar)
                .toolbarBackground(.visible, for: .tabBar)
        }
    }
}

// MARK: - Call sites

extension View {
    /// Glass on a control-shaped surface — a pill, a circle, a rounded panel.
    ///
    /// - Parameters:
    ///   - shape: the surface's outline. Pad the content first; this fills
    ///     behind whatever it is given.
    ///   - tint: a colour under the glass, for a control carrying an action.
    ///   - elevation: whether the control floats over content or sits flush.
    ///   - interactive: whether it should react to a finger. Buttons only.
    func f33Glass<S: InsettableShape>(
        in shape: S,
        tint: Color? = nil,
        elevation: GlassElevation = .flat,
        interactive: Bool = false
    ) -> some View {
        modifier(
            GlassSurface(shape: shape, tint: tint, elevation: elevation, isInteractive: interactive)
        )
    }

    /// Glass under a full-width chrome bar.
    func f33GlassBar(bottomHairline: Bool = false, extending: Edge.Set = []) -> some View {
        modifier(GlassBar(hasBottomHairline: bottomHairline, extending: extending))
    }

    /// Glass behind a presented sheet.
    func f33GlassSheet() -> some View {
        modifier(GlassSheetBackground())
    }

    /// Glass under the tab bar on the systems that need it drawn.
    func f33GlassTabBar() -> some View {
        modifier(GlassTabBar())
    }
}
