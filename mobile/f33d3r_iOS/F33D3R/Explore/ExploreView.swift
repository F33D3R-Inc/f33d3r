import SwiftUI
import F33D3RKit

/// Explore: the search field, the two discovery lanes, and the one place in the
/// app where the reader is looking at works nobody they follow put in front of
/// them.
///
/// That is the whole design constraint. Home's Following lane is a list the
/// reader assembled; this one is assembled for them, so everything the pattern
/// audit said about ranked surfaces applies here first: a standing note that the
/// surface is chosen rather than chronological, the per-work `why:` chip
/// wherever the server explains a pick, and a way to say a pick was wrong. What
/// it deliberately does not have is the rest of what a discovery tab usually
/// grows — no suggested-people row, no trending-topics panel, no promoted slot,
/// no upsell. A surface that recommends works can be honest about doing that;
/// one that also recommends people to follow and topics to read and a plan to
/// buy is a shop.
///
/// Search is the other half of the screen and it is a field, not a button. This
/// is where a reader who is not being shown what they want comes to say what
/// they want, and a magnifying glass that pushes a second screen to hand them a
/// field is one tap and one screen between them and typing. So the field is in
/// the chrome, always visible, and typing into it puts the answers where the
/// browsing was. Clearing it puts the browsing back, exactly where it was left:
/// the list underneath is never torn down, only covered.
///
/// The browsing half is `WorkList` over a different `WorkFeed`, like every other
/// list in the app. The chrome floats and both lists keep a margin its height,
/// the way Home's does; the note explaining the surface rides in the list's own
/// header and scrolls away, because an explanation is read once and then it is
/// in the way.
struct ExploreView: View {
    @Environment(AppModel.self) private var model

    @State private var lane: ExploreLane = .default
    /// The work the reader said was not for them, held until the server has
    /// taken the signal so the sheet can say so.
    @State private var notInterested: Work?
    /// The chrome's height, which is the top content margin of whichever list
    /// is on screen. Measured rather than assumed: the title and the field both
    /// grow with the reader's text size.
    @State private var chromeHeight: CGFloat = 0
    @FocusState private var isFieldFocused: Bool

    var body: some View {
        @Bindable var store = model.searchStore()
        let isSearching = !store.query.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty

        ZStack {
            // Covered rather than replaced. A reader who searches from halfway
            // down Explore and clears the field is back where they were, and
            // the feed behind is neither reachable by a finger nor walkable by
            // VoiceOver while the results are over it.
            browsing
                .allowsHitTesting(!isSearching)
                .accessibilityHidden(isSearching)

            if isSearching {
                results(store)
            }
        }
        // The chrome floats and the lists keep a margin its height — see
        // `laneTopInset`. A margin and not a safe-area inset, because a scroll
        // view clips its content at an inset and the works have to run under
        // the glass for the material to have anything to refract.
        .environment(\.laneTopInset, chromeHeight)
        .overlay(alignment: .top) {
            chrome(query: $store.query, isSearching: isSearching) {
                store.remember(store.query)
            }
            .onGeometryChange(for: CGFloat.self) { $0.size.height } action: { height in
                if height > 0, height != chromeHeight { chromeHeight = height }
            }
        }
        // The header above does the bar's job and does it over the feed rather
        // than above it.
        .toolbar(.hidden, for: .navigationBar)
        // The store debounces and a keystroke cancels the request in flight.
        // This is the same `.task(id:)` the pushed Search screen runs it from,
        // and the same store behind both, so a query typed here is the query
        // that screen opens on.
        .task(id: store.query) { await store.search() }
        // Results arriving in place of a feed is a silent change: nothing was
        // pushed, so the system announces nothing and a VoiceOver reader is
        // left holding a field that appears to have done nothing.
        .onChange(of: store.phase) { _, phase in
            announce(phase, store: store)
        }
        .sheet(item: $notInterested) { work in
            NotInterestedSheet(handle: work.author.handle)
        }
        .onAppear {
            #if DEBUG
            if let forced = model.debugExploreLane { lane = forced }
            // `F33D3R_PAGE=explore F33D3R_QUERY=…` opens on results. The
            // Simulator this is developed against cannot be typed into, so
            // without it the searching half of this screen is a half nobody
            // can look at.
            if let query = model.debugQuery { store.query = query }
            #endif
        }
    }

    // MARK: The two halves

    private var browsing: some View {
        WorkList(
            feed: model.exploreFeed(lane: lane),
            currentUser: model.state.user,
            emptyState: emptyState,
            notInterested: { work in
                notInterested = work
                Task { try? await model.notInterested(work) }
            }
        ) {
            // The note scrolls; the selector does not. An explanation is read
            // once and then it is in the way, while the control that decides
            // what the list contains has to stay reachable from anywhere in it.
            RankedSurfaceNote()
        }
    }

