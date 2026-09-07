import Foundation
import Observation

/// A work that has not been sent yet, kept on the device so it can be finished
/// later.
///
/// This is the one piece of composer state the server never sees, and keeping
/// it here does not bend the rule that the server owns the truth: a draft is
/// input the author has not submitted, the same as text sitting in the text
/// view. Nothing in it claims anything about what exists on F33D3R. The moment
/// it is published it is deleted, and from then on the work is the server's
/// row and nothing else.
///
/// The web keeps only the body, in IndexedDB. This keeps everything the compose
/// screen can hold except the media bytes: an attachment is uploaded the moment
/// it is added, so the path the server answered with is the thing worth saving,
/// and the bytes are the server's already. A draft with a photo therefore
/// reopens showing the uploaded image, fetched back the way any card fetches
/// one; a draft with a video reopens showing its poster, with the transcode
/// already done; a voice note reopens as the length it was. Only what the
/// server has finished with is kept — an upload still in flight has no path
/// yet, and a draft cannot hold a promise.
public struct ComposeDraft: Codable, Identifiable, Sendable, Equatable {

    /// An uploaded image, by the path the server knows it as.
    public struct Attachment: Codable, Sendable, Equatable {
        public var remotePath: String
        public var filename: String
        public var mimeType: String

        public init(remotePath: String, filename: String, mimeType: String) {
            self.remotePath = remotePath
            self.filename = filename
            self.mimeType = mimeType
        }
    }

    /// A transcoded video, by the renditions the transcoder answered with —
    /// the same set the work will carry, see ``WorkComposer/VideoAttachment``.
    public struct Video: Codable, Sendable, Equatable {
        public var renditions: WorkComposer.VideoRenditions
        public var filename: String

        public init(renditions: WorkComposer.VideoRenditions, filename: String) {
            self.renditions = renditions
            self.filename = filename
        }
    }

    /// An uploaded voice note, by its path and the length the recorder
    /// measured — the server does not know the length; see ``VoiceUpload``.
    public struct Voice: Codable, Sendable, Equatable {
        public var remotePath: String
        public var durationSecs: Int

        public init(remotePath: String, durationSecs: Int) {
            self.remotePath = remotePath
            self.durationSecs = durationSecs
        }
    }

    /// A poll as it stood, by option text and the length it would run.
    public struct Poll: Codable, Sendable, Equatable {
        public var options: [String]
        public var durationSeconds: Int

        public init(options: [String], durationSeconds: Int) {
            self.options = options
            self.durationSeconds = durationSeconds
        }
    }

    public var id: UUID
    /// Milliseconds since 1970. An integer rather than a `Date` on purpose:
    /// a `Date` is a `Double` of seconds since 2001, every encoding of it goes
    /// through a conversion to 1970 and back, and the two do not always agree
    /// in the last bit — so a draft read from disk would compare unequal to
    /// the one that was just written. Whole milliseconds round-trip exactly.
    public var savedAtMS: Int64
    public var body: String
    /// The parts after the first, for a thread.
    public var segments: [String]
    public var subscriberOnly: Bool
    public var isNSFW: Bool
    public var commentGating: CommentGating
    public var poll: Poll?
    /// Milliseconds since 1970, for the same reason as ``savedAtMS``.
    public var scheduledAtMS: Int64?
    public var attachments: [Attachment]
    /// The video, once the transcoder had finished with it.
    public var video: Video?
    /// The voice note, once uploaded.
    public var voice: Voice?
    /// The GIF, as the picker handed it over. Nothing to upload — the URL is
    /// the provider's — so it is kept whole.
    public var gif: GifResult?
    /// The CID of a published thread part the rest of this draft continues
    /// from — set when a thread stopped half way and the author kept the rest.
    public var continuesThreadFrom: String?

    public init(
        id: UUID = UUID(),
        savedAt: Date = Date(),
        body: String = "",
        segments: [String] = [],
        subscriberOnly: Bool = false,
        isNSFW: Bool = false,
        commentGating: CommentGating = .default,
        poll: Poll? = nil,
        scheduledAt: Date? = nil,
        attachments: [Attachment] = [],
        video: Video? = nil,
        voice: Voice? = nil,
        gif: GifResult? = nil,
        continuesThreadFrom: String? = nil
    ) {
        self.id = id
        self.savedAtMS = Self.milliseconds(savedAt)
        self.body = body
        self.segments = segments
        self.subscriberOnly = subscriberOnly
        self.isNSFW = isNSFW
        self.commentGating = commentGating
        self.poll = poll
        self.scheduledAtMS = scheduledAt.map(Self.milliseconds)
        self.attachments = attachments
        self.video = video
        self.voice = voice
        self.gif = gif
        self.continuesThreadFrom = continuesThreadFrom
    }

    public var savedAt: Date { Self.date(savedAtMS) }
    public var scheduledAt: Date? { scheduledAtMS.map(Self.date) }

    private static func milliseconds(_ date: Date) -> Int64 {
        Int64((date.timeIntervalSince1970 * 1000).rounded())
    }

