import SwiftUI
import F33D3RKit

/// One audio room.
///
/// Everything on this screen is the server's answer. The stage, the badges, the
/// muted glyphs, the queue and — the part that matters most — which buttons
/// exist at all come from ``FrequencyViewer``'s capability flags, never from a
/// role the client worked out for itself. The server is the only thing that
/// knows about blocks, verity tiers, a full stage and a locked room.
///
/// Nothing is optimistic. A tapped control goes busy and stays busy until the
/// room comes back, exactly as a work's action bar does. A raised hand that
/// appeared before the server agreed would be a hand that vanishes a beat
/// later, and in a room where a moderator is watching the same queue that is
/// not a cosmetic problem.
///
/// The stream is connected on appear and dropped on disappear **only if the
/// reader has not tuned in** — a listener who walks away keeps the room alive
/// for the mini bar, which is the whole reason ``FrequencySession`` holds the
/// store rather than this view.
struct FrequencyRoomView: View {
    let frequencyID: String

    @Environment(FrequencySession.self) private var session
    @Environment(AppModel.self) private var model

    @State private var isDescriptionExpanded = false
    @State private var isShowingControls = false
    @State private var busy: String?

    var body: some View {
        Group {
            if let store = session.room(frequencyID) {
                content(store)
            } else {
                ProgressView()
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        }
        .background(F33Color.bg)
        .navigationTitle("Frequency")
        .navigationBarTitleDisplayMode(.inline)
        .hidesComposeFAB()
    }

    @ViewBuilder
    private func content(_ store: FrequencyRoomStore) -> some View {
        Group {
            switch store.phase {
            case .idle, .loading:
                ProgressView()
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            case .failed(let message):
                FrequencyFailureView(message: FrequencyErrorCopy.sentence(message) ?? message) {
                    Task { await store.reload() }
                }
            case .ended(let reason):
                if let frequency = store.frequency {
                    FrequencyEndedView(frequency: frequency, reason: reason)
                } else {
                    FrequencyFailureView(message: "This frequency is over.") {
                        Task { await store.reload() }
                    }
                }
            case .live:
                live(store)
            }
        }
        .task {
            session.roomAppeared(frequencyID)
            await store.connect()
            session.sync(store)
        }
        .onDisappear {
            session.roomDisappeared(frequencyID)
            // A listener who walks away is still listening: the mini bar draws
            // from this store and the heartbeat keeps their presence alive.
            if store.viewer?.isJoined != true {
                store.disconnect()
                session.releaseIfIdle(frequencyID)
            }
        }
        .onChange(of: store.room) { _, _ in
            session.sync(store)
        }
    }

    @ViewBuilder
    private func live(_ store: FrequencyRoomStore) -> some View {
        if let frequency = store.frequency {
            ScrollView {
                VStack(alignment: .leading, spacing: F33Spacing.lg) {
                    header(frequency)

                    if let description = frequency.description, !description.isEmpty {
                        descriptionBlock(description)
                    }

                    if let note = frequency.audioUnavailable, !note.isEmpty {
                        FrequencyAudioNotice(message: note)
                    }

                    stage(store, frequency: frequency)

                    if store.viewer?.canModerate == true, !store.requests.isEmpty {
                        queue(store)
                    }
                }
                .padding(.horizontal, F33Card.paddingHorizontal)
                .padding(.top, F33Spacing.md)
                .padding(.bottom, F33Spacing.xl)
            }
            .refreshable { await store.reload() }
            .safeAreaInset(edge: .bottom, spacing: 0) {
                actionRow(store, frequency: frequency)
            }
            .sheet(isPresented: $isShowingControls) {
                FrequencyHostControls(store: store)
            }
            .onAppear {
                #if DEBUG
                if FrequencyDebug.opensHostControls, store.viewer?.canModerate == true {
                    isShowingControls = true
                }
                #endif
            }
        }
    }

    // MARK: - Head

    private func header(_ frequency: Frequency) -> some View {
        VStack(alignment: .leading, spacing: F33Spacing.sm) {
            HStack(spacing: F33Spacing.sm) {
                FrequencyStateChip(frequency: frequency)
                if frequency.isNSFW { ContentLabel(kind: .nsfw) }
                if frequency.locked {
                    Label("Locked", systemImage: "lock.fill")
                        .font(.system(size: 11, weight: .semibold))
                        .foregroundStyle(F33Color.ink3)
                }
                Spacer(minLength: 0)
                if frequency.isLive {
                    FrequencyAudioGlyph(tint: F33Color.accent, size: 16)
                }
            }

            Text(frequency.title.isEmpty ? "Untitled frequency" : frequency.title)
                .font(.system(size: 22, weight: .bold))
                .foregroundStyle(F33Color.ink)
                .fixedSize(horizontal: false, vertical: true)

            NavigationLink(value: Route.profile(handle: frequency.host.handle)) {
                HStack(spacing: F33Spacing.sm) {
                    F33Avatar(author: frequency.host, size: 28)
                    VStack(alignment: .leading, spacing: 0) {
                        HStack(spacing: 5) {
                            Text(frequency.host.displayName.isEmpty ? "@\(frequency.host.handle)" : frequency.host.displayName)
                                .font(.system(size: 14, weight: .semibold))
                                .foregroundStyle(F33Color.ink)
                            if let badge = frequency.host.badge { BadgePill(badge: badge) }
                        }
                        Text("Host · @\(frequency.host.handle)")
                            .font(.system(size: 11))
                            .foregroundStyle(F33Color.ink4)
                    }
                    Spacer(minLength: 0)
                }
                .frame(minHeight: F33Layout.minTouchTarget, alignment: .leading)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)

            Text(countsLine(frequency))
                .font(.system(size: 12).monospacedDigit())
                .foregroundStyle(F33Color.ink3)
        }
    }

    private func countsLine(_ frequency: Frequency) -> String {
        var parts = [frequency.listenerLabel]
        if frequency.speakerCount > 0 {
            parts.append("\(Counts.exact(frequency.speakerCount)) on stage")
        }
        if let started = frequency.startedAt, frequency.isLive {
            parts.append("on air \(RelativeTime.compactLabel(for: started))")
        } else if let at = frequency.scheduledAt, frequency.isScheduled {
            parts.append("starts \(RelativeTime.fullLabel(for: at))")
        }
        return parts.joined(separator: " · ")
    }

    /// The description, two lines until asked. A room's description is where
    /// the host says what the rules are, so it has to be reachable — and it is
    /// also often four paragraphs, which is not what somebody deciding whether
    /// to stay wants above the stage.
    private func descriptionBlock(_ description: String) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(description)
                .font(.system(size: 14))
                .foregroundStyle(F33Color.ink2)
                .lineLimit(isDescriptionExpanded ? nil : 2)
                .fixedSize(horizontal: false, vertical: true)

            // Only when there is plausibly a third line to reveal. A "More"
            // that expands nothing is worse than no "More" at all.
            if description.count > 110 {
                Button(isDescriptionExpanded ? "Less" : "More") {
                    withAnimation(F33Motion.easeOut) { isDescriptionExpanded.toggle() }
                }
                .font(.system(size: 13, weight: .semibold))
                .foregroundStyle(F33Color.accent)
                .frame(minHeight: 32, alignment: .leading)
            }
        }
    }

    // MARK: - Stage

    private func stage(_ store: FrequencyRoomStore, frequency: Frequency) -> some View {
        VStack(alignment: .leading, spacing: F33Spacing.md) {
            Text("On stage")
                .font(.system(size: 13, weight: .semibold))
                .foregroundStyle(F33Color.ink3)

            if store.speakers.isEmpty {
                Text("Nobody is on stage yet.")
                    .font(.system(size: 13))
                    .foregroundStyle(F33Color.ink4)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(.vertical, F33Spacing.lg)
            } else {
                LazyVGrid(
                    columns: Array(repeating: GridItem(.flexible(), spacing: F33Spacing.md), count: 3),
                    alignment: .leading,
                    spacing: F33Spacing.lg
                ) {
                    ForEach(store.speakers) { speaker in
                        FrequencySpeakerCell(speaker: speaker)
                    }
                }
            }
        }
    }

    // MARK: - Queue

    private func queue(_ store: FrequencyRoomStore) -> some View {
        VStack(alignment: .leading, spacing: F33Spacing.sm) {
            Text("Hands up · \(store.requests.count)")
                .font(.system(size: 13, weight: .semibold))
                .foregroundStyle(F33Color.ink3)

            ForEach(store.requests) { request in
                FrequencyRequestRow(
                    request: request,
                    busyKey: busy,
                    onApprove: { run("approve-\(request.id)") { await store.approveRequest(request.id) } },
                    onDecline: { run("decline-\(request.id)") { await store.declineRequest(request.id) } },
                    onUpvote: { run("upvote-\(request.id)") { await store.upvoteRequest(request.id) } }
                )
            }
        }
    }

    // MARK: - Actions

    private func actionRow(_ store: FrequencyRoomStore, frequency: Frequency) -> some View {
        let viewer = store.viewer

        return VStack(spacing: F33Spacing.sm) {
            if let message = FrequencyErrorCopy.sentence(store.lastError) {
                FrequencyErrorLine(message: message)
            }

            HStack(spacing: F33Spacing.sm) {
                if viewer?.isJoined != true {
                    if frequency.isLive, viewer?.canListen == true {
                        primaryButton(
                            title: "Tune in",
                            systemImage: "dot.radiowaves.left.and.right",
                            key: "join"
                        ) { await store.join() }
                    } else if canOwn(viewer), !frequency.isLive {
                        // The room's owner, before it is on air. `frequency_start`
                        // is the server's transition; this only asks for it.
                        primaryButton(
                            title: "Go on air",
                            systemImage: "waveform",
                            key: "start"
                        ) { await store.start() }
                    } else {
                        Text(waitingLine(frequency, viewer: viewer))
                            .font(.system(size: 13))
                            .foregroundStyle(F33Color.ink3)
                            .frame(maxWidth: .infinity, minHeight: F33Layout.minTouchTarget, alignment: .leading)
                    }
                } else {
                    if viewer?.canSpeak == true {
                        actionButton(
                            title: viewer?.muted == true ? "Unmute" : "Mute",
                            systemImage: viewer?.muted == true ? "mic.slash.fill" : "mic.fill",
                            key: "self-mute",
                            tint: viewer?.muted == true ? F33Color.danger : F33Color.ink
                        ) {
                            await store.setMuted(!(viewer?.muted ?? false), handle: myHandle)
                        }
                    } else if let request = viewer?.request {
                        actionButton(
                            title: "Requested",
                            systemImage: "hand.raised.fill",
                            key: "withdraw",
                            tint: F33Color.accent
                        ) {
                            await store.withdrawRequest(request.id)
                        }
                    } else if viewer?.canRequestMic == true {
                        actionButton(
                            title: "Raise hand",
                            systemImage: "hand.raised",
                            key: "raise",
                            tint: F33Color.ink
                        ) {
                            await store.requestMic()
                        }
                    }

                    actionButton(
                        title: "Leave",
                        systemImage: "rectangle.portrait.and.arrow.right",
                        key: "leave",
                        tint: F33Color.danger
                    ) {
                        await store.leave()
                    }
                }

                if canOwn(viewer) {
                    Button {
                        isShowingControls = true
                    } label: {
                        Image(systemName: "ellipsis")
                            .font(.system(size: 17, weight: .semibold))
                            .foregroundStyle(F33Color.ink)
                            .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
                            .overlay(Circle().strokeBorder(F33Color.hairlineStrong, lineWidth: 1))
                            .contentShape(Circle())
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel("Host controls")
                }
            }
        }
        .padding(.horizontal, F33Card.paddingHorizontal)
        .padding(.top, F33Spacing.sm)
        .padding(.bottom, F33Spacing.sm)
        .frame(maxWidth: .infinity)
        .f33GlassBar()
    }

    private var myHandle: String {
        model.state.user?.user.handle ?? ""
    }

    /// Whether the server has told this reader they run this room. Both flags
    /// are the server's; neither is worked out from the host's handle, which is
    /// the one thing a client must never gate a control on.
    private func canOwn(_ viewer: FrequencyViewer?) -> Bool {
        viewer?.canModerate == true || viewer?.canEnd == true
    }

    /// What to say when there is nothing to press.
    private func waitingLine(_ frequency: Frequency, viewer: FrequencyViewer?) -> String {
        if viewer?.blocked == true { return "You can't join this frequency." }
        if frequency.isScheduled, let at = frequency.scheduledAt {
            return "Starts \(RelativeTime.fullLabel(for: at))."
        }
        if frequency.locked { return "The host has locked this room." }
        if !frequency.isLive { return "This frequency isn't on air." }
        return "You can't tune in to this one."
    }

    private func primaryButton(
        title: String,
        systemImage: String,
        key: String,
        enabled: Bool = true,
        action: @escaping () async -> Void
    ) -> some View {
        Button {
            run(key, action)
        } label: {
            Group {
                if busy == key {
                    ProgressView().tint(F33Color.accentInk)
                } else {
                    Label(title, systemImage: systemImage)
                }
            }
            .font(.system(size: 15, weight: .semibold))
            .foregroundStyle(F33Color.accentInk)
            .frame(maxWidth: .infinity)
            .frame(height: F33Layout.minTouchTarget)
            .background(F33Color.accent, in: Capsule())
            .opacity(enabled ? 1 : 0.45)
        }
        .buttonStyle(.plain)
        .disabled(!enabled || busy != nil)
    }

    private func actionButton(
        title: String,
        systemImage: String,
        key: String,
        tint: Color,
        action: @escaping () async -> Void
    ) -> some View {
        Button {
            run(key, action)
        } label: {
            Group {
                if busy == key {
                    ProgressView().controlSize(.small)
                } else {
                    Label(title, systemImage: systemImage)
                }
            }
            .font(.system(size: 14, weight: .semibold))
            .foregroundStyle(tint)
            .frame(maxWidth: .infinity)
            .frame(height: F33Layout.minTouchTarget)
            .overlay(Capsule().strokeBorder(tint.opacity(0.4), lineWidth: 1))
            .contentShape(Capsule())
        }
        .buttonStyle(.plain)
        .disabled(busy != nil)
    }

    /// One call at a time, and the control that fired it stays busy until the
    /// server has answered. See the note at the top of the file.
    private func run(_ key: String, _ action: @escaping () async -> Void) {
        guard busy == nil else { return }
        busy = key
        Task {
            await action()
            busy = nil
        }
    }
}

