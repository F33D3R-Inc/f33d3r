import Foundation

/// The Number lane: the owner's side of a contact address, and the one way to
/// spend somebody else's.
///
/// Reads go through the JSON surface like every other read. Writes go down the
/// `POST /events` lane with everything else — a minted Number, a revoked one, a
/// changed policy and an answered request are all ordinary mutations, and none
/// of them needs a shape the flat event body cannot carry.
///
/// One asymmetry is worth naming. Everything about the owner's own Numbers is
/// specific: which one, what policy, when. Everything about *reaching* somebody
/// by Number is deliberately not. ``startConversation(number:body:handle:)``
/// answers the same three states as the handle path, after the same fixed
/// delay, and this client neither asks for more nor infers any.
public extension APIClient {

    // MARK: The owner's side

    /// The account's own Numbers, its contact policy, and the policies this
    /// server offers.
    func contactNumbers() async throws -> NumbersPage {
        try await send(.contactNumbers, body: Optional<Never>.none, as: NumbersPage.self)
    }

    /// The contact requests waiting on the reader.
    func contactRequests() async throws -> ContactRequestPage {
        try await send(.contactRequests, body: Optional<Never>.none, as: ContactRequestPage.self)
    }

    /// Mints a Number and answers it.
    ///
    /// The digits are the server's to choose. A client that could name them
    /// could name one somebody else already holds, and the check digit would
    /// not notice.
    func mintNumber() async throws -> ContactNumber {
        let payload = CanonicalJSON.object([("event_type", .string("number_mint"))])
        let (data, status) = try await sendRaw(.events, body: payload, contentType: "application/json")
        guard (200..<300).contains(status) else {
            throw MessagingError.from(status: status, data: data)
        }
        do {
            return try APIClient.makeDecoder().decode(ContactNumber.self, from: data)
        } catch {
            throw MessagingError.malformed("the new Number was not understood")
        }
    }

    /// Retires one Number. The others keep working, which is why an account
    /// holds more than one.
    ///
    /// Nobody is told. Somebody who writes to a retired Number gets the same
    /// answer as somebody who writes to a Number that never existed — that
    /// indistinguishability is the server's, and revoking is what makes use of
    /// it.
    func revokeNumber(id: String) async throws {
        try await postNumberEvent("number_revoke", fields: [("number_id", .string(id))])
    }

    /// Sets who may come through one Number.
    func setNumberPolicy(id: String, policy: String) async throws {
        try await postNumberEvent("number_policy", fields: [
            ("number_id", .string(id)),
            ("policy", .string(policy)),
        ])
    }

    /// Sets the account-wide policy, which applies where a Number does not say
    /// otherwise.
    func setContactPolicy(_ policy: String) async throws {
        try await postNumberEvent("contact_policy", fields: [("policy", .string(policy))])
    }

    /// Answers one request.
    ///
    /// Declining is silent by design: the person who asked is told nothing, so
    /// a decline cannot be read as a fact about the account. Accepting opens
    /// the conversation and releases whatever they had already written.
    func decideContactRequest(id: String, accept: Bool) async throws {
        try await postNumberEvent("contact_decide", fields: [
            ("request_id", .string(id)),
            ("decision", .string(accept ? "accept" : "decline")),
        ])
    }

    // MARK: Spending somebody else's

    /// Writes to whoever holds `number`, opening a conversation if the policy
    /// on it allows one.
    ///
    /// The counterpart of ``startConversation(handle:body:)``, with the same
    /// three outcomes and the same reason for there being only three. Every way
    /// this can fail to arrive — a Number that was never minted, one since
    /// retired, an account that takes no messages, a block — collapses into one
    /// answer after a fixed minimum delay. Asking this app to tell them apart
    /// would rebuild the oracle the server spends that delay refusing to be, so
    /// nothing below tries.
    ///
    /// `handle` is passed only when the caller already had one on screen — it
    /// is a thread they are trying to reach, not a guess at who answers. The
    /// server treats it as a hint and the answer does not change if it is
    /// wrong.
    func startConversation(number: F33Number, body text: String, handle: String? = nil) async throws -> ConversationStart {
        var fields: [(String, CanonicalJSON.Value)] = [
            ("event_type", .string("message_number")),
            ("number", .string(number.canonical)),
            ("body", .string(text)),
        ]
        if let handle, !handle.isEmpty {
            fields.append(("handle", .string(handle.hasPrefix("@") ? String(handle.dropFirst()) : handle)))
        }
        let (data, status) = try await sendRaw(
            .events, body: CanonicalJSON.object(fields), contentType: "application/json"
        )
        guard (200..<300).contains(status) else {
            throw MessagingError.from(status: status, data: data)
        }
        do {
            return try APIClient.makeDecoder().decode(ConversationStart.self, from: data)
        } catch {
            throw MessagingError.malformed("the answer was not understood")
        }
    }

    private func postNumberEvent(
        _ eventType: String,
        fields: [(String, CanonicalJSON.Value)]
    ) async throws {
        let payload = CanonicalJSON.object([("event_type", .string(eventType))] + fields)
        let (data, status) = try await sendRaw(.events, body: payload, contentType: "application/json")
        guard (200..<300).contains(status) else {
            throw MessagingError.from(status: status, data: data)
        }
    }
}
