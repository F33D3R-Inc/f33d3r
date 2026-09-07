import SwiftUI
import F33D3RKit

/// Search: works, people and tags, from one field.
///
/// The screen a search route pushes. Everyday searching happens in Explore,
/// whose chrome carries the field; this is the same store and the same results
/// under the system's own `searchable` bar, kept because a route that lands
/// here — a deep link, a run rooted at `F33D3R_PAGE=search` — has to land
/// somewhere, and because recents and trending tags are worth a screen that
/// opens on them.
///
/// Results come as the reader types: a keystroke cancels the request in
/// flight, and the store waits a beat before asking, so a word typed quickly
/// is one request rather than five. Everything shown is the server's answer;
/// the only thing kept on the device is the list of recent searches, which is
/// the reader's habit rather than the platform's state.
struct SearchView: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        @Bindable var store = model.searchStore()

        ScrollView {
            SearchResultsList(store: store)
        }
        .background(F33Color.bg)
        .scrollDismissesKeyboard(.interactively)
        .navigationTitle("Search")
        .navigationBarTitleDisplayMode(.inline)
        .searchable(text: $store.query, prompt: "Works, people, #tags")
        .textInputAutocapitalization(.never)
        .autocorrectionDisabled()
        .onSubmit(of: .search) { store.remember(store.query) }
        .task(id: store.query) { await store.search() }
        .task { await store.loadTrending() }
        .animation(F33Motion.easeOut, value: store.phase)
    }
}

/// Everything a search shows below the field, whichever field that is.
///
/// A view of its own because two surfaces show it — this screen, and the field
/// pinned in Explore's chrome — and a second copy of the sections is how the
/// two would come to disagree about what a result looks like. It carries no
/// scroll view and no field: the surface owns both, because where the field
/// sits is the only thing the two disagree about.
struct SearchResultsList: View {
    @Environment(AppModel.self) private var model

    let store: SearchStore
    /// Whether an empty query shows recents and trending tags. This screen
    /// opens on them. Explore does not: an empty field there means the reader
    /// is browsing, and the browsing is already on screen behind this.
    var showsSuggestions = true

    var body: some View {
        LazyVStack(alignment: .leading, spacing: 0) {
            switch store.phase {
            case .idle:
                if showsSuggestions {
                    idle(store)
                } else {
                    // Explore reaches this state for the frame between a
                    // keystroke and the request it starts, and the query is
                    // never empty in it.
                    searchingRow
                }
            case .searching:
                if store.results.isEmpty {
                    searchingRow
                } else {
                    results(store)
                        .opacity(0.6)
                }
            case .results:
                results(store)
            case .empty:
                EmptyStateView(
                    icon: "magnifyingglass",
                    title: "Nothing for “\(store.query)”",
                    message: "Try a handle, a word from a work, or a tag."
                )
            case .failed(let error):
                ErrorStateView(error: error) {
                    Task { await store.search() }
                }
            }
        }
    }

    // MARK: Idle: recents and trending

    @ViewBuilder
    private func idle(_ store: SearchStore) -> some View {
        if !store.recents.isEmpty {
            sectionTitle("Recent") {
                Button("Clear") { store.clearRecents() }
                    .font(.system(size: 13, weight: .medium))
                    .foregroundStyle(F33Color.accent)
            }
            FlowLayout(spacing: F33Spacing.sm) {
                ForEach(store.recents, id: \.self) { recent in
                    RecentChip(text: recent) {
                        store.query = recent
                    } remove: {
                        store.forget(recent)
                    }
                }
            }
            .padding(.horizontal, F33Card.paddingHorizontal)
            .padding(.bottom, F33Spacing.lg)
        }

        if !store.trending.isEmpty {
            sectionTitle("Trending tags") { EmptyView() }
            ForEach(Array(store.trending.enumerated()), id: \.element.id) { index, tag in
                NavigationLink(value: Route.tag(tag.tag)) {
                    TagCountRow(tag: tag, rank: index + 1)
                }
                .buttonStyle(.plain)
                CardDivider()
            }
        } else if store.recents.isEmpty {
            EmptyStateView(
                icon: "magnifyingglass",
                title: "Find anything on F33D3R",
                message: "Works by what they say, people by handle or name, tags by name."
            )
        }
    }

    private var searchingRow: some View {
        HStack(spacing: F33Spacing.sm) {
            ProgressView()
            Text("Searching…")
                .font(.footnote)
                .foregroundStyle(F33Color.ink4)
        }
        .frame(maxWidth: .infinity)
        .padding(F33Spacing.xl)
    }

    // MARK: Results

    @ViewBuilder
    private func results(_ store: SearchStore) -> some View {
        let r = store.results

        if !r.people.isEmpty {
            sectionTitle("People") { EmptyView() }
            ForEach(r.people) { user in
                NavigationLink(value: Route.profile(handle: user.handle)) {
                    PersonRow(user: user)
                }
                .buttonStyle(.plain)
                .simultaneousGesture(TapGesture().onEnded { store.remember(store.query) })
                CardDivider()
            }
        }

        if !r.tags.isEmpty {
            sectionTitle("Tags") { EmptyView() }
            ForEach(r.tags) { tag in
                NavigationLink(value: Route.tag(tag.tag)) {
                    TagCountRow(tag: tag, rank: nil)
                }
                .buttonStyle(.plain)
                .simultaneousGesture(TapGesture().onEnded { store.remember(store.query) })
                CardDivider()
            }
        }

        if !r.works.isEmpty {
            sectionTitle("Works") { EmptyView() }
            ForEach(r.works) { work in
                NavigationLink(value: Route.work(id: work.id)) {
                    WorkCard(work: model.current(work), currentUser: model.state.user)
                }
                .buttonStyle(.plain)
                .simultaneousGesture(TapGesture().onEnded { store.remember(store.query) })
                CardDivider()
            }
        }
    }

