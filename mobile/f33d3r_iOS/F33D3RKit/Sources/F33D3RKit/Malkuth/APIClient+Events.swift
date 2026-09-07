import Foundation

/// The write lane.
///
/// Everything that changes state goes through `POST /events` with an
/// `event_type`, and the requests fall into two shapes that are not
/// interchangeable:
///
/// - **Signed works.** A body carrying a `cid` is routed to `workEvent`
///   regardless of its `event_type`, and must carry the full signed envelope.
///   These are the only events with content in them.
/// - **Unsigned interactions.** A like, a bookmark, a follow: a flat object
///   with an `event_type` and one or two ids. There is nothing to sign — the
///   act is entirely described by who is authenticated and what they pointed
///   at, both of which the server already knows.
///
/// The second shape has one more split inside it, which is not documented
/// anywhere and is only visible by reading each handler: **some events read
/// their fields only through `r.FormValue`**, which does not look at a JSON
/// body. `followEvent`, `tipEvent` and `set_reply_restriction` are in that
/// group; `workReactionEvent` and `workDeleteEvent` read both. Sending JSON to a
/// form-only handler does not fail loudly — `eventsPost` still routes on
/// `event_type`, and the handler then reports the field as missing. So the
/// encoding is chosen per event here, once, rather than guessed at each call
/// site.
public extension APIClient {

    // MARK: - Signed works

    /// Posts a signed Work and returns the row the server stored it as.
    ///
    /// The envelope's bytes are sent exactly as built. Retrying with the *same*
    /// envelope is safe by construction — see ``WorkSubmission`` for why, and
    /// prefer it to calling this directly from a compose screen.
    func post(_ envelope: WorkEnvelope) async throws -> WorkAccepted {
        let (data, status) = try await sendRaw(
            .events, body: envelope.httpBody, contentType: "application/json"
        )
        guard status == 201 || status == 200 else {
            throw MalkuthError.rejected(status: status, reason: Self.reason(from: data, status: status))
        }
        do {
            return try JSONDecoder().decode(WorkAccepted.self, from: data)
        } catch {
            // A 2xx whose body is not the documented shape is still a work that
            // exists. Saying so is more useful than a decode error, because the
            // caller's next question is "did it post", not "what did it answer".
            throw MalkuthError.rejected(
                status: status,
                reason: "the work was accepted but the response was not understood: \(String(decoding: data.prefix(200), as: UTF8.self))"
            )
        }
    }

    // MARK: - Signing key registration

    /// Registers this device's public signing key with the key authority.
    ///
    /// Nantar forwards this to Elohim Veni and keeps no copy — so this is not a
    /// cache that can be warmed locally, and a deployment without Elohim Veni
    /// running answers `502 {"error":"key_authority_unavailable"}` every time.
    /// That is surfaced as ``MalkuthError/keyAuthorityUnavailable`` specifically,
    /// because it is the one failure here that is a missing service rather than
    /// a wrong client.
    ///
    /// Call it on every launch, as `malkuth.js` does. The authority holds one
    /// key per PIAL, so the registration is what makes *this* device the one
    /// whose signatures verify.
    func registerSigningKey(publicKeySPKIBase64: String) async throws {
        let body = CanonicalJSON.object([
            ("public_key_b64", .string(publicKeySPKIBase64)),
            ("algorithm", .string("ECDSA-P256")),
        ])
        let (data, status) = try await sendRaw(
            .registerSigningKey, body: body, contentType: "application/json"
        )
        guard (200..<300).contains(status) else {
            let reason = Self.reason(from: data, status: status)
            if status == 502 || reason.contains("key_authority_unavailable") {
                throw MalkuthError.keyAuthorityUnavailable
            }
            throw MalkuthError.rejected(status: status, reason: reason)
        }
    }

    // MARK: - Unsigned interaction events

    /// A reaction to a Work. Idempotent on both sides: the insert is
    /// `ON CONFLICT DO NOTHING` and the delete is a no-op when there is nothing
    /// to delete, so a retry after a timeout cannot double-count.
    ///
    /// The names are `work_like`, not `like`. `eventsPost` has no `like` case at
    /// all — that vocabulary belongs to the older posts table — and an
    /// `event_type` it does not know answers `404 unknown event_type: like`.
    enum WorkReaction: String, CaseIterable, Sendable {
        case like = "work_like"
        case unlike = "work_unlike"
        case repost = "work_repost"
        case unrepost = "work_unrepost"
        case bookmark = "work_bookmark"
        case unbookmark = "work_unbookmark"
        case dislike = "work_dislike"
        case undislike = "work_undislike"