    private func results(_ store: SearchStore) -> some View {
        ScrollView {
            SearchResultsList(store: store, showsSuggestions: false)
        }
        // Opaque, so the browsing behind it is covered rather than shining
        // through the gaps between rows.
        .background(F33Color.bg)
        .contentMargins(.top, chromeHeight, for: .scrollContent)
        .contentMargins(.top, chromeHeight, for: .scrollIndicators)
        // The keyboard goes as soon as the reader starts reading answers, and
        // comes back on a tap in the field, which is still on screen.
        .scrollDismissesKeyboard(.interactively)
        .animation(F33Motion.easeOut, value: store.phase)
    }

    // MARK: Chrome

    /// The title, the field, and the lanes when there is browsing to select.
    ///
    /// One glass slab of three rows, stacked and never side by side: the lane
    /// strip and a field that both want the width would leave the field a few
    /// points wide, which is the bug the home strip already wore once.
    private func chrome(
        query: Binding<String>,
        isSearching: Bool,
        onSubmit: @escaping () -> Void
    ) -> some View {
        VStack(spacing: 0) {
            SurfaceHeaderRow(title: "Explore", closesBar: false)

            ExploreSearchField(
                query: query,
                isFocused: $isFieldFocused,
                // The hairline belongs to whichever bar is last, or the stack
                // reads as separate slabs.
                closesBar: isSearching,
                onSubmit: onSubmit
            )

            // The lanes choose between two things to browse. While there is a
            // query they choose between two things that are not on screen, so
            // they leave with the browsing they belong to and come back with it.
            if !isSearching {
                ExploreLaneStrip(lane: $lane)
            }
        }
    }

    // MARK: Announcements

    private func announce(_ phase: SearchStore.Phase, store: SearchStore) {
        let message: String
        switch phase {
        case .results:
            let found = store.results
            let count = found.works.count + found.people.count + found.tags.count
            message = count == 1 ? "1 result for \(store.query)" : "\(count) results for \(store.query)"
        case .empty:
            message = "Nothing for \(store.query)"
        case .idle, .searching, .failed:
            return
        }
        AccessibilityNotification.Announcement(message).post()
    }

    private var emptyState: EmptyStateView {
        switch lane {
        case .forYou:
            return EmptyStateView(
                icon: "sparkles",
                title: "Nothing to explore yet",
                message: "Works from across F33D3R land here. As people post, this fills up."
            )
        case .trending:
            return EmptyStateView(
                icon: "chart.line.uptrend.xyaxis",
                title: "Nothing is moving yet",
                message: "Works picking up replies, reposts and tips show up here first."
            )
        }
    }
}

/// The field Explore searches from.
///
/// It sits in the chrome rather than behind a button, because a search that
/// costs a screen to reach is a search nobody runs. The row is glass like the
/// bars above and below it; the well on it is not, because a blur behind text
/// somebody is in the middle of typing costs more legibility than it buys look
/// — which is what `f33Field` already says.
struct ExploreSearchField: View {
    @Binding var query: String
    var isFocused: FocusState<Bool>.Binding
    /// Whether this row closes the chrome with a hairline. It does while the
    /// lanes are away.
    let closesBar: Bool
    let onSubmit: () -> Void

    var body: some View {
        HStack(spacing: F33Spacing.sm) {
            Image(systemName: "magnifyingglass")
                .font(.system(size: 15, weight: .semibold))
                .foregroundStyle(F33Color.ink4)
                // The field's own label says what this is; a glyph that repeats
                // it is a stop on the way to the text.
                .accessibilityHidden(true)

            TextField("Works, people, #tags", text: $query)
                .font(.system(size: 16))
                .foregroundStyle(F33Color.ink)
                .focused(isFocused)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
                .submitLabel(.search)
                .onSubmit(onSubmit)
                .accessibilityLabel("Search works, people and tags")

            if !query.isEmpty {
                Button {
                    query = ""
                    // The keyboard stays: clearing is nearly always the start
                    // of a different query rather than the end of searching.
                    isFocused.wrappedValue = true
                } label: {
                    Image(systemName: "xmark.circle.fill")
                        .font(.system(size: 16, weight: .semibold))
                        .foregroundStyle(F33Color.ink4)
                        // The glyph is 16pt and the target is the whole 44: a
                        // view is hit-tested where it draws, so a button sized
                        // by its label is a button the size of its label.
                        .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
                        .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .accessibilityLabel("Clear the search")
            }
        }
        .f33Field()
        // A tap anywhere in the well starts typing. The text field only draws
        // where its text is, and the well behind it is a background, which adds
        // no hit region at all.
        .contentShape(RoundedRectangle(cornerRadius: F33Radius.md))
        .onTapGesture { isFocused.wrappedValue = true }
        .padding(.horizontal, F33Spacing.lg)
        .padding(.bottom, F33Spacing.sm)
        .f33GlassBar(bottomHairline: closesBar)
        .accessibilityElement(children: .contain)
    }
}

/// The Explore lane selector.
///
/// Two lanes, so a segmented pair rather than the home strip's scroller: five
/// lanes need to scroll and two do not, and a two-item scroll view is a control
/// that moves under the thumb for no reason. Both carry an icon *and* a word,
/// which is the rule the tab bar follows for the same reason — a glyph seen
/// fifty times a day is still guessed at.
struct ExploreLaneStrip: View {
    @Binding var lane: ExploreLane

    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        HStack(spacing: F33Spacing.sm) {
            ForEach(ExploreLane.allCases) { option in
                laneButton(option)
            }
            Spacer(minLength: 0)
        }
        .padding(.horizontal, F33Card.paddingHorizontal)
        .padding(.vertical, F33Spacing.sm)
        // The same bar treatment the home strip carries, hairline included, so
        // the two selectors are one thing in two places rather than two things.
        .f33GlassBar(bottomHairline: true)
    }

