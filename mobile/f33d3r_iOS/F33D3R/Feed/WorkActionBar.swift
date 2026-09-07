import SwiftUI
import F33D3RKit

/// What a card can do. Built by `WorkCard`, which owns the sheets and the
/// model calls; the bar only draws and dispatches.
struct WorkActions {
    var reply: () -> Void = {}
    var like: () -> Void = {}
    var repost: () -> Void = {}
    var quote: () -> Void = {}
    var tip: () -> Void = {}
    var save: () -> Void = {}
    var whyThis: () -> Void = {}
    var mute: () -> Void = {}
    var report: () -> Void = {}
    /// Owner-only. Nil hides the item rather than disabling it.
    var delete: (() -> Void)?
    var pin: (() -> Void)?
    var restrictReplies: ((CommentGating) -> Void)?
    /// Owner-only, and only inside the server's edit window.
    var edit: (() -> Void)?
    /// True while a mutation on this work is in flight; the bar dims the
    /// controls rather than allowing a second tap to race the first.
    var isBusy = false
}

/// The row under a work: reply, like, repost, tip, save, and the overflow menu.
///
/// Every control reads the server's row and sends one event. State changes
/// when the server answers — a like is drawn pink because the server has
/// recorded it, not because the finger came down. Counts ride only where they
/// invite: reply and like. Repost and save show their state in tint.
///
/// Six controls have to share a phone's width. The row tries the labelled form
/// first and falls back to icons; a count is on one line or not drawn at all.
struct WorkActionBar: View {
    let work: Work
    let currentUser: CurrentUser?
    let actions: WorkActions

    /// Where this instance lives, for building a work's public link. Set once
    /// for the whole app in `F33D3RApp`.
    @Environment(\.mediaOrigin) private var origin

    @ScaledMetric(relativeTo: .footnote) private var typeScale: CGFloat = 1

    private var scale: CGFloat { min(typeScale, 1.2) }

    private var canReply: Bool { work.viewerMayReply(currentUser: currentUser) }

    var body: some View {
        ViewThatFits(in: .horizontal) {
            row(showingLabels: true)
            row(showingLabels: false)
        }
        .padding(.top, F33Spacing.xs)
        .opacity(actions.isBusy ? 0.6 : 1)
        .animation(F33Motion.easeOut, value: actions.isBusy)
        // A tap is felt when the server has answered, not when the finger
        // lands: the trigger is the row the server sent back.
        .sensoryFeedback(.impact(weight: .light), trigger: work.likedByViewer)
        .sensoryFeedback(.impact(weight: .light), trigger: work.repostedByViewer)
        .sensoryFeedback(.selection, trigger: work.bookmarkedByViewer)
    }

    private func row(showingLabels: Bool) -> some View {
        HStack(spacing: 4) {
            control(
                icon: "bubble.left",
                label: "reply",
                value: Counts.label(work.replyCount),
                isOn: false,
                tint: F33Color.accent,
                enabled: canReply,
                showingLabel: showingLabels,
                accessibility: canReply
                    ? "Reply, \(Counts.exact(work.replyCount)) replies"
                    : "Reply — replies are restricted",
                action: actions.reply
            )

            control(
                icon: work.likedByViewer ? "heart.fill" : "heart",
                label: "like",
                value: Counts.label(work.likeCount),
                isOn: work.likedByViewer,
                tint: F33Action.like,
                showingLabel: showingLabels,
                accessibility: "Like, \(Counts.exact(work.likeCount)) likes",
                action: actions.like
            )

            Menu {
                Button(action: actions.repost) {
                    Label(work.repostedByViewer ? "Undo repost" : "Repost", systemImage: "arrow.2.squarepath")
                }
                Button(action: actions.quote) {
                    Label("Quote", systemImage: "quote.opening")
                }
            } label: {
                controlLabel(
                    icon: "arrow.2.squarepath",
                    label: "repost",
                    value: Counts.label(work.repostCount),
                    isOn: work.repostedByViewer,
                    tint: F33Action.repost,
                    enabled: true,
                    showingLabel: showingLabels
                )
            }
            .menuStyle(.button)
            .buttonStyle(.plain)
            .accessibilityLabel("Repost or quote, \(Counts.exact(work.repostCount)) reposts")
            .accessibilityValue(work.repostedByViewer ? "Reposted" : "Not reposted")

            tipControl(showingLabel: showingLabels)

            control(
                icon: work.bookmarkedByViewer ? "bookmark.fill" : "bookmark",
                label: "save",
                value: nil,
                isOn: work.bookmarkedByViewer,
                tint: F33Action.bookmark,
                showingLabel: showingLabels,
                accessibility: "Save",
                action: actions.save
            )

            shareControl(showingLabel: showingLabels)

            Spacer(minLength: 0)

            WorkOverflowMenu(
                work: work,
                provenance: work.visibleProvenance,
                actions: actions
            )
        }
    }

