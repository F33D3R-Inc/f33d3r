import SwiftUI
import F33D3RKit

/// Everyone this account has silenced, and the way back.
///
/// Two sections rather than one list with a badge: blocking severs the
/// relationship in both directions and muting only hides what somebody writes,
/// and undoing one is not undoing the other. Swipe to undo, then re-read from
/// the server — the row leaves because the list came back without it, not
/// because the finger said so.
struct BlockedMutedView: View {
    @Environment(AppModel.self) private var model

    @State private var list: BlockList?
    @State private var error: APIError?
    @State private var busy: Set<String> = []
    @State private var actionError: String?

    var body: some View {
        List {
            if let list {
                if list.isEmpty {
                    Section {
                        EmptyStateView(
                            icon: "hand.raised",
                            title: "Nobody's silenced",
                            message: "Block someone to cut the relationship both ways, or mute them to keep it and stop seeing what they write."
                        )
                        .listRowBackground(Color.clear)
                        .listRowSeparator(.hidden)
                    }
                } else {
                    section(
                        "Blocked",
                        footer: "They can't see your works or reach you, and you can't see theirs. Swipe to unblock.",
                        users: list.blocked,
                        undo: "Unblock"
                    ) { handle in
                        try await model.setBlocked(false, handle: handle)
                    }

                    section(
                        "Muted",
                        footer: "Still following each other; you just don't see what they write. Swipe to unmute.",
                        users: list.muted,
                        undo: "Unmute"
                    ) { handle in
                        try await model.setMuted(false, handle: handle)
                    }
                }
            } else if let error {
                Section {
                    ErrorStateView(error: error) {
                        Task { await load() }
                    }
                    .listRowBackground(Color.clear)
                    .listRowSeparator(.hidden)
                }
            } else {
                Section {
                    HStack(spacing: F33Spacing.md) {
                        ProgressView()
                        Text("Loading…")
                            .foregroundStyle(F33Color.ink4)
                    }
                }
            }
        }
        .listStyle(.insetGrouped)
        .scrollContentBackground(.hidden)
        .background(F33Color.bg)
        .hidesComposeFAB()
        .navigationTitle("Blocked & muted")
        .navigationBarTitleDisplayMode(.inline)
        .tint(F33Color.accent)
        .alert("That didn't go through", isPresented: Binding(get: { actionError != nil }, set: { if !$0 { actionError = nil } })) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(actionError ?? "")
        }
        .task { await load() }
    }

    @ViewBuilder
    private func section(
        _ title: String,
        footer: String,
        users: [User],
        undo: String,
        _ work: @escaping (String) async throws -> Void
    ) -> some View {
        Section {
            if users.isEmpty {
                Text("Nobody.")
                    .font(.subheadline)
                    .foregroundStyle(F33Color.ink4)
            } else {
                ForEach(users) { user in
                    row(user)
                        .swipeActions(edge: .trailing, allowsFullSwipe: false) {
                            Button(role: .destructive) {
                                Task { await self.undo(user.handle, work) }
                            } label: {
                                Label(undo, systemImage: "arrow.uturn.backward")
                            }
                        }
                }
            }
        } header: {
            Text(title)
        } footer: {
            Text(footer)
        }
    }

    private func row(_ user: User) -> some View {
        HStack(spacing: F33Spacing.md) {
            F33Avatar(user: user, size: 36)
            VStack(alignment: .leading, spacing: 1) {
                Text(user.displayName.isEmpty ? user.handle : user.displayName)
                    .font(.system(size: 15, weight: .medium))
                    .foregroundStyle(F33Color.ink)
                    .lineLimit(1)
                Text("@\(user.handle)")
                    .font(.system(size: 13))
                    .foregroundStyle(F33Color.ink4)
                    .lineLimit(1)
            }
            Spacer(minLength: 0)
            if busy.contains(user.handle) {
                ProgressView()
            }
        }
        .frame(minHeight: F33Layout.minTouchTarget)
    }

    private func load() async {
        do {
            list = try await model.blocks()
            error = nil
        } catch let error as APIError {
            // A deployment without the route is an empty list, not a failure:
            // there is nothing to show and nothing broken.
            if error.isUnservedSurface {
                list = BlockList()
                self.error = nil
            } else {
                self.error = error
            }
        } catch {
            self.error = .transport(error.localizedDescription)
        }
    }

    private func undo(_ handle: String, _ work: @escaping (String) async throws -> Void) async {
        busy.insert(handle)
        defer { busy.remove(handle) }
        do {
            try await work(handle)
            await load()
        } catch let error as MalkuthError {
            actionError = error.description
        } catch let error as APIError {
            actionError = error.userMessage
        } catch {
            actionError = error.localizedDescription
        }
    }
}