        /// The event that undoes this one, so a UI toggle does not have to hold
        /// the pairing itself.
        public var inverse: WorkReaction {
            switch self {
            case .like: .unlike
            case .unlike: .like
            case .repost: .unrepost
            case .unrepost: .repost
            case .bookmark: .unbookmark
            case .unbookmark: .bookmark
            case .dislike: .undislike
            case .undislike: .dislike
            }
        }
    }

    /// Applies a reaction and returns the work as the server now holds it.
    ///
    /// A JSON caller gets `200` and the same `WorkDTO` `GET /works/{id}` would
    /// answer, which is the whole row — counts and the viewer's own state —
    /// after the write. Nil means the server answered `204` with no body: that
    /// is what a deployment older than this contract does, and the caller's job
    /// is then to fetch the work back rather than to guess at the new numbers.
    /// The fallback exists for exactly that reason and should stop being
    /// exercised once every box is rebuilt.
    @discardableResult
    func react(_ reaction: WorkReaction, workID: String) async throws -> Work? {
        let data = try await postJSONEventReturningData(
            reaction.rawValue, fields: [("work_id", .string(workID))]
        )
        guard !data.isEmpty else { return nil }
        return try? APIClient.makeDecoder().decode(Work.self, from: data)
    }

    /// Asks the server to push `work_engagement` for this work while it is on
    /// screen. Idempotent, and a registration rather than client state: the
    /// server decides what it sends and to whom.
    func watchWork(id: String) async throws {
        try await postJSONEvent("watch_post", fields: [("post_id", .string(id))])
    }

    /// Stops the pushes for one work. Idempotent, including for a work that was
    /// never watched.
    func unwatchWork(id: String) async throws {
        try await postJSONEvent("unwatch_post", fields: [("post_id", .string(id))])
    }

    /// Soft-deletes one of the caller's own works. `403` when the caller does
    /// not own it — ownership is checked in `SoftDeleteWork`, not here.
    func deleteWork(id: String) async throws {
        try await postJSONEvent("work_delete", fields: [("work_id", .string(id))])
    }

    /// Follows or unfollows by handle.
    ///
    /// Form-encoded, not JSON: `followEvent` reads `target_handle` through
    /// `r.FormValue` and never looks at the JSON body, so a JSON request is
    /// answered `400 target_pial or target_handle required`.
    ///
    /// By handle rather than by PIAL deliberately. `target_pial` is the other
    /// accepted address, and a PIAL must never appear in anything a client
    /// holds — the server resolves the handle to the identity itself.
    ///
    /// The response is an HTML fragment (the re-rendered follow button), which
    /// is of no use here; the status is the answer.
    func setFollowing(_ following: Bool, handle: String) async throws {
        try await postFormEvent(
            following ? "follow" : "unfollow",
            fields: ["target_handle": handle.hasPrefix("@") ? String(handle.dropFirst()) : handle]
        )
    }

    /// Blocks or unblocks by handle. Reads `target_handle` from either encoding;
    /// JSON is used for consistency with the other JSON-capable events.
    func setBlocked(_ blocked: Bool, handle: String) async throws {
        try await postJSONEvent(
            blocked ? "block" : "unblock",
            fields: [("target_handle", .string(handle))]
        )
    }

    /// Mutes or unmutes by handle.
    func setMuted(_ muted: Bool, handle: String) async throws {
        try await postJSONEvent(
            muted ? "mute" : "unmute",
            fields: [("target_handle", .string(handle))]
        )
    }

    /// Sends a tip, in hundredths of an AET — `amount_aet` is parsed with
    /// `strconv.Atoi` and then divided by 100 before it reaches the ledger.
    ///
    /// Form-encoded: `tipEvent` is another `r.FormValue`-only handler.
    /// `workID`, when given, attributes the tip to a work so its card can show
    /// what it has earned; the server checks the work belongs to `handle`.
    func tip(handle: String, amountAETHundredths: Int, workID: String? = nil) async throws {
        var fields = [
            "target_handle": handle.hasPrefix("@") ? String(handle.dropFirst()) : handle,
            "amount_aet": String(amountAETHundredths),
        ]
        if let workID { fields["work_id"] = workID }
        try await postFormEvent("tip", fields: fields)
    }

