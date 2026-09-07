import SwiftUI
import PhotosUI
import F33D3RKit

/// Writing a work: a post, a reply, a quote, or a thread.
///
/// The sheet edits a `WorkComposer` and shows what it says. Every rule about
/// what makes a valid work — length, attachment count, poll shape, who signs,
/// when a thread may be a thread, how far ahead a work may be scheduled — is
/// the composer's; this view only lays the controls out and reports the
/// composer's verdicts.
///
/// The draft is the screen. Text runs full width with nothing drawn around it,
/// because a box around a text area is a box the reader has to look past, and
/// everything else — the audience line, the row of controls, the counter — sits
/// under it in the order it is needed. Glass on the sheet and the bottom bar:
/// chrome refracts, editable text does not.
///
/// Typing `@`, `#` or `$` opens a list of completions above the keyboard. The
/// rules for what counts as a token are the server's, in ``ComposeToken``.
///
/// Pictures and a video come from the Photos picker, a voice note from the
/// recorder sheet, a GIF from the picker sheet. Each is a tile under the text
/// that draws the composer's state for it — uploading, processing, ready,
/// failed, or a duplicate waiting on a decision — and the Post button wears
/// the composer's word for what it is waiting on. See `ComposeAttachmentViews`.
///
/// A thread is the same screen with more parts under the first, each its own
/// text view down a connector line. One part is *active* at a time — the one
/// holding the keyboard — and the caret, the completions, the counter and the
/// emoji all belong to it. ``ComposeField`` names which.
struct ComposeSheet: View {
    let mode: WorkComposer.Mode

    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    @State private var composer: WorkComposer?
    @State private var suggestions: ComposeSuggestions?
    @State private var drafts: ComposeDraftStore?
    @State private var recentEmoji = RecentEmoji()

    /// The part being written.
    @State private var activeField: ComposeField = .head
    /// Where the caret is in the active part, as the text view counts. Nil
    /// while a range is selected, or while the keyboard is away.
    @State private var caret: Int?
    /// The caret at the moment the emoji sheet opened. Opening a sheet takes
    /// the keyboard, and with it the caret; this is the spot the emoji is for.
    @State private var emojiCaret: Int?

    @State private var pickedItems: [PhotosPickerItem] = []
    @State private var isPickingPhotos = false
    @State private var isScheduling = false
    @State private var isPickingEmoji = false
    @State private var isRecordingVoice = false
    @State private var isPickingGIF = false
    @State private var isBrowsingDrafts = false
    @State private var isConfirmingClose = false
    /// A draft chosen while there was already something written.
    @State private var pendingDraft: ComposeDraft?
    /// Why drafts are unavailable or the last draft operation did not happen.
    @State private var draftProblem: String?
    /// Why something picked did not end up on the draft: a file the picker
    /// could not hand over, or a second video where only one goes.
    @State private var attachmentNote: String?
    #if DEBUG
    /// A staged scene that is about what sits under the text, not the text:
    /// the keyboard would cover it, and nothing can dismiss a keyboard on a
    /// Simulator nobody can tap.
    @State private var debugKeepsKeyboardAway = false
    #endif

