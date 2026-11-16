import Foundation

/// One attempt to publish a Work, retried without ever becoming two Works.
///
/// ## Signed at send time, resent as signed
///
/// The server accepts a work signed within the last hour (and no more than
/// five minutes ahead), so a submission signs its draft the first time it is
/// sent — never when the draft was composed. Every retry after that resends
/// the **same bytes**: the server answers a re-delivered CID with the work it
/// already created, before any rule that depends on the clock, so resending is
/// always safe and never makes a second work.
///
/// The one time a submission signs again is on the server's own word that the
/// signature is too old (`400` naming `timestamp_ms`). That answer is proof the
/// CID never landed — a CID that had landed would have been answered with its
/// work — so the new signature cannot duplicate anything. The server's content
/// dedup (`409 Duplicate`) stands behind that as well.
///
/// ## How answers are classified
///
/// | answer | outcome |
/// |---|---|
/// | `201` | `accepted` |
/// | `409` Duplicate | `rejected` — final, never retried |
/// | `429` RateLimited | backed off and retried; `rejected` once the attempts run out (nothing was written) |
/// | `400` on `timestamp_ms` | signed again and resent |
/// | other `400`, `401`, `403`, `404`, `422` | `rejected` |
/// | `500`, `502`, `503`, transport failure | retried; `uncertain` once the attempts run out |
public struct WorkSubmission: Sendable {

    /// How a submission ended.
    public enum Outcome: Sendable, Equatable {
        /// The server stored it and named the row.
        case accepted(WorkAccepted)
        /// The server refused it. Nothing was written.
        case rejected(MalkuthError)
        /// The request may or may not have landed. Sending the same bytes again
        /// is always safe.
        case uncertain(cid: String, reason: String)
    }

    /// The unsigned outer fields of the envelope.
    public struct Unsigned: Sendable, Equatable {
        public var eventType: String?
        public var isNSFW: Bool
        public var videoWidth: Int?
        public var videoHeight: Int?
        public var quotedWorkID: String?
        public var reactLayout: String?
        public var downloadsAllowed: Bool?

        public init(
            eventType: String? = nil,
            isNSFW: Bool = false,
            videoWidth: Int? = nil,
            videoHeight: Int? = nil,
            quotedWorkID: String? = nil,
            reactLayout: String? = nil,
            downloadsAllowed: Bool? = nil
        ) {
            self.eventType = eventType
            self.isNSFW = isNSFW
            self.videoWidth = videoWidth
            self.videoHeight = videoHeight
            self.quotedWorkID = quotedWorkID
            self.reactLayout = reactLayout
            self.downloadsAllowed = downloadsAllowed
        }
    }

    /// What the author wrote. Its text is normalised and its `timestamp_ms`
    /// stamped when it is signed (``WorkPayload/preparedForSigning(at:)``).
    public let draft: WorkPayload
    public let unsigned: Unsigned
    /// The signed envelope, once the first send has signed it.
    public private(set) var envelope: WorkEnvelope?
    private let signer: MalkuthSigner

    public init(draft: WorkPayload, unsigned: Unsigned = Unsigned(), signer: MalkuthSigner) {
        self.draft = draft
        self.unsigned = unsigned
        self.signer = signer
    }

    /// The CID of the signed envelope; nil before the first send.
    public var contentID: String? { envelope?.contentID }

    /// Signs the draft as of now, replacing any earlier envelope.
    public mutating func sign(now: Date = Date()) async throws {
        let stamp = Int64((now.timeIntervalSince1970 * 1000).rounded())
        envelope = try await signer.sign(
            draft.preparedForSigning(at: stamp),
            eventType: unsigned.eventType,
            isNSFW: unsigned.isNSFW,
            videoWidth: unsigned.videoWidth,
            videoHeight: unsigned.videoHeight,
            quotedWorkID: unsigned.quotedWorkID,
            reactLayout: unsigned.reactLayout,
            downloadsAllowed: unsigned.downloadsAllowed
        )
    }

    /// What one attempt came to.
    enum Attempt: Equatable {
        case done(Outcome)
        /// Nothing was decided; send the same bytes again.
        case retry(reason: String)
        /// Nothing was written; wait longer, then send the same bytes again.
        case backOff(MalkuthError)
        /// The signature is too old by the server's clock; sign again.
        case resign
    }

