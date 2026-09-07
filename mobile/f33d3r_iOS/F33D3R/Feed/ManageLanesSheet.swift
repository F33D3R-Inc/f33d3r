import SwiftUI
import F33D3RKit

/// Where the reader chooses which feeds are in their strip.
///
/// Two kinds of thing can go up there. The surfaces this platform ships —
/// Trending, Sports, Music, Visions, Live, and 18+ for an account cleared for
/// it — and topics, which are tags read as feeds. A topic is how a subject
/// nobody has shipped a lane for still becomes one: the server serves any tag
/// as a feed, so all that was missing was somewhere to say which tags.
///
/// Following and For you are not offered, because they cannot be removed.
struct ManageLanesSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    @State private var query = ""
    @State private var trending: [TagCount] = []
    @State private var found: [TagCount] = []
    @State private var isSearching = false
    @State private var searchTask: Task<Void, Never>?

    private var preferences: FeedPreferences { model.preferences }

    var body: some View {
        NavigationStack {
            List {
                inStrip
                surfacesToAdd
                topicsToAdd
                resetSection
            }
            .listStyle(.insetGrouped)
            .navigationTitle("Your feeds")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("Done") { dismiss() }
                }
            }
            .searchable(text: $query, prompt: "Find a topic, like sports")
            .onChange(of: query) { _, new in search(new) }
            .task { await loadTrending() }
        }
    }

    // MARK: What is up now

    private var inStrip: some View {
        Section {
            ForEach(preferences.lanes) { lane in
                HStack {
                    Text(lane.title)
                        .foregroundStyle(F33Color.ink)
                    Spacer()
                    if lane.isRemovable {
                        Button {
                            withAnimation { preferences.remove(lane) }
                        } label: {
                            Image(systemName: "minus.circle.fill")
                                .foregroundStyle(F33Color.ink4)
                        }
                        .buttonStyle(.plain)
                        .accessibilityLabel("Remove \(lane.title)")
                    } else {
                        // Said rather than left blank: a row with no control is
                        // otherwise indistinguishable from one that is broken.
                        Text("always on")
                            .font(.system(size: 13))
                            .foregroundStyle(F33Color.ink4)
                    }
                }
            }
            .onMove { preferences.move(fromOffsets: $0, toOffset: $1) }
        } header: {
            Text("In your strip")
        } footer: {
            Text("Drag to reorder. Following and For you stay.")
        }
        .environment(\.editMode, .constant(.active))
    }

    /// The way back. A reader who has pruned the strip down to the two that
    /// cannot leave has otherwise no way to find what used to be there, since
    /// the only record of a lane is the lane.
    private var resetSection: some View {
        Section {
            Button("Reset to the default feeds", role: .destructive) {
                withAnimation(F33Motion.easeOut) { preferences.resetLanes() }
            }
        } footer: {
            Text("Puts back every feed F33D3R ships, in their original order.")
        }
    }

    // MARK: What can be added

    private var surfacesToAdd: some View {
        // The 18+ lane is only offered to an account cleared for it. The server
        // refuses it either way; this keeps a lane the reader cannot open from
        // appearing as something they are missing out on.
        let adultAllowed = model.state.user?.contentSetting == "adult_enabled" && model.state.user?.isAdult == true
        let available = FeedSurface.allCases
            .filter { !$0.requiresAdultContent || adultAllowed }
            .map(FeedLane.surface)
            .filter { !preferences.lanes.contains($0) }

        return Group {
            if !available.isEmpty {
                Section("Feeds") {
                    ForEach(available) { lane in
                        addRow(title: lane.title, subtitle: nil) {
                            preferences.add(lane)
                        }
                    }
                }
            }
        }
    }

    private var topicsToAdd: some View {
        let typed = FeedLane.normalise(query)
        let suggestions = (query.isEmpty ? trending : found)
            .filter { tag in
                guard let normalised = FeedLane.normalise(tag.tag) else { return false }
                return !preferences.lanes.contains(.topic(normalised))
            }

        return Section {
            // What was typed, offered first, so a topic nobody has posted to
            // yet can still be followed.
            if let typed, !preferences.lanes.contains(.topic(typed)) {
                addRow(title: "#" + typed, subtitle: "Add this topic") {
                    preferences.add(.topic(typed))
                    query = ""
                    dismiss()
                }
            }
            ForEach(suggestions) { tag in
                addRow(title: "#" + tag.tag, subtitle: "\(tag.count) works") {
                    guard let normalised = FeedLane.normalise(tag.tag) else { return }
                    preferences.add(.topic(normalised))
                    dismiss()
                }
            }
            if suggestions.isEmpty, typed == nil, !isSearching {
                Text(query.isEmpty ? "Nothing trending yet." : "No topic by that name.")
                    .font(.system(size: 14))
                    .foregroundStyle(F33Color.ink4)
            }
        } header: {
            Text(query.isEmpty ? "Trending topics" : "Topics")
        } footer: {
            Text("A topic is a tag read as a feed. Add #sports and every work tagged sports lands in it.")
        }
    }

    private func addRow(title: String, subtitle: String?, add: @escaping () -> Void) -> some View {
        Button(action: { withAnimation(F33Motion.easeOut) { add() } }) {
            HStack {
                VStack(alignment: .leading, spacing: 2) {
                    Text(title).foregroundStyle(F33Color.ink)
                    if let subtitle {
                        Text(subtitle)
                            .font(.system(size: 12))
                            .foregroundStyle(F33Color.ink4)
                    }
                }
                Spacer()
                Image(systemName: "plus.circle.fill").foregroundStyle(F33Color.accent)
            }
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel("Add \(title)")
    }

    // MARK: Loading

    private func loadTrending() async {
        trending = (try? await model.client.trendingTags()) ?? []
    }

    /// Debounced: the field is searched as it is typed, and a request per
    /// keystroke would be a request per keystroke.
    private func search(_ text: String) {
        searchTask?.cancel()
        guard !text.trimmingCharacters(in: .whitespaces).isEmpty else {
            found = []
            isSearching = false
            return
        }
        isSearching = true
        searchTask = Task {
            try? await Task.sleep(for: .milliseconds(250))
            guard !Task.isCancelled else { return }
            let results = try? await model.client.search(text)
            guard !Task.isCancelled else { return }
            found = results?.tags ?? []
            isSearching = false
        }
    }
}

#Preview("Your feeds") {
    ManageLanesSheet().environment(AppModel())
}