    /// Share, through the system sheet rather than anything of ours.
    ///
    /// `ShareLink` is the whole implementation on purpose. Everything a reader
    /// expects from a share button on iOS — Messages, AirDrop, copy, the
    /// shortcuts they have pinned, the people they message most — is the
    /// system's, and a hand-built sheet would be a worse copy of it that also
    /// has to be maintained. What we choose is only what gets shared: the
    /// work's public link, which is what a reader means by "share this".
    ///
    /// Nil origin means no link can be built, and the control is left out
    /// rather than shown broken. That only happens before the app knows which
    /// instance it is talking to.
    @ViewBuilder
    private func shareControl(showingLabel: Bool) -> some View {
        if let url = shareURL {
            ShareLink(item: url, subject: Text(work.author.handle), message: Text(work.body)) {
                controlLabel(
                    icon: "square.and.arrow.up",
                    label: "share",
                    value: nil,
                    isOn: false,
                    tint: F33Color.accent,
                    enabled: true,
                    showingLabel: showingLabel
                )
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Share")
        }
    }

    /// A work's public address on this instance. `/work/{id}` is the same path
    /// the website serves, so a link shared from the app opens the same page a
    /// link shared from the web does.
    private var shareURL: URL? {
        origin?.appendingPathComponent("work").appendingPathComponent(work.id)
    }

    /// Tip is the highlighted control: currency tint, a soft fill behind it, and
    /// the amount the work has taken so far when there is one.
    private func tipControl(showingLabel: Bool) -> some View {
        Button(action: actions.tip) {
            HStack(spacing: 4) {
                Image(systemName: "bolt.fill")
                    .font(.system(size: 13 * scale, weight: .semibold))

                if let amount = work.tipLabel {
                    Text(amount)
                        .lineLimit(1)
                        .fixedSize()
                        .font(.system(size: 12 * scale, weight: .semibold).monospacedDigit())
                } else if showingLabel {
                    Text("tip")
                        .lineLimit(1)
                        .fixedSize()
                        .font(.system(size: 12 * scale, weight: .semibold))
                }
            }
            .foregroundStyle(F33Action.tip)
            .padding(.horizontal, 7)
            .padding(.vertical, 5)
            .background(F33Action.tip.opacity(0.14), in: Capsule())
            .frame(minHeight: F33Layout.minTouchTarget)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .disabled(actions.isBusy)
        .accessibilityLabel(work.tipLabel.map { "Tip, \($0) tipped so far" } ?? "Tip")
    }

    private func control(
        icon: String,
        label: String,
        value: String?,
        isOn: Bool,
        tint: Color,
        enabled: Bool = true,
        showingLabel: Bool,
        accessibility: String,
        action: @escaping () -> Void
    ) -> some View {
        Button(action: action) {
            controlLabel(icon: icon, label: label, value: value, isOn: isOn, tint: tint, enabled: enabled, showingLabel: showingLabel)
        }
        .buttonStyle(.plain)
        .disabled(!enabled || actions.isBusy)
        .accessibilityLabel(accessibility)
        .accessibilityValue(isOn ? "On" : "Off")
    }

    private func controlLabel(
        icon: String,
        label: String,
        value: String?,
        isOn: Bool,
        tint: Color,
        enabled: Bool,
        showingLabel: Bool
    ) -> some View {
        HStack(spacing: 4) {
            Image(systemName: icon)
                .font(.system(size: 14 * scale))
                .contentTransition(.symbolEffect(.replace))
                // The glyph jumps once as the state the server confirmed lands.
                .symbolEffect(.bounce, options: .nonRepeating, value: isOn)

            HStack(spacing: 4) {
                if showingLabel {
                    Text(label)
                }
                if let value {
                    Text(value).monospacedDigit()
                }
            }
            .lineLimit(1)
            .fixedSize()
            .font(.system(size: 12 * scale, weight: .medium))
        }
        .foregroundStyle(isOn ? tint : (enabled ? F33Color.ink3 : F33Color.ink5))
        .padding(.horizontal, 2)
        .frame(minHeight: F33Layout.minTouchTarget)
        .contentShape(Rectangle())
        .opacity(enabled ? 1 : 0.55)
    }
}