/// One face on the stage: who, what they are, and whether they can be heard.
struct FrequencySpeakerCell: View {
    let speaker: FrequencyParticipant

    var body: some View {
        VStack(spacing: 6) {
            ZStack(alignment: .bottomTrailing) {
                F33Avatar(author: speaker.author, size: 56)
                    .overlay(
                        Circle().strokeBorder(
                            speaker.isHost ? F33Color.accent : (speaker.isCohost ? F33Color.accent.opacity(0.5) : Color.clear),
                            lineWidth: 2
                        )
                    )
                if speaker.muted {
                    Image(systemName: "mic.slash.fill")
                        .font(.system(size: 9, weight: .bold))
                        .foregroundStyle(F33Color.accentInk)
                        .frame(width: 20, height: 20)
                        .background(F33Color.ink3, in: Circle())
                        .overlay(Circle().strokeBorder(F33Color.bg, lineWidth: 2))
                }
            }

            Text(speaker.author.displayName.isEmpty ? "@\(speaker.author.handle)" : speaker.author.displayName)
                .font(.system(size: 12, weight: .semibold))
                .foregroundStyle(F33Color.ink)
                .lineLimit(1)

            if speaker.isHost || speaker.isCohost {
                Text(speaker.roleLabel)
                    .font(.system(size: 9, weight: .bold))
                    .tracking(0.4)
                    .textCase(.uppercase)
                    .foregroundStyle(F33Color.accent)
                    .padding(.horizontal, 6)
                    .padding(.vertical, 2)
                    .background(F33Color.accent.opacity(0.12), in: Capsule())
            }
        }
        .frame(maxWidth: .infinity)
        // A lapsed heartbeat dims the face rather than removing it: somebody
        // whose phone slept is still in the room, and a stage that reshuffles
        // every time a connection hiccups is a stage nobody can follow.
        .opacity(speaker.present ? 1 : 0.45)
        .accessibilityElement(children: .combine)
        .accessibilityLabel(label)
    }

