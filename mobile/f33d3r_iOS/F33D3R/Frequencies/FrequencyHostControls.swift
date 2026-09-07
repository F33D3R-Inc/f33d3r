import SwiftUI
import F33D3RKit

/// What a host or co-host can do to a room, in one sheet.
///
/// Every control here is gated twice, and both gates are the server's. The
/// sheet only exists when `viewer.canModerate`; End only exists when
/// `viewer.canEnd`; and an action aimed at a particular person is only offered
/// when that person is outranked — a co-host does not get a Remove button
/// pointed at the host. The server refuses all of it anyway, and when it does
/// its sentence is what appears at the top of the sheet: this is a courtesy so
/// the reader is not offered doors that do not open, not a permission system.
///
/// Chrome, so it takes glass.
struct FrequencyHostControls: View {
    let store: FrequencyRoomStore

    @Environment(\.dismiss) private var dismiss
    @State private var busy: String?
    @State private var isConfirmingEnd = false

    var body: some View {
        NavigationStack {
            List {
                if let message = FrequencyErrorCopy.sentence(store.lastError) {
                    Section {
                        FrequencyErrorLine(message: message)
                    }
                }

                requestsSection
                roomSection
                participantsSection
                endSection
            }
            .listStyle(.insetGrouped)
            .scrollContentBackground(.hidden)
            .navigationTitle("Host controls")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("Done") { dismiss() }
                        .foregroundStyle(F33Color.accent)
                }
            }
        }
        .presentationDetents([.large])
        .presentationDragIndicator(.visible)
        .f33GlassSheet()
    }

    // MARK: - Hands up

    @ViewBuilder
    private var requestsSection: some View {
        Section {
            if store.requests.isEmpty {
                Text("Nobody has their hand up.")
                    .font(.system(size: 13))
                    .foregroundStyle(F33Color.ink4)
            } else {
                ForEach(store.requests) { request in
                    FrequencyRequestRow(
                        request: request,
                        busyKey: busy,
                        onApprove: { run("approve-\(request.id)") { await store.approveRequest(request.id) } },
                        onDecline: { run("decline-\(request.id)") { await store.declineRequest(request.id) } },
                        onUpvote: { run("upvote-\(request.id)") { await store.upvoteRequest(request.id) } }
                    )
                    .listRowBackground(F33Color.bgElevated)
                }
            }
        } header: {
            Text("Hands up")
        } footer: {
            Text("Approving puts someone on the stage. The stage holds \(store.frequency?.maxSpeakers ?? 10).")
        }
    }

    // MARK: - The room itself

    @ViewBuilder
    private var roomSection: some View {
        if let frequency = store.frequency {
            Section("Room") {
                toggleRow(
                    title: "Lock room",
                    subtitle: "Nobody new can tune in.",
                    isOn: frequency.locked,
                    key: "lock"
                ) { await store.setLocked(!frequency.locked) }

                toggleRow(
                    title: "Requests open",
                    subtitle: "Listeners can raise a hand.",
                    isOn: frequency.requestsOpen,
                    key: "requests"
                ) { await store.setRequestsOpen(!frequency.requestsOpen) }
            }
            .listRowBackground(F33Color.bgElevated)
        }
    }

    private func toggleRow(
        title: String,
        subtitle: String,
        isOn: Bool,
        key: String,
        action: @escaping () async -> Void
    ) -> some View {
        Button {
            run(key, action)
        } label: {
            HStack(spacing: F33Spacing.md) {
                VStack(alignment: .leading, spacing: 1) {
                    Text(title)
                        .font(.system(size: 15))
                        .foregroundStyle(F33Color.ink)
                    Text(subtitle)
                        .font(.system(size: 11))
                        .foregroundStyle(F33Color.ink4)
                }
                Spacer(minLength: F33Spacing.sm)
                if busy == key {
                    ProgressView().controlSize(.small)
                } else {
                    // Drawn, not bound: a `Toggle` flips under the thumb and
                    // this state belongs to the server until it says otherwise.
                    Image(systemName: isOn ? "checkmark.circle.fill" : "circle")
                        .font(.system(size: 20))
                        .foregroundStyle(isOn ? F33Color.accent : F33Color.ink5)
                }
            }
            .frame(minHeight: F33Layout.minTouchTarget)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .disabled(busy != nil)
        .accessibilityAddTraits(isOn ? [.isSelected, .isButton] : .isButton)
    }

    // MARK: - People

    @ViewBuilder
    private var participantsSection: some View {
        Section {
            if store.speakers.isEmpty {
                Text("Nobody is on stage.")
                    .font(.system(size: 13))
                    .foregroundStyle(F33Color.ink4)
            } else {
                ForEach(store.speakers) { speaker in
                    participantRow(speaker)
                        .listRowBackground(F33Color.bgElevated)
                }
            }
        } header: {
            Text("On stage")
        } footer: {
            Text("A co-host can moderate everyone except the host.")
        }
    }

    private func participantRow(_ speaker: FrequencyParticipant) -> some View {
        HStack(spacing: F33Spacing.sm) {
            F33Avatar(author: speaker.author, size: 32)
                .opacity(speaker.present ? 1 : 0.45)

            VStack(alignment: .leading, spacing: 1) {
                Text(speaker.author.displayName.isEmpty ? "@\(speaker.author.handle)" : speaker.author.displayName)
                    .font(.system(size: 14, weight: .semibold))
                    .foregroundStyle(F33Color.ink)
                    .lineLimit(1)
                Text(subtitle(speaker))
                    .font(.system(size: 11))
                    .foregroundStyle(F33Color.ink4)
            }

            Spacer(minLength: F33Spacing.xs)

            if busy?.hasSuffix(speaker.author.handle) == true {
                ProgressView().controlSize(.small)
            } else if canAct(on: speaker) {
                Menu {
                    Button(speaker.muted ? "Unmute" : "Mute") {
                        run("mute-\(speaker.author.handle)") {
                            await store.setMuted(!speaker.muted, handle: speaker.author.handle)
                        }
                    }
                    Button(speaker.isCohost ? "Remove co-host" : "Make co-host") {
                        run("cohost-\(speaker.author.handle)") {
                            await store.setCohost(!speaker.isCohost, handle: speaker.author.handle)
                        }
                    }
                    Button("Take off stage") {
                        run("demote-\(speaker.author.handle)") {
                            await store.demote(handle: speaker.author.handle)
                        }
                    }
                    Divider()
                    Button("Remove from room", role: .destructive) {
                        run("remove-\(speaker.author.handle)") {
                            await store.remove(handle: speaker.author.handle)
                        }
                    }
                    Button("Block", role: .destructive) {
                        run("block-\(speaker.author.handle)") {
                            await store.block(handle: speaker.author.handle)
                        }
                    }
                } label: {
                    Image(systemName: "ellipsis")
                        .font(.system(size: 15, weight: .semibold))
                        .foregroundStyle(F33Color.ink3)
                        .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
                        .contentShape(Rectangle())
                }
                .disabled(busy != nil)
                .accessibilityLabel("Moderate @\(speaker.author.handle)")
            }
        }
        .frame(minHeight: F33Layout.minTouchTarget)
    }

    private func subtitle(_ speaker: FrequencyParticipant) -> String {
        var parts = [speaker.roleLabel]
        if speaker.muted { parts.append("muted") }
        if !speaker.present { parts.append("away") }
        return parts.joined(separator: " · ")
    }

    /// Who this moderator outranks. Nobody moderates the host; only the host
    /// moderates a co-host; and nobody moderates themselves from here.
    private func canAct(on speaker: FrequencyParticipant) -> Bool {
        guard store.viewer?.canModerate == true else { return false }
        if speaker.isHost { return false }
        if speaker.isCohost, store.viewer?.roleValue != .host { return false }
        return true
    }

    // MARK: - End

    @ViewBuilder
    private var endSection: some View {
        if store.viewer?.canEnd == true, let frequency = store.frequency {
            Section {
                Button(role: .destructive) {
                    isConfirmingEnd = true
                } label: {
                    HStack {
                        Text(frequency.isScheduled ? "Call it off" : "End frequency")
                            .font(.system(size: 15, weight: .semibold))
                        Spacer()
                        if busy == "end" { ProgressView().controlSize(.small) }
                    }
                    .frame(minHeight: F33Layout.minTouchTarget)
                }
                .disabled(busy != nil)
                .listRowBackground(F33Color.bgElevated)
                .confirmationDialog(
                    frequency.isScheduled ? "Call off this frequency?" : "End this frequency?",
                    isPresented: $isConfirmingEnd,
                    titleVisibility: .visible
                ) {
                    Button(frequency.isScheduled ? "Call it off" : "End it", role: .destructive) {
                        run("end") {
                            if frequency.isScheduled {
                                await store.cancel()
                            } else {
                                await store.end()
                            }
                            dismiss()
                        }
                    }
                    Button("Keep going", role: .cancel) {}
                } message: {
                    Text("Everyone in the room is dropped. This can't be undone.")
                }
            } footer: {
                Text(frequency.recordingEnabled
                     ? "A replay will be prepared after it ends."
                     : "Nothing is being recorded, so there will be no replay.")
            }
        }
    }

    private func run(_ key: String, _ action: @escaping () async -> Void) {
        guard busy == nil else { return }
        busy = key
        Task {
            await action()
            busy = nil
        }
    }
}
