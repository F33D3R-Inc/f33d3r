import SwiftUI

extension View {
    /// Hides the enclosing tab bar while `hidden` is true.
    ///
    /// Applied by a tab's own content rather than by the shell: whether the bar
    /// belongs over a screen is that screen's business, and Home is the only one
    /// that takes the whole display while the reader is scrolling.
    ///
    /// Two spellings of one idea. `toolbarVisibility(_:for:)` is the iOS 18 name
    /// and `toolbar(_:for:)` the older one; the older is deprecated on 18 and the
    /// newer does not exist before it, so both are here and the deployment target
    /// decides which runs.
    @ViewBuilder
    func tabBarHidden(_ hidden: Bool) -> some View {
        if #available(iOS 18.0, *) {
            toolbarVisibility(hidden ? .hidden : .automatic, for: .tabBar)
        } else {
            toolbar(hidden ? .hidden : .automatic, for: .tabBar)
        }
    }
}