    private func laneButton(_ option: ExploreLane) -> some View {
        let isActive = option == lane

        return Button {
            withAnimation(reduceMotion ? nil : F33Motion.easeOut) { lane = option }
        } label: {
            HStack(spacing: 6) {
                Image(systemName: icon(option))
                    .font(.system(size: 12, weight: .semibold))
                Text(option.title)
                    .font(.system(size: 14, weight: isActive ? .semibold : .regular))
                    // The label wraps rather than truncates at accessibility
                    // sizes: two chips can afford the height, and "Trend…" is a
                    // word nobody can act on.
                    .fixedSize(horizontal: false, vertical: true)
            }
            .foregroundStyle(isActive ? F33Color.accentInk : F33Color.ink2)
            .padding(.horizontal, F33Spacing.md)
            .padding(.vertical, F33Spacing.sm)
            .background {
                if isActive {
                    Capsule().fill(F33Color.accent)
                } else {
                    Capsule().strokeBorder(F33Color.hairlineStrong, lineWidth: 1)
                }
            }
            .frame(minHeight: F33Layout.minTouchTarget)
            .contentShape(Capsule())
        }
        .buttonStyle(.plain)
        .accessibilityLabel(option.title)
        .accessibilityAddTraits(isActive ? [.isSelected, .isButton] : .isButton)
    }

    private func icon(_ option: ExploreLane) -> String {
        switch option {
        case .forYou: return "sparkles"
        case .trending: return "chart.line.uptrend.xyaxis"
        }
    }
}

/// The standing note at the top of a ranked surface.
///
/// It says two true things and stops: F33D3R chose these, and there is
/// something the reader can do about a choice they disagree with. It does not
/// describe *how* the choosing works — the app has no view into that, and a
/// sentence written here about ranking would be the client inventing an account
/// of a decision it did not make. The per-work account is the server's `why:`
/// chip, which appears on a card the moment the server sends one.
struct RankedSurfaceNote: View {
    var body: some View {
        HStack(alignment: .top, spacing: F33Spacing.sm) {
            Image(systemName: "sparkles")
                .font(.system(size: 11, weight: .semibold))
                .foregroundStyle(F33Color.accent)
                // Nudged onto the first line's baseline; the icon is a bullet
                // for the sentence, not a column of its own.
                .padding(.top, 1)

            Text("F33D3R chooses what appears here. Press and hold a work to say it isn't for you.")
                .font(.system(size: 12))
                .foregroundStyle(F33Color.ink4)
                .fixedSize(horizontal: false, vertical: true)

            Spacer(minLength: 0)
        }
        .padding(.horizontal, F33Card.paddingHorizontal)
        .padding(.bottom, F33Spacing.sm)
        .background(F33Color.bg)
        .overlay(alignment: .bottom) { CardDivider() }
        .accessibilityElement(children: .combine)
    }
}

#Preview("Explore") {
    NavigationStack {
        ExploreView()
    }
    .environment(AppModel())
}


/// Confirms the signal went, and says what it was and was not.
struct NotInterestedSheet: View {
    let handle: String

    @Environment(\.dismiss) private var dismiss

    var body: some View {
        VStack(spacing: F33Spacing.lg) {
            Capsule()
                .fill(F33Color.ink5)
                .frame(width: 36, height: 5)
                .padding(.top, F33Spacing.sm)

            Image(systemName: "hand.thumbsdown")
                .font(.system(size: 32))
                .foregroundStyle(F33Color.accent)

            Text("Noted")
                .font(.title3.weight(.semibold))
                .foregroundStyle(F33Color.ink)

            Text("F33D3R will show you less like this. Nothing was sent about @\(handle), and the author sees nothing.")
                .font(.callout)
                .foregroundStyle(F33Color.ink3)
                .multilineTextAlignment(.center)
                .fixedSize(horizontal: false, vertical: true)

            Spacer(minLength: 0)

            Button("Done") { dismiss() }
                .buttonStyle(F33PrimaryButtonStyle())
        }
        .padding(F33Spacing.xl)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .f33GlassSheet()
        .presentationDetents([.height(320)])
        .presentationDragIndicator(.hidden)
    }
}
