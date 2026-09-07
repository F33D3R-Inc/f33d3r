import SwiftUI
import F33D3RKit

/// Notifications: what happened to the reader's works while they were away.
///
/// Grouped by the server — three likes on one work within an hour are one row
/// with three faces — and read state is the server's: a row turns read after
/// the event has been recorded, not when the finger lands. Tapping a row does
/// both things at once: marks it read and goes where it points.
struct NotificationsView: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        let feed = model.notificationsFeed()

        ScrollView {
            LazyVStack(spacing: 0) {
                #if DEBUG
                if model.isSampleMode {
                    sampleList
                } else {
                    list(feed)
                }
                #else
                list(feed)
                #endif
            }
        }
        .background(F33Color.bg)
        .navigationTitle("Notifications")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                if let unread = feed.unreadCount, unread > 0 {
                    Button("Mark all read") {
                        Task { await feed.markAllRead() }
                    }
                    .font(.system(size: 14, weight: .medium))
                    .foregroundStyle(F33Color.accent)
                    .frame(minHeight: F33Layout.minTouchTarget)
                }
            }
        }
        .refreshable { await feed.reload() }
        .task { await feed.loadFirstPageIfNeeded() }
        // A rising unread count means something new landed; the list asks for
        // the page again rather than guessing what it was.
        .onChange(of: model.signals?.unread) { old, new in
            guard let old, let new, new > old, feed.phase != .idle else { return }
            Task { await feed.reload() }
        }
    }

    @ViewBuilder
    private func list(_ feed: NotificationsFeed) -> some View {
        switch feed.phase {
        case .idle, .loading:
            ForEach(0..<4, id: \.self) { _ in
                NotificationRowSkeleton()
                CardDivider()
            }
        case .empty:
            EmptyStateView(
                icon: "tray",
                title: "Nothing yet",
                message: "Likes, replies, follows and tips on your works land here."
            )
        case .failed(let error):
            ErrorStateView(error: error) {
                Task { await feed.reload() }
            }
        case .loaded:
            ForEach(feed.notifications) { notification in
                NotificationRow(notification: notification) {
                    Task { await feed.markRead(notification) }
                }
                .onAppear {
                    guard feed.shouldLoadMore(after: notification) else { return }
                    Task { await feed.loadMore() }
                }
                CardDivider()
            }

            if let error = feed.loadMoreError {
                VStack(spacing: F33Spacing.sm) {
                    Text(error.userMessage)
                        .font(.footnote)
                        .foregroundStyle(F33Color.ink4)
                    Button("Try again") { Task { await feed.loadMore() } }
                        .font(.subheadline.weight(.semibold))
                        .foregroundStyle(F33Color.accent)
                        .frame(minHeight: F33Layout.minTouchTarget)
                }
                .frame(maxWidth: .infinity)
                .padding(F33Spacing.lg)
            } else if feed.isLoadingMore {
                ProgressView()
                    .frame(maxWidth: .infinity)
                    .padding(F33Spacing.lg)
            }
        }
    }

    #if DEBUG
    @ViewBuilder
    private var sampleList: some View {
        ForEach(SampleData.notifications) { notification in
            NotificationRow(notification: notification) {}
            CardDivider()
        }

        Text("Sample notifications.")
            .font(.footnote)
            .foregroundStyle(F33Color.ink5)
            .frame(maxWidth: .infinity)
            .padding(.vertical, F33Spacing.xl)
    }
    #endif
}

/// One notification.
///
/// Laid out as the card is — an avatar column, a gutter, then everything else —
/// so the two lists read as the same product. A grouped row stacks up to three
/// faces in the leading column, because "who" is the content of a like in a
/// way it never is for a work. The whole row is one VoiceOver element.
struct NotificationRow: View {
    let notification: NotificationItem
    /// Called when the row is opened, so the owner can mark it read.
    let onOpen: () -> Void

    var body: some View {
        rowContent
            .padding(.vertical, F33Spacing.md)
            .padding(.horizontal, F33Card.paddingHorizontal)
            .frame(maxWidth: .infinity, alignment: .leading)
            .frame(minHeight: F33Layout.minTouchTarget)
            .background(notification.isRead ? F33Color.bg : F33Color.accentSoft.opacity(0.35))
            .contentShape(Rectangle())
            .accessibilityElement(children: .ignore)
            .accessibilityLabel(accessibilityLabel)
            .accessibilityAddTraits(.isButton)
            .animation(F33Motion.easeOut, value: notification.isRead)
    }

    @ViewBuilder
    private var rowContent: some View {
        if let destination {
            NavigationLink(value: destination) { content }
                .buttonStyle(.plain)
                .simultaneousGesture(TapGesture().onEnded { onOpen() })
        } else {
            Button(action: onOpen) { content }
                .buttonStyle(.plain)
        }
    }