    /// Casts (or changes) the viewer's ballot on a poll. `400` for an option
    /// out of range, a closed poll, or a work that is not a poll.
    func votePoll(workID: String, optionIndex: Int) async throws {
        try await postJSONEvent("poll_vote", fields: [
            ("work_id", .string(workID)),
            ("option_idx", .int(Int64(optionIndex))),
        ])
    }

    /// Why a work is being reported. The server's closed list.
    enum ReportReason: String, CaseIterable, Sendable, Identifiable {
        case spam, harassment, hate, nsfw, violence, illegal, other

        public var id: String { rawValue }

        public var title: String {
            switch self {
            case .spam: return "Spam"
            case .harassment: return "Harassment"
            case .hate: return "Hate"
            case .nsfw: return "Adult content without a gate"
            case .violence: return "Violence"
            case .illegal: return "Illegal content"
            case .other: return "Something else"
            }
        }
    }

    /// Reports a work. Goes to moderation with the reason, never to the author.
    func report(workID: String, reason: ReportReason) async throws {
        try await postJSONEvent("report", fields: [
            ("post_id", .string(workID)),
            ("reason", .string(reason.rawValue)),
        ])
    }

    /// Marks one notification — and the group it heads — read.
    func markNotificationRead(id: String) async throws {
        try await postJSONEvent("notif_read", fields: [("notif_id", .string(id))])
    }

    /// Clears the badge.
    func markAllNotificationsRead() async throws {
        try await postJSONEvent("notif_read_all", fields: [])
    }

    /// Pins (or unpins) one of the caller's own works to their profile.
    func setPinned(_ pinned: Bool, workID: String) async throws {
        try await postJSONEvent(pinned ? "pin_work" : "unpin_work", fields: [("work_id", .string(workID))])
    }

    /// Buys a priced work outright. The server reads the price from the work
    /// itself inside the transaction that moves the money, so nothing about
    /// the amount is sent — a client that could name a price could name a
    /// lower one. `402` when the balance cannot cover it, `400` for a work
    /// with no price or the caller's own; a repeat is `204` and charges
    /// nothing, so a retry after a timeout is safe.
    func purchase(workID: String) async throws {
        try await postJSONEvent("work_purchase", fields: [("work_id", .string(workID))])
    }

    /// One signal about a work on a ranked surface. Never about the author,
    /// never visible to them.
    func notInterested(workID: String) async throws {
        try await postJSONEvent("not_interested", fields: [("work_id", .string(workID))])
    }

    /// Sets who may reply to a work the caller owns.
    ///
    /// Form-encoded, and note the vocabulary is not the payload's: this endpoint
    /// maps `everyone` to the stored value `open`, while `comment_gating` inside
    /// a signed payload is stored verbatim. The two spellings of the same idea
    /// are the server's; ``CommentGating`` is the one this client speaks and the
    /// mapping happens on the far side.
    func setReplyRestriction(_ gating: CommentGating, workID: String) async throws {
        try await postFormEvent("set_reply_restriction", fields: [
            "post_id": workID,
            "restriction": gating.rawValue,
        ])
    }

    // MARK: - Visions

    /// What `vision` takes. Unsigned, as feed-engine's `POST /visions` is.
    struct VisionDraft: Sendable, Equatable {
        /// text | image
        public var contentType = "text"
        public var body = ""
        public var background = "void"
        public var typeface = "grotesk"
        public var align = "center"
        /// Server path of the uploaded image, for `image`.
        public var mediaURL: String?
        /// everyone | subscribers
        public var audience = "everyone"
        public var ttlHours = 24
        public var allowReplies = true
        public var pollOptions: [String]?
        public var isNSFW = false
        /// camera | composer
        public var source = "composer"

        public init() {}
    }

    /// Posts a vision and returns its id.
    func postVision(_ draft: VisionDraft) async throws -> String {
        var fields: [(String, CanonicalJSON.Value)] = [
            ("content_type", .string(draft.contentType)),
            ("body", .string(draft.body)),
            ("artboard_background", .string(draft.background)),
            ("artboard_typeface", .string(draft.typeface)),
            ("artboard_align", .string(draft.align)),
            ("audience", .string(draft.audience)),
            ("ttl_hours", .int(Int64(draft.ttlHours))),
            ("allow_replies", .bool(draft.allowReplies)),
            ("is_nsfw", .bool(draft.isNSFW)),
            ("source", .string(draft.source)),
        ]
        if let mediaURL = draft.mediaURL { fields.append(("media_url", .string(mediaURL))) }
        if let options = draft.pollOptions { fields.append(("poll_options", .array(options.map { .string($0) }))) }
        let data = try await postJSONEventReturningData("vision", fields: fields)
        struct Created: Decodable { let visionID: String; enum CodingKeys: String, CodingKey { case visionID = "vision_id" } }
        return try JSONDecoder().decode(Created.self, from: data).visionID
    }

