import SwiftUI

// MARK: - The Photos transition
//
// A picture opened full-screen grows out of the tile that was tapped and, when
// it is put away, shrinks back into it. That is iOS 18's zoom transition, and
// the two ends of it are declared here: the tile is a matched source, the
// viewer's root is the destination. Both are keyed by the tile's position in
// the grid, and the destination's key follows the page the viewer is on, so
// swiping to the third image and closing lands on the third tile rather than
// flying back to the first.
//
// Below iOS 18 there is no such transition and both modifiers are inert, which
// leaves the plain cover the viewer has always had. The version question is
// asked here and nowhere else, the way `Glass.swift` asks its own.

/// The tile a zoom transition grows out of.
struct ZoomTransitionSource: ViewModifier {
    let id: Int
    /// Nil when the caller has no viewer to pair with, in which case the tile
    /// is left alone.
    let namespace: Namespace.ID?

    func body(content: Content) -> some View {
        if #available(iOS 18.0, *), let namespace {
            content.matchedTransitionSource(id: id, in: namespace)
        } else {
            content
        }
    }
}

/// The presented root a zoom transition grows into, and shrinks out of.
///
/// Goes on the root of the cover's content. The system also owns the
/// interactive dismissal from here — a pull down drags the picture back
/// toward its tile — so a view wearing this must not run a dismiss drag of
/// its own on the same finger.
struct ZoomTransitionDestination: ViewModifier {
    let sourceID: Int
    let namespace: Namespace.ID?

    func body(content: Content) -> some View {
        if #available(iOS 18.0, *), let namespace {
            content.navigationTransition(.zoom(sourceID: sourceID, in: namespace))
        } else {
            content
        }
    }

    /// Whether the system, rather than the view, is animating and dismissing.
    static func isSystemDriven(_ namespace: Namespace.ID?) -> Bool {
        if #available(iOS 18.0, *) {
            return namespace != nil
        }
        return false
    }
}