    private var label: String {
        var parts = ["@\(speaker.author.handle)", speaker.roleLabel]
        if speaker.muted { parts.append("muted") }
        if !speaker.present { parts.append("away") }
        return parts.joined(separator: ", ")
    }
}

/// A raised hand, as a moderator sees it.
struct FrequencyRequestRow: View {
    let request: FrequencyRequest
    let busyKey: String?
    let onApprove: () -> Void
    let onDecline: () -> Void
    let onUpvote: () -> Void

    var body: some View {
        HStack(spacing: F33Spacing.sm) {
            F33Avatar(author: request.author, size: 32)

            VStack(alignment: .leading, spacing: 1) {
                Text(request.author.displayName.isEmpty ? "@\(request.author.handle)" : request.author.displayName)
                    .font(.system(size: 13, weight: .semibold))
                    .foregroundStyle(F33Color.ink)
                    .lineLimit(1)
                Text(request.reason.isEmpty ? "@\(request.author.handle)" : request.reason)
                    .font(.system(size: 11))
                    .foregroundStyle(F33Color.ink4)
                    .lineLimit(1)
            }

            Spacer(minLength: F33Spacing.xs)

            Button(action: onUpvote) {
                HStack(spacing: 3) {
                    Image(systemName: "arrow.up")
                        .font(.system(size: 10, weight: .bold))
                    Text("\(request.upvotes)")
                        .font(.system(size: 11, weight: .semibold).monospacedDigit())
                }
                .foregroundStyle(F33Color.ink3)
                .frame(minWidth: 36, minHeight: F33Layout.minTouchTarget)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .disabled(busyKey != nil)
            .accessibilityLabel("\(request.upvotes) backing this hand")

            iconButton("checkmark", tint: F33Color.ok, key: "approve-\(request.id)", action: onApprove)
                .accessibilityLabel("Approve @\(request.author.handle)")
            iconButton("xmark", tint: F33Color.danger, key: "decline-\(request.id)", action: onDecline)
                .accessibilityLabel("Decline @\(request.author.handle)")
        }
        .padding(.vertical, 4)
    }

    private func iconButton(_ symbol: String, tint: Color, key: String, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            Group {
                if busyKey == key {
                    ProgressView().controlSize(.small)
                } else {
                    Image(systemName: symbol)
                        .font(.system(size: 13, weight: .bold))
                        .foregroundStyle(tint)
                }
            }
            .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
            .overlay(Circle().strokeBorder(tint.opacity(0.35), lineWidth: 1))
            .contentShape(Circle())
        }
        .buttonStyle(.plain)
        .disabled(busyKey != nil)
    }
}

