import SwiftUI
import F33D3RKit

/// Someone's profile: header, then their works under a tab strip.
///
/// The header collapses into the navigation bar as you scroll, which is the one
/// place a native profile can be better than the web one without diverging from
/// it — the same information, arranged for a thumb.
struct ProfileView: View {
    let handle: String
    /// DEBUG: opens the editor as soon as the profile has loaded, for a run
    /// nobody can tap in.
    var debugOpensEditor = false

    @Environment(AppModel.self) private var model

    @State private var profile: Profile?
    @State private var error: APIError?
    @State private var tab: Profile.Tab = .works
    @State private var isChangingFollow = false
    @State private var isWriting = false
    @State private var isTipping = false
    @State private var isEditing = false
    @State private var isConfirmingBlock = false
    @State private var actionError: String?

    var body: some View {
        Group {
            if let error, profile == nil {
                ErrorStateView(error: error) {
                    Task { await load() }
                }
            } else {
                WorkList(
                    feed: model.profileFeed(handle: handle, tab: tab),
                    currentUser: model.state.user,
                    emptyState: emptyState
                ) {
                    header
                }
            }
        }
        .navigationTitle(profile?.user.displayName ?? "@\(handle)")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            if isOwnProfile {
                ToolbarItem(placement: .topBarTrailing) {
                    NavigationLink(value: Route.settings) {
                        Image(systemName: "gearshape")
                    }
                    .accessibilityLabel("Settings")
                }
            }
        }
        .task { await load() }
        .sheet(isPresented: $isTipping) {
            TipSheet(handle: handle)
        }
        .sheet(isPresented: $isWriting) {
            if let user = profile?.user {
                NewMessageSheet(
                    handle: user.handle,
                    displayName: user.displayName.isEmpty ? user.handle : user.displayName,
                    avatarURL: user.avatarURL
                )
            }
        }
        .sheet(isPresented: $isEditing) {
            if let user = profile?.user {
                EditProfileView(user: user) {
                    await load()
                }
            }
        }
        .confirmationDialog(
            "Block @\(handle)?",
            isPresented: $isConfirmingBlock,
            titleVisibility: .visible
        ) {
            Button("Block", role: .destructive) {
                Task { await setBlocked() }
            }
        } message: {
            Text("You won't see each other's works, and they can't follow you or reach you. Undo it under Settings → Blocked & muted.")
        }
        .alert("That didn't go through", isPresented: Binding(get: { actionError != nil }, set: { if !$0 { actionError = nil } })) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(actionError ?? "")
        }
        // Reached with `.likes` selected — a deep link, or a session that
        // changed underneath the screen — the strip would show no selection at
        // all. Fall back to the tab every profile has.
        .onChange(of: isOwnProfile) { _, isOwner in
            if !isOwner, tab.isPrivate { tab = .works }
        }
        // A profile the reader is looking at should show what the server holds
        // once they have changed it elsewhere — Settings, the editor.
        .onChange(of: model.state.user?.user) { _, _ in
            if isOwnProfile { Task { await load() } }
        }
        // A work begun while looking at somebody opens addressed to them. The
        // compose button is the shell's, so this is how the profile says so.
        .onAppear { armComposer() }
        .onDisappear { model.seedComposer("") }
        .onChange(of: model.composerSeed) { _, seed in
            // The sheet takes the seed as it opens. Arming it again means a
            // second work started from this same profile is addressed the same
            // way as the first, rather than the reader having to type it.
            if seed.isEmpty { armComposer() }
        }
    }

    /// Nobody is mentioned into their own work.
    private func armComposer() {
        model.seedComposer(isOwnProfile ? "" : "@\(handle) ")
    }

    @ViewBuilder
    private var header: some View {
        VStack(alignment: .leading, spacing: 0) {
            banner

            VStack(alignment: .leading, spacing: F33Spacing.sm) {
                avatarRow
                nameRow

                if let bio = profile?.user.bio, !bio.isEmpty {
                    WorkBodyText(text: bio)
                        .padding(.top, 2)
                }

                metaRow
                statsRow
            }
            .padding(.horizontal, F33Card.paddingHorizontal)
            .padding(.bottom, F33Spacing.lg)

            tabStrip
        }
    }

    private var banner: some View {
        RemoteImage(path: profile?.user.headerURL, seed: handle)
            .frame(height: 130)
            .frame(maxWidth: .infinity)
            .clipped()
    }

    private var avatarRow: some View {
        HStack(alignment: .bottom) {
            Group {
                if let user = profile?.user {
                    F33Avatar(user: user, size: 76)
                } else {
                    F33Avatar(handle: handle, displayName: handle, avatarURL: nil, size: 76)
                }
            }
            .overlay(Circle().strokeBorder(F33Color.bg, lineWidth: 4))
            // Pulled up so it straddles the banner edge, the way the web's
            // profile header overlaps its own.
            .offset(y: -28)
            .padding(.bottom, -28)

            Spacer()

            if isOwnProfile {
                editButton
            } else {
                followButton
            }
        }
    }

    /// The owner's own profile: no follow button, an editor instead.
    private var editButton: some View {
        Button {
            isEditing = true
        } label: {
            Text("Edit profile")
                .font(.system(size: 14, weight: .semibold))
                .foregroundStyle(F33Color.ink2)
                .padding(.horizontal, F33Spacing.lg)
                .frame(minHeight: 36)
                .background(Capsule().strokeBorder(F33Color.hairlineStrong, lineWidth: 1))
                .frame(minHeight: F33Layout.minTouchTarget)
                .contentShape(Capsule())
        }
        .buttonStyle(.plain)
        .disabled(profile == nil)
        .onChange(of: profile == nil) { _, isNil in
            #if DEBUG
            if !isNil, debugOpensEditor { isEditing = true }
            #endif
        }
    }

    /// Follow, unfollow, and tip. Each is one event; the button shows the
    /// relationship the server has confirmed, and reloads the profile after
    /// the write so the counts beside it are the server's too.
    @ViewBuilder
    private var followButton: some View {
        if let profile, profile.user.handle != model.state.user?.user.handle {
            HStack(spacing: F33Spacing.sm) {
                // Blocking and muting: the two things you do about a person
                // rather than about a work, so they live on the person's page
                // and nowhere else on it. Unblocking is Settings → Blocked &
                // muted, because the profile DTO does not say whether the
                // viewer has blocked this account and a button that guessed
                // would be a button that lies half the time.
                Menu {
                    Button {
                        Task { await setMuted(true) }
                    } label: {
                        Label("Mute @\(profile.user.handle)", systemImage: "speaker.slash")
                    }
                    Button(role: .destructive) {
                        isConfirmingBlock = true
                    } label: {
                        Label("Block @\(profile.user.handle)", systemImage: "hand.raised")
                    }
                } label: {
                    Image(systemName: "ellipsis")
                        .font(.system(size: 14, weight: .semibold))
                        .foregroundStyle(F33Color.ink2)
                        .frame(width: 36, height: 36)
                        .background(F33Color.bgSunken, in: Circle())
                        .frame(minWidth: F33Layout.minTouchTarget, minHeight: F33Layout.minTouchTarget)
                        .contentShape(Circle())
                }
                .menuStyle(.button)
                .buttonStyle(.plain)
                .accessibilityLabel("More actions for @\(profile.user.handle)")

                Button {
                    isTipping = true
                } label: {
                    Image(systemName: "bolt.fill")
                        .font(.system(size: 14, weight: .semibold))
                        .foregroundStyle(F33Action.tip)
                        .frame(width: 36, height: 36)
                        .background(F33Action.tip.opacity(0.14), in: Circle())
                        .frame(minWidth: F33Layout.minTouchTarget, minHeight: F33Layout.minTouchTarget)
                        .contentShape(Circle())
                }
                .buttonStyle(.plain)
                .accessibilityLabel("Tip @\(profile.user.handle)")

                Button {
                    isWriting = true
                } label: {
                    Image(systemName: "envelope.fill")
                        .font(.system(size: 14, weight: .semibold))
                        .foregroundStyle(F33Color.ink2)
                        .frame(width: 36, height: 36)
                        .background(F33Color.bgSunken, in: Circle())
                        .frame(minWidth: F33Layout.minTouchTarget, minHeight: F33Layout.minTouchTarget)
                        .contentShape(Circle())
                }
                .buttonStyle(.plain)
                .accessibilityLabel("Message @\(profile.user.handle)")

                Button {
                    Task { await setFollowing(!profile.viewerFollows) }
                } label: {
                    Group {
                        if isChangingFollow {
                            ProgressView().tint(profile.viewerFollows ? F33Color.ink2 : F33Color.accentInk)
                        } else {
                            Text(profile.viewerFollows ? "Following" : "Follow")
                        }
                    }
                    .font(.system(size: 14, weight: .semibold))
                    .foregroundStyle(profile.viewerFollows ? F33Color.ink2 : F33Color.accentInk)
                    .padding(.horizontal, F33Spacing.lg)
                    .frame(minHeight: 36)
                    .background {
                        if profile.viewerFollows {
                            Capsule().strokeBorder(F33Color.hairlineStrong, lineWidth: 1)
                        } else {
                            Capsule().fill(F33Color.accent)
                        }
                    }
                    .frame(minHeight: F33Layout.minTouchTarget)
                    .contentShape(Capsule())
                }
                .buttonStyle(.plain)
                .disabled(isChangingFollow)
                .accessibilityLabel(profile.viewerFollows ? "Unfollow @\(profile.user.handle)" : "Follow @\(profile.user.handle)")
            }
        }
    }

    /// Blocking severs the relationship on the server, so the profile is
    /// re-read afterwards: the follow state, the counts and whether there is
    /// anything left to show are all the server's answer, not this screen's.
    private func setBlocked() async {
        guard let profile else { return }
        do {
            try await model.setBlocked(true, handle: profile.user.handle)
            await load()
        } catch let error as MalkuthError {
            actionError = error.description
        } catch let error as APIError {
            actionError = error.userMessage
        } catch {
            actionError = error.localizedDescription
        }
    }

    private func setMuted(_ muted: Bool) async {
        guard let profile else { return }
        do {
            try await model.setMuted(muted, handle: profile.user.handle)
        } catch let error as MalkuthError {
            actionError = error.description
        } catch let error as APIError {
            actionError = error.userMessage
        } catch {
            actionError = error.localizedDescription
        }
    }

    private func setFollowing(_ following: Bool) async {
        guard let profile, !isChangingFollow else { return }
        isChangingFollow = true
        defer { isChangingFollow = false }
        do {
            try await model.setFollowing(following, handle: profile.user.handle)
            await load()
            // Following changes what the home lanes hold; the next pull gets it.
        } catch let error as MalkuthError {
            actionError = error.description
        } catch let error as APIError {
            actionError = error.userMessage
        } catch {
            actionError = error.localizedDescription
        }
    }

    private var nameRow: some View {
        VStack(alignment: .leading, spacing: 2) {
            HStack(spacing: F33Card.authorRowGap) {
                Text(profile?.user.displayName ?? handle)
                    .font(.title3.weight(.bold))
                    .foregroundStyle(F33Color.ink)

                if let badge = profile?.user.badge {
                    BadgePill(badge: badge)
                }
            }

            HStack(spacing: F33Spacing.sm) {
                Text("@\(handle)")
                    .font(.subheadline)
                    .foregroundStyle(F33Color.ink4)

                if profile?.followsViewer == true {
                    Text("Follows you")
                        .font(.system(size: 11, weight: .medium))
                        .foregroundStyle(F33Color.ink3)
                        .padding(.horizontal, 6)
                        .padding(.vertical, 2)
                        .background(F33Color.bgSunken, in: RoundedRectangle(cornerRadius: 4))
                }
            }
        }
    }

    @ViewBuilder
    private var metaRow: some View {
        if let user = profile?.user {
            // Only the fields the person actually filled in. An empty row of
            // grey placeholder icons says nothing.
            let items: [(String, String)] = [
                user.pronouns.map { ("person", $0) },
                user.location.map { ("mappin.and.ellipse", $0) },
                user.website.map { ("link", $0) },
            ].compactMap { $0 }

            if !items.isEmpty {
                FlowLayout(spacing: F33Spacing.md) {
                    ForEach(items, id: \.1) { icon, text in
                        Label(text, systemImage: icon)
                            .font(.system(size: 13))
                            .foregroundStyle(F33Color.ink3)
                    }
                }
                .padding(.top, 2)
            }

            HStack(spacing: F33Spacing.xs) {
                Image(systemName: "seal")
                    .font(.system(size: 11))
                // `realmName` is resolved server-side. The ladder is the
                // server's to name and the app never hardcodes it.
                Text("\(user.realmName) · Realm \(user.realm)")
                    .font(.system(size: 13, weight: .medium))
            }
            .foregroundStyle(F33Color.accent)
            .padding(.top, 2)
        }
    }

    @ViewBuilder
    private var statsRow: some View {
        if let user = profile?.user {
            HStack(spacing: F33Spacing.lg) {
                NavigationLink(value: Route.following(handle: handle)) {
                    stat(user.followingCount, "Following")
                }
                .buttonStyle(.plain)
                NavigationLink(value: Route.followers(handle: handle)) {
                    stat(user.followerCount, "Followers")
                }
                .buttonStyle(.plain)
                stat(user.postCount, "Works")
                Spacer(minLength: 0)
            }
            .padding(.top, F33Spacing.xs)
        }
    }

    private func stat(_ value: Int, _ label: String) -> some View {
        HStack(spacing: 4) {
            Text(Counts.exact(value))
                .font(.system(size: 14, weight: .semibold))
                .foregroundStyle(F33Color.ink)
            Text(label)
                .font(.system(size: 14))
                .foregroundStyle(F33Color.ink4)
        }
        .frame(minHeight: F33Layout.minTouchTarget)
        .contentShape(Rectangle())
        .accessibilityElement(children: .combine)
    }

    /// Whose profile this is. Likes are the owner's alone, and the tab is not
    /// drawn at all on anyone else's.
    private var isOwnProfile: Bool {
        handle == model.state.user?.user.handle
    }

    private var tabStrip: some View {
        HStack(spacing: 0) {
            ForEach(Profile.Tab.visible(isOwner: isOwnProfile), id: \.self) { option in
                Button { tab = option } label: {
                    VStack(spacing: F33Spacing.sm) {
                        Text(option.title)
                            .font(.system(size: 14, weight: tab == option ? .semibold : .regular))
                            .foregroundStyle(tab == option ? F33Color.ink : F33Color.ink3)
                        Capsule()
                            .fill(tab == option ? F33Color.accent : .clear)
                            .frame(height: 3)
                            .frame(maxWidth: 48)
                    }
                    .frame(maxWidth: .infinity)
                    .frame(minHeight: F33Layout.minTouchTarget)
                    .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .accessibilityAddTraits(tab == option ? [.isSelected, .isButton] : .isButton)
            }
        }
        .background(F33Color.bg)
        .overlay(alignment: .bottom) { CardDivider() }
        .animation(F33Motion.easeOut, value: tab)
    }

    private var emptyState: EmptyStateView {
        switch tab {
        case .works:
            return EmptyStateView(icon: "square.stack", title: "No works yet", message: "@\(handle) hasn't posted.")
        case .replies:
            return EmptyStateView(icon: "bubble.left", title: "No replies", message: "@\(handle) hasn't replied to anything.")
        case .media:
            return EmptyStateView(icon: "photo", title: "No media", message: "Works with images or video appear here.")
        case .likes:
            // The tab is hidden on other people's profiles, so this normally
            // only ever speaks to the owner. The other branch is what a deep
            // link into someone else's likes meets: the rule, stated plainly,
            // rather than a failure with a retry that cannot work.
            return isOwnProfile
                ? EmptyStateView(icon: "heart", title: "No likes yet", message: "Works you like appear here, and only you can see them.")
                : EmptyStateView(icon: "lock", title: "Likes are private", message: "Nobody but @\(handle) can see what they have liked.")
        case .saves:
            return isOwnProfile
                ? EmptyStateView(icon: "bookmark", title: "Nothing saved yet", message: "Works you save appear here, and only you can see them.")
                : EmptyStateView(icon: "lock", title: "Saves are private", message: "Nobody but @\(handle) can see what they have saved.")
        }
    }

    private func load() async {
        error = nil
        do {
            profile = try await model.profile(handle: handle)
        } catch let apiError as APIError {
            if profile == nil { error = apiError }
        } catch {
            if profile == nil { self.error = .transport(error.localizedDescription) }
        }
    }
}
