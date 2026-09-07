import SwiftUI
import F33D3RKit

/// The ⋯ menu at the end of the action row.
///
/// For someone else's work: mute the creator, explain the placement, report.
/// For the reader's own: pin, choose who can reply, delete. One level deep;
/// the reply restriction is a picker inside the menu rather than a second
/// screen, because it is four words and a tick.
struct WorkOverflowMenu: View {
    let work: Work
    /// Nil when the server sent no provenance. The item is hidden rather than
    /// shown disabled: an explanation nobody can give is not a feature that is
    /// temporarily off.
    let provenance: WorkProvenance?
    let actions: WorkActions

    /// Blocking is done here rather than handed in as another closure on
    /// ``WorkActions``: it needs no state from the card, only the handle the
    /// menu is already showing, and the confirmation belongs next to the item
    /// that raises it. Every other action stays where it is.
    @Environment(AppModel.self) private var model

    @State private var isConfirmingBlock = false
    @State private var blockError: String?

    private var isOwner: Bool { actions.delete != nil }

    var body: some View {
        Menu {
            if provenance != nil {
                Button(action: actions.whyThis) {
                    Label("Why this", systemImage: "questionmark.circle")
                }
            }

            if isOwner {
                if let edit = actions.edit {
                    Button(action: edit) {
                        Label("Edit", systemImage: "pencil")
                    }
                }
                if let pin = actions.pin {
                    Button(action: pin) {
                        Label(work.isPinned ? "Unpin from profile" : "Pin to profile", systemImage: work.isPinned ? "pin.slash" : "pin")
                    }
                }
                if let restrict = actions.restrictReplies {
                    Menu {
                        ForEach(gatingOptions, id: \.0) { value, title, icon in
                            Button {
                                restrict(value)
                            } label: {
                                if isCurrentGating(value) {
                                    Label(title, systemImage: "checkmark")
                                } else {
                                    Label(title, systemImage: icon)
                                }
                            }
                        }
                    } label: {
                        Label("Who can reply", systemImage: "bubble.left.and.bubble.right")
                    }
                }
                if let delete = actions.delete {
                    Button(role: .destructive, action: delete) {
                        Label("Delete", systemImage: "trash")
                    }
                }
            } else {
                Button(action: actions.mute) {
                    Label("Mute @\(work.author.handle)", systemImage: "speaker.slash")
                }
                Button(role: .destructive) {
                    isConfirmingBlock = true
                } label: {
                    Label("Block @\(work.author.handle)", systemImage: "hand.raised")
                }
                Button(role: .destructive, action: actions.report) {
                    Label("Report", systemImage: "flag")
                }
            }
        } label: {
            Image(systemName: "ellipsis")
                .font(.system(size: 15, weight: .semibold))
                .foregroundStyle(F33Color.ink3)
                // Drawn narrower than the 44pt minimum and given the missing
                // width back as hit area.
                .frame(width: 34, height: F33Layout.minTouchTarget)
                .contentShape(Rectangle().inset(by: -5))
        }
        .menuStyle(.button)
        .buttonStyle(.plain)
        .accessibilityLabel("More actions")
        .confirmationDialog(
            "Block @\(work.author.handle)?",
            isPresented: $isConfirmingBlock,
            titleVisibility: .visible
        ) {
            Button("Block", role: .destructive) {
                Task { await block() }
            }
        } message: {
            Text("You won't see each other's works, and they can't follow you or reach you. Undo it under Settings → Blocked & muted.")
        }
        .alert("That didn't go through", isPresented: Binding(get: { blockError != nil }, set: { if !$0 { blockError = nil } })) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(blockError ?? "")
        }
    }

    private func block() async {
        do {
            try await model.setBlocked(true, handle: work.author.handle)
        } catch let error as MalkuthError {
            blockError = error.description
        } catch let error as APIError {
            blockError = error.userMessage
        } catch {
            blockError = error.localizedDescription
        }
    }

    private var gatingOptions: [(CommentGating, String, String)] {
        [
            (.everyone, "Everyone", "globe"),
            (.followers, "People you follow", "person.2"),
            (.circle, "Your circle", "person.3"),
            (.none, "Nobody", "bubble.left.and.exclamationmark.bubble.right"),
        ]
    }

    /// The column stores `open` for everyone; the picker speaks the payload's
    /// vocabulary. Both spellings are the server's.
    private func isCurrentGating(_ value: CommentGating) -> Bool {
        switch value {
        case .everyone: return work.commentGating == "open" || work.commentGating == "everyone"
        default: return work.commentGating == value.rawValue
        }
    }
}