/// What is left when a room is over.
struct FrequencyEndedView: View {
    let frequency: Frequency
    let reason: String?

    var body: some View {
        VStack(spacing: F33Spacing.md) {
            Image(systemName: "waveform.slash")
                .font(.system(size: 40))
                .foregroundStyle(F33Color.ink5)

            Text(frequency.title.isEmpty ? "Frequency ended" : frequency.title)
                .font(.system(size: 19, weight: .semibold))
                .foregroundStyle(F33Color.ink)
                .multilineTextAlignment(.center)

            Text(endedLine)
                .font(.subheadline)
                .foregroundStyle(F33Color.ink3)
                .multilineTextAlignment(.center)

            if let duration {
                Text(duration)
                    .font(.footnote.monospacedDigit())
                    .foregroundStyle(F33Color.ink4)
            }

            Text(replayLine)
                .font(.footnote)
                .foregroundStyle(F33Color.ink4)
                .multilineTextAlignment(.center)

            NavigationLink(value: Route.profile(handle: frequency.host.handle)) {
                HStack(spacing: F33Spacing.sm) {
                    F33Avatar(author: frequency.host, size: 28)
                    Text("@\(frequency.host.handle)")
                        .font(.system(size: 13, weight: .semibold))
                        .foregroundStyle(F33Color.accent)
                }
                .frame(minHeight: F33Layout.minTouchTarget)
            }
            .buttonStyle(.plain)
        }
        .padding(F33Spacing.xl)
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .center)
    }

    private var endedLine: String {
        if let reason = FrequencyErrorCopy.sentence(reason ?? frequency.endReason), !reason.isEmpty {
            return reason
        }
        switch frequency.stateValue {
        case .cancelled: return "The host called this one off."
        case .moderationTerminated: return "This frequency was stopped."
        case .failed: return "This frequency ended unexpectedly."
        default: return "This frequency has ended."
        }
    }

    private var duration: String? {
        guard let started = frequency.startedAt, let ended = frequency.endedAt else { return nil }
        let seconds = max(0, Int(ended.timeIntervalSince(started)))
        let hours = seconds / 3600
        let minutes = (seconds % 3600) / 60
        if hours > 0 { return "Ran \(hours)h \(minutes)m" }
        if minutes > 0 { return "Ran \(minutes)m" }
        return "Ran under a minute"
    }

    private var replayLine: String {
        switch frequency.replayStatus {
        case "processing": return "A replay is being prepared."
        case "ready": return "A replay is ready."
        case "failed": return "The replay could not be saved."
        default:
            return frequency.recordingEnabled
                ? "No replay was saved."
                : "The host didn't record this one."
        }
    }
}

/// A room that would not load, with the retry that asks again.
struct FrequencyFailureView: View {
    let message: String
    let retry: () -> Void

    var body: some View {
        VStack(spacing: F33Spacing.md) {
            Image(systemName: "exclamationmark.triangle")
                .font(.system(size: 34))
                .foregroundStyle(F33Color.ink5)
            Text(message)
                .font(.subheadline)
                .foregroundStyle(F33Color.ink3)
                .multilineTextAlignment(.center)
            Button("Try again", action: retry)
                .font(.subheadline.weight(.semibold))
                .foregroundStyle(F33Color.accent)
                .frame(minHeight: F33Layout.minTouchTarget)
        }
        .padding(F33Spacing.xl)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}