    private static func date(_ milliseconds: Int64) -> Date {
        Date(timeIntervalSince1970: TimeInterval(milliseconds) / 1000)
    }

    /// The line a list shows for this draft: the first line of the body, or a
    /// word for what the draft holds when it has no text yet.
    public var title: String {
        let trimmed = body.trimmingCharacters(in: .whitespacesAndNewlines)
        if let line = trimmed.split(whereSeparator: \.isNewline).first {
            return String(line)
        }
        if voice != nil { return "Voice note" }
        if video != nil { return "Video" }
        if !attachments.isEmpty { return attachments.count == 1 ? "Photo" : "\(attachments.count) photos" }
        if gif != nil { return "GIF" }
        if poll != nil { return "Poll" }
        if let first = segments.first(where: { !$0.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty }) {
            return first
        }
        return "Empty draft"
    }
}

/// The drafts on this device, one JSON file each.
///
/// Files rather than `UserDefaults` because a draft can be two thousand
/// characters times twenty-five parts, and defaults is a single plist read and
/// rewritten whole on every change. Application Support rather than Documents
/// because these are the app's working files, not the reader's, and should not
/// turn up in a file browser or a backup they did not ask for.
///
/// Kept per account. Two people sharing a phone should not find each other's
/// half-written works in the composer, so the directory is named for the
/// handle that was signed in when the draft was saved.
///
/// Every operation throws rather than returning a best effort. A draft that
/// could not be written is a draft the author believes exists and does not,
/// which is the one failure mode a drafts feature must never produce quietly.
@MainActor
@Observable
public final class ComposeDraftStore {

    /// Newest first.
    public private(set) var drafts: [ComposeDraft] = []

    @ObservationIgnored private let directory: URL
    @ObservationIgnored private let encoder: JSONEncoder = {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys, .prettyPrinted]
        return encoder
    }()
    @ObservationIgnored private let decoder = JSONDecoder()

    /// Some files could not be read. The ones that could are in ``drafts``.
    public struct UnreadableDrafts: Error, CustomStringConvertible {
        public let failures: [(file: String, error: any Error)]
        public var description: String {
            let names = failures.map { "\($0.file): \($0.error)" }.joined(separator: "; ")
            return "\(failures.count) draft file(s) could not be read — \(names)"
        }
    }

    public init(directory: URL) {
        self.directory = directory
    }

    /// `Application Support/F33D3R/drafts/<handle>`, created if it is not there.
    public static func directory(forHandle handle: String, fileManager: FileManager = .default) throws -> URL {
        let support = try fileManager.url(
            for: .applicationSupportDirectory, in: .userDomainMask, appropriateFor: nil, create: true
        )
        // A handle is `[A-Za-z0-9_]` on the server, but the path is built from
        // it, so anything else is replaced rather than trusted.
        let safe = String(handle.lowercased().map { $0.isLetter || $0.isNumber || $0 == "_" ? $0 : "_" })
        let url = support
            .appendingPathComponent("F33D3R", isDirectory: true)
            .appendingPathComponent("drafts", isDirectory: true)
            .appendingPathComponent(safe, isDirectory: true)
        try fileManager.createDirectory(at: url, withIntermediateDirectories: true)
        return url
    }

    /// Reads every draft file. A file that will not decode does not hide the
    /// others: the readable drafts are loaded first and the failures are then
    /// thrown together, so the caller can show both.
    public func reload() throws {
        let files = try FileManager.default.contentsOfDirectory(
            at: directory, includingPropertiesForKeys: nil, options: [.skipsHiddenFiles]
        ).filter { $0.pathExtension == "json" }

        var loaded: [ComposeDraft] = []
        var failures: [(file: String, error: any Error)] = []
        for file in files {
            do {
                loaded.append(try decoder.decode(ComposeDraft.self, from: Data(contentsOf: file)))
            } catch {
                failures.append((file.lastPathComponent, error))
            }
        }
        drafts = loaded.sorted { $0.savedAt > $1.savedAt }
        if !failures.isEmpty { throw UnreadableDrafts(failures: failures) }
    }

    /// Writes a draft, replacing one with the same id.
    ///
    /// Atomic, so a crash mid-write leaves the previous file rather than half
    /// of the new one.
    public func save(_ draft: ComposeDraft) throws {
        try encoder.encode(draft).write(to: url(for: draft.id), options: .atomic)
        drafts.removeAll { $0.id == draft.id }
        drafts.insert(draft, at: 0)
        drafts.sort { $0.savedAt > $1.savedAt }
    }

    /// Removes a draft. Removing one that is not there is not an error —
    /// the state asked for is the state that exists.
    public func delete(id: UUID) throws {
        let file = url(for: id)
        if FileManager.default.fileExists(atPath: file.path) {
            try FileManager.default.removeItem(at: file)
        }
        drafts.removeAll { $0.id == id }
    }

    private func url(for id: UUID) -> URL {
        directory.appendingPathComponent("\(id.uuidString).json", isDirectory: false)
    }
}
