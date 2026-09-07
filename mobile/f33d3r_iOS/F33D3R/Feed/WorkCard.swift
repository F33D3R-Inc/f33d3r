import SwiftUI
import F33D3RKit

/// One work in a list.
///
/// The 3a card: a 44pt avatar column, a 12pt gutter, then a content column whose
/// first line is the handle and whose second is age, kind and gate. The
/// identity line leads with `@handle` rather than a display name because the
/// handle is the thing that is unique, the thing people type, and the thing that
/// stays the same when someone renames themselves.
///
/// Card and compact are the same card at two densities, resolved from the
/// environment; neither is a separate layout.
///
/// The card renders the server's row and sends events. It does not decide what
/// a work is — the gate, the lock and the tombstone are all driven by fields
/// the server already resolved — and it does not flip a value before the
/// server has: every action here goes through `AppModel`, which refetches the
/// work and hands the row back to the lists that hold it.
struct WorkCard: View {
    let work: Work
    let currentUser: CurrentUser?
    /// Detail draws the same card with a larger body and no tap target of its
    /// own, since it is already the screen you would be taken to.
    var isDetail = false

    @Environment(AppModel.self) private var model
    @Environment(\.feedDensity) private var density

    /// Revealing gated media is per-card and lasts as long as the card does.
    @State private var isMediaRevealed = false
    @State private var isShowingWhy = false
    @State private var sheet: CardSheet?
    @State private var isConfirmingDelete = false
    @State private var isBusy = false
    @State private var actionError: String?
    /// The image the reader opened full-screen.
    @State private var viewer: MediaViewerRequest?
    /// Ties each grid tile to the viewer, so an image zooms out of its tile
    /// and back into it the way Photos does.
    @Namespace private var mediaNamespace
    /// Flips when a double tap on media liked the work; drives the heart burst.
    @State private var heartBurst = 0

    private var metrics: F33Card.Metrics { F33Card.metrics(density) }

    private var isLocked: Bool {
        work.isLockedForViewer(currentHandle: currentUser?.user.handle)
    }

    private var isOwner: Bool {
        currentUser?.user.handle == work.author.handle
    }

