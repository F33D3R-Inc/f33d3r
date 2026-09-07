import Foundation

/// One attempt to publish a Work, retried without ever becoming two Works.
///
/// ## What the CID gives us for free
///
/// `works.cid` is `UNIQUE NOT NULL`. The CID is a hash of the signed payload,
/// and `timestamp_ms` is inside that payload. Put together:
///
/// - **The same envelope can be sent any number of times and produce at most one
///   Work.** The second insert violates the unique index and no row is written.
/// - **A re-signed envelope is a different Work**, because re-signing means
///   re-stamping `timestamp_ms`, which changes the payload, which changes the
///   CID. The duplicate-post bug is therefore not a race in the request — it is
///   the client deciding to rebuild the envelope on retry.
///
/// So the whole of idempotency here is one rule: **build and sign once, then
/// retry those exact bytes.** ``WorkSubmission`` holds the envelope so that rule
/// is structural rather than remembered. Nothing needs an idempotency key,
/// because the content already is one.
///
/// ## The gap the server leaves
///
/// The server does not answer the duplicate case gracefully. `workEvent` calls
/// `InsertWork`, and a unique violation comes back as
/// `500 server error` — indistinguishable, from outside, from a genuine failure.
/// So a retry that races a first attempt which *did* land looks like a server
/// error, and this type reports that honestly as ``Outcome/uncertain`` rather
/// than guessing. The correct resolution is a read: the CID is known, so the
/// question "did this post" has a definite answer as soon as any surface can be
/// asked about a CID. There is no such route on `api/v1` today, which is why
/// ``Outcome/uncertain`` exists as a state and not as a retry.
///
/// What it must not do is retry an `uncertain` result by re-signing. That is the
/// one action that can turn a network blip into two posts.
public struct WorkSubmission: Sendable {

    /// How a submission ended.
    public enum Outcome: Sendable, Equatable {
        /// The server stored it and named the row.
        case accepted(WorkAccepted)
        /// The server refused it, and will refuse the identical bytes again.
        /// Retrying is pointless; the payload or the key is wrong.
        case rejected(MalkuthError)
        /// The request may or may not have landed: it timed out, or answered
        /// `500`, which is also what a duplicate insert answers.
        ///
        /// The envelope is carried so the caller can retry *these* bytes, which
        /// is always safe — either it lands for the first time, or the unique
        /// index refuses it and the state is unchanged.
        case uncertain(cid: String, reason: String)
    }

    public let envelope: WorkEnvelope

    public init(envelope: WorkEnvelope) {
        self.envelope = envelope
    }

    /// Signs once and returns a submission that can be retried freely.
    public static func prepare(
        _ payload: WorkPayload,
        signer: MalkuthSigner,
        eventType: String? = nil,
        isNSFW: Bool = false,
        videoWatermarkedURL: String? = nil,
        videoWidth: Int? = nil,
        videoHeight: Int? = nil,
        quotedWorkID: String? = nil,
        reactLayout: String? = nil,
        tusUploadID: String? = nil
    ) async throws -> WorkSubmission {
        WorkSubmission(envelope: try await signer.sign(
            payload,
            eventType: eventType,
            isNSFW: isNSFW,
            videoWatermarkedURL: videoWatermarkedURL,
            videoWidth: videoWidth,
            videoHeight: videoHeight,
            quotedWorkID: quotedWorkID,
            reactLayout: reactLayout,
            tusUploadID: tusUploadID
        ))
    }

    public var contentID: String { envelope.contentID }

    /// Sends the envelope once and classifies what came back.
    ///
    /// The classification is the substance of this type:
    ///
    /// | answer | outcome | why |
    /// |---|---|---|
    /// | `201` | `accepted` | the row exists and is named |
    /// | `400` | `rejected` | `cid mismatch`, `unknown work kind`, `reply requires parent_cid` — deterministic, the same bytes fail the same way |
    /// | `401` | `rejected` | the session is gone; nothing was written and a retry cannot help |
    /// | `403` | `rejected` | `signature invalid`, posting restricted, or the CSRF middleware refusing an origin-less write |
    /// | `429` | `uncertain` | nothing was written, but the caller should back off rather than treat it as refusal |
    /// | `500` | `uncertain` | a genuine fault **or** a duplicate insert — the server does not distinguish them |
    /// | `502` | `uncertain` | the key authority is down; nothing was written |
    /// | timeout | `uncertain` | the request may have been processed after the client stopped listening |
    public func send(using client: APIClient) async -> Outcome {
        do {
            return .accepted(try await client.post(envelope))
        } catch let error as MalkuthError {
            switch error {
            case .rejected(let status, let reason):
                switch status {
                case 400, 401, 403, 404, 413, 422:
                    return .rejected(error)
                default:
                    return .uncertain(cid: envelope.contentID, reason: "HTTP \(status): \(reason)")
                }
            case .keyAuthorityUnavailable:
                return .rejected(error)
            default:
                return .rejected(error)
            }
        } catch let error as APIError {
            // Transport failures are the whole reason this type exists: the
            // request may have been received and processed after the client
            // stopped waiting for the answer.
            return .uncertain(cid: envelope.contentID, reason: error.userMessage)
        } catch {
            return .uncertain(cid: envelope.contentID, reason: String(describing: error))
        }
    }

    /// Sends, retrying the identical bytes on an uncertain outcome.
    ///
    /// Safe to call with any `attempts` because every attempt sends the same
    /// CID: the unique index, not this loop, is what guarantees one Work. The
    /// loop only decides how long to keep asking.
    ///
    /// It deliberately does **not** retry a `rejected` outcome. `cid mismatch`
    /// answered once is `cid mismatch` answered a hundred times, and spending
    /// the write rate limit on it delays the error reaching the author.
    public func sendWithRetries(
        using client: APIClient,
        attempts: Int = 3,
        backoff: @Sendable (Int) -> Duration = { .seconds(1 << $0) }
    ) async -> Outcome {
        var last: Outcome = .uncertain(cid: envelope.contentID, reason: "not attempted")
        for attempt in 0..<max(1, attempts) {
            last = await send(using: client)
            switch last {
            case .accepted, .rejected:
                return last
            case .uncertain:
                if attempt < attempts - 1 {
                    try? await Task.sleep(for: backoff(attempt))
                }
            }
        }
        return last
    }
}
