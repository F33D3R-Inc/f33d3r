import SwiftUI
import F33D3RKit

/// Who follows someone, or who they follow.
///
/// Each row carries a follow button for everyone but the reader. The button
/// shows the relationship the server has confirmed: a tap sends the event,
/// then the row asks for that person's profile back and draws what it says.
struct FollowListView: View {
    enum Kind {
        case followers, following

        var title: String {
            switch self {
            case .followers: return "Followers"
            case .following: return "Following"
            }
        }
    }

    let handle: String
    let kind: Kind

    @Environment(AppModel.self) private var model

    @State private var users: [User] = []
    @State private var viewerFollows: Set<String> = []
    @State private var nextCursor: String?
    @State private var isLoading = false
    @State private var isLoadingMore = false
    @State private var error: APIError?
    @State private var busy: Set<String> = []
    @State private var actionError: String?

    var body: some View {
        ScrollView {
            LazyVStack(spacing: 0) {
                if isLoading, users.isEmpty {
                    ForEach(0..<5, id: \.self) { _ in
                        PersonRowSkeleton()
                        CardDivider()
                    }
                } else if let error, users.isEmpty {
                    ErrorStateView(error: error) { Task { await load() } }
                } else if users.isEmpty {
                    emptyState
                } else {
                    ForEach(users) { user in
                        NavigationLink(value: Route.profile(handle: user.handle)) {
                            PersonRow(user: user) {
                                followButton(user)
                            }
                        }
                        .buttonStyle(.plain)
                        .onAppear {
                            if user.id == users.last?.id { Task { await loadMore() } }
                        }
                        CardDivider()
                    }
                    if isLoadingMore {
                        ProgressView()
                            .frame(maxWidth: .infinity)
                            .padding(F33Spacing.lg)
                    }
                }
            }
        }
        .background(F33Color.bg)
        .navigationTitle(kind.title)
        .navigationBarTitleDisplayMode(.inline)
        .refreshable { await load() }
        .task { await load() }
        .alert("That didn't go through", isPresented: Binding(get: { actionError != nil }, set: { if !$0 { actionError = nil } })) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(actionError ?? "")
        }
    }

    private var emptyState: EmptyStateView {
        switch kind {
        case .followers:
            return EmptyStateView(icon: "person.2", title: "No followers yet", message: "People who follow @\(handle) appear here.")
        case .following:
            return EmptyStateView(icon: "person.2", title: "Not following anyone", message: "Accounts @\(handle) follows appear here.")
        }
    }

    @ViewBuilder
    private func followButton(_ user: User) -> some View {
        if user.handle != model.state.user?.user.handle {
            let follows = viewerFollows.contains(user.handle)
            let isBusy = busy.contains(user.handle)
            Button {
                Task { await setFollowing(!follows, user: user) }
            } label: {
                Group {
                    if isBusy {
                        ProgressView().tint(follows ? F33Color.ink2 : F33Color.accentInk)
                    } else {
                        Text(follows ? "Following" : "Follow")
                    }
                }
                .font(.system(size: 13, weight: .semibold))
                .foregroundStyle(follows ? F33Color.ink2 : F33Color.accentInk)
                .frame(width: 92, height: 32)
                .background {
                    if follows {
                        Capsule().strokeBorder(F33Color.hairlineStrong, lineWidth: 1)
                    } else {
                        Capsule().fill(F33Color.accent)
                    }
                }
                .frame(minHeight: F33Layout.minTouchTarget)
                .contentShape(Capsule())
            }
            .buttonStyle(.plain)
            .disabled(isBusy)
            .accessibilityLabel(follows ? "Unfollow @\(user.handle)" : "Follow @\(user.handle)")
        }
    }

    private func setFollowing(_ following: Bool, user: User) async {
        guard !busy.contains(user.handle) else { return }
        busy.insert(user.handle)
        defer { busy.remove(user.handle) }
        do {
            try await model.setFollowing(following, handle: user.handle)
            // The row's state is the server's: ask for the relationship back.
            let profile = try await model.profile(handle: user.handle)
            if profile.viewerFollows {
                viewerFollows.insert(user.handle)
            } else {
                viewerFollows.remove(user.handle)
            }
        } catch let error as MalkuthError {
            actionError = error.description
        } catch let error as APIError {
            actionError = error.userMessage
        } catch {
            actionError = error.localizedDescription
        }
    }

    private func load() async {
        isLoading = true
        error = nil
        defer { isLoading = false }
        do {
            let page = try await fetch(cursor: nil)
            users = page.users
            viewerFollows = page.viewerFollows
            nextCursor = page.nextCursor
        } catch let apiError as APIError {
            error = apiError
        } catch {
            self.error = .transport(error.localizedDescription)
        }
    }

    private func loadMore() async {
        guard let cursor = nextCursor, !isLoadingMore else { return }
        isLoadingMore = true
        defer { isLoadingMore = false }
        do {
            let page = try await fetch(cursor: cursor)
            let known = Set(users.map(\.handle))
            users.append(contentsOf: page.users.filter { !known.contains($0.handle) })
            viewerFollows.formUnion(page.viewerFollows)
            nextCursor = page.nextCursor
        } catch {
            nextCursor = nil
        }
    }

    private func fetch(cursor: String?) async throws -> UserPage {
        switch kind {
        case .followers: return try await model.followers(handle: handle, cursor: cursor)
        case .following: return try await model.following(handle: handle, cursor: cursor)
        }
    }
}

/// A person row's shape while the page loads.
struct PersonRowSkeleton: View {
    var body: some View {
        HStack(spacing: F33Card.columnGap) {
            Circle()
                .fill(F33Color.bgSunken)
                .frame(width: F33Card.avatarColumnWidth, height: F33Card.avatarColumnWidth)
            VStack(alignment: .leading, spacing: 6) {
                RoundedRectangle(cornerRadius: 4).fill(F33Color.bgSunken).frame(width: 140, height: 14)
                RoundedRectangle(cornerRadius: 4).fill(F33Color.bgSunken).frame(width: 90, height: 12)
            }
            Spacer()
        }
        .padding(.horizontal, F33Card.paddingHorizontal)
        .padding(.vertical, F33Spacing.md)
        .redacted(reason: .placeholder)
        .accessibilityHidden(true)
    }
}
