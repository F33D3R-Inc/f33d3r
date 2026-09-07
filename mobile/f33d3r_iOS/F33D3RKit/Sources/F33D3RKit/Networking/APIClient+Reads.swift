import Foundation

/// The read calls that arrived with the Notifications, Wallet and compose screens.
///
/// Same shape as the calls in `APIClient.swift`: one method per endpoint,
/// decoded through the shared decoder, with nothing about the screen in here.
public extension APIClient {

    struct SignupRequest: Encodable, Sendable {
        public let handle: String
        public let password: String
        public let displayName: String
        public let deviceName: String

        enum CodingKeys: String, CodingKey {
            case handle, password
            case displayName = "display_name"
            case deviceName = "device_name"
        }

        public init(handle: String, password: String, displayName: String, deviceName: String) {
            self.handle = handle
            self.password = password
            self.displayName = displayName
            self.deviceName = deviceName
        }
    }

    /// Creates an account and signs it in.
    func signup(_ request: SignupRequest) async throws -> Session {
        try await send(.signup, body: request, as: Session.self)
    }

    /// One page of the reader's notifications.
    func notifications(cursor: String? = nil, limit: Int = 20) async throws -> NotificationPage {
        try await send(.notifications(cursor: cursor, limit: limit), body: Optional<Never>.none, as: NotificationPage.self)
    }

    /// The reader's wallet.
    func wallet() async throws -> WalletSnapshot {
        try await send(.wallet, body: Optional<Never>.none, as: WalletSnapshot.self)
    }

    /// The vision tray.
    func visions() async throws -> VisionTray {
        try await send(.visions, body: Optional<Never>.none, as: VisionTray.self)
    }

    /// Rooms live now.
    func liveRooms() async throws -> LiveList {
        try await send(.live, body: Optional<Never>.none, as: LiveList.self)
    }

    /// The scoreboard.
    func sports(league: String? = nil) async throws -> SportsBoard {
        try await send(.sports(league: league), body: Optional<Never>.none, as: SportsBoard.self)
    }

    /// One stock quote.
    func cashtag(ticker: String) async throws -> CashtagQuote {
        try await send(.cashtag(ticker: ticker), body: Optional<Never>.none, as: CashtagQuote.self)
    }

    /// Tickers matching `query`, for the composer's dropdown.
    func cashtagSearch(_ query: String) async throws -> [CashtagSuggestion] {
        struct Page: Decodable { let results: [CashtagSuggestion] }
        return try await send(.cashtagSearch(query), body: Optional<Never>.none, as: Page.self).results
    }

    /// One room with its chat backlog.
    func liveRoom(id: String) async throws -> LiveRoom {
        try await send(.liveRoom(id: id), body: Optional<Never>.none, as: LiveRoom.self)
    }

    /// Works and people matching `query`.
    func followers(handle: String, cursor: String? = nil) async throws -> UserPage {
        try await send(.followers(handle: handle, cursor: cursor), body: Optional<Never>.none, as: UserPage.self)
    }

    func following(handle: String, cursor: String? = nil) async throws -> UserPage {
        try await send(.following(handle: handle, cursor: cursor), body: Optional<Never>.none, as: UserPage.self)
    }

    func trendingTags() async throws -> [TagCount] {
        struct Page: Decodable { let tags: [TagCount] }
        return try await send(.trendingTags, body: Optional<Never>.none, as: Page.self).tags
    }

    func sessions() async throws -> [SessionInfo] {
        struct Page: Decodable { let sessions: [SessionInfo] }
        return try await send(.sessions, body: Optional<Never>.none, as: Page.self).sessions
    }

    func tagFeed(_ tag: String, cursor: String? = nil, limit: Int = 20) async throws -> WorkPage {
        try await send(.tagFeed(tag, cursor: cursor, limit: limit), body: Optional<Never>.none, as: WorkPage.self)
    }

    func search(_ query: String) async throws -> SearchResults {
        try await send(.search(query), body: Optional<Never>.none, as: SearchResults.self)
    }

    /// Uploads one image and returns the server path to reference it by.
    ///
    /// Multipart by hand rather than through a dependency: it is one boundary,
    /// one part, and the body has to be bytes this client controls so the
    /// upload is retried with identical content.
    func uploadImage(_ data: Data, filename: String, mimeType: String) async throws -> String {
        let part = MultipartPart(field: "file", filename: filename, mimeType: mimeType)
        let (response, status) = try await sendRaw(
            .uploadMedia, body: part.body(wrapping: data), contentType: part.contentType
        )
        struct Uploaded: Decodable { let url: String }
        return try Self.decodeUpload(Uploaded.self, from: response, status: status).url
    }