    func markVisionSeen(id: String) async throws {
        try await postJSONEvent("vision_seen", fields: [("vision_id", .string(id))])
    }

    func deleteVision(id: String) async throws {
        try await postJSONEvent("vision_delete", fields: [("vision_id", .string(id))])
    }

    /// A private reply: reaches the author and nobody else.
    func replyToVision(id: String, body: String) async throws {
        try await postJSONEvent("vision_reply", fields: [("vision_id", .string(id)), ("body", .string(body))])
    }

    func voteVisionPoll(id: String, optionIndex: Int) async throws {
        try await postJSONEvent("vision_poll_vote", fields: [("vision_id", .string(id)), ("option_idx", .int(Int64(optionIndex)))])
    }

    /// Hides one creator's visions from the tray; their works stay.
    func setVisionsMuted(_ muted: Bool, handle: String) async throws {
        try await postJSONEvent(muted ? "vision_mute" : "vision_unmute", fields: [("target_handle", .string(handle))])
    }

    // MARK: - Live

    /// Opens a room and answers it, already live.
    func startLive(_ request: LiveStartRequest) async throws -> LiveStream {
        let data = try await postJSONEventReturningData("live_start", fields: [
            ("title", .string(request.title.trimmingCharacters(in: .whitespacesAndNewlines))),
            ("description", .string(request.description)),
            ("audience", .string(request.audience)),
            ("lane", .string(request.lane)),
            ("tip_goal_aet", .int(Int64(request.tipsEnabled ? request.tipGoalAET : 0))),
            ("notify_followers", .bool(request.notifyFollowers)),
            ("save_replay", .bool(request.saveReplay)),
            ("is_nsfw", .bool(request.isNSFW)),
        ])
        return try APIClient.makeDecoder().decode(LiveStream.self, from: data)
    }

    /// Ends the caller's room and answers the summary.
    func endLive(id: String) async throws -> LiveSummary {
        let data = try await postJSONEventReturningData("live_end", fields: [("stream_id", .string(id))])
        return try APIClient.makeDecoder().decode(LiveSummary.self, from: data)
    }

    /// Says something in a room; answers the stored line.
    func sendLiveChat(id: String, body: String) async throws -> LiveChatMessage {
        let data = try await postJSONEventReturningData("live_chat", fields: [("stream_id", .string(id)), ("body", .string(body))])
        return try APIClient.makeDecoder().decode(LiveChatMessage.self, from: data)
    }

    /// Broadcaster pins a line; "" clears it.
    func pinLive(id: String, body: String) async throws {
        try await postJSONEvent("live_pin", fields: [("stream_id", .string(id)), ("body", .string(body))])
    }

    /// A viewer's ♥.
    func heartLive(id: String) async throws {
        try await postJSONEvent("live_heart", fields: [("stream_id", .string(id))])
    }

    /// Tips the broadcaster, attributed to the room so the goal moves.
    func tipLive(id: String, amountAETHundredths: Int) async throws {
        try await postFormEvent("tip", fields: ["stream_id": id, "amount_aet": String(amountAETHundredths)])
    }

    // MARK: - Account

    /// Changes the fields set on `update` and answers the fresh profile.
    ///
    /// Encoded from ``ProfileUpdate/Event`` rather than assembled here: two of
    /// the fields are objects (`social_links`, `external_tip_links`) and the
    /// canonical writer has no object case — it exists to reproduce a hash, not
    /// to serve as a general encoder. The declaration of what this event
    /// carries therefore lives with the model, in one place a test can read.
    func updateProfile(_ update: ProfileUpdate) async throws -> CurrentUser {
        let data = try await postEncodedEvent(update.event())
        return try APIClient.makeDecoder().decode(CurrentUser.self, from: data)
    }

    func changePassword(current: String, new: String) async throws {
        try await postJSONEvent("password_change", fields: [("current_password", .string(current)), ("new_password", .string(new))])
    }