    var body: some View {
        Group {
            if work.isBlocked {
                BlockedWorkNotice()
                    .padding(.horizontal, metrics.paddingHorizontal)
                    .padding(.vertical, F33Spacing.sm)
            } else {
                card
            }
        }
        .sheet(isPresented: $isShowingWhy) {
            if let provenance = work.visibleProvenance {
                WhyThisSheet(provenance: provenance, handle: work.author.handle)
            }
        }
        .sheet(item: $sheet) { sheet in
            switch sheet {
            case .reply:
                ComposeSheet(mode: .reply(to: work))
            case .quote:
                ComposeSheet(mode: .quote(work))
            case .tip:
                TipSheet(handle: work.author.handle, work: work)
            case .report:
                ReportSheet(work: work)
            case .edit:
                EditWorkSheet(work: work)
            }
        }
        .fullScreenCover(item: $viewer) { request in
            MediaViewerView(request: request, transitionNamespace: mediaNamespace)
        }
        // The server pushes `work_engagement` for the works this client has
        // asked to watch, and what it should be watching is what is on screen.
        // Registering here rather than in a list means every surface that draws
        // a card — feed, thread, profile — gets live counts by drawing it.
        .onAppear {
            model.noteWorkVisible(work.id)
            #if DEBUG
            // A stand-in video takes the media block, so the viewer hook opens
            // the player instead of the images it replaced.
            if isDetail, model.debugMediaViewer, !work.mediaURLs.isEmpty, viewer == nil, WorkVideoView.debugVideo == nil {
                viewer = MediaViewerRequest(urls: work.mediaURLs, index: 0, seed: work.id)
            }
            #endif
        }
        .onDisappear {
            model.noteWorkHidden(work.id)
        }
        .confirmationDialog("Delete this work?", isPresented: $isConfirmingDelete, titleVisibility: .visible) {
            Button("Delete", role: .destructive) {
                perform { try await model.deleteWork(work) }
            }
        } message: {
            Text("It leaves every feed now. Replies to it stay, pointing at a work that is gone.")
        }
        .alert("That didn't go through", isPresented: Binding(get: { actionError != nil }, set: { if !$0 { actionError = nil } })) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(actionError ?? "")
        }
    }

    private var card: some View {
        VStack(alignment: .leading, spacing: 0) {
            if let reposter = work.repostedBy {
                repostAttribution(reposter)
            }

            HStack(alignment: .top, spacing: metrics.columnGap) {
                NavigationLink(value: Route.profile(handle: work.author.handle)) {
                    F33Avatar(author: work.author, size: metrics.avatarSize)
                }
                .buttonStyle(.plain)

                VStack(alignment: .leading, spacing: 0) {
                    if work.isPinned {
                        PinnedLabel()
                    }

                    // The chip rides the identity row when the whole reason
                    // fits, and drops to its own line when it doesn't.
                    ViewThatFits(in: .horizontal) {
                        VStack(alignment: .leading, spacing: 0) {
                            identityRow(includingProvenance: true)
                            metaRow
                        }
                        VStack(alignment: .leading, spacing: 0) {
                            identityRow(includingProvenance: false)
                            metaRow
                            provenanceRow
                        }
                    }

                    if isLocked {
                        SubscriberGate(handle: work.author.handle)
                            .padding(.top, F33Spacing.sm)
                        actionBar
                    } else {
                        content
                    }
                }
            }
        }
        .padding(.top, metrics.paddingTop)
        .padding(.horizontal, metrics.paddingHorizontal)
        .padding(.bottom, metrics.paddingBottom)
        .frame(maxWidth: .infinity, alignment: .leading)
        .contentShape(Rectangle())
        .accessibilityElement(children: .contain)
        .accessibilityLabel(cardSummary)
    }

    // MARK: Header

    private func repostAttribution(_ reposter: WorkAuthor) -> some View {
        HStack(spacing: F33Card.authorRowGap) {
            Image(systemName: "arrow.2.squarepath")
                .font(.system(size: 12, weight: .semibold))
            Text("\(reposter.displayName) reposted")
                .font(.system(size: 13, weight: .semibold))
        }
        .foregroundStyle(F33Color.ink4)
        .padding(.leading, metrics.avatarSize - 10)
        .padding(.bottom, F33Spacing.xs)
        .accessibilityLabel("Reposted by \(reposter.displayName)")
    }

    private func identityRow(includingProvenance: Bool) -> some View {
        HStack(spacing: F33Card.authorRowGap) {
            NavigationLink(value: Route.profile(handle: work.author.handle)) {
                Text("@\(work.author.handle)")
                    .font(.system(size: F33Card.authorNameSize, weight: .semibold))
                    .foregroundStyle(F33Color.ink)
                    .lineLimit(1)
            }
            .buttonStyle(.plain)
            .layoutPriority(1)

            if let badge = work.author.badge {
                BadgePill(badge: badge)
            }

            Spacer(minLength: F33Spacing.xs)

            if includingProvenance, let provenance = work.visibleProvenance {
                WhyChip(reason: provenance.text) { isShowingWhy = true }
                    .fixedSize()
            }
        }
    }

    @ViewBuilder
    private var provenanceRow: some View {
        if let provenance = work.visibleProvenance {
            HStack(spacing: 0) {
                WhyChip(reason: provenance.text) { isShowingWhy = true }
                Spacer(minLength: 0)
            }
            .padding(.top, 3)
        }
    }

    private var metaRow: some View {
        HStack(spacing: 5) {
            Text(work.timeLabel)
                .accessibilityLabel(RelativeTime.fullLabel(for: work.createdAt))

            Text("·")

            Text(work.kindLabel)

            if let gate = work.gate {
                GateChip(gate: gate)
            }

            if let expiry = work.expiryLabel {
                VisionCountdown(label: expiry)
            }

            if work.commentGating == "none" {
                Image(systemName: "bubble.left.and.exclamationmark.bubble.right")
                    .font(.system(size: 10))
                    .accessibilityLabel("Replies are off")
            }

            Spacer(minLength: 0)
        }
        .font(.system(size: 11))
        .foregroundStyle(F33Color.ink4)
        .lineLimit(1)
        .padding(.bottom, F33Spacing.xs)
    }

    // MARK: Content

    @ViewBuilder
    private var content: some View {
        if !work.body.isEmpty {
            WorkBodyText(
                text: work.body,
                lineLimit: isDetail ? nil : 12,
                size: isDetail ? 17 : metrics.bodySize
            )
        }

        if work.isEdited {
            editedLabel
        }

        if !work.tags.isEmpty {
            TagRow(tags: work.tags)
        }

        media

        if let poll = work.poll {
            PollCard(poll: poll) { option in
                perform { try await model.votePoll(work: work, option: option) }
            }
        }

        if let quoted = work.quoted {
            NavigationLink(value: Route.work(id: quoted.id)) {
                QuotedWorkCard(quoted: quoted)
            }
            .buttonStyle(.plain)
        }

        if let preview = work.linkPreview {
            LinkPreviewCard(preview: preview)
        }

        // One card, for the first ticker in the body — the same one the server
        // stores against the work, so a work naming three companies shows the
        // same single quote here as it does on the web.
        if let ticker = Cashtag.first(in: work.body) {
            CashtagCard(ticker: ticker)
        }

        if let voice = work.voice {
            VoiceNoteView(voice: voice, seed: work.id)
        }

        if let credit = lineageCredit {
            credit
        }

        if !work.latestReplierHandles.isEmpty, density == .card {
            ReplyPreview(
                handles: work.latestReplierHandles,
                avatars: work.latestReplierAvatars,
                replyCount: work.replyCount
            )
        }

        actionBar
    }

    private var actionBar: some View {
        WorkActionBar(work: work, currentUser: currentUser, actions: actions)
    }

    /// Every action, bound to the model. Owner-only items are nil for everyone
    /// else so the menu hides them.
    private var actions: WorkActions {
        var a = WorkActions()
        a.isBusy = isBusy
        a.reply = { sheet = .reply }
        a.like = { perform { try await model.toggleLike(work) } }
        a.repost = { perform { try await model.toggleRepost(work) } }
        a.quote = { sheet = .quote }
        a.tip = { sheet = .tip }
        a.save = { perform { try await model.toggleBookmark(work) } }
        a.whyThis = { isShowingWhy = true }
        a.mute = { perform { try await model.setMuted(true, handle: work.author.handle) } }
        a.report = { sheet = .report }
        if isOwner {
            a.delete = { isConfirmingDelete = true }
            if work.isEditable() {
                a.edit = { sheet = .edit }
            }
            a.pin = { perform { try await model.setPinned(!work.isPinned, work: work) } }
            a.restrictReplies = { gating in perform { try await model.setReplyRestriction(gating, work: work) } }
        }
        return a
    }

    /// Runs one mutation, holding the bar while it is in flight and surfacing
    /// the server's refusal if there is one.
    private func perform(_ operation: @escaping () async throws -> Void) {
        guard !isBusy else { return }
        isBusy = true
        Task {
            defer { isBusy = false }
            do {
                try await operation()
            } catch let error as MalkuthError {
                actionError = error.description
            } catch let error as APIError {
                actionError = error.userMessage
            } catch {
                actionError = error.localizedDescription
            }
        }
    }

    @ViewBuilder
    private var media: some View {
        if work.hasMedia {
            Group {
                if work.needsContentGate {
                    ContentGate(isNSFW: work.isNSFW, isGore: work.isGore, isRevealed: $isMediaRevealed) {
                        mediaBody
                    }
                } else {
                    mediaBody
                }
            }
            .padding(.top, F33Card.mediaTopInset)
        }
    }

    @ViewBuilder
    private var mediaBody: some View {
        if let video = displayedVideo {
            videoView(video)
        } else {
            MediaGrid(
                mediaURLs: work.mediaURLs,
                seed: work.id,
                singleImageMaxHeight: metrics.singleImageMaxHeight,
                onTap: { index in
                    viewer = MediaViewerRequest(urls: work.mediaURLs, index: index, seed: work.id)
                },
                onDoubleTap: likeFromMedia,
                transitionNamespace: mediaNamespace
            )
            .overlay { HeartBurst(trigger: heartBurst) }
        }
    }

    /// The work's video — or, on a detail page in a debug run, the stand-in
    /// one from `F33D3R_VIDEO_URL`, since no work here carries a video yet.
    private var displayedVideo: WorkVideo? {
        #if DEBUG
        if isDetail, let video = WorkVideoView.debugVideo { return video }
        #endif
        return work.video
    }

    private func videoView(_ video: WorkVideo) -> WorkVideoView {
        var view = WorkVideoView(video: video, seed: work.id, portraitMaxWidth: metrics.portraitVideoMaxWidth)
        #if DEBUG
        view.debugOpensFullScreen = isDetail && model.debugMediaViewer && WorkVideoView.debugVideo != nil
        #endif
        return view
    }

    /// A double tap on the picture likes the work — and only likes: a second
    /// double tap is not an unlike, because nobody means that by it. The heart
    /// is drawn when the server has recorded the like, and the burst rides the
    /// same answer.
    private func likeFromMedia() {
        guard !work.likedByViewer, !isBusy else { return }
        perform {
            try await model.react(.like, on: work)
            heartBurst += 1
        }
    }

    private var editedLabel: some View {
        HStack(spacing: 4) {
            Image(systemName: "pencil")
                .font(.system(size: 10))
            Text(work.editedAt.map { "Edited · \(RelativeTime.label(for: $0))" } ?? "Edited")
                .font(.system(size: 11))
        }
        .foregroundStyle(F33Color.ink4)
        .padding(.top, F33Spacing.xs)
    }

    private var lineageCredit: (some View)? {
        guard let handle = lineageHandle else { return Optional<AnyView>.none }
        return AnyView(
            HStack(spacing: 4) {
                Image(systemName: "arrow.triangle.branch")
                    .font(.system(size: 10))
                Text("Originally by @\(handle)")
                    .font(.system(size: 11))
            }
            .foregroundStyle(F33Color.ink4)
            .padding(.top, F33Spacing.xs)
        )
    }

    private var lineageHandle: String? {
        guard let handle = work.lineageHandle, !handle.isEmpty, handle != work.author.handle else {
            return nil
        }
        return handle
    }

    private var cardSummary: String {
        var parts = ["@\(work.author.handle)", work.kindLabel, RelativeTime.fullLabel(for: work.createdAt)]
        if let gate = work.gate { parts.append(gate.label) }
        return parts.joined(separator: ", ")
    }
}

