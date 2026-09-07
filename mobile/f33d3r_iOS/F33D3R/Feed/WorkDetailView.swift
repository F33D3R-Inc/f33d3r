import SwiftUI
import F33D3RKit

/// One work with the conversation around it.
///
/// Ancestors above give a reply its context; replies below are the
/// conversation, oldest first. The main card is the same `WorkCard` the feed
/// draws, so every action works here too, and it reads through the model's
/// latest row so a like taken on this screen shows on this screen.
struct WorkDetailView: View {
    let workID: String

    @Environment(AppModel.self) private var model

    @State private var thread: WorkThread?
    @State private var error: APIError?
    @State private var isLoading = true
    @State private var isReplying = false

    var body: some View {
        ScrollView {
            LazyVStack(spacing: 0) {
                if model.isDeleted(workID) {
                    EmptyStateView(
                        icon: "trash",
                        title: "This work was deleted",
                        message: "It's gone from every feed. Replies to it stay."
                    )
                } else if isLoading {
                    WorkCardSkeleton()
                } else if let error {
                    ErrorStateView(error: error) {
                        Task { await load() }
                    }
                } else if let thread {
                    content(thread)
                }
            }
        }
        .background(F33Color.bg)
        .navigationTitle("Work")
        .navigationBarTitleDisplayMode(.inline)
        .refreshable { await load() }
        .task { await load() }
        .safeAreaInset(edge: .bottom, spacing: 0) {
            if let thread, !model.isDeleted(workID) {
                replyBar(thread.work)
            }
        }
        .sheet(isPresented: $isReplying, onDismiss: { Task { await load() } }) {
            if let thread {
                ComposeSheet(mode: .reply(to: model.current(thread.work)))
            }
        }
    }

    @ViewBuilder
    private func content(_ thread: WorkThread) -> some View {
        ForEach(thread.ancestors) { ancestor in
            NavigationLink(value: Route.work(id: ancestor.id)) {
                WorkCard(work: model.current(ancestor), currentUser: model.state.user)
            }
            .buttonStyle(.plain)
            CardDivider()
        }

        let work = model.current(thread.work)

        WorkCard(work: work, currentUser: model.state.user, isDetail: true)

        postedLine(work)

        statRow(work)

        CardDivider()

        if thread.replies.isEmpty {
            EmptyStateView(
                icon: "bubble.left",
                title: "No replies yet",
                message: replyPrompt(for: work)
            )
        } else {
            ForEach(thread.replies) { reply in
                if !model.isDeleted(reply.id) {
                    NavigationLink(value: Route.work(id: reply.id)) {
                        WorkCard(work: model.current(reply), currentUser: model.state.user)
                    }
                    .buttonStyle(.plain)
                    CardDivider()
                }
            }
            if thread.repliesCursor != nil {
                Text("Older replies load on the next visit.")
                    .font(.footnote)
                    .foregroundStyle(F33Color.ink5)
                    .frame(maxWidth: .infinity)
                    .padding(.vertical, F33Spacing.lg)
            }
        }
    }

    /// When it was posted, and how many people have seen it.
    ///
    /// The line the web and every other timeline puts under a work it has
    /// opened: the exact time, because a detail screen is where "2h" stops
    /// being an answer, and the view count, which belongs here rather than in
    /// the counts row below — views are not something anyone did to the work,
    /// they are how far it travelled.
    @ViewBuilder
    private func postedLine(_ work: Work) -> some View {
        HStack(spacing: F33Spacing.xs) {
            Text(RelativeTime.fullLabel(for: work.createdAt))
            if let views = Counts.label(work.viewCount) {
                Text("·")
                Text(views).fontWeight(.semibold).foregroundStyle(F33Color.ink2)
                Text(work.viewCount == 1 ? "View" : "Views")
            }
            Spacer(minLength: 0)
        }
        .font(.system(size: 13))
        .foregroundStyle(F33Color.ink4)
        .padding(.horizontal, F33Card.paddingHorizontal)
        .padding(.vertical, F33Spacing.md)
        .overlay(alignment: .top) { CardDivider() }
        .accessibilityElement(children: .combine)
    }

