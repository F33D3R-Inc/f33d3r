import Foundation

/// The state of one video upload, as `POST /api/v1/media/video` answers on
/// admission and `GET /api/v1/media/video/{id}` answers on every poll.
///
/// Mirrors `VideoJobDTO` in `feed-engine/internal/handler/api_v1_media.go`.
/// One shape for both routes, so the composer keeps one decoder and one
/// switch: whatever the server says about the upload, it says it in these
/// fields.
///
/// Every field but `status` is optional because every one of them is
/// `omitempty` on the Go side, and which ones are present is what the status
/// means — see ``Status``. The media URLs are the relative paths the rest of
/// `/api/v1` uses; the app resolves them against the server it is talking to.
public struct VideoJob: Codable, Hashable, Sendable {

    /// Where the upload is. The closed set the server maps every transcoder
    /// answer onto; an enum so a word outside it is a decoding error the
    /// composer shows, not a state it guesses at.
    public enum Status: String, Codable, Hashable, Sendable {
        /// Admitted; the transcoder has not yet taken the job. `uploadID` is set.
        case queued
        /// The transcoder has it, or could not be asked this instant. Keep polling.
        case processing
        /// `masterURL`, `posterURL`, `watermarkedURL`, `durationSecs`, `width`
        /// and `height` are set.
        case ready
        /// `error` says why. Upload again.
        case failed
        /// The same bytes are already on F33D3R. `duplicateOfWorkID` and
        /// `duplicateOfHandle` name the original, and the media fields carry
        /// its renditions when the work is known. Nothing was transcoded.
        case duplicate
    }

    /// The id to poll. Set on `queued`; absent on an admission-time duplicate,
    /// which has no job to poll.
    public let uploadID: String?
    public let status: Status
    public let masterURL: String?
    public let posterURL: String?
    /// The moving-watermark rendition — the file that leaves the platform.
    /// Rides unsigned on the envelope; see ``WorkEnvelope``.
    public let watermarkedURL: String?
    /// Fractional seconds, as the transcoder measured them. The signed payload
    /// carries whole seconds — see ``WorkPayload/videoDurationSecs``.
    public let durationSecs: Double?
    public let width: Int?
    public let height: Int?
    /// Why a `failed` job failed, in the server's words.
    public let error: String?
    public let duplicateOfWorkID: String?
    public let duplicateOfHandle: String?

    enum CodingKeys: String, CodingKey {
        case uploadID = "upload_id"
        case status
        case masterURL = "master_url"
        case posterURL = "poster_url"
        case watermarkedURL = "watermarked_url"
        case durationSecs = "duration_secs"
        case width, height, error
        case duplicateOfWorkID = "duplicate_of_work_id"
        case duplicateOfHandle = "duplicate_of_handle"
    }

    public init(
        uploadID: String? = nil,
        status: Status,
        masterURL: String? = nil,
        posterURL: String? = nil,
        watermarkedURL: String? = nil,
        durationSecs: Double? = nil,
        width: Int? = nil,
        height: Int? = nil,
        error: String? = nil,
        duplicateOfWorkID: String? = nil,
        duplicateOfHandle: String? = nil
    ) {
        self.uploadID = uploadID
        self.status = status
        self.masterURL = masterURL
        self.posterURL = posterURL
        self.watermarkedURL = watermarkedURL
        self.durationSecs = durationSecs
        self.width = width
        self.height = height
        self.error = error
        self.duplicateOfWorkID = duplicateOfWorkID
        self.duplicateOfHandle = duplicateOfHandle
    }

    /// Whether the server has finished with this upload, one way or another.
    /// `queued` and `processing` are the two states worth asking again about.
    public var isSettled: Bool {
        switch status {
        case .queued, .processing: return false
        case .ready, .failed, .duplicate: return true
        }
    }
}

/// The answer to `POST /api/v1/media/voice`. Mirrors `VoiceUploadDTO`.
///
/// There is no duration here on purpose: the server does not probe audio, so
/// it has nothing to say that the recorder did not already know. The client
/// measures the recording and sends `voice_duration_secs` with the work.
public struct VoiceUpload: Codable, Hashable, Sendable {
    /// The path the work references the recording by.
    public let url: String

    enum CodingKeys: String, CodingKey {
        case url
    }

    public init(url: String) {
        self.url = url
    }
}

/// The answer to `GET /api/v1/gif/search`. Mirrors `GifSearchDTO`.
///
/// The provider's own shape stays on the server; this is the contract the app
/// owns, and it is the same for a search and for trending (an empty query).
public struct GifSearchPage: Codable, Hashable, Sendable {
    public let results: [GifResult]

    enum CodingKeys: String, CodingKey {
        case results
    }

    public init(results: [GifResult]) {
        self.results = results
    }
}

/// One GIF the picker can offer. Mirrors `GifResultDTO`.
///
/// Both URLs are the provider's — absolute, on a host that is not this
/// server's — so they are fetched without a credential and attached as they
/// are: `url` joins `media_urls` on the work exactly as the web's picker
/// stores it.
public struct GifResult: Codable, Hashable, Sendable, Identifiable {
    public let id: String
    /// The GIF to attach.
    public let url: String
    /// A smaller rendition for the picker grid.
    public let previewURL: String
    /// Of `url`. Zero when the provider did not say.
    public let width: Int
    public let height: Int
    public let title: String

    enum CodingKeys: String, CodingKey {
        case id, url, width, height, title
        case previewURL = "preview_url"
    }

    public init(id: String, url: String, previewURL: String, width: Int, height: Int, title: String) {
        self.id = id
        self.url = url
        self.previewURL = previewURL
        self.width = width
        self.height = height
        self.title = title
    }
}