    private var content: some View {
        HStack(alignment: .top, spacing: F33Card.columnGap) {
            avatars

            VStack(alignment: .leading, spacing: 3) {
                Text(notification.summary)
                    .font(.system(size: 14))
                    .foregroundStyle(F33Color.ink)
                    .fixedSize(horizontal: false, vertical: true)
                    .multilineTextAlignment(.leading)

                if let preview = notification.preview, !preview.isEmpty {
                    Text(preview)
                        .font(.system(size: 13))
                        .foregroundStyle(F33Color.ink4)
                        .lineLimit(2)
                        .fixedSize(horizontal: false, vertical: true)
                }

                Text(RelativeTime.label(for: notification.createdAt))
                    .font(.system(size: 11))
                    .foregroundStyle(F33Color.ink5)
            }

            Spacer(minLength: 0)

            if !notification.isRead {
                Circle()
                    .fill(F33Color.accent)
                    .frame(width: 8, height: 8)
                    .padding(.top, 5)
            }
        }
    }

    private var avatars: some View {
        HStack(spacing: overlapSpacing) {
            ForEach(notification.actors.prefix(3)) { actor in
                F33Avatar(author: actor, size: faceSize)
                    .overlay(Circle().strokeBorder(F33Color.bg, lineWidth: 2))
            }
        }
        .frame(width: Self.avatarColumnWidth, alignment: .leading)
        .overlay(alignment: .bottomLeading) {
            Image(systemName: icon)
                .font(.system(size: 9, weight: .bold))
                .foregroundStyle(.white)
                .frame(width: 17, height: 17)
                .background(tint, in: Circle())
                .overlay(Circle().strokeBorder(F33Color.bg, lineWidth: 1.5))
                .offset(x: -3, y: 3)
        }
        .frame(width: Self.avatarColumnWidth, height: Self.avatarColumnWidth)
    }

    static let avatarColumnWidth: CGFloat = 56

    private var faceCount: Int { min(3, max(1, notification.actors.count)) }

    private var faceSize: CGFloat {
        switch faceCount {
        case 1: return 40
        case 2: return 32
        default: return 28
        }
    }

    private var overlapSpacing: CGFloat {
        guard faceCount > 1 else { return 0 }
        let total = CGFloat(faceCount) * faceSize
        return (Self.avatarColumnWidth - total) / CGFloat(faceCount - 1)
    }

    private var icon: String {
        switch notification.kind {
        case "like": return "heart.fill"
        case "repost": return "arrow.2.squarepath"
        case "follow": return "person.fill.badge.plus"
        case "reply", "thread_reply": return "bubble.left.fill"
        case "quote": return "quote.opening"
        case "mention": return "at"
        case "tip": return "bolt.fill"
        case "purchase": return "cart.fill"
        case "subscribe": return "star.fill"
        default: return "bell.fill"
        }
    }

    private var tint: Color {
        switch notification.kind {
        case "like": return F33Action.like
        case "repost": return F33Action.repost
        case "tip", "purchase": return F33Action.tip
        default: return F33Color.accent
        }
    }

    private var destination: Route? {
        if let workID = notification.destinationWorkID { return .work(id: workID) }
        if let handle = notification.destinationHandle { return .profile(handle: handle) }
        return nil
    }

    private var accessibilityLabel: String {
        var parts = [notification.summary]
        if let preview = notification.preview, !preview.isEmpty { parts.append(preview) }
        parts.append(RelativeTime.fullLabel(for: notification.createdAt))
        if !notification.isRead { parts.append("Unread") }
        return parts.joined(separator: ", ")
    }
}

/// The row's shape while the page loads.
struct NotificationRowSkeleton: View {
    @State private var isPulsing = false

    var body: some View {
        HStack(alignment: .top, spacing: F33Card.columnGap) {
            Circle()
                .fill(F33Color.ink5.opacity(0.3))
                .frame(width: 40, height: 40)
                .frame(width: NotificationRow.avatarColumnWidth, alignment: .leading)
            VStack(alignment: .leading, spacing: F33Spacing.sm) {
                RoundedRectangle(cornerRadius: 3).fill(F33Color.ink5.opacity(0.3)).frame(width: 200, height: 13)
                RoundedRectangle(cornerRadius: 3).fill(F33Color.ink5.opacity(0.22)).frame(width: 120, height: 11)
            }
            Spacer(minLength: 0)
        }
        .padding(.vertical, F33Spacing.md)
        .padding(.horizontal, F33Card.paddingHorizontal)
        .opacity(isPulsing ? 0.55 : 1)
        .animation(.easeInOut(duration: 0.9).repeatForever(autoreverses: true), value: isPulsing)
        .onAppear { isPulsing = true }
        .accessibilityHidden(true)
    }
}

#Preview("Notifications") {
    NavigationStack {
        NotificationsView()
    }
    .environment(AppModel())
}