    func revokeSession(id: String) async throws {
        try await postJSONEvent("session_revoke", fields: [("session_id", .string(id))])
    }

    func revokeOtherSessions() async throws {
        try await postJSONEvent("sessions_revoke_others", fields: [])
    }

    /// Rewrites the body of the caller's own work, within the server's edit
    /// window. `409` when the window has closed.
    func editWork(id: String, body: String) async throws {
        try await postJSONEvent("work_edit", fields: [("work_id", .string(id)), ("body", .string(body))])
    }

    // MARK: - Settings

    /// Flips one of the web's settings switches.
    ///
    /// Two things here are the server's spelling and not a choice:
    ///
    /// - A toggle sends `"1"` or `"0"` as the string `value`. It is a form
    ///   checkbox on the far side and the handler parses the string; a JSON
    ///   `true` is not what it reads.
    /// - The content setting's field is called `setting`, not `value`. It is
    ///   the one handler in the group that names its field after itself, and
    ///   sending `value` to it posts nothing at all — the option comes back
    ///   unchanged and the picker springs back with no error to show.
    func setSetting(_ setting: AccountSetting, value: String) async throws {
        let field = setting == .contentSetting ? "setting" : "value"
        try await postJSONEvent(setting.rawValue, fields: [(field, .string(value))])
    }

    /// Changes the notification preferences named by `change` and answers the
    /// whole set as Herald now holds it.
    ///
    /// The answer replaces what the screen was showing rather than being merged
    /// into it. A preference the server refused, or one another device changed
    /// while this request was in flight, is then visible immediately instead of
    /// waiting for the next read.
    func setNotificationPrefs(_ change: NotificationPrefsChange) async throws -> NotificationPrefs {
        let data = try await postEncodedEvent(NotificationPrefsEvent(change: change))
        return try APIClient.makeDecoder().decode(NotificationPrefs.self, from: data)
    }

    /// Turns two-factor sign-in on, proving the authenticator was enrolled by
    /// sending a code it produced. Answers the account as it now stands.
    func twoFactorEnable(code: String) async throws -> CurrentUser {
        let data = try await postJSONEventReturningData("two_fa_enable", fields: [("code", .string(code))])
        return try APIClient.makeDecoder().decode(CurrentUser.self, from: data)
    }

    /// Turns it off, which also takes a current code: an unlocked phone left on
    /// a table is not authority to remove the second factor.
    func twoFactorDisable(code: String) async throws -> CurrentUser {
        let data = try await postJSONEventReturningData("two_fa_disable", fields: [("code", .string(code))])
        return try APIClient.makeDecoder().decode(CurrentUser.self, from: data)
    }

    /// A fresh set of single-use codes. The previous set stops working.
    ///
    /// Called rather than guarded behind a capability check: a deployment
    /// without a generator answers `404 unknown event_type`, and the message it
    /// sends is the one worth showing — the alternative is an app that decides
    /// on its own that a feature does not exist.
    func twoFactorBackupCodes() async throws -> [String] {
        let data = try await postJSONEventReturningData("two_fa_backup_codes", fields: [])
        struct Codes: Decodable { let codes: [String] }
        return try APIClient.makeDecoder().decode(Codes.self, from: data).codes
    }

    /// Asks for the account to be deleted. The server records the request and
    /// revokes every session; the account goes on existing for the grace period
    /// the deployment sets, and this client is signed out either way.
    func deleteAccount(reason: String = "") async throws {
        try await postJSONEvent("account_delete", fields: [("reason", .string(reason))])
    }

    /// Hides the account until the next sign-in. Sessions are revoked, so the
    /// app signs itself out after this returns.
    func deactivateAccount(reason: String = "") async throws {
        try await postJSONEvent("deactivate_account", fields: [("reason", .string(reason))])
    }

    /// The `notification_prefs` event: the changed keys, flattened alongside
    /// the event type rather than nested under one, because that is how every
    /// other event on this lane is read.
    private struct NotificationPrefsEvent: Encodable {
        let change: NotificationPrefsChange

        private struct Key: CodingKey {
            let stringValue: String
            var intValue: Int? { nil }
            init(_ stringValue: String) { self.stringValue = stringValue }
            init?(stringValue: String) { self.stringValue = stringValue }
            init?(intValue: Int) { nil }
        }

        func encode(to encoder: any Encoder) throws {
            var c = encoder.container(keyedBy: Key.self)
            try c.encode("notification_prefs", forKey: Key("event_type"))
            try change.encode(to: encoder)
        }
    }

