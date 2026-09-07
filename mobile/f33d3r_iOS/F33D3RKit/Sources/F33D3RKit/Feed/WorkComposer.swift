import Foundation
import Observation

/// A draft work and the act of publishing it.
///
/// The compose screen edits this and calls ``submit()``; everything that has
/// to be right — which fields make a reply a reply, when a poll is a poll,
/// what the signed payload contains, that the envelope is built once and the
/// same bytes are retried — lives here and not in the view.
///
/// Attachments are uploaded as they are added, so by the time the reader taps
/// Post the payload only has to carry paths the server already knows. A video
/// is the long case: it is uploaded, then the transcoder is asked about it
/// every few seconds until it is ready, failed, or turns out to be a copy of
/// something already here — and Post waits, saying which, until it is one of
/// those. See ``VideoAttachment``.
///
/// What the work *is* follows from what is on it, in the order the web's
/// composer decides it: a voice note makes it a voice work whatever else is
/// attached, a video makes it a video work, and only then does answering or
/// quoting count — see ``kind``.
///
/// A draft with parts after the first is a thread. Every part is published as
/// `thread_post`, each chained to the one before it by `parent_cid`, and the
/// chain is signed whole before any of it is sent — see ``ThreadSubmission``.
/// When a chain stops part way the published parts are real and the rest stay
/// here, addressed to the last part that landed, so the author can carry on
/// from where it broke rather than start over or post the head twice.
@MainActor
@Observable
public final class WorkComposer {

    /// What the draft is.
    public enum Mode: Sendable, Equatable {
        case post
        case reply(to: Work)
        case quote(Work)

        public var parent: Work? {
            if case .reply(let work) = self { return work }
            return nil
        }

        public var quoted: Work? {
            if case .quote(let work) = self { return work }
            return nil
        }

        /// A new work, not an answer to one. The only mode that can be a
        /// thread, carry a poll, be scheduled, or be kept as a draft.
        public var isPost: Bool {
            if case .post = self { return true }
            return false
        }
    }

    /// One image on the draft: shown from the local bytes when it was picked
    /// this session, or fetched by path when it came back from a draft; sent
    /// as the path the server answered with either way.
    public struct Attachment: Identifiable, Sendable, Equatable {
        public let id: UUID
        /// `nil` for an attachment restored from a draft — the bytes were
        /// uploaded when it was first picked and are the server's now.
        public let data: Data?
        public let filename: String
        public let mimeType: String
        public var remotePath: String?
        public var failure: String?

        public var isUploading: Bool { remotePath == nil && failure == nil }
    }

    /// What a finished transcode hands back, in the units the work carries.
    ///
    /// The signed half — master, poster, whole seconds — goes in the payload;
    /// the unsigned half — the watermarked file, the frame size, the upload id
    /// that keeps the file from the orphan sweep — rides on the envelope. One
    /// value here so a draft can keep it and the payload can be built from it
    /// without either side knowing about the other. `Codable` for the draft.
    public struct VideoRenditions: Codable, Hashable, Sendable {
        public var masterURL: String
        public var posterURL: String?
        public var watermarkedURL: String?
        /// Whole seconds. The transcoder measures fractions; the payload's
        /// field is an integer, so it is rounded here, once.
        public var durationSecs: Int
        public var width: Int?
        public var height: Int?
        /// Our upload, when it was one. A duplicate references somebody else's
        /// file and has no upload of its own to keep alive.
        public var uploadID: String?
        /// The handle whose bytes these are, when the author chose to post a
        /// video F33D3R already had. Kept so the notice stays on the draft — the
        /// web's banner "persists so the user knows they're posting a
        /// reference" — and because the credit will show on the work.
        public var creditedTo: String?

        public init(
            masterURL: String, posterURL: String? = nil, watermarkedURL: String? = nil,
            durationSecs: Int, width: Int? = nil, height: Int? = nil,
            uploadID: String? = nil, creditedTo: String? = nil
        ) {
            self.masterURL = masterURL
            self.posterURL = posterURL
            self.watermarkedURL = watermarkedURL
            self.durationSecs = durationSecs
            self.width = width
            self.height = height
            self.uploadID = uploadID
            self.creditedTo = creditedTo
        }

        /// The renditions a job reports, or nil when it reports none — a
        /// `ready` job always has a master; a `duplicate` has one only when
        /// the server could resolve the original work.
        public init?(job: VideoJob, uploadID: String?) {
            guard let master = job.masterURL, !master.isEmpty else { return nil }
            self.init(
                masterURL: master,
                posterURL: job.posterURL.flatMap { $0.isEmpty ? nil : $0 },
                watermarkedURL: job.watermarkedURL.flatMap { $0.isEmpty ? nil : $0 },
                durationSecs: Int((job.durationSecs ?? 0).rounded()),
                width: job.width,
                height: job.height,
                uploadID: uploadID,
                creditedTo: nil
            )
        }
    }

