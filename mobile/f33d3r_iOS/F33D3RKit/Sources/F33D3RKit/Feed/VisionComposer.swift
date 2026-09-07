import Foundation
import Observation

/// A vision being written: a text card or a photo, with audience and lifetime
/// as explicit choices rather than buried settings.
///
/// The photo is uploaded when it is chosen, so Post sends a path the server
/// already has. The composer holds every rule about what makes a valid
/// vision; the sheet lays the controls out.
@MainActor
@Observable
public final class VisionComposer {

    public enum Mode: String, Sendable, CaseIterable {
        case text
        case photo
    }

    public static let maxBodyLength = 280
    public static let lifetimes: [(label: String, hours: Int)] = [("6h", 6), ("12h", 12), ("24h", 24), ("3 days", 72)]

    public var mode: Mode = .text
    public var body = ""
    public var background = "void"
    public var typeface = "grotesk"
    public var align = "center"
    /// everyone | subscribers
    public var audience = "everyone"
    public var ttlHours = 24
    public var allowReplies = true
    public var isNSFW = false
    /// Present while a poll is being written.
    public var pollOptions: [String]?

    /// The chosen image, shown from these bytes.
    public private(set) var imageData: Data?
    public private(set) var imagePath: String?
    public private(set) var isUploading = false
    public private(set) var uploadFailure: String?

    public private(set) var isSubmitting = false
    public private(set) var failure: String?

    private let client: APIClient

    public init(client: APIClient) {
        self.client = client
    }

    public var remaining: Int { Self.maxBodyLength - body.count }

    public var canSubmit: Bool {
        guard !isSubmitting, remaining >= 0 else { return false }
        switch mode {
        case .text:
            let hasWords = !body.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
            if let pollOptions {
                let filled = pollOptions.map { $0.trimmingCharacters(in: .whitespaces) }
                return hasWords && filled.count >= 2 && filled.allSatisfy { !$0.isEmpty && $0.count <= 40 }
            }
            return hasWords
        case .photo:
            return imagePath != nil && !isUploading && uploadFailure == nil
        }
    }

    // MARK: - Image

    /// Chooses a photo and uploads it now.
    public func attach(_ data: Data, filename: String, mimeType: String) async {
        imageData = data
        imagePath = nil
        uploadFailure = nil
        isUploading = true
        defer { isUploading = false }
        do {
            imagePath = try await client.uploadImage(data, filename: filename, mimeType: mimeType)
        } catch let error as APIError {
            uploadFailure = error.userMessage
        } catch {
            uploadFailure = error.localizedDescription
        }
    }

    public func removeImage() {
        imageData = nil
        imagePath = nil
        uploadFailure = nil
    }

    // MARK: - Poll

    public func togglePoll() {
        pollOptions = pollOptions == nil ? ["", ""] : nil
    }

    public func addPollOption() {
        guard var options = pollOptions, options.count < 4 else { return }
        options.append("")
        pollOptions = options
    }

    public func removePollOption(at index: Int) {
        guard var options = pollOptions, options.count > 2, options.indices.contains(index) else { return }
        options.remove(at: index)
        pollOptions = options
    }

    // MARK: - Submit

    /// Posts the vision and returns its id.
    public func submit(source: String) async -> String? {
        guard canSubmit else { return nil }
        isSubmitting = true
        failure = nil
        defer { isSubmitting = false }

        var draft = APIClient.VisionDraft()
        draft.contentType = mode == .photo ? "image" : "text"
        draft.body = body.trimmingCharacters(in: .whitespacesAndNewlines)
        draft.background = background
        draft.typeface = typeface
        draft.align = align
        draft.mediaURL = mode == .photo ? imagePath : nil
        draft.audience = audience
        draft.ttlHours = ttlHours
        draft.allowReplies = allowReplies
        draft.isNSFW = isNSFW
        draft.source = source
        if mode == .text, let pollOptions {
            draft.pollOptions = pollOptions.map { $0.trimmingCharacters(in: .whitespaces) }
        }
        do {
            return try await client.postVision(draft)
        } catch let error as MalkuthError {
            if case .rejected(_, let reason) = error { failure = reason } else { failure = error.description }
        } catch let error as APIError {
            failure = error.userMessage
        } catch {
            failure = error.localizedDescription
        }
        return nil
    }
}
