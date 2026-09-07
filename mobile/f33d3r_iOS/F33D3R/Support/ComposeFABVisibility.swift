import SwiftUI

/// Whether the compose button belongs over this screen.
///
/// The button is the shell's, not any one screen's. Wanting to say something
/// does not arrive only while reading Home — it arrives while looking at a
/// profile, a thread of replies, a tag, a wallet — and a button that appears on
/// one screen and not the next is a button nobody builds a habit around.
///
/// So the shell draws it everywhere and screens opt *out*. A preference rather
/// than a list of routes held by the shell, because the screen knows why it
/// should not have one and the shell would only be guessing: a settings form is
/// not a place you post from, a conversation already has a composer of its own,
/// and a full-screen viewer is somebody else's work being looked at.
private struct ComposeFABHiddenKey: PreferenceKey {
    static let defaultValue = false
    /// Any screen in the stack asking for it to go wins, which matters while a
    /// push is animating and two screens briefly both have an opinion.
    static func reduce(value: inout Bool, nextValue: () -> Bool) {
        value = value || nextValue()
    }
}

extension View {
    /// Takes the compose button off this screen.
    func hidesComposeFAB(_ hidden: Bool = true) -> some View {
        preference(key: ComposeFABHiddenKey.self, value: hidden)
    }

    /// Reads what the screens in this stack have asked for.
    func onComposeFABVisibility(_ action: @escaping (Bool) -> Void) -> some View {
        onPreferenceChange(ComposeFABHiddenKey.self) { action($0) }
    }
}