    private func statRow(_ work: Work) -> some View {
        HStack(spacing: F33Spacing.lg) {
            stat(work.repostCount, "Reposts")
            stat(work.quoteCount, "Quotes")
            stat(work.likeCount, "Likes")
            stat(work.bookmarkCount, "Saves")
            Spacer(minLength: 0)
            // Only when there is a list to open. "View quotes" over an empty
            // list is a promise the next screen cannot keep.
            if work.quoteCount > 0 {
                NavigationLink(value: Route.workQuotes(id: work.id)) {
                    HStack(spacing: 2) {
                        Text("View quotes")
                            .font(.system(size: 13, weight: .semibold))
                        Image(systemName: "chevron.right")
                            .font(.system(size: 11, weight: .semibold))
                    }
                    .foregroundStyle(F33Color.accent)
                    .padding(.vertical, F33Spacing.xs)
                    .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .accessibilityLabel("View the \(Counts.exact(work.quoteCount)) works quoting this")
            }
        }
        .padding(.horizontal, F33Card.paddingHorizontal)
        .padding(.vertical, F33Spacing.md)
        .overlay(alignment: .top) { CardDivider() }
    }

    /// One count and what it counts. `plural` is the label; one of a thing
    /// drops the "s", because "1 Quotes" is the kind of thing a reader notices
    /// and nothing else on the screen gets wrong.
    @ViewBuilder
    private func stat(_ value: Int, _ plural: String) -> some View {
        if value > 0 {
            HStack(spacing: 4) {
                Text(Counts.exact(value))
                    .font(.system(size: 13, weight: .semibold))
                    .foregroundStyle(F33Color.ink)
                Text(value == 1 ? String(plural.dropLast()) : plural)
                    .font(.system(size: 13))
                    .foregroundStyle(F33Color.ink4)
            }
        }
    }

    /// The reply affordance at the foot of the screen: a glass bar with the
    /// reader's face and a prompt, or the restriction stated plainly.
    private func replyBar(_ work: Work) -> some View {
        let live = model.current(work)
        let canReply = live.viewerMayReply(currentUser: model.state.user)
        return Button {
            isReplying = true
        } label: {
            HStack(spacing: F33Spacing.md) {
                if let user = model.state.user?.user {
                    F33Avatar(user: user, size: 30)
                }
                Text(canReply ? "Reply to @\(live.author.handle)" : replyPrompt(for: live))
                    .font(.system(size: 14))
                    .foregroundStyle(canReply ? F33Color.ink4 : F33Color.ink5)
                    .lineLimit(1)
                Spacer(minLength: 0)
                if canReply {
                    Image(systemName: "arrow.up.circle.fill")
                        .font(.system(size: 22))
                        .foregroundStyle(F33Color.accent)
                }
            }
            .padding(.horizontal, F33Spacing.lg)
            .padding(.vertical, F33Spacing.sm)
            .frame(minHeight: F33Layout.minTouchTarget + 8)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .disabled(!canReply)
        .f33GlassBar(extending: .bottom)
        .overlay(alignment: .top) { CardDivider() }
        .accessibilityLabel(canReply ? "Reply to @\(live.author.handle)" : replyPrompt(for: live))
    }

    private func replyPrompt(for work: Work) -> String {
        switch work.commentGating {
        case "none": return "The author has turned off replies."
        case "followers", "circle": return "Only people the author follows can reply."
        case "verified": return "Only verified accounts can reply."
        default: return "Be the first to reply."
        }
    }

    private func load() async {
        error = nil
        do {
            thread = try await model.workThread(id: workID)
        } catch let apiError as APIError {
            if thread == nil { error = apiError }
        } catch {
            if thread == nil { self.error = .transport(error.localizedDescription) }
        }
        isLoading = false
    }
}