    /// The one video on the draft, and where it is on its way to the server.
    ///
    /// A video is not an image with a longer wait. It goes up, and then a
    /// transcoder somewhere else works on it for as long as it takes, and the
    /// composer learns how that went by asking — every five seconds, for up to
    /// half an hour, the web's cadence. Each answer is one of these states, and
    /// Post is held until the state is ``State/ready(_:)``.
    ///
    /// ``State/duplicate(handle:workID:renditions:)`` is not a failure. The
    /// server is saying these exact bytes are already on F33D3R under someone
    /// else's name, and offering their renditions instead of transcoding a
    /// second copy. Whether to post it anyway — with the credit going to them
    /// — is the author's call, so the state waits for one: ``acceptDuplicateVideo()``
    /// or ``removeVideo()``.
    public struct VideoAttachment: Identifiable, Sendable, Equatable {
        public enum State: Sendable, Equatable {
            /// Bytes going up; the fraction sent so far.
            case uploading(Double)
            /// The server has the file and the transcoder is on it.
            case processing
            /// Transcoded. These are what the work will carry.
            case ready(VideoRenditions)
            /// Already on F33D3R. `renditions` is the original's, when the
            /// server could name it; without them there is nothing to attach
            /// and the only honest choice is to remove.
            case duplicate(handle: String, workID: String?, renditions: VideoRenditions?)
            /// The server's reason, or the transport's.
            case failed(String)
        }

        public let id: UUID
        /// The file on this device. `nil` for a video restored from a draft:
        /// the bytes were uploaded when it was first picked.
        public let fileURL: URL?
        public let filename: String
        public let mimeType: String
        public var state: State
        /// The job to poll, once the server has accepted the upload.
        public var uploadID: String?

        public init(id: UUID = UUID(), fileURL: URL?, filename: String, mimeType: String, state: State, uploadID: String? = nil) {
            self.id = id
            self.fileURL = fileURL
            self.filename = filename
            self.mimeType = mimeType
            self.state = state
            self.uploadID = uploadID
        }

        /// Set once the video is something the work can carry.
        public var renditions: VideoRenditions? {
            if case .ready(let renditions) = state { return renditions }
            return nil
        }

        /// Whether the server is still being waited on.
        public var isInFlight: Bool {
            switch state {
            case .uploading, .processing: return true
            case .ready, .duplicate, .failed: return false
            }
        }
    }

    /// The one voice note on the draft.
    ///
    /// The length is the recorder's measurement, not the server's — the server
    /// does not probe audio and says so (``VoiceUpload``) — so it is taken at
    /// attach time and travels with the path.
    public struct VoiceAttachment: Sendable, Equatable {
        /// The recording on this device. `nil` when restored from a draft.
        public let fileURL: URL?
        public let filename: String
        public let mimeType: String
        /// Whole seconds, as the payload carries them.
        public let durationSecs: Int
        public var remotePath: String?
        public var failure: String?

        public init(fileURL: URL?, filename: String, mimeType: String, durationSecs: Int, remotePath: String? = nil, failure: String? = nil) {
            self.fileURL = fileURL
            self.filename = filename
            self.mimeType = mimeType
            self.durationSecs = durationSecs
            self.remotePath = remotePath
            self.failure = failure
        }

        public var isUploading: Bool { remotePath == nil && failure == nil }
    }

    /// A poll being written. Two to four options, closing after `duration`.
    public struct PollDraft: Sendable, Equatable {
        public var options: [String]
        public var duration: Duration

        public static let durations: [(label: String, value: Duration)] = [
            ("1 day", .seconds(86_400)),
            ("3 days", .seconds(3 * 86_400)),
            ("7 days", .seconds(7 * 86_400)),
        ]

        public init(options: [String] = ["", ""], duration: Duration = .seconds(86_400)) {
            self.options = options
            self.duration = duration
        }

        public var trimmedOptions: [String] {
            options.map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }
        }