    // MARK: Video

    /// Hands a video file to the transcoder and returns the job to poll — or,
    /// when the bytes are already on F33D3R, a `duplicate` job naming the
    /// work that has them.
    ///
    /// The body is assembled on disk and streamed from there rather than read
    /// into memory: the multipart framing is a fixed prefix and suffix around
    /// the file, so the three are copied into one temporary file in pieces
    /// and `URLSession` uploads that file. A phone's video can be larger than
    /// the memory the app is allowed, and a `Data` of it is the process being
    /// killed mid-upload with nothing on screen to say why. The temporary file
    /// is removed when the request ends, however it ends.
    ///
    /// `progress` is called with the fraction of the body sent so far.
    func uploadVideo(
        fileURL: URL,
        filename: String,
        mimeType: String,
        progress: (@Sendable (Double) -> Void)? = nil
    ) async throws -> VideoJob {
        let part = MultipartPart(field: "file", filename: filename, mimeType: mimeType)
        let body = try part.spool(fileURL)
        defer { try? FileManager.default.removeItem(at: body) }
        let (response, status) = try await sendRawFile(
            .uploadVideo, fileURL: body, contentType: part.contentType, progress: progress
        )
        return try Self.decodeUpload(VideoJob.self, from: response, status: status)
    }

    /// The in-memory form of ``uploadVideo(fileURL:filename:mimeType:progress:)``,
    /// for a clip already held as bytes. Spooled to a file and sent the same
    /// way, so there is one upload path and one set of framing to get right.
    func uploadVideo(_ data: Data, filename: String, mimeType: String) async throws -> VideoJob {
        let spool = FileManager.default.temporaryDirectory
            .appendingPathComponent("f33d3r-video-\(UUID().uuidString)", isDirectory: false)
        try data.write(to: spool, options: .atomic)
        defer { try? FileManager.default.removeItem(at: spool) }
        return try await uploadVideo(fileURL: spool, filename: filename, mimeType: mimeType)
    }

    /// One upload's state. Poll until ``VideoJob/isSettled``.
    func videoJob(id: String) async throws -> VideoJob {
        try await send(.videoJob(id: id), body: Optional<Never>.none, as: VideoJob.self)
    }

    // MARK: Voice

    /// Uploads one voice recording. A recording is seconds to minutes of AAC,
    /// small enough to hold, so it goes as bytes under the field the web's
    /// form uses.
    func uploadVoice(_ data: Data, filename: String, mimeType: String) async throws -> VoiceUpload {
        let part = MultipartPart(field: "audio", filename: filename, mimeType: mimeType)
        let (response, status) = try await sendRaw(
            .uploadVoice, body: part.body(wrapping: data), contentType: part.contentType
        )
        return try Self.decodeUpload(VoiceUpload.self, from: response, status: status)
    }

    // MARK: GIFs

    /// GIFs matching `query`; trending when it is empty.
    func gifSearch(_ query: String) async throws -> [GifResult] {
        try await send(.gifSearch(query), body: Optional<Never>.none, as: GifSearchPage.self).results
    }

    // MARK: Frequencies

    /// One lane of the audio rooms. `lane` is `live`, `scheduled`, `ended` or
    /// `mine`; anything else is the server's to refuse.
    func frequencies(lane: String) async throws -> FrequencyList {
        try await send(.frequencies(lane: lane), body: Optional<Never>.none, as: FrequencyList.self)
    }

    /// One room, as this reader may see it.
    func frequency(id: String) async throws -> FrequencyRoom {
        try await send(.frequency(id: id), body: Optional<Never>.none, as: FrequencyRoom.self)
    }

    // MARK: - Settings

    /// Everyone this account has blocked, and everyone it has muted.
    func blocks() async throws -> BlockList {
        try await send(.blocks, body: Optional<Never>.none, as: BlockList.self)
    }

    /// What Herald may send this account.
    func notificationPreferences() async throws -> NotificationPrefs {
        try await send(.notificationPreferences, body: Optional<Never>.none, as: NotificationPrefs.self)
    }