    /// Sends once, signing first when nothing has been signed yet.
    public mutating func send(using client: APIClient) async -> Outcome {
        switch await attempt(using: client) {
        case .done(let outcome): return outcome
        case .retry(let reason): return .uncertain(cid: envelope?.contentID ?? "", reason: reason)
        case .backOff(let error): return .rejected(error)
        case .resign: return .rejected(.signedOutsideWindow)
        }
    }

    mutating func attempt(using client: APIClient) async -> Attempt {
        if envelope == nil {
            do {
                try await sign()
            } catch let error as MalkuthError {
                return .done(.rejected(error))
            } catch {
                return .done(.rejected(.keyGenerationFailed(String(describing: error))))
            }
        }
        guard let envelope else { return .done(.rejected(.accountUnknown)) }
        do {
            return .done(.accepted(try await client.post(envelope)))
        } catch let error as MalkuthError {
            return Self.classify(error)
        } catch let error as APIError {
            // The request may have been received and processed after the
            // client stopped waiting for the answer.
            return .retry(reason: error.userMessage)
        } catch {
            return .retry(reason: String(describing: error))
        }
    }

    static func classify(_ error: MalkuthError) -> Attempt {
        switch error {
        case .workRefused(let state, let message):
            switch state {
            case .rateLimited:
                return .backOff(error)
            case .serverError:
                return .retry(reason: message)
            case .invalidField where WorkRefusal.field(in: message) == "timestamp_ms":
                return .resign
            case .ok, .duplicate, .textTooLong, .textBlank, .invalidField,
                 .citedNotFound, .repliesRestricted, .postingRestricted:
                return .done(.rejected(error))
            }
        case .rejected(let status, let reason):
            // An answer that is not the create path's own: the session
            // middleware, or a proxy in front of the server.
            if status >= 500 || status == 0 { return .retry(reason: "HTTP \(status): \(reason)") }
            return .done(.rejected(error))
        default:
            return .done(.rejected(error))
        }
    }

    /// Sends, retrying the identical bytes on anything undecided.
    ///
    /// A `429` waits longer before the next attempt. A signature the server
    /// calls too old is signed again once and sent. A refusal is never retried.
    public mutating func sendWithRetries(
        using client: APIClient,
        attempts: Int = 3,
        backoff: @Sendable (Int) -> Duration = { .seconds(1 << $0) }
    ) async -> Outcome {
        var resigned = false
        var last: Attempt = .retry(reason: "not attempted")
        var attempt = 0
        while attempt < max(1, attempts) {
            last = await self.attempt(using: client)
            switch last {
            case .done(let outcome):
                return outcome
            case .resign:
                guard !resigned else { return .rejected(.signedOutsideWindow) }
                resigned = true
                do {
                    try await sign()
                } catch let error as MalkuthError {
                    return .rejected(error)
                } catch {
                    return .rejected(.keyGenerationFailed(String(describing: error)))
                }
                continue
            case .retry:
                if attempt < attempts - 1 { try? await Task.sleep(for: backoff(attempt)) }
            case .backOff:
                if attempt < attempts - 1 { try? await Task.sleep(for: backoff(attempt + 3)) }
            }
            attempt += 1
        }
        switch last {
        case .done(let outcome): return outcome
        case .backOff(let error): return .rejected(error)
        case .resign: return .rejected(.signedOutsideWindow)
        case .retry(let reason): return .uncertain(cid: envelope?.contentID ?? "", reason: reason)
        }
    }
}

/// Reading the create path's plain-text refusals.
public enum WorkRefusal {
    /// The field an `invalid_field` refusal names. The server words it as
    /// `This post could not be sent: <field> — <detail>.` (work_create.go).
    public static func field(in message: String) -> String? {
        let prefix = "This post could not be sent: "
        guard message.hasPrefix(prefix) else { return nil }
        let rest = message.dropFirst(prefix.count)
        guard let dash = rest.range(of: " — ") else { return nil }
        return String(rest[..<dash.lowerBound])
    }
}