    // MARK: - Frequencies

    // The audio rooms. Every one of these is `POST /events` with a
    // `frequency_*` event_type and a `frequency_id`, and almost every one
    // answers the whole ``FrequencyRoom`` as the caller may now see it —
    // roles, capability flags and counts included. That is deliberate: after a
    // mute or a promotion the client must not work out what changed, it is
    // handed the new truth and replaces what it held.
    //
    // Targets are named by **handle**. The brain speaks PIALs and feed-engine
    // resolves them; a handle is the only address a client is allowed to know.
    //
    // Errors keep the brain's own code (`locked`, `speakers_full`,
    // `verity_tier_too_low`, `not_live`, …) in the message, so a screen can
    // branch on it rather than showing one apology for twenty situations.

    /// Opens a room, or schedules one when the draft carries a date.
    /// Answers the summary alone — there is no stage yet to answer with.
    func frequencyCreate(_ draft: FrequencyDraft) async throws -> Frequency {
        let data = try await postJSONEventReturningData("frequency_create", fields: draft.eventFields())
        return try APIClient.makeDecoder().decode(Frequency.self, from: data)
    }

    /// Changes a room's settings. Only the fields the draft carries are sent.
    func frequencyUpdate(id: String, draft: FrequencyDraft) async throws -> FrequencyRoom {
        try await frequencyRoomEvent("frequency_update", fields: [("frequency_id", .string(id))] + draft.eventFields())
    }

    /// Moves, or clears, the start time. `nil` sends an empty `scheduled_at`,
    /// which is how the server is told to put the room back to a draft.
    func frequencySchedule(id: String, at date: Date?) async throws -> FrequencyRoom {
        try await frequencyRoomEvent("frequency_schedule", fields: [
            ("frequency_id", .string(id)),
            ("scheduled_at", .string(date.map(APIClient.rfc3339String) ?? "")),
        ])
    }

    /// Goes live.
    func frequencyStart(id: String) async throws -> FrequencyRoom {
        try await frequencyRoomEvent("frequency_start", fields: [("frequency_id", .string(id))])
    }

    /// Ends the room for everyone. Host and cohosts only — `viewer.canEnd`.
    func frequencyEnd(id: String) async throws -> FrequencyRoom {
        try await frequencyRoomEvent("frequency_end", fields: [("frequency_id", .string(id))])
    }

    /// Calls off a room that never started.
    func frequencyCancel(id: String) async throws -> FrequencyRoom {
        try await frequencyRoomEvent("frequency_cancel", fields: [("frequency_id", .string(id))])
    }

    /// Tunes in. The room comes back with this reader's own `viewer` filled in.
    func frequencyJoin(id: String) async throws -> FrequencyRoom {
        try await frequencyRoomEvent("frequency_join", fields: [("frequency_id", .string(id))])
    }

    /// Leaves. `204`, no body — the room the caller can still read is a fresh
    /// `GET`, not something inferred from here.
    func frequencyLeave(id: String) async throws {
        try await postJSONEvent("frequency_leave", fields: [("frequency_id", .string(id))])
    }

    /// Raises a hand. Answers the request as stored; the verity tier that
    /// decides whether it is allowed is read server-side and never sent.
    func frequencyRequestMic(id: String, reason: String) async throws -> FrequencyRequest {
        let data = try await postJSONEventReturningData("frequency_request_mic", fields: [
            ("frequency_id", .string(id)),
            ("reason", .string(reason)),
        ])
        return try APIClient.makeDecoder().decode(FrequencyRequest.self, from: data)
    }

    /// Takes a hand back down. `204`.
    func frequencyRequestWithdraw(id: String, requestID: String) async throws {
        try await postJSONEvent("frequency_request_withdraw", fields: [
            ("frequency_id", .string(id)),
            ("request_id", .string(requestID)),
        ])
    }

    /// Backs someone else's raised hand, and answers the count as it now
    /// stands. A second upvote from the same reader is counted once — the
    /// answer says `counted: false` and the number is unchanged.
    @discardableResult
    func frequencyRequestUpvote(id: String, requestID: String) async throws -> Int {
        let data = try await postJSONEventReturningData("frequency_request_upvote", fields: [
            ("frequency_id", .string(id)),
            ("request_id", .string(requestID)),
        ])
        struct Upvoted: Decodable {
            let counted: Bool
            let upvotes: Int
        }
        return try APIClient.makeDecoder().decode(Upvoted.self, from: data).upvotes
    }