    /// A fresh TOTP secret to enrol an authenticator with.
    func twoFactorSetup() async throws -> TwoFactorSetup {
        try await send(.twoFactorSetup, body: Optional<Never>.none, as: TwoFactorSetup.self)
    }

    /// The account's own data, as the bytes the server sent.
    ///
    /// Deliberately not decoded. The document is whatever F33D3R holds about
    /// this account on the day it is asked for, and its shape is the server's
    /// to grow; a model here would either have to be extended in lockstep or
    /// silently drop the parts it did not know about — from a file whose entire
    /// purpose is completeness. So the bytes go to the share sheet untouched.
    func accountExport() async throws -> Data {
        let (data, status) = try await sendRaw(.accountExport, body: nil, contentType: nil)
        guard (200..<300).contains(status) else {
            if let wrapped = try? APIClient.makeDecoder().decode(WrappedAPIError.self, from: data) {
                throw APIError.api(status: status, body: wrapped.error)
            }
            if let body = try? APIClient.makeDecoder().decode(APIErrorBody.self, from: data) {
                throw APIError.api(status: status, body: body)
            }
            throw APIError.unexpectedStatus(status)
        }
        return data
    }

    // MARK: Upload plumbing

    /// Reads an upload route's answer: the DTO on success, the server's own
    /// `code` and `message` on a refusal, and a decoding error — never a
    /// guess — when the body is neither.
    private static func decodeUpload<T: Decodable>(_ type: T.Type, from response: Data, status: Int) throws -> T {
        guard (200..<300).contains(status) else {
            if let wrapped = try? APIClient.makeDecoder().decode(WrappedAPIError.self, from: response) {
                throw APIError.api(status: status, body: wrapped.error)
            }
            throw APIError.unexpectedStatus(status)
        }
        do {
            return try APIClient.makeDecoder().decode(T.self, from: response)
        } catch {
            throw APIError.decoding(String(describing: error))
        }
    }

    private struct WrappedAPIError: Decodable {
        let error: APIErrorBody
    }

    // MARK: - Quotes

    /// One page of the works that quote `workID`, newest first.
    func quotes(workID: String, cursor: String? = nil, limit: Int = 20) async throws -> WorkPage {
        try await send(.quotes(workID: workID, cursor: cursor, limit: limit), body: Optional<Never>.none, as: WorkPage.self)
    }

}

/// The framing of a single-file `multipart/form-data` body.
///
/// One boundary, one part. Split into a prefix and a suffix so the same
/// framing serves a body in memory (``body(wrapping:)``) and a body spooled
/// to disk around a file too large to hold (``spool(_:)``); the bytes on the
/// wire are identical either way.
struct MultipartPart: Sendable {
    let field: String
    let filename: String
    let mimeType: String
    let boundary = "f33d3r-" + UUID().uuidString

    var contentType: String { "multipart/form-data; boundary=\(boundary)" }

    var prefix: Data {
        Data(
            ("--\(boundary)\r\n"
                + "Content-Disposition: form-data; name=\"\(field)\"; filename=\"\(filename)\"\r\n"
                + "Content-Type: \(mimeType)\r\n\r\n").utf8
        )
    }

    var suffix: Data { Data("\r\n--\(boundary)--\r\n".utf8) }

    func body(wrapping data: Data) -> Data {
        var body = prefix
        body.append(data)
        body.append(suffix)
        return body
    }

    /// Writes prefix, the file's bytes and suffix to a new temporary file, a
    /// megabyte at a time, and returns it. The caller removes it.
    func spool(_ fileURL: URL) throws -> URL {
        let out = FileManager.default.temporaryDirectory
            .appendingPathComponent("f33d3r-upload-\(UUID().uuidString)", isDirectory: false)
        guard FileManager.default.createFile(atPath: out.path, contents: nil) else {
            throw APIError.transport("Could not create a temporary file for the upload.")
        }
        let writer = try FileHandle(forWritingTo: out)
        defer { try? writer.close() }
        let reader = try FileHandle(forReadingFrom: fileURL)
        defer { try? reader.close() }

        try writer.write(contentsOf: prefix)
        while let chunk = try reader.read(upToCount: 1 << 20), !chunk.isEmpty {
            try writer.write(contentsOf: chunk)
        }
        try writer.write(contentsOf: suffix)
        return out
    }
}