    private func sectionTitle<Trailing: View>(_ title: String, @ViewBuilder trailing: () -> Trailing) -> some View {
        HStack {
            Text(title)
                .font(.system(size: 13, weight: .semibold))
                .foregroundStyle(F33Color.ink4)
                .textCase(.uppercase)
            Spacer()
            trailing()
        }
        .padding(.horizontal, F33Card.paddingHorizontal)
        .padding(.top, F33Spacing.lg)
        .padding(.bottom, F33Spacing.sm)
        .accessibilityAddTraits(.isHeader)
    }
}

/// A recent query: tap to run it again, the × to forget it.
private struct RecentChip: View {
    let text: String
    let run: () -> Void
    let remove: () -> Void

    var body: some View {
        HStack(spacing: 6) {
            Button(action: run) {
                HStack(spacing: 5) {
                    Image(systemName: "clock.arrow.circlepath")
                        .font(.system(size: 11, weight: .semibold))
                    Text(text)
                        .font(.system(size: 14))
                        .lineLimit(1)
                }
                .foregroundStyle(F33Color.ink2)
            }
            .buttonStyle(.plain)

            Button(action: remove) {
                Image(systemName: "xmark")
                    .font(.system(size: 10, weight: .bold))
                    .foregroundStyle(F33Color.ink4)
                    .frame(width: 22, height: 22)
                    .contentShape(Circle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Forget “\(text)”")
        }
        .padding(.leading, F33Spacing.md)
        .padding(.trailing, 6)
        .frame(minHeight: 36)
        .background(F33Color.bgSunken, in: Capsule())
        .overlay(Capsule().strokeBorder(F33Color.hairline, lineWidth: 1))
    }
}

/// One tag with how many works carry it.
struct TagCountRow: View {
    let tag: TagCount
    /// Its position on the trending list, or nil in a search result.
    let rank: Int?

    var body: some View {
        HStack(spacing: F33Spacing.md) {
            if let rank {
                Text("\(rank)")
                    .font(.system(size: 13, weight: .semibold).monospacedDigit())
                    .foregroundStyle(F33Color.ink4)
                    .frame(width: 20, alignment: .trailing)
            } else {
                Image(systemName: "number")
                    .font(.system(size: 15, weight: .semibold))
                    .foregroundStyle(F33Color.accent)
                    .frame(width: 20)
            }

            VStack(alignment: .leading, spacing: 2) {
                Text("#\(tag.tag)")
                    .font(.system(size: 15, weight: .semibold))
                    .foregroundStyle(F33Color.ink)
                Text(tag.count == 1 ? "1 work" : "\(Counts.exact(tag.count)) works")
                    .font(.system(size: 13))
                    .foregroundStyle(F33Color.ink4)
            }

            Spacer(minLength: 0)

            Image(systemName: "chevron.right")
                .font(.system(size: 12, weight: .semibold))
                .foregroundStyle(F33Color.ink5)
        }
        .padding(.horizontal, F33Card.paddingHorizontal)
        .padding(.vertical, F33Spacing.md)
        .frame(minHeight: F33Layout.minTouchTarget)
        .contentShape(Rectangle())
        .accessibilityElement(children: .combine)
    }
}

/// One person: avatar, name, handle, a line of bio.
struct PersonRow<Trailing: View>: View {
    let user: User
    @ViewBuilder var trailing: () -> Trailing

    var body: some View {
        HStack(alignment: .center, spacing: F33Card.columnGap) {
            F33Avatar(user: user, size: F33Card.avatarColumnWidth)

            VStack(alignment: .leading, spacing: 2) {
                HStack(spacing: F33Card.authorRowGap) {
                    Text(user.displayName)
                        .font(.system(size: 15, weight: .semibold))
                        .foregroundStyle(F33Color.ink)
                        .lineLimit(1)
                    if let badge = user.badge {
                        BadgePill(badge: badge)
                    }
                }
                Text("@\(user.handle)")
                    .font(.system(size: 13))
                    .foregroundStyle(F33Color.ink4)
                    .lineLimit(1)
                if let bio = user.bio, !bio.isEmpty {
                    Text(bio)
                        .font(.system(size: 13))
                        .foregroundStyle(F33Color.ink3)
                        .lineLimit(2)
                        .padding(.top, 2)
                }
            }

            Spacer(minLength: F33Spacing.sm)

            trailing()
        }
        .padding(.horizontal, F33Card.paddingHorizontal)
        .padding(.vertical, F33Spacing.md)
        .contentShape(Rectangle())
    }
}

extension PersonRow where Trailing == EmptyView {
    init(user: User) {
        self.init(user: user) { EmptyView() }
    }
}