    /// Moderator: brings someone up to the stage.
    func frequencyRequestApprove(id: String, requestID: String) async throws -> FrequencyRoom {
        try await frequencyRoomEvent("frequency_request_approve", fields: [
            ("frequency_id", .string(id)),
            ("request_id", .string(requestID)),
        ])
    }

    /// Moderator: turns a hand down.
    func frequencyRequestDecline(id: String, requestID: String) async throws -> FrequencyRoom {
        try await frequencyRoomEvent("frequency_request_decline", fields: [
            ("frequency_id", .string(id)),
            ("request_id", .string(requestID)),
        ])
    }

    /// Mutes or unmutes a speaker. A speaker muting themselves is the same
    /// event with their own handle.
    func frequencyMute(id: String, handle: String, muted: Bool) async throws -> FrequencyRoom {
        try await frequencyRoomEvent("frequency_mute", fields: [
            ("frequency_id", .string(id)),
            ("handle", .string(APIClient.bareHandle(handle))),
            ("muted", .bool(muted)),
        ])
    }

    /// Sends a speaker back to the audience.
    func frequencyDemote(id: String, handle: String) async throws -> FrequencyRoom {
        try await frequencyRoomEvent("frequency_demote", fields: [
            ("frequency_id", .string(id)),
            ("handle", .string(APIClient.bareHandle(handle))),
        ])
    }

    /// Removes someone from the room. They may come back unless blocked.
    func frequencyRemove(id: String, handle: String, reason: String = "") async throws -> FrequencyRoom {
        try await frequencyRoomEvent("frequency_remove", fields: [
            ("frequency_id", .string(id)),
            ("handle", .string(APIClient.bareHandle(handle))),
            ("reason", .string(reason)),
        ])
    }

    /// Bars someone from this room for good.
    func frequencyBlock(id: String, handle: String, reason: String = "") async throws -> FrequencyRoom {
        try await frequencyRoomEvent("frequency_block", fields: [
            ("frequency_id", .string(id)),
            ("handle", .string(APIClient.bareHandle(handle))),
            ("reason", .string(reason)),
        ])
    }

    func frequencyUnblock(id: String, handle: String) async throws -> FrequencyRoom {
        try await frequencyRoomEvent("frequency_unblock", fields: [
            ("frequency_id", .string(id)),
            ("handle", .string(APIClient.bareHandle(handle))),
        ])
    }

    /// Makes someone a cohost, or takes it back.
    func frequencyCohost(id: String, handle: String, cohost: Bool) async throws -> FrequencyRoom {
        try await frequencyRoomEvent("frequency_cohost", fields: [
            ("frequency_id", .string(id)),
            ("handle", .string(APIClient.bareHandle(handle))),
            ("cohost", .bool(cohost)),
        ])
    }

    /// Locks the room: nobody new tunes in.
    func frequencyLock(id: String, locked: Bool) async throws -> FrequencyRoom {
        try await frequencyRoomEvent("frequency_lock", fields: [
            ("frequency_id", .string(id)),
            ("locked", .bool(locked)),
        ])
    }

    /// Opens or closes the hand-raising queue.
    func frequencyRequestsOpen(id: String, open: Bool) async throws -> FrequencyRoom {
        try await frequencyRoomEvent("frequency_requests_open", fields: [
            ("frequency_id", .string(id)),
            ("open", .bool(open)),
        ])
    }

    /// Every room-answering frequency event, decoded once.
    private func frequencyRoomEvent(
        _ eventType: String,
        fields: [(String, CanonicalJSON.Value)]
    ) async throws -> FrequencyRoom {
        let data = try await postJSONEventReturningData(eventType, fields: fields)
        return try APIClient.makeDecoder().decode(FrequencyRoom.self, from: data)
    }

    // MARK: - Plumbing

    /// A JSON event whose success body is wanted back.
    private func postJSONEventReturningData(
        _ eventType: String,
        fields: [(String, CanonicalJSON.Value)]
    ) async throws -> Data {
        let body = CanonicalJSON.object([("event_type", .string(eventType))] + fields)
        let (data, status) = try await sendRaw(.events, body: body, contentType: "application/json")
        guard (200..<300).contains(status) else {
            throw MalkuthError.rejected(status: status, reason: Self.reason(from: data, status: status))
        }
        return data
    }

