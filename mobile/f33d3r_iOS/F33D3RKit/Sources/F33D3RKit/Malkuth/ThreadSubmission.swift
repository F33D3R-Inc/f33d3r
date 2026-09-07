import Foundation

/// A thread: several Works signed together and published one after another.
///
/// ## Why the whole chain is signed up front
///
/// Each part after the first carries `parent_cid` — the CID of the part before
/// it. The web composer waits for each response and reads `cid` out of it, but
/// that value is not something the server invents: `workEvent` checks the CID
/// it was sent against the payload bytes and answers with the same string, so
/// the parent of part *n* is known the moment part *n − 1* is signed. Signing
/// everything before anything is sent buys the same guarantee
/// ``WorkSubmission`` buys for one work: **the bytes never change after the
/// first attempt.** A retry after a timeout resends the part that did not
/// answer, and the unique index on `works.cid` makes a second row impossible.
///
/// ## What a stop means
///
/// The parts are published in order and the chain halts at the first that is
/// not accepted. Everything before it is real — rows the server holds, visible
/// on the author's profile, not something this type can or should undo. So the
/// outcome names exactly which parts landed, and the caller keeps the rest for
/// the author to send again from where it stopped. Pretending an unfinished
/// thread either fully posted or fully failed would be a lie in one direction
/// or the other.
public struct ThreadSubmission: Sendable {

    /// Why a chain stopped before the last part.
    public enum Stop: Sendable, Equatable {
        /// The server refused the part, and will refuse the same bytes again.
        case rejected(MalkuthError)
        /// The part may or may not have landed. Sending the same bytes again
        /// is always safe — see ``WorkSubmission/Outcome/uncertain(cid:reason:)``.
        case uncertain(cid: String, reason: String)
    }

    /// How one call to ``publish(using:)`` ended. Both cases carry only the
    /// parts *that call* landed, so a caller keeping a running tally across
    /// retries is not handed the same rows twice.
    public enum Outcome: Sendable, Equatable {
        case published(landed: [WorkAccepted])
        case stopped(landed: [WorkAccepted], by: Stop)
    }

    /// Every part, head first, each signed once.
    public let parts: [WorkSubmission]
    /// The rows the server has named so far, in order. `accepted.count` is the
    /// index of the next part to send.
    public private(set) var accepted: [WorkAccepted] = []

    /// Signs the head and every continuation.
    ///
    /// `head` supplies everything the continuations share — author, reply
    /// gating, audience, publish time — and is the only part that carries
    /// media. Its kind is set to `thread_post` here regardless of what the
    /// caller put in it, because the kind is a property of the chain and not
    /// of any one part: a head signed as `post` would be a work that happens to
    /// have replies, not a thread.
    ///
    /// Timestamps step by one millisecond per part. The same reading on two
    /// parts would be harmless — their `parent_cid` already differs — but a
    /// strictly increasing stamp is what the author would expect to find if
    /// they ever compared them.
    ///
    /// The unsigned video fields — the watermarked rendition, the frame size,
    /// the upload id that keeps the file from the orphan sweep — ride on the
    /// head's envelope only, where the video is; see ``WorkEnvelope`` for why
    /// they are outside the signature at all.
    public static func prepare(
        head: WorkPayload,
        continuations: [String],
        signer: MalkuthSigner,
        isNSFW: Bool = false,
        videoWatermarkedURL: String? = nil,
        videoWidth: Int? = nil,
        videoHeight: Int? = nil,
        tusUploadID: String? = nil
    ) async throws -> ThreadSubmission {
        var first = head
        first.kind = .threadPost
        var parts = [try await WorkSubmission.prepare(
            first,
            signer: signer,
            isNSFW: isNSFW,
            videoWatermarkedURL: videoWatermarkedURL,
            videoWidth: videoWidth,
            videoHeight: videoHeight,
            tusUploadID: tusUploadID
        )]

        var previous = parts[0].contentID
        for (offset, body) in continuations.enumerated() {
            let payload = WorkPayload(
                authorPIAL: first.authorPIAL,
                body: body,
                commentGating: first.commentGating,
                kind: .threadPost,
                mediaURLs: [],
                parentCID: previous,
                scheduledAt: first.scheduledAt,
                subscriberOnly: first.subscriberOnly,
                timestampMS: first.timestampMS + Int64(offset + 1)
            )
            let part = try await WorkSubmission.prepare(payload, signer: signer, isNSFW: isNSFW)
            previous = part.contentID
            parts.append(part)
        }
        return ThreadSubmission(parts: parts)
    }

    public init(parts: [WorkSubmission]) {
        self.parts = parts
    }

    public var isComplete: Bool { accepted.count == parts.count }

    /// Sends the parts not yet accepted, in order, and stops at the first that
    /// does not land.
    ///
    /// Each part goes through ``WorkSubmission/sendWithRetries(using:attempts:backoff:)``,
    /// so a transient fault on one part is retried with identical bytes before
    /// the chain gives up on it. When the server names a row it is checked
    /// against the CID that was signed: the next part's `parent_cid` was
    /// computed from that CID, and continuing after a disagreement would hang
    /// the rest of the thread off a work the author never chose as its parent.
    public mutating func publish(using client: APIClient) async -> Outcome {
        let before = accepted.count
        while accepted.count < parts.count {
            let part = parts[accepted.count]
            switch await part.sendWithRetries(using: client) {
            case .accepted(let row):
                guard row.cid == part.contentID else {
                    return .stopped(
                        landed: Array(accepted[before...]),
                        by: .rejected(.rejected(
                            status: 201,
                            reason: "the server stored part \(accepted.count + 1) under a different content id (\(row.cid)) than was signed"
                        ))
                    )
                }
                accepted.append(row)
            case .rejected(let error):
                return .stopped(landed: Array(accepted[before...]), by: .rejected(error))
            case .uncertain(let cid, let reason):
                return .stopped(landed: Array(accepted[before...]), by: .uncertain(cid: cid, reason: reason))
            }
        }
        return .published(landed: Array(accepted[before...]))
    }
}
