import Foundation

/// A thread: several Works published one after another, each answering the
/// one before it.
///
/// ## Each part is signed when it is sent
///
/// Each part after the first carries `parent_cid` — the CID of the part before
/// it, as the server confirmed it on acceptance. A part is signed the moment it
/// is first sent (the server accepts a work signed within the last hour), and
/// every retry of that part resends its bytes, exactly as ``WorkSubmission``
/// does for one work. A part is signed again only when the server says its
/// signature is too old, which proves that part never landed; parts after it
/// have not been signed yet, so nothing downstream is invalidated.
///
/// ## What a stop means
///
/// The parts are published in order and the chain halts at the first that is
/// not accepted. Everything before it is real — rows the server holds, visible
/// on the author's profile. So the outcome names exactly which parts landed,
/// and the caller keeps the rest for the author to send again from where it
/// stopped.
public struct ThreadSubmission: Sendable {

    /// Why a chain stopped before the last part.
    public enum Stop: Sendable, Equatable {
        /// The server refused the part. Nothing of it was written.
        case rejected(MalkuthError)
        /// The part may or may not have landed. Publishing again resends the
        /// same bytes, which is always safe.
        case uncertain(cid: String, reason: String)
    }

    /// How one call to ``publish(using:)`` ended. Both cases carry only the
    /// parts *that call* landed.
    public enum Outcome: Sendable, Equatable {
        case published(landed: [WorkAccepted])
        case stopped(landed: [WorkAccepted], by: Stop)
    }

    /// The first part. Its kind is `thread_post` whatever the caller put in it,
    /// because the kind is a property of the chain.
    public let head: WorkPayload
    /// The text of every part after the first.
    public let continuations: [String]
    /// The head's unsigned fields — the video's frame size rides on the head,
    /// where the video is. Continuations carry only `is_nsfw`.
    public let headUnsigned: WorkSubmission.Unsigned
    private let signer: MalkuthSigner
    /// The rows the server has named so far, in order. `accepted.count` is the
    /// index of the next part to send.
    public private(set) var accepted: [WorkAccepted] = []
    /// The part in flight, kept between calls so a retry resends its bytes.
    private var current: WorkSubmission?

    /// `head` supplies everything the continuations share — author, reply
    /// gating, audience, publish time — and is the only part that carries
    /// media.
    public init(
        head: WorkPayload,
        continuations: [String],
        signer: MalkuthSigner,
        headUnsigned: WorkSubmission.Unsigned = WorkSubmission.Unsigned()
    ) {
        var first = head
        first.kind = .threadPost
        self.head = first
        self.continuations = continuations
        self.headUnsigned = headUnsigned
        self.signer = signer
    }

    public var partCount: Int { 1 + continuations.count }

    public var isComplete: Bool { accepted.count == partCount }

    /// The submission for part `index`, whose parent is the part accepted
    /// before it.
    private func part(_ index: Int) -> WorkSubmission {
        guard index > 0 else {
            return WorkSubmission(draft: head, unsigned: headUnsigned, signer: signer)
        }
        let payload = WorkPayload(
            authorAccount: head.authorAccount,
            body: continuations[index - 1],
            commentGating: head.commentGating,
            kind: .threadPost,
            mediaURLs: [],
            parentCID: accepted[index - 1].cid,
            scheduledAt: head.scheduledAt,
            subscriberOnly: head.subscriberOnly,
            timestampMS: 0
        )
        return WorkSubmission(
            draft: payload,
            unsigned: WorkSubmission.Unsigned(isNSFW: headUnsigned.isNSFW),
            signer: signer
        )
    }

    /// Sends the parts not yet accepted, in order, and stops at the first that
    /// does not land.
    ///
    /// When the server names a row it is checked against the CID that was
    /// signed: the next part's `parent_cid` is that row's CID, and continuing
    /// after a disagreement would hang the rest of the thread off a work the
    /// author never chose as its parent.
    public mutating func publish(using client: APIClient) async -> Outcome {
        let before = accepted.count
        while accepted.count < partCount {
            var submission = current ?? part(accepted.count)
            let outcome = await submission.sendWithRetries(using: client)
            current = submission
            switch outcome {
            case .accepted(let row):
                guard row.cid == submission.contentID else {
                    current = nil
                    return .stopped(
                        landed: Array(accepted[before...]),
                        by: .rejected(.rejected(
                            status: 201,
                            reason: "the server stored part \(accepted.count + 1) under a different content id (\(row.cid)) than was signed"
                        ))
                    )
                }
                accepted.append(row)
                current = nil
            case .rejected(let error):
                current = nil
                return .stopped(landed: Array(accepted[before...]), by: .rejected(error))
            case .uncertain(let cid, let reason):
                return .stopped(landed: Array(accepted[before...]), by: .uncertain(cid: cid, reason: reason))
            }
        }
        return .published(landed: Array(accepted[before...]))
    }
}
