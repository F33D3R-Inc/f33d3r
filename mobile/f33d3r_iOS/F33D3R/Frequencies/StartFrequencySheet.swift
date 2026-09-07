import SwiftUI
import F33D3RKit

/// Opening a frequency: what it's called, who's allowed in, and when.
///
/// Two outcomes from one form, chosen by the last control on it. **Start now**
/// creates the room and puts it on air in the same breath — `frequency_create`
/// then `frequency_start`, because the server owns the transition and a client
/// that invented a "live" room would be lying to everyone reading the lane.
/// **Schedule** creates it with a time on it and leaves it alone; it appears on
/// Home as a dimmed pill with a clock until the host comes back to start it.
///
/// Chrome, so it takes glass. Nothing is optimistic: the button spins until the
/// server has answered, and the server's own sentence appears above it when the
/// answer is no.
struct StartFrequencySheet: View {
    /// Handed the id of a room that went on air, so the caller can open it.
    let onStarted: (String) -> Void
    /// Called when a room was scheduled rather than started, so the lanes can
    /// be re-read and the pill appears.
    var onScheduled: () -> Void = {}

    @Environment(AppModel.self) private var model
    @Environment(FrequencySession.self) private var session
    @Environment(\.dismiss) private var dismiss

    @State private var draft = FrequencyDraft()
    @State private var isScheduling = false
    @State private var scheduledAt = Date().addingTimeInterval(3600)
    @State private var isWorking = false
    @State private var failure: String?
    @FocusState private var isTitleFocused: Bool

    private static let visibilities: [(id: String, label: String, note: String)] = [
        ("public", "Anyone", "Everyone on F33D3R can tune in."),
        ("followers", "Followers", "Only people who follow you."),
        ("subscribers", "Subscribers", "Only your paying subscribers."),
        ("private", "Invite only", "Nobody unless you bring them in."),
    ]

    private var titleCount: Int { draft.title.count }
    private var canOpen: Bool { draft.isValid && !isWorking && draft.description.count <= 600 }

    var body: some View {
        NavigationStack {
            Form {
                titleSection
                audienceSection
                stageSection
                whenSection

                if let failure {
                    Section {
                        FrequencyErrorLine(message: failure)
                    }
                    .listRowBackground(Color.clear)
                }

            }
            .scrollContentBackground(.hidden)
            // Out of the form and pinned, so the one control that commits the
            // sheet is never the thing the keyboard or the home indicator eats.
            .safeAreaInset(edge: .bottom, spacing: 0) {
                Button {
                    Task { await open() }
                } label: {
                    Group {
                        if isWorking {
                            ProgressView().tint(F33Color.accentInk)
                        } else {
                            Text(isScheduling ? "Schedule it" : "Go on air")
                        }
                    }
                    .frame(maxWidth: .infinity)
                }
                .buttonStyle(F33PrimaryButtonStyle())
                .disabled(!canOpen)
                .padding(.horizontal, F33Spacing.lg)
                .padding(.top, F33Spacing.sm)
                .padding(.bottom, F33Spacing.md)
                .background(.clear)
            }
            .navigationTitle("Start a Frequency")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                        .foregroundStyle(F33Color.ink2)
                }
            }
        }
        .presentationDetents([.large])
        .presentationDragIndicator(.visible)
        .f33GlassSheet()
        .onAppear { isTitleFocused = true }
    }

    // MARK: - Sections

    private var titleSection: some View {
        Section {
            TextField("What's it about?", text: $draft.title, axis: .vertical)
                .font(.system(size: 16, weight: .semibold))
                .focused($isTitleFocused)
                .lineLimit(1...3)
                .onChange(of: draft.title) { _, new in
                    if new.count > 120 { draft.title = String(new.prefix(120)) }
                }

            TextField("A line or two about it (optional)", text: $draft.description, axis: .vertical)
                .font(.system(size: 14))
                .lineLimit(2...6)
                .onChange(of: draft.description) { _, new in
                    if new.count > 600 { draft.description = String(new.prefix(600)) }
                }
        } header: {
            Text("Title")
        } footer: {
            Text("\(titleCount)/120 · a title is the only thing people see before they decide to come in.")
                .monospacedDigit()
        }
        .listRowBackground(F33Color.bgElevated)
    }

    private var audienceSection: some View {
        Section {
            Picker("Who can tune in", selection: $draft.visibility) {
                ForEach(Self.visibilities, id: \.id) { option in
                    Text(option.label).tag(option.id)
                }
            }
            .pickerStyle(.menu)
            .tint(F33Color.accent)

            if model.state.user?.isAdult == true {
                Toggle("18+", isOn: $draft.isNSFW)
                    .tint(F33Color.danger)
            }
        } header: {
            Text("Audience")
        } footer: {
            Text(Self.visibilities.first { $0.id == draft.visibility }?.note ?? "")
        }
        .listRowBackground(F33Color.bgElevated)
    }

    private var stageSection: some View {
        Section {
            Stepper(value: $draft.maxSpeakers, in: 1...50) {
                HStack {
                    Text("Speakers")
                    Spacer()
                    Text("\(draft.maxSpeakers)")
                        .foregroundStyle(F33Color.ink3)
                        .monospacedDigit()
                }
            }

            Toggle("Record it", isOn: $draft.recordingEnabled)
                .tint(F33Color.accent)
        } header: {
            Text("Stage")
        } footer: {
            Text(draft.recordingEnabled
                 ? "A replay is prepared when the room ends. Recording can't be changed once you're on air."
                 : "No replay. Nothing is kept once the room ends.")
        }
        .listRowBackground(F33Color.bgElevated)
    }

    private var whenSection: some View {
        Section {
            Picker("When", selection: $isScheduling) {
                Text("Now").tag(false)
                Text("Later").tag(true)
            }
            .pickerStyle(.segmented)

            if isScheduling {
                DatePicker(
                    "Starts",
                    selection: $scheduledAt,
                    in: Date().addingTimeInterval(300)...,
                    displayedComponents: [.date, .hourAndMinute]
                )
                .tint(F33Color.accent)
            }
        } header: {
            Text("When")
        } footer: {
            Text(isScheduling
                 ? "It shows on Home as a scheduled pill until you start it."
                 : "There's no audio transport yet — the room, the stage and the queue work, and the server says so on the way in.")
        }
        .listRowBackground(F33Color.bgElevated)
    }

    // MARK: - Opening

    private func open() async {
        guard canOpen else { return }
        isTitleFocused = false
        isWorking = true
        failure = nil
        defer { isWorking = false }

        var request = draft
        request.title = draft.title.trimmingCharacters(in: .whitespacesAndNewlines)
        request.scheduledAt = isScheduling ? scheduledAt : nil

        do {
            let created = try await model.client.frequencyCreate(request)
            if isScheduling {
                if let lanes = session.lanes { await lanes.refresh() }
                onScheduled()
                dismiss()
            } else {
                // The server owns the transition to live. A room that answered
                // `frequency_create` is a draft until `frequency_start` says
                // otherwise, and the id is the only thing carried across.
                _ = try await model.client.frequencyStart(id: created.id)
                if let lanes = session.lanes { await lanes.refresh() }
                onStarted(created.id)
                dismiss()
            }
        } catch let error as MalkuthError {
            failure = FrequencyErrorCopy.sentence(error.description)
        } catch let error as APIError {
            failure = FrequencyErrorCopy.sentence(error.code) ?? error.userMessage
        } catch {
            failure = error.localizedDescription
        }
    }
}