        /// Every option filled, at least two, none over the label limit.
        public var isValid: Bool {
            let filled = trimmedOptions
            return filled.count >= 2 && filled.count <= 4 && filled.allSatisfy { !$0.isEmpty && $0.count <= 40 }
        }
    }

    /// One part of a thread after the first. Text only — see ``payload()``
    /// for why the head is the only part that carries media.
    public struct Segment: Identifiable, Sendable, Equatable {
        public let id: UUID
        public var body: String

        public init(id: UUID = UUID(), body: String = "") {
            self.id = id
            self.body = body
        }

        public var trimmed: String { body.trimmingCharacters(in: .whitespacesAndNewlines) }
    }

    public static let maxBodyLength = 2000
    /// Pictures and a GIF together. The web's media strip holds four tiles
    /// whatever they are.
    public static let maxAttachments = 4
    /// How many times the transcoder is asked about a video before the
    /// composer gives up on it: the web's 360 tries at five seconds, half an
    /// hour. A transcode that long is one the server should be explaining.
    public static let videoPollLimit = 360
    /// Head included. The server has no cap, but every part is a signed
    /// request and a thread this long is one the author should be writing
    /// somewhere with a table of contents.
    public static let maxThreadParts = 25

    /// How far ahead a work has to be scheduled, from now.
    ///
    /// The server has no floor of its own: `workEvent` parses `scheduled_at`
    /// and asks only whether it is after the insert, and the sweep that
    /// publishes due works runs once a minute. A time thirty seconds out would
    /// be stored, hidden, and published by the next tick — a slower way of
    /// posting now. Thirty minutes is the web composer's floor, kept so the
    /// two agree about what "scheduled" means.
    public static let scheduleFloor: TimeInterval = 30 * 60

    public let mode: Mode
    public var body = ""
    /// The parts after the first. Empty for an ordinary work.
    public private(set) var segments: [Segment] = []
    public private(set) var attachments: [Attachment] = []
    /// One video, exclusive with pictures, a GIF and a poll — the web's rule.
    public private(set) var video: VideoAttachment?
    /// One voice note. It sits beside anything else on the draft, and when it
    /// is there the work is a voice work — see ``kind``.
    public private(set) var voice: VoiceAttachment?
    /// One GIF, from the picker. It joins `media_urls` as the web's does, and
    /// takes one of the four media slots.
    public private(set) var gif: GifResult?
    /// How long between two questions to the transcoder. Five seconds is the
    /// web's; a test sets it to almost nothing.
    public var videoPollInterval: Duration = .seconds(5)
    public var poll: PollDraft?
    public var commentGating: CommentGating = .default
    public var isNSFW = false
    public var subscriberOnly = false
    /// When the work goes public, or `nil` for as soon as it is accepted.
    public private(set) var scheduledAt: Date?

    /// The draft this was opened from, so saving again replaces it and
    /// publishing removes it.
    public private(set) var draftID: UUID?

    public private(set) var isSubmitting = false
    /// The last refusal, in the server's own words where it had any.
    public private(set) var failure: String?
    /// Set when a submission may have landed. The reader is told, and asked to
    /// look before posting again — see `WorkSubmission`.
    public private(set) var uncertain: WorkSubmission.Outcome?

    /// Parts of this thread the server has already accepted, in order. They
    /// are live; nothing here can take them back.
    public private(set) var publishedParts: [WorkAccepted] = []
    /// The CID the next part continues from, once part of a thread has landed.
    /// Restored from a draft too, so a thread kept for later still continues
    /// the one it started.
    public private(set) var continuesThreadFrom: String?

    private let client: APIClient
    private let signer: MalkuthSigner
    private let identity: AuthenticatedIdentity
    /// Built once. A retry sends these bytes and never re-signs.
    private var submission: WorkSubmission?
    /// The same rule for a thread: every part signed once, resent as signed.
    private var threadSubmission: ThreadSubmission?
    /// The upload-then-poll of the current video, so removing the video can
    /// stop it rather than leave it running against a tile that is gone.
    private var videoTask: Task<Void, Never>?

    public init(mode: Mode, client: APIClient, signer: MalkuthSigner, identity: AuthenticatedIdentity) {
        self.mode = mode
        self.client = client
        self.signer = signer
        self.identity = identity
    }

    // MARK: - Validity

    public var remaining: Int { Self.maxBodyLength - body.count }

    /// Characters left in one part after the first.
    public func remaining(in segmentID: UUID) -> Int {
        Self.maxBodyLength - (segments.first { $0.id == segmentID }?.body.count ?? 0)
    }

    private var trimmedBody: String { body.trimmingCharacters(in: .whitespacesAndNewlines) }

    public var hasContent: Bool {
        !trimmedBody.isEmpty
            || attachments.contains { $0.remotePath != nil }
            || video?.renditions != nil
            || voice?.remotePath != nil
            || gif != nil
            || (poll?.isValid ?? false)
            || segments.contains { !$0.trimmed.isEmpty }
    }

    /// Whether this is a thread: parts after the first, or the rest of one
    /// that already began.
    public var isThread: Bool { !segments.isEmpty || continuesThreadFrom != nil }

    /// Why the draft cannot be sent as scheduled, if it cannot. A time that
    /// slipped into the past while the draft was open would not fail on the
    /// server — it would publish immediately, which is not what was asked.
    public var scheduleProblem: String? {
        guard let scheduledAt, scheduledAt <= Date() else { return nil }
        return "The scheduled time has passed. Pick another, or remove the schedule to post now."
    }

    public var canSubmit: Bool {
        guard !isSubmitting, remaining >= 0, hasContent else { return false }
        if attachments.contains(where: \.isUploading) { return false }
        if attachments.contains(where: { $0.failure != nil }) { return false }
        // A video on the draft has to be one the work can carry. Uploading,
        // processing, failed, or waiting on the author's word about a
        // duplicate — none of those is a master URL.
        if let video, video.renditions == nil { return false }
        if let voice, voice.remotePath == nil { return false }
        if let poll, !poll.isValid { return false }
        // A poll is a question with options; options alone are not a work.
        if poll != nil, trimmedBody.isEmpty { return false }
        if scheduleProblem != nil { return false }
        if !segments.isEmpty {
            // A thread opens with words. The head is the part the feed shows,
            // and a blank one followed by three parts of text is a thread the
            // reader cannot find.
            if trimmedBody.isEmpty { return false }
            // Every part is a work of its own and is held to a work's limits.
            // An empty part is not dropped on the author's behalf: it is on
            // the screen, and removing it is a tap.
            if segments.contains(where: { $0.trimmed.isEmpty || $0.body.count > Self.maxBodyLength }) { return false }
        }
        return true
    }

    /// Whether another picture can be added. The four slots are shared with a
    /// GIF; a video or a poll takes the whole strip.
    public var canAttach: Bool { mediaSlotsUsed < Self.maxAttachments && poll == nil && video == nil }
    /// One video, on its own: no pictures, no GIF, no poll beside it. A video
    /// work is the video; the web's compose box stops taking files once one is
    /// a video, and the feed's card has one place for it.
    public var canAttachVideo: Bool { video == nil && attachments.isEmpty && gif == nil && poll == nil }
    /// One GIF. It is a picture as far as the slots and the poll are concerned.
    public var canAttachGIF: Bool { gif == nil && canAttach }
    /// One voice note. It is exclusive only with a poll: a poll is a question
    /// with options and a voice note would make the work a voice work, and the
    /// server stores one kind.
    public var canRecordVoice: Bool { voice == nil && poll == nil }
    public var canAddPoll: Bool {
        attachments.isEmpty && gif == nil && video == nil && voice == nil && mode.parent == nil && !isThread
    }

    private var mediaSlotsUsed: Int { attachments.count + (gif == nil ? 0 : 1) }

    /// What Post is waiting on, when it is waiting on an upload rather than on
    /// the author — the label the button wears instead of "Post", so a disabled
    /// button says why. Short, because it sits in a toolbar pill: the tile
    /// under the text has the room to say which upload. `nil` when nothing is
    /// in flight.
    public var blockingActivity: String? {
        if let video {
            switch video.state {
            case .uploading: return "Uploading…"
            case .processing: return "Processing…"
            case .ready, .duplicate, .failed: break
            }
        }
        if let voice, voice.isUploading { return "Uploading…" }
        if attachments.contains(where: \.isUploading) { return "Uploading…" }
        return nil
    }
    public var canSchedule: Bool { mode.isPost }
    /// Whether another part can be added. A poll is one work by construction —
    /// its options belong to the head — so a poll and a thread are exclusive.
    public var canAddSegment: Bool {
        mode.isPost && poll == nil && publishedParts.count + segments.count + 1 < Self.maxThreadParts
    }
    /// Only a new work is kept for later. A reply answers a specific work and
    /// a quote is about one; a draft holds text and cannot hold the work it
    /// was addressed to, so restoring either would publish it as something
    /// else.
    public var canSaveDraft: Bool { mode.isPost }

    /// What the server will store this as. Derived, not chosen: a draft with a
    /// poll is a poll, a draft answering a work is a reply, a draft in parts
    /// is a thread, and so on.
    ///
    /// The order is the web composer's, in `f33d3r.js`: a voice note makes it
    /// `voice` before anything else is considered, a video makes it `video`,
    /// and only then does a reply parent or a quoted work name it. So a voice
    /// reply is a `voice` work with a `parent_cid`, exactly as the web posts
    /// one. A thread outranks all of it because the kind of a chain belongs to
    /// the chain — ``ThreadSubmission`` signs every part `thread_post`, and the
    /// head's video or voice rides along as the attachment it is.
    public var kind: WorkKind {
        if isThread { return .threadPost }
        if voice != nil { return .voice }
        if video != nil { return .video }
        switch mode {
        case .reply: return .reply
        case .quote: return .quote
        case .post: return poll != nil ? .poll : .post
        }
    }

    // MARK: - Attachments

    /// Adds an image and uploads it straight away.
    public func attach(_ data: Data, filename: String, mimeType: String) async {
        guard canAttach else { return }
        let attachment = Attachment(id: UUID(), data: data, filename: filename, mimeType: mimeType)
        attachments.append(attachment)
        await upload(attachment.id)
    }

    /// Retries a failed upload. An attachment restored from a draft has no
    /// bytes to send and cannot have failed, so there is nothing to retry.
    public func retryUpload(_ id: UUID) async {
        guard let index = attachments.firstIndex(where: { $0.id == id }), attachments[index].data != nil else { return }
        attachments[index].failure = nil
        await upload(id)
    }

    public func removeAttachment(_ id: UUID) {
        attachments.removeAll { $0.id == id }
    }

    private func upload(_ id: UUID) async {
        guard let attachment = attachments.first(where: { $0.id == id }), let data = attachment.data else { return }
        do {
            let path = try await client.uploadImage(data, filename: attachment.filename, mimeType: attachment.mimeType)
            if let index = attachments.firstIndex(where: { $0.id == id }) {
                attachments[index].remotePath = path
            }
        } catch let error as APIError {
            if let index = attachments.firstIndex(where: { $0.id == id }) {
                attachments[index].failure = error.userMessage
            }
        } catch {
            if let index = attachments.firstIndex(where: { $0.id == id }) {
                attachments[index].failure = error.localizedDescription
            }
        }
    }

    // MARK: - Video

    /// Attaches a video and starts it on its way: upload, then ask the
    /// transcoder about it until it is settled.
    ///
    /// Returns at once; the tile follows ``video``'s state. The work runs in a
    /// task the composer owns, so ``removeVideo()`` can stop it — a video the
    /// author has taken off the draft should not go on costing them bandwidth,
    /// and its answers, when they came, would have nowhere to go. A caller
    /// that needs the outcome — a test, or a sheet deciding whether to close —
    /// awaits ``settleVideo()``.
    public func attachVideo(fileURL: URL, filename: String, mimeType: String) {
        guard canAttachVideo else { return }
        let attachment = VideoAttachment(fileURL: fileURL, filename: filename, mimeType: mimeType, state: .uploading(0))
        video = attachment
        videoTask?.cancel()
        videoTask = Task { [weak self] in await self?.uploadVideo(attachment.id) }
    }

    /// Waits for the video's upload and transcode to reach a settled state.
    public func settleVideo() async {
        await videoTask?.value
    }

    /// Sends a failed video again. Only one picked this session has bytes to
    /// send; one restored from a draft was already transcoded and cannot fail.
    public func retryVideo() {
        guard let current = video, case .failed = current.state, current.fileURL != nil else { return }
        video?.state = .uploading(0)
        video?.uploadID = nil
        videoTask?.cancel()
        videoTask = Task { [weak self] in await self?.uploadVideo(current.id) }
    }

    /// Takes the video off the draft and stops whatever it was doing. The
    /// picker's copy of the file goes too; nothing references it now.
    public func removeVideo() {
        videoTask?.cancel()
        videoTask = nil
        Self.removeLocalFile(video?.fileURL)
        video = nil
    }

    /// The author's answer to a duplicate: post it anyway, referencing the
    /// original's renditions, with the credit staying where the server put it.
    /// Nothing to accept when the server named no renditions.
    public func acceptDuplicateVideo() {
        guard let current = video,
              case .duplicate(let handle, _, let renditions) = current.state,
              var renditions
        else { return }
        renditions.creditedTo = handle.isEmpty ? nil : handle
        video?.state = .ready(renditions)
    }

    private func uploadVideo(_ id: UUID) async {
        guard let attachment = video, attachment.id == id, let fileURL = attachment.fileURL else { return }
        let job: VideoJob
        do {
            job = try await client.uploadVideo(
                fileURL: fileURL, filename: attachment.filename, mimeType: attachment.mimeType
            ) { [weak self] fraction in
                // Reported from the transport's thread; the tile is main-actor.
                Task { @MainActor in self?.setVideoState(id, .uploading(fraction)) }
            }
        } catch {
            if Task.isCancelled { return }
            setVideoState(id, .failed(Self.message(for: error)))
            return
        }
        guard apply(job, to: id), let uploadID = job.uploadID, !uploadID.isEmpty else { return }
        setVideoUploadID(id, uploadID)

        // The web's cadence: ask every five seconds, up to half an hour, and
        // keep asking through a network blip — the job is still there whether
        // or not this phone could reach it. Anything the server *says* is
        // acted on: a 404 means the upload is gone, not that it is slow.
        for _ in 0..<Self.videoPollLimit {
            do {
                try await Task.sleep(for: videoPollInterval)
            } catch {
                return
            }
            guard video?.id == id else { return }
            do {
                let update = try await client.videoJob(id: uploadID)
                if !apply(update, to: id) { return }
            } catch let error as APIError where Self.isTransient(error) {
                continue
            } catch {
                if Task.isCancelled { return }
                setVideoState(id, .failed(Self.message(for: error)))
                return
            }
        }
        setVideoState(id, .failed("F33D3R was still processing this video after \(Self.videoPollLimit * 5 / 60) minutes. Remove it and try again."))
    }

    /// Puts the server's answer onto the tile. Returns true when the answer
    /// means there is more to wait for.
    @discardableResult
    private func apply(_ job: VideoJob, to id: UUID) -> Bool {
        guard let current = video, current.id == id else { return false }
        switch job.status {
        case .queued, .processing:
            setVideoState(id, .processing)
            return true
        case .ready:
            if let renditions = VideoRenditions(job: job, uploadID: job.uploadID ?? current.uploadID) {
                setVideoState(id, .ready(renditions))
            } else {
                setVideoState(id, .failed("The transcoder reported the video ready but gave no stream for it."))
            }
            return false
        case .failed:
            let reason = job.error?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
            setVideoState(id, .failed(reason.isEmpty ? "The transcoder failed without saying why." : reason))
            return false
        case .duplicate:
            setVideoState(id, .duplicate(
                handle: job.duplicateOfHandle ?? "",
                workID: job.duplicateOfWorkID,
                renditions: VideoRenditions(job: job, uploadID: nil)
            ))
            return false
        }
    }

    private func setVideoState(_ id: UUID, _ state: VideoAttachment.State) {
        guard video?.id == id else { return }
        video?.state = state
    }

    private func setVideoUploadID(_ id: UUID, _ uploadID: String) {
        guard video?.id == id else { return }
        video?.uploadID = uploadID
    }

    /// Whether an error is the kind the web's poll loop `continue`s past: the
    /// request did not get through, or the server was having a moment. A
    /// refusal it did make — not found, unauthenticated — is acted on.
    private static func isTransient(_ error: APIError) -> Bool {
        switch error {
        case .transport, .unreachable: return true
        case .unexpectedStatus(let status): return status >= 500 || status == 429
        case .api(let status, _): return status >= 500 || status == 429
        case .decoding: return false
        }
    }

    // MARK: - Voice

    /// Attaches a recording and uploads it. The length is the recorder's, in
    /// whole seconds, because the server will not measure it.
    public func attachVoice(fileURL: URL, durationSecs: Int, filename: String, mimeType: String) async {
        guard canRecordVoice else { return }
        voice = VoiceAttachment(fileURL: fileURL, filename: filename, mimeType: mimeType, durationSecs: durationSecs)
        await uploadVoice()
    }

    public func retryVoice() async {
        guard let current = voice, current.failure != nil, current.fileURL != nil else { return }
        voice?.failure = nil
        await uploadVoice()
    }

    public func removeVoice() {
        Self.removeLocalFile(voice?.fileURL)
        voice = nil
    }

    private func uploadVoice() async {
        guard let current = voice, let fileURL = current.fileURL else { return }
        do {
            // A recording is seconds to minutes of AAC; holding it is fine.
            let data = try Data(contentsOf: fileURL)
            let uploaded = try await client.uploadVoice(data, filename: current.filename, mimeType: current.mimeType)
            // The author may have removed it while it was going up.
            guard voice?.fileURL == fileURL else { return }
            voice?.remotePath = uploaded.url
            // The bytes are the server's now; the recorder's file has done
            // its work. A retry only happens on a failure, which has none.
            Self.removeLocalFile(fileURL)
        } catch {
            guard voice?.fileURL == fileURL else { return }
            voice?.failure = Self.message(for: error)
        }
    }

    // MARK: - GIF

    /// Attaches a GIF from the picker. Nothing to upload: the URL is the
    /// provider's and joins `media_urls` as it is, the way the web attaches one.
    public func attachGIF(_ result: GifResult) {
        guard canAttachGIF else { return }
        gif = result
    }

    public func removeGIF() {
        gif = nil
    }

    /// One wording for an upload's failure, whoever failed: the server's
    /// message when it sent one, the transport's otherwise.
    private static func message(for error: any Error) -> String {
        if let error = error as? APIError { return error.userMessage }
        return error.localizedDescription
    }

    // MARK: - Poll

    public func addPoll() {
        guard canAddPoll, poll == nil else { return }
        poll = PollDraft()
    }

    public func removePoll() {
        poll = nil
    }

    public func addPollOption() {
        guard var draft = poll, draft.options.count < 4 else { return }
        draft.options.append("")
        poll = draft
    }

    public func removePollOption(at index: Int) {
        guard var draft = poll, draft.options.count > 2, draft.options.indices.contains(index) else { return }
        draft.options.remove(at: index)
        poll = draft
    }

    // MARK: - Schedule

    /// The soonest a work opened now can be scheduled for.
    public static func earliestSchedule(from now: Date = Date()) -> Date {
        now.addingTimeInterval(scheduleFloor)
    }

    /// Schedules the work, to the minute.
    ///
    /// Refuses a time under the floor rather than rounding it up: the picker
    /// already will not offer one, so being handed one means the clock moved
    /// while the sheet was open, and the author should see the time they are
    /// actually getting. Seconds are dropped because the picker does not show
    /// them and a stored `14:30:41` would be a surprise on the profile.
    @discardableResult
    public func schedule(for date: Date, now: Date = Date()) -> Bool {
        guard canSchedule else { return false }
        let minute = Date(timeIntervalSince1970: (date.timeIntervalSince1970 / 60).rounded(.down) * 60)
        guard minute >= Self.earliestSchedule(from: now).addingTimeInterval(-59) else { return false }
        scheduledAt = minute
        return true
    }

    public func clearSchedule() {
        scheduledAt = nil
    }

    // MARK: - Thread

    /// Adds an empty part after the last, and returns it so the screen can
    /// put the caret in it.
    @discardableResult
    public func addSegment() -> Segment? {
        guard canAddSegment else { return nil }
        let segment = Segment()
        segments.append(segment)
        return segment
    }

    public func removeSegment(_ id: UUID) {
        segments.removeAll { $0.id == id }
    }

    public func segmentBody(_ id: UUID) -> String {
        segments.first { $0.id == id }?.body ?? ""
    }

    public func setSegmentBody(_ id: UUID, _ text: String) {
        guard let index = segments.firstIndex(where: { $0.id == id }) else { return }
        segments[index].body = text
    }

    /// Turns the body into a thread, one part per paragraph, as the web's
    /// "Make it a thread" does. Anything past the part limit stays in the last
    /// part rather than being lost.
    @discardableResult
    public func makeThread(from paragraphs: [String]) -> Bool {
        guard canAddSegment, paragraphs.count >= 2 else { return false }
        let room = Self.maxThreadParts - publishedParts.count - 1
        var rest = Array(paragraphs.dropFirst())
        if rest.count > room {
            let overflow = rest[(room - 1)...].joined(separator: "\n\n")
            rest = Array(rest.prefix(room - 1)) + [overflow]
        }
        body = paragraphs[0]
        segments = rest.map { Segment(body: $0) }
        return true
    }

    // MARK: - Drafts

    /// The draft as it stands, ready to be written. Names itself on first
    /// use so a later save replaces this file and not a new one.
    public func draft(savedAt: Date = Date()) -> ComposeDraft {
        let id = draftID ?? UUID()
        draftID = id
        return ComposeDraft(
            id: id,
            savedAt: savedAt,
            body: body,
            segments: segments.map(\.body),
            subscriberOnly: subscriberOnly,
            isNSFW: isNSFW,
            commentGating: commentGating,
            poll: poll.map { ComposeDraft.Poll(options: $0.options, durationSeconds: Int($0.duration.components.seconds)) },
            scheduledAt: scheduledAt,
            attachments: attachments.compactMap { attachment in
                attachment.remotePath.map {
                    ComposeDraft.Attachment(remotePath: $0, filename: attachment.filename, mimeType: attachment.mimeType)
                }
            },
            // Only what the server has finished with. A video still uploading
            // or transcoding, a voice note still going up, have no path yet.
            video: video.flatMap { attachment in
                attachment.renditions.map { ComposeDraft.Video(renditions: $0, filename: attachment.filename) }
            },
            voice: voice.flatMap { attachment in
                attachment.remotePath.map { ComposeDraft.Voice(remotePath: $0, durationSecs: attachment.durationSecs) }
            },
            gif: gif,
            continuesThreadFrom: continuesThreadFrom
        )
    }

    /// Replaces the draft with a saved one. Only a new work can take one —
    /// see ``canSaveDraft``.
    ///
    /// A schedule already in the past comes back as it was and is reported by
    /// ``scheduleProblem`` rather than dropped: the author chose it, and the
    /// composer should say it is stale instead of quietly turning a scheduled
    /// work into an immediate one.
    public func restore(_ draft: ComposeDraft) {
        guard canSaveDraft else { return }
        draftID = draft.id
        body = draft.body
        segments = draft.segments.map { Segment(body: $0) }
        subscriberOnly = draft.subscriberOnly
        isNSFW = draft.isNSFW
        commentGating = draft.commentGating
        poll = draft.poll.map { PollDraft(options: $0.options, duration: .seconds($0.durationSeconds)) }
        scheduledAt = draft.scheduledAt
        attachments = draft.attachments.map {
            Attachment(id: UUID(), data: nil, filename: $0.filename, mimeType: $0.mimeType, remotePath: $0.remotePath)
        }
        removeVideo()
        video = draft.video.map {
            VideoAttachment(fileURL: nil, filename: $0.filename, mimeType: "video/mp4", state: .ready($0.renditions), uploadID: $0.renditions.uploadID)
        }
        voice = draft.voice.map {
            VoiceAttachment(fileURL: nil, filename: "voice.m4a", mimeType: "audio/mp4", durationSecs: $0.durationSecs, remotePath: $0.remotePath)
        }
        gif = draft.gif
        continuesThreadFrom = draft.continuesThreadFrom
        // A restored draft is new bytes; whatever was signed before it is not
        // what is on the screen now.
        submission = nil
        threadSubmission = nil
        uncertain = nil
        failure = nil
    }

    // MARK: - Submit

    /// Publishes the draft. Signs once on the first call; a later call after
    /// an uncertain outcome resends the same bytes.
    ///
    /// For a thread the accepted row returned is the **head's**, whichever
    /// call landed it — that is the work the feed shows, and the one the
    /// caller should fetch back.
    public func submit() async -> WorkSubmission.Outcome? {
        guard canSubmit || uncertain != nil else { return nil }
        isSubmitting = true
        failure = nil
        defer { isSubmitting = false }

        do {
            try await signer.registerIfNeeded(with: client)
        } catch let error as MalkuthError {
            failure = error.isKeyAuthorityUnavailable
                ? "The key authority is unreachable, so this device cannot sign yet."
                : error.description
            return .rejected(error)
        } catch {
            failure = error.localizedDescription
            return nil
        }

        if isThread { return await submitThread() }

        if submission == nil {
            do {
                let renditions = video?.renditions
                submission = try await WorkSubmission.prepare(
                    payload(),
                    signer: signer,
                    isNSFW: isNSFW,
                    videoWatermarkedURL: renditions?.watermarkedURL,
                    videoWidth: renditions?.width,
                    videoHeight: renditions?.height,
                    quotedWorkID: mode.quoted?.id,
                    tusUploadID: renditions?.uploadID
                )
            } catch let error as MalkuthError {
                failure = error.description
                return .rejected(error)
            } catch {
                failure = error.localizedDescription
                return nil
            }
        }

        let outcome = await submission!.sendWithRetries(using: client)
        switch outcome {
        case .accepted:
            uncertain = nil
        case .rejected(let error):
            failure = Self.message(for: error)
            // A refusal is final for these bytes; a corrected draft is a new work.
            submission = nil
        case .uncertain(_, let reason):
            uncertain = outcome
            failure = "F33D3R didn't confirm the post (\(reason)). It may have gone through — check your profile before posting again, or retry to send the same post."
        }
        return outcome
    }

    /// Publishes the parts in order and keeps whatever did not land.
    private func submitThread() async -> WorkSubmission.Outcome? {
        if threadSubmission == nil {
            do {
                let renditions = video?.renditions
                threadSubmission = try await ThreadSubmission.prepare(
                    head: payload(),
                    continuations: segments.map(\.trimmed),
                    signer: signer,
                    isNSFW: isNSFW,
                    videoWatermarkedURL: renditions?.watermarkedURL,
                    videoWidth: renditions?.width,
                    videoHeight: renditions?.height,
                    tusUploadID: renditions?.uploadID
                )
            } catch let error as MalkuthError {
                failure = error.description
                return .rejected(error)
            } catch {
                failure = error.localizedDescription
                return nil
            }
        }

        // Copied out and back rather than mutated in place across the await:
        // a stored property cannot be held exclusively for the duration.
        var chain = threadSubmission!
        let outcome = await chain.publish(using: client)
        threadSubmission = chain

        switch outcome {
        case .published(let landed):
            publishedParts += landed
            uncertain = nil
            threadSubmission = nil
            // What was on the screen is now on the server, and the thread is
            // finished: whatever is typed next is a new work, not another part.
            body = ""
            segments = []
            clearMedia()
            continuesThreadFrom = nil
            return .accepted(publishedParts[0])

        case .stopped(let landed, let stop):
            publishedParts += landed
            let stoppedAt = publishedParts.count + 1
            let total = publishedParts.count + remainingParts(afterDropping: landed.count)
            dropPublishedParts(landed.count)

            switch stop {
            case .rejected(let error):
                failure = Self.threadStopMessage(
                    published: publishedParts.count, total: total,
                    detail: Self.message(for: error)
                )
                // These bytes are refused for good; a corrected draft is a new
                // chain, addressed to the last part that landed.
                threadSubmission = nil
                return .rejected(error)
            case .uncertain(let cid, let reason):
                let outcome = WorkSubmission.Outcome.uncertain(cid: cid, reason: reason)
                uncertain = outcome
                failure = Self.threadStopMessage(
                    published: publishedParts.count, total: total,
                    detail: "F33D3R didn't confirm part \(stoppedAt) (\(reason)). It may have gone through. Retry sends that same part again, which cannot post it twice."
                )
                return outcome
            }
        }
    }

    /// How many parts are still on the screen once `count` of them have gone.
    private func remainingParts(afterDropping count: Int) -> Int {
        max(0, 1 + segments.count - count)
    }

    /// Removes the parts that landed from the front of the draft. The first
    /// part still here becomes the head, addressed to the last part published;
    /// the media went with the original head and is the server's now.
    private func dropPublishedParts(_ count: Int) {
        guard count > 0 else { return }
        continuesThreadFrom = publishedParts.last?.cid
        clearMedia()
        // Parts are [body] + segments; index `count` is the first unpublished.
        if count - 1 < segments.count {
            body = segments[count - 1].body
            segments = Array(segments[count...])
        } else {
            body = ""
            segments = []
        }
    }

    /// Everything the head carried, once the head is the server's.
    private func clearMedia() {
        attachments = []
        removeVideo()
        removeVoice()
        gif = nil
    }

    /// Removes the files the recorder and the picker left in the temporary
    /// directory for this draft. The sheet calls it when the draft is
    /// abandoned or has posted; a draft saved for later keeps only the
    /// server's paths, so the local copies go then too.
    public func discardLocalFiles() {
        Self.removeLocalFile(video?.fileURL)
        Self.removeLocalFile(voice?.fileURL)
    }

    /// Deletes a temporary file this draft was handed. Only a file URL, and
    /// only quietly: a file already gone is the outcome wanted.
    private static func removeLocalFile(_ url: URL?) {
        guard let url, url.isFileURL else { return }
        try? FileManager.default.removeItem(at: url)
    }

    private static func threadStopMessage(published: Int, total: Int, detail: String) -> String {
        let live: String
        switch published {
        case 0: live = "Nothing from this thread has been published yet."
        case 1: live = "1 of \(total) parts of this thread is live on your profile."
        default: live = "\(published) of \(total) parts of this thread are live on your profile."
        }
        let rest = published == 0
            ? ""
            : " The rest stayed here — post again to continue the thread from where it stopped."
        return "\(live) \(detail)\(rest)"
    }

    private func payload() throws -> WorkPayload {
        let pial = try identity.requirePIAL()
        // Media rides on the head only. The feed shows a thread by its first
        // part, so that is where a picture is seen; the web's threads carry no
        // media at all, and the one list of attachments on this screen has no
        // way to say which part a picture belongs to. One rule, stated once:
        // the head has the pictures, the rest have the words.
        var media: [String] = []
        for attachment in attachments {
            if let path = attachment.remotePath { media.append(path) }
        }
        // A GIF is a media URL like any picture — the web's picker stores it
        // as one — just not one this server hosts.
        if let gif { media.append(gif.url) }
        // The signed half of a video. The rest rides on the envelope; see
        // `submit()` and `WorkEnvelope`.
        let renditions = video?.renditions
        var pollOptions: [String]?
        var pollEndsAt: String?
        if let poll, kind == .poll {
            pollOptions = poll.trimmedOptions
            // A poll runs from when it is seen, not from when it was written:
            // a scheduled poll closes `duration` after it publishes.
            let opens = scheduledAt ?? Date()
            pollEndsAt = WorkPayload.rfc3339String(opens.addingTimeInterval(TimeInterval(poll.duration.components.seconds)))
        }
        return WorkPayload(
            authorPIAL: pial,
            body: trimmedBody,
            commentGating: commentGating,
            kind: kind,
            mediaURLs: media,
            parentCID: continuesThreadFrom ?? mode.parent?.cid,
            pollEndsAt: pollEndsAt,
            pollOptions: pollOptions,
            scheduledAt: scheduledAt.map(WorkPayload.rfc3339String),
            subscriberOnly: subscriberOnly,
            timestampMS: WorkPayload.nowMS(),
            videoDurationSecs: renditions?.durationSecs,
            videoMasterURL: renditions?.masterURL,
            videoPosterURL: renditions?.posterURL,
            voiceDurationSecs: voice?.remotePath == nil ? nil : voice?.durationSecs,
            voiceURL: voice?.remotePath
        )
    }

    #if DEBUG
    /// Puts a video on the draft in a given state without a server, for
    /// looking at the tile in states a Simulator with no camera roll and no
    /// transcoder can never reach. Nothing here is uploaded or posted.
    public func debugStageVideo(_ attachment: VideoAttachment) {
        removeVideo()
        video = attachment
    }
    #endif

    private static func message(for error: MalkuthError) -> String {
        switch error {
        case .rejected(let status, let reason):
            switch reason.trimmingCharacters(in: .whitespacesAndNewlines) {
            case "signature invalid":
                return "F33D3R couldn't verify this device's signature. Try again; the key is re-registered on each attempt."
            case "replies are restricted on this work":
                return "The author has limited who can reply to this work."
            case let text where status == 401:
                return text.isEmpty ? "Your session has expired. Sign in again." : text
            case let text:
                return text.isEmpty ? "F33D3R refused the post (HTTP \(status))." : text
            }
        default:
            return error.description
        }
    }
}