/// The sheets a card can open.
private enum CardSheet: String, Identifiable {
    case reply, quote, tip, report, edit
    var id: String { rawValue }
}

/// The heart that blooms over media on a double-tap like, once per trigger.
struct HeartBurst: View {
    let trigger: Int

    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @State private var shown = 0
    @State private var isVisible = false

    var body: some View {
        Image(systemName: "heart.fill")
            .font(.system(size: 88))
            .foregroundStyle(F33Action.like)
            .shadow(color: .black.opacity(0.25), radius: 8)
            .scaleEffect(isVisible ? 1 : 0.4)
            .opacity(isVisible ? 1 : 0)
            .allowsHitTesting(false)
            .accessibilityHidden(true)
            .onChange(of: trigger) { _, value in
                guard value != shown else { return }
                shown = value
                if reduceMotion {
                    isVisible = true
                    Task { try? await Task.sleep(for: .milliseconds(500)); isVisible = false }
                } else {
                    withAnimation(F33Motion.spring) { isVisible = true }
                    Task {
                        try? await Task.sleep(for: .milliseconds(650))
                        withAnimation(F33Motion.easeOut) { isVisible = false }
                    }
                }
            }
    }
}

/// The `gated · sub` / `gated · 2 AET` chip on the meta row.
struct GateChip: View {
    let gate: WorkGate