    var body: some View {
        NavigationStack {
            Group {
                if let composer {
                    editor(composer)
                } else {
                    cannotSign
                }
            }
            .background(F33Color.bg.opacity(0.001))
            .navigationTitle(title)
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                // One group, so Cancel stays first: a separate
                // `.cancellationAction` item is placed after a leading one.
                ToolbarItemGroup(placement: .topBarLeading) {
                    Button("Cancel") { requestClose() }
                        .foregroundStyle(F33Color.ink2)
                    if mode.isPost, let drafts, !drafts.drafts.isEmpty {
                        Button("Drafts") { isBrowsingDrafts = true }
                            .foregroundStyle(F33Color.accent)
                            .accessibilityLabel("Drafts, \(drafts.drafts.count)")
                    }
                }
                ToolbarItem(placement: .confirmationAction) {
                    if let composer {
                        postButton(composer)
                    }
                }
            }
        }
        .f33GlassSheet()
        .presentationDragIndicator(.visible)
        // A swipe cannot throw writing away. With something written the sheet
        // holds, and Cancel asks what to do with it.
        .interactiveDismissDisabled(composer?.hasContent == true)
        .onAppear { start() }
        // Identity arrives from `/me` a moment after sign-in. A sheet opened
        // before it lands waits and then becomes the editor, rather than
        // leaving the reader on a screen that has already been resolved.
        .onChange(of: model.identity) { _, _ in start() }
        .photosPicker(
            isPresented: $isPickingPhotos,
            selection: $pickedItems,
            maxSelectionCount: pickerLimit,
            matching: .any(of: [.images, .videos])
        )
        .onChange(of: pickedItems) { _, items in
            guard !items.isEmpty, let composer else { return }
            pickedItems = []
            Task { await attach(items, to: composer) }
        }
        .sheet(isPresented: $isRecordingVoice) {
            VoiceRecorderSheet { fileURL, seconds in
                guard let composer else { return }
                Task {
                    await composer.attachVoice(fileURL: fileURL, durationSecs: seconds, filename: "voice.m4a", mimeType: "audio/mp4")
                }
            }
        }
        .sheet(isPresented: $isPickingGIF) {
            GifPickerSheet(client: model.client) { gif in
                composer?.attachGIF(gif)
            }
        }
        .sheet(isPresented: $isScheduling) {
            if let composer { ComposeScheduleSheet(composer: composer) }
        }
        .sheet(isPresented: $isPickingEmoji) {
            EmojiPickerSheet(recent: recentEmoji) { emoji in
                if let composer { insert(emoji, into: composer) }
            }
        }
        .sheet(isPresented: $isBrowsingDrafts) {
            if let drafts {
                ComposeDraftsSheet(store: drafts) { draft in
                    guard let composer else { return }
                    if composer.hasContent {
                        pendingDraft = draft
                    } else {
                        open(draft, in: composer)
                    }
                }
            }
        }
        .confirmationDialog(closeTitle, isPresented: $isConfirmingClose, titleVisibility: .visible) {
            if let composer, composer.canSaveDraft, drafts != nil {
                Button("Save draft") { saveDraftAndClose(composer) }
            }
            Button(mode.isPost ? "Discard" : "Discard \(title.lowercased())", role: .destructive) { close() }
        } message: {
            if let composer, !composer.publishedParts.isEmpty {
                Text(publishedNote(composer))
            }
        }
        .confirmationDialog(
            "Replace what you're writing?",
            isPresented: Binding(get: { pendingDraft != nil }, set: { if !$0 { pendingDraft = nil } }),
            titleVisibility: .visible
        ) {
            if let composer, let draft = pendingDraft {
                if composer.canSaveDraft, drafts != nil {
                    Button("Save it as a draft, then open") {
                        if saveDraft(composer) { open(draft, in: composer) }
                    }
                }
                Button("Replace", role: .destructive) { open(draft, in: composer) }
            }
        }
    }

    /// Builds the draft, and opens it with whatever the screen behind asked
    /// for — a profile leaves `@handle ` here. Taken rather than read, so the
    /// next work started somewhere else begins empty.
    private func start() {
        guard composer == nil, let made = model.makeComposer(mode) else { return }
        let seed = model.takeComposerSeed()
        // Only a new work is addressed this way. A reply already names who it
        // answers, and a quote carries the work it is about.
        if case .post = mode, !seed.isEmpty {
            made.body = seed
            caret = (seed as NSString).length
        }
        composer = made
        suggestions = model.makeComposerSuggestions()
        loadDrafts()
        #if DEBUG
        stage(made)
        #endif
    }

    #if DEBUG
    /// Puts the sheet into the state `F33D3R_COMPOSE_SCENE` names. Words
    /// only; nothing here is posted.
    private func stage(_ composer: WorkComposer) {
        guard let scene = model.debugComposeScene, mode.isPost else { return }
        debugKeepsKeyboardAway = scene.hasPrefix("video") || scene == "gif" || scene == "voice"
        let paragraph = "The feed shows a thread by its first part, so the first part has to stand on its own. "
            + "Everything after it is for the reader who stopped, which is the only reader a second part has."
        switch scene {
        case "thread":
            composer.body = "Three things about writing threads here, in three parts."
            for text in ["First: every part is its own signed work, chained to the one before it.", "Second: pictures go on the first part, words on the rest."] {
                if let segment = composer.addSegment() { composer.setSegmentBody(segment.id, text) }
            }
            _ = composer.schedule(for: WorkComposer.earliestSchedule().addingTimeInterval(86_400))
        case "long":
            composer.body = Array(repeating: paragraph, count: 3).joined(separator: " ")
        case "suggest":
            composer.body = paragraph + "\n\n" + paragraph
        case "scheduled":
            composer.body = "Going out tomorrow."
            _ = composer.schedule(for: WorkComposer.earliestSchedule().addingTimeInterval(86_400))
        case "scheduler":
            composer.body = "Going out tomorrow."
            isScheduling = true
        case "emoji":
            composer.body = "Feeling 🔥 — and a ❤️"
            recentEmoji.note("🔥")
            recentEmoji.note("✨")
            isPickingEmoji = true
        case "drafts":
            if let drafts {
                let sample = ComposeDraft(
                    body: "A draft kept for later\nwith a second line",
                    segments: ["and a second part"],
                    scheduledAt: WorkComposer.earliestSchedule().addingTimeInterval(2 * 86_400)
                )
                do { try drafts.save(sample) } catch { draftProblem = "Couldn't save the sample draft: \(error)" }
            }
            isBrowsingDrafts = true
        case "video":
            // Uploaded, transcoder on it. Post says "Processing video…".
            composer.body = "A clip from the studio."
            composer.debugStageVideo(.init(fileURL: nil, filename: "studio.mov", mimeType: "video/quicktime", state: .processing, uploadID: "upl-staged"))
        case "video-uploading":
            composer.body = "A clip from the studio."
            composer.debugStageVideo(.init(fileURL: nil, filename: "studio.mov", mimeType: "video/quicktime", state: .uploading(0.42)))
        case "video-ready":
            composer.body = "A clip from the studio."
            composer.debugStageVideo(.init(
                fileURL: nil, filename: "studio.mov", mimeType: "video/quicktime",
                state: .ready(.init(masterURL: "/media/sample/clip/master.m3u8", posterURL: "/media/sample/clip/poster.webp",
                                    watermarkedURL: "/media/sample/clip/watermarked.mp4", durationSecs: 42, width: 1280, height: 720, uploadID: "upl-staged"))
            ))
        case "video-duplicate":
            composer.body = "A clip from the studio."
            composer.debugStageVideo(.init(
                fileURL: nil, filename: "studio.mov", mimeType: "video/quicktime",
                state: .duplicate(handle: "miiyazuko", workID: "w-1",
                                  renditions: .init(masterURL: "/media/sample/clip/master.m3u8", posterURL: "/media/sample/clip/poster.webp", durationSecs: 12))
            ))
        case "video-failed":
            composer.body = "A clip from the studio."
            composer.debugStageVideo(.init(fileURL: nil, filename: "studio.mov", mimeType: "video/quicktime", state: .failed("ffmpeg exited with 1")))
        case "gif":
            composer.body = "This, exactly."
            // A public GIF, since no provider answers a Simulator; the tile
            // fetches it exactly as it would a provider's.
            let earth = "https://upload.wikimedia.org/wikipedia/commons/2/2c/Rotating_earth_%28large%29.gif"
            composer.attachGIF(GifResult(id: "staged", url: earth, previewURL: earth, width: 400, height: 400, title: "Rotating earth"))
        case "voice":
            composer.body = "Said out loud."
            composer.restore(ComposeDraft(body: "Said out loud.", voice: .init(remotePath: "/media/sample/voice.m4a", durationSecs: 47)))
        case "recorder":
            isRecordingVoice = true
        case "gifs":
            isPickingGIF = true
        default:
            break
        }
    }
    #endif

    /// How many more items the Photos picker may hand over: the media slots
    /// left, or one when the draft is empty and that one could be a video.
    private var pickerLimit: Int {
        guard let composer else { return 0 }
        if composer.video != nil { return 0 }
        let slots = WorkComposer.maxAttachments - composer.attachments.count - (composer.gif == nil ? 0 : 1)
        return max(composer.canAttachVideo ? 1 : 0, slots)
    }

    /// Opens this account's drafts. A folder that cannot be read is reported
    /// and Save is withheld — see ``AppModel/draftStore(forHandle:)``.
    private func loadDrafts() {
        guard mode.isPost, drafts == nil, let handle = model.state.user?.user.handle else { return }
        do {
            let store = try model.draftStore(forHandle: handle)
            drafts = store
            try store.reload()
        } catch {
            draftProblem = "Drafts: \(error)"
        }
    }

    private var title: String {
        switch mode {
        case .post: return "New work"
        case .reply: return "Reply"
        case .quote: return "Quote"
        }
    }

    // MARK: - Editor

    @ViewBuilder
    private func editor(_ composer: WorkComposer) -> some View {
        let activeText = text(of: activeField, in: composer)
        VStack(spacing: 0) {
            ScrollView {
                VStack(alignment: .leading, spacing: F33Spacing.md) {
                    if let parent = mode.parent {
                        replyContext(parent)
                    }

                    headRow(composer)

                    ForEach(composer.segments) { segment in
                        ThreadSegmentRow(
                            text: segmentBinding(segment.id, on: composer),
                            caret: $caret,
                            isActive: activeField == .segment(segment.id),
                            remaining: composer.remaining(in: segment.id),
                            onBeginEditing: { activeField = .segment(segment.id) },
                            onEndEditing: { if activeField == .segment(segment.id) { caret = nil } },
                            remove: {
                                if activeField == .segment(segment.id) { activeField = .head; caret = nil }
                                composer.removeSegment(segment.id)
                            }
                        )
                    }

                    if composer.isThread, composer.canAddSegment {
                        addPartButton(composer)
                    }

                    if let failure = composer.failure {
                        notice(failure, tint: F33Color.danger)
                    } else if let problem = composer.scheduleProblem {
                        notice(problem, tint: F33Color.danger)
                    } else if let problem = model.signingProblem {
                        notice(problem, tint: F33Color.warn)
                    }
                    if let draftProblem {
                        notice(draftProblem, tint: F33Color.warn)
                    }
                    if let attachmentNote {
                        notice(attachmentNote, tint: F33Color.warn)
                    }
                }
                .padding(.horizontal, F33Card.paddingHorizontal)
                .padding(.top, F33Spacing.md)
                .padding(.bottom, F33Spacing.xl)
            }
            .scrollDismissesKeyboard(.interactively)

            if let suggestions, let results = suggestions.results {
                ComposeSuggestionList(results: results) { value in
                    complete(with: value, on: composer)
                }
            }

            composeBar(composer)
        }
        .animation(F33Motion.easeOut, value: composer.failure)
        .animation(F33Motion.easeOut, value: composer.segments.count)
        .animation(F33Motion.easeOut, value: suggestions?.results)
        .onChange(of: activeText) { _, body in
            suggestions?.update(body: body, caret: caret)
        }
        .onChange(of: caret) { _, now in
            suggestions?.update(body: activeText, caret: now)
        }
        .onChange(of: activeField) { _, _ in
            suggestions?.update(body: activeText, caret: caret)
        }
    }

    /// The first part: avatar, text, and everything that belongs to the work
    /// as a whole — pictures, poll, quoted work, schedule, audience. When
    /// there are parts below, a line runs down from the avatar to meet them.
    private func headRow(_ composer: WorkComposer) -> some View {
        HStack(alignment: .top, spacing: F33Card.columnGap) {
            if let user = model.state.user?.user {
                F33Avatar(user: user, size: F33Card.avatarColumnWidth)
            }

            VStack(alignment: .leading, spacing: F33Spacing.md) {
                if composer.continuesThreadFrom != nil {
                    Label("Continuing your thread", systemImage: "text.append")
                        .font(.system(size: 13, weight: .medium))
                        .foregroundStyle(F33Color.accent)
                }

                textWell(composer)

                shapeHint(composer)

                if let video = composer.video {
                    VideoAttachmentTile(
                        video: video,
                        remove: { composer.removeVideo() },
                        retry: { composer.retryVideo() },
                        useAnyway: { composer.acceptDuplicateVideo() }
                    )
                }

                if !composer.attachments.isEmpty || composer.gif != nil {
                    attachmentsRow(composer)
                }

                if let voice = composer.voice {
                    VoiceAttachmentRow(
                        voice: voice,
                        remove: { composer.removeVoice() },
                        retry: { Task { await composer.retryVoice() } }
                    )
                }

                if composer.poll != nil {
                    PollBuilder(composer: composer)
                }

                if let quoted = mode.quoted {
                    QuotedWorkCard(quoted: quotedCard(quoted))
                }

                if let at = composer.scheduledAt {
                    scheduleChip(at, composer)
                }

                audienceMenu(composer)
            }
        }
        .background(alignment: .bottomLeading) {
            if composer.isThread {
                ThreadConnector()
                    .padding(.top, F33Card.avatarColumnWidth + F33Spacing.xs)
                    .padding(.bottom, -F33Spacing.md)
            }
        }
    }

    private func replyContext(_ parent: Work) -> some View {
        VStack(alignment: .leading, spacing: F33Spacing.xs) {
            Text("Replying to @\(parent.author.handle)")
                .font(.system(size: 13, weight: .medium))
                .foregroundStyle(F33Color.accent)
            if !parent.body.isEmpty {
                Text(parent.body)
                    .font(.system(size: 13))
                    .foregroundStyle(F33Color.ink3)
                    .lineLimit(3)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .padding(.leading, F33Card.avatarColumnWidth + F33Card.columnGap)
    }

    private func textWell(_ composer: WorkComposer) -> some View {
        @Bindable var composer = composer
        return ZStack(alignment: .topLeading) {
            if composer.body.isEmpty {
                Text(placeholder)
                    .font(.body)
                    .foregroundStyle(F33Color.ink4)
                    .padding(.top, 4)
                    .allowsHitTesting(false)
            }
            ComposeTextView(
                text: $composer.body,
                caret: $caret,
                isActive: activeField == .head,
                onBeginEditing: { activeField = .head },
                onEndEditing: { if activeField == .head { caret = nil } },
                focusesOnAppear: headTakesKeyboard,
                minHeight: mode.parent == nil ? 160 : 110,
                accessibilityLabel: placeholder
            )
        }
    }

    /// Whether the first part takes the keyboard as the sheet opens. Always,
    /// outside a staged scene.
    private var headTakesKeyboard: Bool {
        #if DEBUG
        return !debugKeepsKeyboardAway
        #else
        return true
        #endif
    }

    private var placeholder: String {
        switch mode {
        case .post: return "What's happening?"
        case .reply: return "Write your reply"
        case .quote: return "Add a comment"
        }
    }

    /// What the length of the first part suggests — the web's `ComposeAER`.
    /// Past five hundred characters, how long it takes to read; short of
    /// that, two paragraphs and three hundred characters are offered as a
    /// thread. Neither changes what is sent unless the author takes the offer.
    @ViewBuilder
    private func shapeHint(_ composer: WorkComposer) -> some View {
        if let minutes = ComposeAnalysis.readingMinutes(of: composer.body) {
            Label("Long post · ~\(minutes) min read", systemImage: "text.alignleft")
                .font(.system(size: 12, weight: .medium))
                .foregroundStyle(F33Color.ink4)
                .transition(.opacity)
        } else if composer.segments.isEmpty, composer.canAddSegment, ComposeAnalysis.suggestsThread(composer.body) {
            HStack(spacing: F33Spacing.sm) {
                Text("Looks like a thread")
                    .font(.system(size: 12, weight: .medium))
                    .foregroundStyle(F33Color.ink3)
                Button("Make it a thread") {
                    if composer.makeThread(from: ComposeAnalysis.paragraphs(of: composer.body)) {
                        activeField = .head
                        caret = nil
                    }
                }
                .font(.system(size: 12, weight: .semibold))
                .foregroundStyle(F33Color.accent)
                .frame(minHeight: F33Layout.minTouchTarget - 12)
            }
            .transition(.opacity)
        }
    }

    /// The publish time, where the audience line is: a thing the reader
    /// needs to be able to read back, not just an icon lit up in the bar. A
    /// tap reopens the picker; the cross removes it.
    private func scheduleChip(_ at: Date, _ composer: WorkComposer) -> some View {
        let stale = composer.scheduleProblem != nil
        return HStack(spacing: 0) {
            Button { isScheduling = true } label: {
                HStack(spacing: F33Spacing.xs) {
                    Image(systemName: "calendar.badge.clock")
                        .font(.system(size: 12, weight: .semibold))
                    Text("Scheduled for \(ScheduleClock.label(at))")
                        .font(.system(size: 13, weight: .semibold))
                        .lineLimit(1)
                }
                .padding(.leading, F33Spacing.md)
                .padding(.trailing, F33Spacing.xs)
                .frame(height: 30)
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Scheduled for \(ScheduleClock.label(at)). Change")

            Button { composer.clearSchedule() } label: {
                Image(systemName: "xmark")
                    .font(.system(size: 10, weight: .bold))
                    .frame(width: 30, height: 30)
                    .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Remove schedule")
        }
        .foregroundStyle(stale ? F33Color.danger : F33Color.accent)
        .f33Glass(in: Capsule(), interactive: true)
        .frame(minHeight: F33Layout.minTouchTarget, alignment: .leading)
        .transition(.opacity)
    }

    private func addPartButton(_ composer: WorkComposer) -> some View {
        Button {
            addSegment(to: composer)
        } label: {
            Label("Add another part", systemImage: "plus")
                .font(.system(size: 13, weight: .semibold))
                .foregroundStyle(F33Color.accent)
                .frame(minHeight: F33Layout.minTouchTarget)
        }
        .buttonStyle(.plain)
        .padding(.leading, F33Card.avatarColumnWidth + F33Card.columnGap)
    }

    private func notice(_ text: String, tint: Color) -> some View {
        Text(text)
            .font(.footnote)
            .foregroundStyle(tint)
            .fixedSize(horizontal: false, vertical: true)
            .transition(.opacity)
    }

    /// The media strip: pictures, and the GIF beside them, in the order they
    /// were added.
    private func attachmentsRow(_ composer: WorkComposer) -> some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: F33Spacing.sm) {
                ForEach(composer.attachments) { attachment in
                    AttachmentThumb(attachment: attachment) {
                        composer.removeAttachment(attachment.id)
                    } retry: {
                        Task { await composer.retryUpload(attachment.id) }
                    }
                }
                if let gif = composer.gif {
                    GifAttachmentTile(gif: gif) { composer.removeGIF() }
                }
            }
        }
        .scrollClipDisabled()
    }

    /// The preview of the work being quoted, in the shape the feed will draw it.
    /// The work's own quote becomes the second level here — the same rail the
    /// published quote will carry, so the preview is not more or less than the
    /// card it turns into.
    private func quotedCard(_ work: Work) -> QuotedWork {
        QuotedWork(
            id: work.id, cid: work.cid, author: work.author, body: work.body, createdAt: work.createdAt,
            mediaURLs: work.mediaURLs, video: work.video, isNSFW: work.isNSFW, isGore: work.isGore,
            nested: work.quoted.map(QuotedWorkRef.init)
        )
    }

    // MARK: - The active part

    private func text(of field: ComposeField, in composer: WorkComposer) -> String {
        switch field {
        case .head: return composer.body
        case .segment(let id): return composer.segmentBody(id)
        }
    }

    private func setText(_ text: String, of field: ComposeField, in composer: WorkComposer) {
        switch field {
        case .head: composer.body = text
        case .segment(let id): composer.setSegmentBody(id, text)
        }
    }

    private func segmentBinding(_ id: UUID, on composer: WorkComposer) -> Binding<String> {
        Binding(
            get: { composer.segmentBody(id) },
            set: { composer.setSegmentBody(id, $0) }
        )
    }

    private func addSegment(to composer: WorkComposer) {
        guard let segment = composer.addSegment() else { return }
        // The new part takes the keyboard when it appears; the caret it
        // reports belongs to it from the start.
        activeField = .segment(segment.id)
        caret = nil
    }

    /// Puts a chosen name, tag or ticker into the active part in place of the
    /// token being typed. Only that token's own characters move.
    private func complete(with value: String, on composer: WorkComposer) {
        guard let suggestions, let token = suggestions.token else { return }
        let (body, moved) = token.completing(with: value, in: text(of: activeField, in: composer))
        setText(body, of: activeField, in: composer)
        caret = moved
        suggestions.settle(on: body)
    }

    /// Puts an emoji where the caret was when the picker opened, or at the
    /// end when the keyboard was already away.
    private func insert(_ emoji: String, into composer: WorkComposer) {
        let current = text(of: activeField, in: composer) as NSString
        let at = min(emojiCaret ?? current.length, current.length)
        let updated = current.replacingCharacters(in: NSRange(location: at, length: 0), with: emoji)
        setText(updated, of: activeField, in: composer)
        let moved = at + (emoji as NSString).length
        caret = moved
        emojiCaret = moved
        recentEmoji.note(emoji)
    }

    // MARK: - Audience

    /// Who the work is for, and who may answer it.
    ///
    /// Under the draft rather than in the row of controls: it is the one
    /// setting whose value the reader needs to be able to read at a glance,
    /// and a row of icons is not somewhere a sentence belongs.
    private func audienceMenu(_ composer: WorkComposer) -> some View {
        @Bindable var composer = composer
        return Menu {
            if model.state.user?.user.isCreator == true {
                Section("Who sees this") {
                    Toggle(isOn: $composer.subscriberOnly) {
                        Label("Subscribers only", systemImage: "lock")
                    }
                }
            }
            Section("Who can reply") {
                Picker("Who can reply", selection: $composer.commentGating) {
                    Label("Everyone", systemImage: "globe").tag(CommentGating.everyone)
                    Label("People you follow", systemImage: "person.2").tag(CommentGating.followers)
                    Label("Your circle", systemImage: "person.3").tag(CommentGating.circle)
                    Label("Nobody", systemImage: "bubble.left.and.exclamationmark.bubble.right").tag(CommentGating.none)
                }
            }
        } label: {
            HStack(spacing: F33Spacing.xs) {
                Image(systemName: audienceIcon(composer))
                    .font(.system(size: 12, weight: .semibold))
                Text(audienceLabel(composer))
                    .font(.system(size: 13, weight: .semibold))
                    .lineLimit(1)
                Image(systemName: "chevron.down")
                    .font(.system(size: 9, weight: .bold))
            }
            .foregroundStyle(F33Color.accent)
            .padding(.horizontal, F33Spacing.md)
            .frame(height: 30)
            .f33Glass(in: Capsule(), interactive: true)
            .frame(minHeight: F33Layout.minTouchTarget, alignment: .leading)
            .contentShape(Capsule())
        }
        .accessibilityLabel("Audience: \(audienceLabel(composer))")
    }

    private func audienceIcon(_ composer: WorkComposer) -> String {
        if composer.subscriberOnly { return "lock" }
        switch composer.commentGating {
        case .everyone: return "globe"
        case .followers: return "person.2"
        case .circle: return "person.3"
        case .none: return "bubble.left.and.exclamationmark.bubble.right"
        }
    }

    private func audienceLabel(_ composer: WorkComposer) -> String {
        if composer.subscriberOnly { return "Subscribers only" }
        switch composer.commentGating {
        case .everyone: return "Everyone can reply"
        case .followers: return "People you follow can reply"
        case .circle: return "Your circle can reply"
        case .none: return "Nobody can reply"
        }
    }

    // MARK: - Bottom bar

    /// The row of controls under the draft, with the counter pinned at the
    /// end. Eight controls do not fit a phone's width side by side — and a bar
    /// that overflows widens the whole sheet, since the sheet is as wide as
    /// its widest row — so the controls scroll under the fixed counter, the
    /// way a keyboard accessory bar does.
    private func composeBar(_ composer: WorkComposer) -> some View {
        HStack(spacing: F33Spacing.sm) {
            ScrollView(.horizontal, showsIndicators: false) {
                barButtons(composer)
            }
            .scrollClipDisabled()

            CharacterRing(remaining: activeRemaining(composer), limit: WorkComposer.maxBodyLength)
        }
        .padding(.horizontal, F33Spacing.lg)
        .padding(.vertical, F33Spacing.sm)
        .f33GlassBar(extending: .bottom)
        .overlay(alignment: .top) { CardDivider() }
    }

    private func barButtons(_ composer: WorkComposer) -> some View {
        HStack(spacing: F33Spacing.sm) {
            barButton("photo.on.rectangle", label: "Add photos or a video", enabled: composer.canAttach || composer.canAttachVideo) {
                isPickingPhotos = true
            }

            barButton(text: "GIF", label: "Add a GIF", enabled: composer.canAttachGIF) {
                isPickingGIF = true
            }

            barButton("mic", label: "Record a voice note", enabled: composer.canRecordVoice, isOn: composer.voice != nil) {
                isRecordingVoice = true
            }

            barButton("chart.bar.xaxis", label: composer.poll == nil ? "Add a poll" : "Remove the poll",
                      enabled: composer.canAddPoll || composer.poll != nil, isOn: composer.poll != nil) {
                if composer.poll == nil { composer.addPoll() } else { composer.removePoll() }
            }

            barButton("face.smiling", label: "Add an emoji", enabled: true) {
                emojiCaret = caret
                isPickingEmoji = true
            }

            if composer.canAddSegment || composer.isThread {
                barButton("text.append", label: "Add to thread", enabled: composer.canAddSegment, isOn: composer.isThread) {
                    addSegment(to: composer)
                }
            }

            if composer.canSchedule {
                barButton("calendar.badge.clock", label: composer.scheduledAt == nil ? "Schedule" : "Change the schedule",
                          enabled: true, isOn: composer.scheduledAt != nil) {
                    isScheduling = true
                }
            }

            if model.state.user?.isAdult == true {
                barButton("18.circle", label: composer.isNSFW ? "Marked 18+" : "Mark as 18+",
                          enabled: true, isOn: composer.isNSFW) {
                    composer.isNSFW.toggle()
                }
            }
        }
    }

    /// The counter follows the keyboard: it is about the part being written.
    private func activeRemaining(_ composer: WorkComposer) -> Int {
        switch activeField {
        case .head: return composer.remaining
        case .segment(let id): return composer.remaining(in: id)
        }
    }

    private func barButton(_ icon: String, label: String, enabled: Bool, isOn: Bool = false, action: @escaping () -> Void) -> some View {
        barButton(label: label, enabled: enabled, isOn: isOn, action: action) {
            Image(systemName: icon)
                .font(.system(size: 17, weight: .semibold))
        }
    }

    /// The same pill with a word on it, for the one control SF Symbols has no
    /// glyph for.
    private func barButton(text: String, label: String, enabled: Bool, isOn: Bool = false, action: @escaping () -> Void) -> some View {
        barButton(label: label, enabled: enabled, isOn: isOn, action: action) {
            Text(text)
                .font(.system(size: 12, weight: .heavy))
                .kerning(0.4)
        }
    }

    private func barButton<Glyph: View>(
        label: String, enabled: Bool, isOn: Bool, action: @escaping () -> Void, @ViewBuilder glyph: () -> Glyph
    ) -> some View {
        Button(action: action) {
            glyph()
                .foregroundStyle(isOn ? F33Color.accentInk : (enabled ? F33Color.accent : F33Color.ink5))
                .frame(width: 40, height: 34)
                .f33Glass(in: Capsule(), tint: isOn ? F33Color.accent : nil, interactive: enabled)
                .frame(minHeight: F33Layout.minTouchTarget)
                .contentShape(Capsule())
        }
        .buttonStyle(.plain)
        .disabled(!enabled)
        .accessibilityLabel(label)
        .accessibilityAddTraits(isOn ? [.isSelected, .isButton] : .isButton)
    }

    // MARK: - Post

    private func postButton(_ composer: WorkComposer) -> some View {
        Button {
            Task { await submit(composer) }
        } label: {
            Group {
                if composer.isSubmitting {
                    ProgressView().tint(F33Color.accentInk)
                } else {
                    Text(postTitle(composer))
                        .font(.system(size: 15, weight: .semibold))
                        // "Processing…" has to fit where "Post" does; the
                        // toolbar will not widen for it.
                        .lineLimit(1)
                        .minimumScaleFactor(0.7)
                }
            }
            .foregroundStyle(F33Color.accentInk)
            .padding(.horizontal, F33Spacing.lg)
            .frame(height: 34)
            .f33Glass(in: Capsule(), tint: F33Color.accent, interactive: true)
        }
        .buttonStyle(.plain)
        .disabled(!(composer.canSubmit || composer.uncertain != nil) || composer.isSubmitting)
        .opacity((composer.canSubmit || composer.uncertain != nil) ? 1 : 0.5)
        .accessibilityLabel(postTitle(composer))
    }

    /// What the button does, said plainly: a retry is a retry, a scheduled
    /// work is scheduled, and a reply is a reply. While an upload holds it,
    /// the button says which — the web's "Uploading…" on the same button.
    private func postTitle(_ composer: WorkComposer) -> String {
        if composer.uncertain != nil { return "Retry" }
        if let activity = composer.blockingActivity { return activity }
        if composer.scheduledAt != nil { return "Schedule" }
        switch mode {
        case .post: return "Post"
        case .reply: return "Reply"
        case .quote: return "Quote"
        }
    }

    private func submit(_ composer: WorkComposer) async {
        let scheduled = composer.scheduledAt != nil
        guard let outcome = await composer.submit() else { return }
        guard case .accepted(let accepted) = outcome else { return }
        await model.didPublish(accepted, mode: mode, scheduled: scheduled)
        // Posted: the files the recorder and the picker left are not needed
        // for anything now.
        composer.discardLocalFiles()

        // The draft was for this work, and this work now exists.
        if let drafts, let id = composer.draftID {
            do {
                try drafts.delete(id: id)
            } catch {
                // Published — so nothing here may be posted again — but the
                // file is still there and would come back as a draft of a work
                // that already exists. A fresh composer and a plain account of
                // what happened, rather than a sheet that closes as if all
                // went well.
                self.composer = model.makeComposer(mode)
                activeField = .head
                caret = nil
                draftProblem = "Posted. The saved draft couldn't be removed (\(error.localizedDescription)) — delete it from Drafts."
                return
            }
        }
        dismiss()
    }

    /// Puts what the picker handed over onto the draft: a movie as the one
    /// video, everything else as a picture. The composer's rules decide what
    /// goes on — a second video, or a picture beside a video, is refused there
    /// — and what was refused, or could not be read, is said on the screen
    /// rather than dropped.
    private func attach(_ items: [PhotosPickerItem], to composer: WorkComposer) async {
        attachmentNote = nil
        var refused: [String] = []
        for item in items {
            let isMovie = item.supportedContentTypes.contains { $0.conforms(to: .movie) || $0.conforms(to: .video) }
            do {
                if isMovie {
                    guard let movie = try await item.loadTransferable(type: PickedMovie.self) else {
                        refused.append("A video couldn't be read from Photos.")
                        continue
                    }
                    guard composer.canAttachVideo else {
                        try? FileManager.default.removeItem(at: movie.url)
                        refused.append(composer.video == nil
                            ? "A video goes on its own — remove the pictures, GIF or poll first."
                            : "One video per work; remove the one that's there to use another.")
                        continue
                    }
                    composer.attachVideo(fileURL: movie.url, filename: movie.url.lastPathComponent, mimeType: movie.mimeType)
                } else {
                    guard let data = try await item.loadTransferable(type: Data.self) else {
                        refused.append("A photo couldn't be read from Photos.")
                        continue
                    }
                    guard composer.canAttach else {
                        refused.append(composer.video != nil
                            ? "Pictures can't go beside a video."
                            : "That's the most media one work can carry.")
                        continue
                    }
                    let (mime, ext) = ImageSniff.type(of: data)
                    await composer.attach(data, filename: "photo-\(UUID().uuidString.prefix(8)).\(ext)", mimeType: mime)
                }
            } catch {
                refused.append("Couldn't read that from Photos: \(error.localizedDescription)")
            }
        }
        if !refused.isEmpty {
            attachmentNote = Array(Set(refused)).sorted().joined(separator: " ")
        }
    }

    // MARK: - Closing and drafts

    private var closeTitle: String {
        guard let composer else { return "Discard?" }
        if !composer.publishedParts.isEmpty { return "Keep the rest of this thread?" }
        return mode.isPost ? "Keep this work?" : "Discard this \(title.lowercased())?"
    }

    private func publishedNote(_ composer: WorkComposer) -> String {
        let n = composer.publishedParts.count
        return "\(n) \(n == 1 ? "part is" : "parts are") already live on your profile. Only what hasn't posted is affected."
    }

    private func requestClose() {
        guard let composer, composer.hasContent || composer.uncertain != nil else {
            close()
            return
        }
        isConfirmingClose = true
    }

    /// Leaves. Parts of a thread that landed are real, so the feed is told
    /// about the head before the sheet goes, exactly as it would be had the
    /// whole thread posted.
    private func close() {
        if let head = composer?.publishedParts.first {
            Task { await model.didPublish(head, mode: mode) }
        }
        composer?.discardLocalFiles()
        dismiss()
    }

    /// Writes the draft. False, with the reason on screen, when it could not
    /// be written — the sheet stays so nothing is lost.
    private func saveDraft(_ composer: WorkComposer) -> Bool {
        guard let drafts else { return false }
        do {
            try drafts.save(composer.draft())
            return true
        } catch {
            draftProblem = "Couldn't save the draft: \(error.localizedDescription)"
            return false
        }
    }

    private func saveDraftAndClose(_ composer: WorkComposer) {
        if saveDraft(composer) { close() }
    }

    private func open(_ draft: ComposeDraft, in composer: WorkComposer) {
        composer.restore(draft)
        activeField = .head
        caret = nil
    }

    // MARK: - Cannot sign

    private var cannotSign: some View {
        VStack(spacing: F33Spacing.md) {
            if model.signingProblem == nil {
                ProgressView()
                Text("Getting this device ready to sign")
                    .font(.headline)
                    .foregroundStyle(F33Color.ink2)
            } else {
                Image(systemName: "signature")
                    .font(.system(size: 34))
                    .foregroundStyle(F33Color.ink5)
                Text("This device can't sign yet")
                    .font(.headline)
                    .foregroundStyle(F33Color.ink2)
                Text(model.signingProblem ?? "")
                    .font(.subheadline)
                    .foregroundStyle(F33Color.ink4)
                    .multilineTextAlignment(.center)
                    .fixedSize(horizontal: false, vertical: true)
                Button("Try again") {
                    Task {
                        await model.loadIdentity()
                        start()
                    }
                }
                .font(.subheadline.weight(.semibold))
                .foregroundStyle(F33Color.accent)
                .frame(minHeight: F33Layout.minTouchTarget)
            }
        }
        .padding(F33Spacing.xl)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}

/// Which part of the draft holds the keyboard.
enum ComposeField: Hashable {
    case head
    case segment(UUID)
}

/// One part of a thread after the first.
///
/// The connector down the avatar column is the web's `thread-seg` line: it
/// says these are one work in several pieces, not several works. The part is
/// text only — see `WorkComposer.payload()` for why the pictures stay on the
/// head — so there is nothing under the text but the next part.
private struct ThreadSegmentRow: View {
    @Binding var text: String
    @Binding var caret: Int?
    let isActive: Bool
    let remaining: Int
    let onBeginEditing: () -> Void
    let onEndEditing: () -> Void
    let remove: () -> Void

    var body: some View {
        HStack(alignment: .top, spacing: F33Spacing.xs) {
            ZStack(alignment: .topLeading) {
                if text.isEmpty {
                    Text("Continue your thread…")
                        .font(.body)
                        .foregroundStyle(F33Color.ink4)
                        .padding(.top, 4)
                        .allowsHitTesting(false)
                }
                ComposeTextView(
                    text: $text,
                    caret: $caret,
                    isActive: isActive,
                    onBeginEditing: onBeginEditing,
                    onEndEditing: onEndEditing,
                    // A part restored from a draft must not take the keyboard
                    // from the one before it; a part just added should.
                    focusesOnAppear: isActive,
                    minHeight: 64,
                    accessibilityLabel: "Thread part"
                )
            }

            Button(action: remove) {
                Image(systemName: "xmark.circle.fill")
                    .font(.system(size: 18))
                    .foregroundStyle(F33Color.ink4)
                    .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
                    .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Remove this part")
        }
        .overlay(alignment: .bottomTrailing) {
            // Over the limit is said on the part itself: the bar's ring only
            // follows the keyboard, and a part written earlier can be the one
            // that is over.
            if remaining < 0 {
                Text("\(remaining)")
                    .font(.system(size: 12, weight: .semibold).monospacedDigit())
                    .foregroundStyle(F33Color.danger)
                    .padding(.trailing, F33Layout.minTouchTarget + F33Spacing.xs)
            }
        }
        .padding(.leading, F33Card.avatarColumnWidth + F33Card.columnGap)
        .background(alignment: .leading) {
            ThreadConnector()
                // Reaches up across the gap to the row above, so the parts
                // read as one line rather than dashes.
                .padding(.top, -F33Spacing.md)
        }
    }
}

/// The line down the avatar column that joins the parts of a thread.
private struct ThreadConnector: View {
    var body: some View {
        Rectangle()
            .fill(F33Color.ink5)
            .frame(width: 2)
            .padding(.leading, (F33Card.avatarColumnWidth - 2) / 2)
    }
}

/// How much of the limit is spent.
///
/// It says nothing until the draft is half way there, because a gauge sitting
/// at empty from the first keystroke is a gauge nobody reads. Past that the
/// ring fills; near the end it puts the number beside itself, and over the end
/// it turns and counts down past zero — the composer refuses the post either
/// way, but refusing without having said how close it was is the part that
/// makes a reader feel cheated.
private struct CharacterRing: View {
    let remaining: Int
    let limit: Int

    /// Where the ring starts being worth drawing.
    private static let appearsAt = 0.5
    /// Where the exact number starts being worth reading.
    private static let countsFrom = 100

    private var spent: Double {
        guard limit > 0 else { return 0 }
        return Double(limit - remaining) / Double(limit)
    }

    private var tint: Color {
        if remaining < 0 { return F33Color.danger }
        if remaining <= Self.countsFrom { return F33Color.warn }
        return F33Color.accent
    }

    var body: some View {
        HStack(spacing: F33Spacing.xs) {
            if remaining <= Self.countsFrom {
                Text("\(remaining)")
                    .font(.system(size: 13, weight: .semibold).monospacedDigit())
                    .foregroundStyle(tint)
            }

            ZStack {
                Circle()
                    .strokeBorder(F33Color.hairlineStrong, lineWidth: 2.5)
                Circle()
                    .trim(from: 0, to: min(1, max(0, spent)))
                    .stroke(tint, style: StrokeStyle(lineWidth: 2.5, lineCap: .round))
                    .rotationEffect(.degrees(-90))
            }
            // Both dimensions. A circle given one is greedy in the other and
            // would push the whole bar taller as the draft grows.
            .frame(width: 22, height: 22)
        }
        .opacity(spent >= Self.appearsAt ? 1 : 0)
        .animation(F33Motion.easeOut, value: remaining)
        .accessibilityHidden(spent < Self.appearsAt)
        .accessibilityLabel(remaining < 0
            ? "\(-remaining) characters over the limit"
            : "\(remaining) characters left")
    }
}

/// One image on the draft. From the bytes when it was picked this session;
/// fetched by its path when it came back from a draft, the way a card fetches
/// any image.
private struct AttachmentThumb: View {
    let attachment: WorkComposer.Attachment
    let remove: () -> Void
    let retry: () -> Void

    var body: some View {
        ZStack(alignment: .topTrailing) {
            Group {
                if let data = attachment.data, let image = UIImage(data: data) {
                    Image(uiImage: image)
                        .resizable()
                        .aspectRatio(contentMode: .fill)
                } else if attachment.data == nil, let path = attachment.remotePath {
                    RemoteImage(path: path, seed: attachment.filename)
                } else {
                    F33Color.bgSunken
                }
            }
            .frame(width: 96, height: 96)
            .clipShape(RoundedRectangle(cornerRadius: F33Radius.sm))
            .overlay {
                if attachment.isUploading {
                    ZStack {
                        Color.black.opacity(0.35)
                        ProgressView().tint(.white)
                    }
                    .clipShape(RoundedRectangle(cornerRadius: F33Radius.sm))
                } else if attachment.failure != nil {
                    Button(action: retry) {
                        ZStack {
                            Color.black.opacity(0.55)
                            VStack(spacing: 4) {
                                Image(systemName: "arrow.clockwise")
                                Text("Retry").font(.system(size: 11, weight: .semibold))
                            }
                            .foregroundStyle(.white)
                        }
                    }
                    .buttonStyle(.plain)
                    .clipShape(RoundedRectangle(cornerRadius: F33Radius.sm))
                    .accessibilityLabel("Upload failed. Retry")
                }
            }

            Button(action: remove) {
                Image(systemName: "xmark")
                    .font(.system(size: 10, weight: .bold))
                    .foregroundStyle(.white)
                    .frame(width: 22, height: 22)
                    .background(Color.black.opacity(0.6), in: Circle())
                    .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget, alignment: .topTrailing)
                    .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .offset(x: 6, y: -6)
            .accessibilityLabel("Remove photo")
        }
        .padding(.top, 6)
        .padding(.trailing, 6)
    }
}

/// The poll rows inside the composer.
private struct PollBuilder: View {
    let composer: WorkComposer

    var body: some View {
        @Bindable var composer = composer
        VStack(alignment: .leading, spacing: F33Spacing.sm) {
            if let poll = composer.poll {
                ForEach(poll.options.indices, id: \.self) { index in
                    HStack(spacing: F33Spacing.sm) {
                        TextField("Option \(index + 1)", text: optionBinding(index))
                            .textInputAutocapitalization(.sentences)
                            .f33Field()
                        if poll.options.count > 2 {
                            Button {
                                composer.removePollOption(at: index)
                            } label: {
                                Image(systemName: "minus.circle.fill")
                                    .foregroundStyle(F33Color.ink4)
                                    .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
                                    .contentShape(Rectangle())
                            }
                            .buttonStyle(.plain)
                            .accessibilityLabel("Remove option \(index + 1)")
                        }
                    }
                }

                HStack {
                    if poll.options.count < 4 {
                        Button {
                            composer.addPollOption()
                        } label: {
                            Label("Add option", systemImage: "plus")
                                .font(.system(size: 13, weight: .semibold))
                        }
                        .foregroundStyle(F33Color.accent)
                        .frame(minHeight: F33Layout.minTouchTarget)
                    }
                    Spacer(minLength: 0)
                    Picker("Poll length", selection: durationBinding) {
                        ForEach(WorkComposer.PollDraft.durations.indices, id: \.self) { i in
                            Text(WorkComposer.PollDraft.durations[i].label).tag(i)
                        }
                    }
                    .pickerStyle(.menu)
                    .tint(F33Color.ink2)
                }
            }
        }
        .padding(F33Spacing.md)
        .background(F33Color.bgSunken, in: RoundedRectangle(cornerRadius: F33Radius.md))
    }

    private func optionBinding(_ index: Int) -> Binding<String> {
        Binding(
            get: { composer.poll?.options[safe: index] ?? "" },
            set: { new in
                guard var poll = composer.poll, poll.options.indices.contains(index) else { return }
                poll.options[index] = String(new.prefix(40))
                composer.poll = poll
            }
        )
    }

    private var durationBinding: Binding<Int> {
        Binding(
            get: {
                WorkComposer.PollDraft.durations.firstIndex { $0.value == composer.poll?.duration } ?? 0
            },
            set: { i in
                guard var poll = composer.poll else { return }
                poll.duration = WorkComposer.PollDraft.durations[i].value
                composer.poll = poll
            }
        )
    }
}

private extension Array {
    subscript(safe index: Int) -> Element? {
        indices.contains(index) ? self[index] : nil
    }
}

/// Reads an image's type off its first bytes, so the upload names what it is
/// rather than what the picker guessed.
enum ImageSniff {
    static func type(of data: Data) -> (mime: String, ext: String) {
        let head = [UInt8](data.prefix(12))
        if head.starts(with: [0xFF, 0xD8, 0xFF]) { return ("image/jpeg", "jpg") }
        if head.starts(with: [0x89, 0x50, 0x4E, 0x47]) { return ("image/png", "png") }
        if head.starts(with: [0x47, 0x49, 0x46]) { return ("image/gif", "gif") }
        if head.count >= 12, Array(head[8..<12]) == Array("WEBP".utf8) { return ("image/webp", "webp") }
        if head.count >= 12, Array(head[4..<8]) == Array("ftyp".utf8) { return ("image/heic", "heic") }
        return ("image/jpeg", "jpg")
    }
}
