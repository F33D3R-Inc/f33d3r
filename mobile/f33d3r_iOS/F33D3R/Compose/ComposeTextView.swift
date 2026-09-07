import SwiftUI
import UIKit
import F33D3RKit

/// The composer's text area.
///
/// A `UITextView` rather than SwiftUI's `TextEditor` because three of the four
/// things this screen has to do need the caret, and `TextEditor` does not offer
/// it on the systems this app still runs on: opening with `@handle ` and the
/// caret *after* it, knowing which token is being typed so the dropdown can
/// offer completions, and putting a completion back without disturbing the rest
/// of the body.
///
/// It also colours `@`, `#` and `$` tokens as they are written, which is the
/// web composer's highlighter and the same rule — ``ComposeToken/highlights(in:)``
/// is `composeHighlightRe`. The colour arriving is the reader being told the
/// server will make that a link.
///
/// It does not scroll: it reports the height its text needs and the sheet's own
/// scroll view carries it, so a long draft scrolls as one page rather than as a
/// small window inside one.
///
/// A thread puts several of these on one screen sharing one caret binding — the
/// sheet tracks a single caret for whichever part is being written. So a view
/// says when it takes the keyboard (`onBeginEditing`), writes the caret only
/// while it holds it, and applies the caret only while the sheet says it is
/// the active one; otherwise every part would jump to the same offset whenever
/// any of them was typed in.
struct ComposeTextView: UIViewRepresentable {
    @Binding var text: String
    /// Where the caret is, as a UTF-16 offset — `nil` while a range is selected
    /// rather than a point, because completing into a selection would replace
    /// text the reader deliberately highlighted.
    @Binding var caret: Int?
    /// Whether the caret binding is this view's to apply. Always true for a
    /// screen with one text view.
    var isActive = true
    /// Called when this view takes the keyboard, before its caret is reported.
    var onBeginEditing: (() -> Void)? = nil
    /// Called when this view gives the keyboard up. When set, the caller
    /// decides whether that clears the caret — it knows which part is active
    /// *now*, where this view only knows what it was told at its last update,
    /// and the keyboard moving from one part to the next must not wipe the
    /// caret the next part has just reported. When unset the caret is cleared.
    var onEndEditing: (() -> Void)? = nil
    /// Whether to take the keyboard as soon as it appears. A part restored from
    /// a draft should not steal it from the one before it.
    var focusesOnAppear = true
    var minHeight: CGFloat = 120
    var accessibilityLabel: String = ""

    func makeUIView(context: Context) -> UITextView {
        let view = UITextView()
        view.delegate = context.coordinator
        view.isScrollEnabled = false
        view.backgroundColor = .clear
        view.textContainerInset = UIEdgeInsets(top: 4, left: 0, bottom: 4, right: 0)
        view.textContainer.lineFragmentPadding = 0
        view.font = Self.font
        view.keyboardDismissMode = .interactive
        view.autocorrectionType = .yes
        // A handle is not a sentence and a ticker is not a word, so neither the
        // shift key nor the dictionary should be volunteering opinions about
        // the first letter after a trigger. Sentence casing stays on because
        // most of what is typed here is prose.
        view.autocapitalizationType = .sentences
        view.textColor = UIColor(F33Color.ink)
        view.adjustsFontForContentSizeCategory = true
        view.text = text
        context.coordinator.highlight(view)
        if focusesOnAppear {
            DispatchQueue.main.async { view.becomeFirstResponder() }
        }
        return view
    }

    func updateUIView(_ view: UITextView, context: Context) {
        context.coordinator.parent = self

        if view.text != text {
            // A prefill or a completion: the body was changed from outside, so
            // the caret goes where the change asked for rather than wherever
            // the old text left it.
            view.text = text
            context.coordinator.highlight(view)
        } else {
            // A trait change — dark mode, a new accent — repaints the same text.
            context.coordinator.highlight(view)
        }

        if isActive, let caret {
            let limit = (view.text as NSString).length
            let wanted = NSRange(location: min(max(0, caret), limit), length: 0)
            if view.selectedRange != wanted { view.selectedRange = wanted }
        }

        view.accessibilityLabel = accessibilityLabel
    }

    func sizeThatFits(_ proposal: ProposedViewSize, uiView: UITextView, context: Context) -> CGSize? {
        let width = proposal.replacingUnspecifiedDimensions(by: CGSize(width: 320, height: 0)).width
        let fitted = uiView.sizeThatFits(CGSize(width: width, height: .greatestFiniteMagnitude))
        return CGSize(width: width, height: max(minHeight, fitted.height))
    }

    func makeCoordinator() -> Coordinator { Coordinator(self) }

    static let font = UIFont.preferredFont(forTextStyle: .body)

    final class Coordinator: NSObject, UITextViewDelegate {
        var parent: ComposeTextView

        init(_ parent: ComposeTextView) {
            self.parent = parent
        }

        func textViewDidBeginEditing(_ view: UITextView) {
            parent.onBeginEditing?()
            parent.caret = point(in: view)
        }

        func textViewDidChange(_ view: UITextView) {
            highlight(view)
            parent.text = view.text
            parent.caret = point(in: view)
        }

        func textViewDidChangeSelection(_ view: UITextView) {
            // Only the view holding the keyboard owns the caret. Setting text
            // on another part from outside moves its selection too, and that
            // must not be reported as the reader's.
            guard view.isFirstResponder else { return }
            parent.caret = point(in: view)
        }

        func textViewDidEndEditing(_ view: UITextView) {
            // No caret means no dropdown. Somebody who has put the keyboard
            // away is not choosing from a list.
            if let onEndEditing = parent.onEndEditing {
                onEndEditing()
            } else {
                parent.caret = nil
            }
        }

        private func point(in view: UITextView) -> Int? {
            view.selectedRange.length == 0 ? view.selectedRange.location : nil
        }

        /// The text and the two colours last painted, so a SwiftUI pass that
        /// changed neither does not repaint the whole body.
        private var painted: (text: String, ink: UIColor, accent: UIColor)?

        /// Paints the tokens the server will linkify.
        ///
        /// Attributes are set on the storage rather than by replacing
        /// `attributedText`, which would drop the caret to the end of the body
        /// on every keystroke.
        func highlight(_ view: UITextView) {
            let text = view.text ?? ""
            let whole = NSRange(location: 0, length: (text as NSString).length)
            // Resolved against the view's own traits rather than left dynamic,
            // so switching to dark mode is a change this can see and repaint.
            let ink = UIColor(F33Color.ink).resolvedColor(with: view.traitCollection)
            let accent = UIColor(F33Color.accent).resolvedColor(with: view.traitCollection)

            if let painted, painted.text == text, painted.ink == ink, painted.accent == accent { return }
            painted = (text, ink, accent)

            let storage = view.textStorage
            storage.beginEditing()
            storage.setAttributes([.font: ComposeTextView.font, .foregroundColor: ink], range: whole)
            for token in ComposeToken.highlights(in: text) {
                storage.addAttribute(.foregroundColor, value: accent, range: token.range)
            }
            storage.endEditing()

            // Otherwise the character typed after a token inherits its colour.
            view.typingAttributes = [.font: ComposeTextView.font, .foregroundColor: ink]
        }
    }
}