    var body: some View {
        Text(gate.label)
            .font(.system(size: 10, weight: .semibold))
            .foregroundStyle(tint)
            .lineLimit(1)
            .fixedSize()
            .padding(.horizontal, 6)
            .padding(.vertical, 2)
            .background(tint.opacity(0.13), in: Capsule())
            .accessibilityLabel(gate == .subscriber ? "Subscribers only" : gate.label)
    }

    private var tint: Color {
        switch gate {
        case .subscriber: return F33Color.ok
        case .priced: return F33Color.aet
        }
    }
}

/// The countdown chip on a work that carries an expiry.
struct VisionCountdown: View {
    let label: String

    var body: some View {
        Text(label)
            .font(.system(size: 10, weight: .semibold))
            .foregroundStyle(F33Color.accent)
            .lineLimit(1)
            .fixedSize()
            .padding(.horizontal, 6)
            .padding(.vertical, 2)
            .background(F33Color.accentSoft, in: Capsule())
    }
}

/// Up to three replier faces and the reply count, under a card.
struct ReplyPreview: View {
    let handles: [String]
    let avatars: [String]
    let replyCount: Int

    var body: some View {
        HStack(spacing: F33Spacing.sm) {
            HStack(spacing: -6) {
                ForEach(Array(handles.prefix(3).enumerated()), id: \.offset) { index, handle in
                    F33Avatar(
                        handle: handle,
                        displayName: handle,
                        avatarURL: index < avatars.count ? avatars[index] : nil,
                        size: 18
                    )
                    .overlay(Circle().strokeBorder(F33Color.bg, lineWidth: 1.5))
                }
            }

            Text(label)
                .font(.system(size: 12))
                .foregroundStyle(F33Color.ink4)
        }
        .padding(.top, F33Spacing.sm)
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(label)
    }

    private var label: String {
        replyCount == 1 ? "1 reply" : "\(Counts.exact(replyCount)) replies"
    }
}