    /// An event whose body is an `Encodable` carrying its own `event_type`.
    ///
    /// For the two events with nested objects in them. `JSONEncoder` rather
    /// than the canonical writer because nothing here is hashed: these bodies
    /// are read as fields, not verified as bytes, so key order does not matter
    /// and object values do.
    private func postEncodedEvent(_ body: some Encodable) async throws -> Data {
        let encoded: Data
        do {
            encoded = try JSONEncoder().encode(body)
        } catch {
            throw MalkuthError.rejected(status: 0, reason: "could not encode the event: \(error)")
        }
        let (data, status) = try await sendRaw(.events, body: encoded, contentType: "application/json")
        guard (200..<300).contains(status) else {
            throw MalkuthError.rejected(status: status, reason: Self.reason(from: data, status: status))
        }
        return data
    }

    private func postJSONEvent(
        _ eventType: String,
        fields: [(String, CanonicalJSON.Value)]
    ) async throws {
        let body = CanonicalJSON.object([("event_type", .string(eventType))] + fields)
        let (data, status) = try await sendRaw(.events, body: body, contentType: "application/json")
        guard (200..<300).contains(status) else {
            throw MalkuthError.rejected(status: status, reason: Self.reason(from: data, status: status))
        }
    }

    private func postFormEvent(_ eventType: String, fields: [String: String]) async throws {
        var items = [URLQueryItem(name: "event_type", value: eventType)]
        items += fields.sorted { $0.key < $1.key }.map { URLQueryItem(name: $0.key, value: $0.value) }
        var components = URLComponents()
        components.queryItems = items
        // `+` is a space in form encoding, so a literal one has to be escaped.
        let encoded = (components.percentEncodedQuery ?? "")
            .replacingOccurrences(of: "+", with: "%2B")

        let (data, status) = try await sendRaw(
            .events,
            body: Data(encoded.utf8),
            contentType: "application/x-www-form-urlencoded"
        )
        guard (200..<300).contains(status) else {
            throw MalkuthError.rejected(status: status, reason: Self.reason(from: data, status: status))
        }
    }

    /// The server's own words, when it left any.
    ///
    /// The write lane answers `http.Error` — a bare line of text — and the
    /// HTMX error path answers a fragment of HTML. Neither is a JSON envelope,
    /// so the text is kept as-is and truncated: `cid mismatch` is the entire
    /// diagnostic for a canonical-encoding bug and losing it would leave nothing
    /// to debug from.
    private static func reason(from data: Data, status: Int) -> String {
        let text = String(decoding: data.prefix(500), as: UTF8.self)
            .trimmingCharacters(in: .whitespacesAndNewlines)
        if text.isEmpty { return "no message (HTTP \(status))" }
        return text
    }
}


public extension FrequencyDraft {

    /// The draft as `frequency_create` and `frequency_update` read it.
    ///
    /// One list, used by both, so the two events cannot drift apart in what
    /// they call the same setting. `scheduled_at` is omitted rather than sent
    /// empty when there is no date: on `frequency_update` an empty value would
    /// read as "clear it", and clearing a schedule is
    /// ``APIClient/frequencySchedule(id:at:)``'s job, not a side effect of
    /// renaming a room.
    func eventFields() -> [(String, CanonicalJSON.Value)] {
        var fields: [(String, CanonicalJSON.Value)] = [
            ("title", .string(title.trimmingCharacters(in: .whitespacesAndNewlines))),
            ("description", .string(description)),
            ("visibility", .string(visibility)),
            ("language", .string(language)),
            ("is_nsfw", .bool(isNSFW)),
            ("recording_enabled", .bool(recordingEnabled)),
            ("max_speakers", .int(Int64(maxSpeakers))),
            ("max_listeners", .int(Int64(maxListeners))),
        ]
        if let scheduledAt {
            fields.append(("scheduled_at", .string(APIClient.rfc3339String(scheduledAt))))
        }
        return fields
    }
}

extension APIClient {

    /// RFC 3339, the spelling the Go side parses `scheduled_at` with.
    static func rfc3339String(_ date: Date) -> String {
        rfc3339Out.string(from: date)
    }

    nonisolated(unsafe) private static let rfc3339Out: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        return f
    }()

    /// `@handle` and `handle` are the same person; the server takes the bare one.
    static func bareHandle(_ handle: String) -> String {
        handle.hasPrefix("@") ? String(handle.dropFirst()) : handle
    }
}
