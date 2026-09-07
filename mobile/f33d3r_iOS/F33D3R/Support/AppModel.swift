import Foundation
import Observation
import F33D3RKit

/// App-wide state: which F33D3R instance we talk to, who is signed in, and the
/// one place every mutation goes through.
///
/// `@MainActor` because everything it publishes drives SwiftUI. The actual work
/// happens in `APIClient`, `SessionStore` and `MalkuthSigner`, which are actors
/// of their own; this type is the bridge that turns their results into
/// observable state.
///
/// ## Mutations are server-confirmed
///
/// Nothing here flips a value before the server has. A like sends `work_like`,
/// then fetches the work back and hands the server's row to every list that
/// holds it. The screen changes when the truth changes, and the round trip to
/// a local server is a few milliseconds — so there is no optimistic state to
/// reconcile and no way for a card to say something the server does not.
@MainActor
@Observable
final class AppModel {
    /// Which server to talk to.
    ///
    /// DEBUG defaults to the dev server on this Mac. The Simulator cannot
    /// reach a Mac's `localhost` under some networking setups, but `127.0.0.1`
    /// works because the Simulator shares the host's network stack.
    ///
    /// `F33D3R_BASE_URL` in the scheme's environment picks another server for
    /// a run. The platform's Docker stack on the Linux box is
    /// `https://xxxg-00w0.local:8443` (its LAN name via Bonjour; the address
    /// in `LIVE_PUBLIC_HOST` works too). That is real TLS from Caddy's local
    /// CA, so no ATS exception is needed — the device trusts the CA once,
    /// see `infra/README-CERTS.md`. Reaching a `.local` name or a LAN address
    /// is what the `NSLocalNetworkUsageDescription` in the project is for;
    /// without it iOS drops the connection silently.
    ///
    /// Release builds always point at production.
    static var defaultBaseURL: URL {
        #if DEBUG
        // The scheme's variable only reaches the app when Xcode launches it.
        // Tapping the icon on a phone gets none, so the build would fall back to
        // 127.0.0.1 — a server that exists on the Mac and never on the device —
        // and fail as "can't reach F33D3R" for a reason nothing on screen could
        // explain. The last address given is therefore remembered, so a build
        // put on a device keeps talking to the server it was pointed at.
        if let override = ProcessInfo.processInfo.environment["F33D3R_BASE_URL"],
           let url = URL(string: override), url.host != nil {
            UserDefaults.standard.set(override, forKey: rememberedBaseURLKey)
            return url
        }
        if let remembered = UserDefaults.standard.string(forKey: rememberedBaseURLKey),
           let url = URL(string: remembered), url.host != nil {
            return url
        }
        return URL(string: "http://127.0.0.1:8081")!
        #else
        return URL(string: "https://f33d3r.com")!
        #endif
    }

    #if DEBUG
    /// Where the last `F33D3R_BASE_URL` is kept. Debug only: a release build
    /// resolves to production and never reads this.
    static let rememberedBaseURLKey = "f33d3r.debug.baseURL"

    /// Forgets the remembered server, so the next launch falls back to the
    /// default. For a device that should stop talking to a development host.
    static func forgetRememberedBaseURL() {
        UserDefaults.standard.removeObject(forKey: rememberedBaseURLKey)
    }
    #endif

    let client: APIClient
    let sessions: SessionStore
    /// Fetches walled media with the session attached. One per app: it holds
    /// the image cache the feed scrolls through.
    let media: MediaLoader
    /// This device's Work-signing key. Registered with the key authority on
    /// every sign-in, so the key the server holds is the key that is posting.
    let signer: MalkuthSigner

    /// The reader's lane and density, restored from the device at launch.
    let preferences: FeedPreferences

    private(set) var state: SessionStore.State = .unknown

    /// The signed-in user together with the PIAL their works are signed as.
    /// Nil until `/me` has answered after sign-in; compose refuses to open
    /// without it rather than sign as nobody.
    private(set) var identity: AuthenticatedIdentity?
    /// Why the last key registration failed, if it did. Shown once on compose.
    private(set) var signingProblem: String?

    /// The freshest row the server has given us for each work touched by a
    /// mutation. Lists read through `current(_:)` so a card in a thread and
    /// the same card in a feed cannot disagree.
    private(set) var latestWorks: [String: Work] = [:]
    /// Works the server confirmed deleted this session.
    private(set) var deletedWorkIDs: Set<String> = []

    /// The signed-in user's own stream: unread count, new works from followed
    /// accounts, deletions, balance. Opened on sign-in, closed on sign-out.
    private(set) var signals: UserSignals?

    init(baseURL: URL = AppModel.defaultBaseURL, preferences: FeedPreferences = FeedPreferences()) {
        let store = SessionStore()
        self.sessions = store
        let apiClient = APIClient(baseURL: baseURL, tokens: store)
        self.client = apiClient
        self.media = MediaLoader(client: apiClient)
        self.signer = MalkuthSigner()
        self.preferences = preferences
    }

    // MARK: - Session

    /// Restores a stored session and mirrors every later change into `state`.
    func start() async {
        #if DEBUG
        if isSampleMode {
            state = .signedIn(SampleData.currentUser)
            return
        }
        #endif
        await sessions.observe { [weak self] newState in
            Task { @MainActor in self?.apply(newState) }
        }
        await sessions.restore(using: client)

        #if DEBUG
        // A Simulator driven through `simctl` cannot type; this signs in for one
        // run with credentials handed to that run, and only when the device has
        // no session of its own.
        if let credentials = Self.debugCredentials, await sessions.currentToken() == nil {
            try? await signIn(handle: credentials.handle, password: credentials.password)
        }
        #endif
    }

    private func apply(_ newState: SessionStore.State) {
        let wasSignedIn = state.user != nil
        state = newState
        switch newState {
        case .signedIn:
            if identity == nil || !wasSignedIn {
                Task { await loadIdentity() }
            }
            if signals == nil {
                #if DEBUG
                if isSampleMode { return }
                #endif
                let stream = UserSignals(client: client)
                stream.onEvent = { [weak self] event in self?.handle(event) }
                stream.onConnectionChange = { [weak self] connected in
                    self?.streamConnectionChanged(connected)
                }
                stream.connect()
                signals = stream
            }
            // The conversation list is what counts unread messages, and the
            // badge on the Messages tab is drawn from it. Created here rather
            // than when the tab is first opened, or the badge reads zero until
            // the reader goes looking for the thing it was supposed to point
            // them at.
            let list = conversationsFeed()
            Task { await list.loadIfNeeded() }
        case .signedOut, .unknown, .needsBackupCodes:
            identity = nil
            forgetWatches()
            signals?.disconnect()
            signals = nil
        }
    }

    /// What the stream said, applied to the lists on screen. Every frame is a
    /// signal to refetch or drop; nothing here invents a row.
    private func handle(_ event: UserEvent) {
        switch event {
        case .postDeleted(let id):
            deletedWorkIDs.insert(id)
            latestWorks[id] = nil
            for feed in feeds.values { feed.remove(id: id) }
        case .message(let conversationID, let message):
            // Two places care. The thread, if this conversation is open, so a
            // message appears while somebody is looking at it; and the list,
            // so the row moves to the top and the badge counts it. A frame that
            // named the conversation but carried no message still does both —
            // the thread refetches, and the list learns something arrived.
            if let thread = threads[conversationID] {
                Task { @MainActor in
                    if let message {
                        await thread.receive(message)
                    } else {
                        await thread.reload()
                    }
                }
            }
            conversations?.noteActivity(
                conversationID: conversationID,
                preview: message.flatMap { $0.mode == .plain ? $0.body : nil },
                at: message?.createdAt ?? Date(),
                isMine: message?.isMine ?? false
            )
        case .workEngagement(let engagement):
            // Counts only, for a work this client asked to watch. The row it is
            // applied to is the freshest one anybody holds; a work nobody is
            // showing is not worth inventing a row for, so the frame is
            // dropped.
            guard let known = knownWork(id: engagement.workID) else { return }
            remember(known.applying(engagement))
        case .liveStart, .frequencyStart:
            // Somebody went on air. The Live lane is the surface that lists
            // them, and it asks the server rather than being told what to add.
            // A frequency room is the same lane's other half today, so it
            // refreshes the same way.
            if let live = liveLaneStore {
                Task { await live.load() }
            }
        case .notify, .balance:
            break
        case .newPost:
            // Deliberately nothing. `UserSignals` has already put the id behind
            // the Following lane's "N new works" pill, which is the one place
            // this frame is allowed to change the screen — a list that grows
            // under a reading thumb is the bug the pill exists to prevent.
            // There is no per-author staleness hook to ring: a profile feed is
            // a `WorkFeed` with no notion of being out of date, and reloading
            // one that may not even be on screen would be a fetch nobody asked
            // for. When profiles need it, the hook belongs on `WorkFeed`.
            break
        }
    }

    /// The freshest row anybody holds for a work: what a mutation last brought
    /// back, else whatever list has it on screen.
    private func knownWork(id: String) -> Work? {
        if let latest = latestWorks[id] { return latest }
        for feed in feeds.values {
            if let work = feed.works.first(where: { $0.id == id }) { return work }
        }
        return nil
    }

    /// Re-reads `/me` so the profile and settings on screen are the server's.
    func refreshMe() async {
        try? await sessions.refreshUser(using: client)
    }

    /// Fetches the PIAL and registers this device's signing key. Called once
    /// per sign-in; safe to call again.
    func loadIdentity() async {
        do {
            identity = try await client.meIdentity()
        } catch {
            identity = nil
            return
        }
        do {
            try await signer.registerIfNeeded(with: client, force: true)
            signingProblem = nil
        } catch let error as MalkuthError {
            signingProblem = error.isKeyAuthorityUnavailable
                ? "The key authority isn't reachable, so this device can't sign works yet."
                : error.description
        } catch {
            signingProblem = error.localizedDescription
        }
    }

    /// Signs in, with the authenticator's code when the server has asked for
    /// one. `code` is nil on the first attempt because the app cannot know an
    /// account has two-factor until the refusal says so.
    func signIn(handle: String, password: String, code: String? = nil) async throws {
        let session = try await client.login(
            .init(handle: handle, password: password, deviceName: Self.deviceName, code: code)
        )
        await sessions.signIn(session)
        // The one moment the messaging key can be unwrapped. It is wrapped
        // under something derived from the password, the server holds only the
        // wrapped form, and nothing else in the app ever sees a password. A
        // failure here does not fail the sign-in: it costs sealed messages,
        // not the account.
        await unlockMessaging(handle: handle, password: password)
    }

    func signUp(handle: String, password: String, displayName: String) async throws {
        let session = try await client.signup(
            .init(handle: handle, password: password, displayName: displayName, deviceName: Self.deviceName)
        )
        await sessions.signIn(session)
        await unlockMessaging(handle: handle, password: password)
    }

    /// Opens, or creates, this account's messaging identity.
    private func unlockMessaging(handle: String, password: String) async {
        let opener = GnosisVault(client: client, account: handle.lowercased())
        vault = opener
        switch await opener.unlock(handle: handle, password: password) {
        case .unlocked, .provisioned:
            vaultProblem = nil
        case .unavailable(let why):
            vaultProblem = why
        }
    }

    func signOut() async {
        if let vault { await vault.lock() }
        vault = nil
        vaultProblem = nil
        forgetWatches()
        signals?.disconnect()
        signals = nil
        await sessions.signOut(using: client)
        discardFeeds()
        identity = nil
        latestWorks.removeAll()
        deletedWorkIDs.removeAll()
    }

    // MARK: - Account

    func updateProfile(_ update: ProfileUpdate) async throws {
        _ = try await client.updateProfile(update)
        try await sessions.refreshUser(using: client)
    }

    func setSetting(_ setting: AccountSetting, on: Bool) async throws {
        try await client.setSetting(setting, value: on ? "1" : "0")
        try await sessions.refreshUser(using: client)
    }

    func setContentSetting(_ id: String) async throws {
        try await client.setSetting(.contentSetting, value: id)
        try await sessions.refreshUser(using: client)
    }

    func changePassword(current: String, new: String) async throws {
        try await client.changePassword(current: current, new: new)
    }

    func activeSessions() async throws -> [SessionInfo] {
        try await client.sessions()
    }

    func revokeSession(id: String) async throws {
        try await client.revokeSession(id: id)
    }

    func revokeOtherSessions() async throws {
        try await client.revokeOtherSessions()
    }

    // MARK: - Settings

    /// What Herald may send this account.
    func notificationPreferences() async throws -> NotificationPrefs {
        try await client.notificationPreferences()
    }

    /// Sends one changed preference and answers the whole set as the server
    /// now holds it. The caller draws that answer; nothing here is inferred.
    func setNotificationPrefs(_ change: NotificationPrefsChange) async throws -> NotificationPrefs {
        try await client.setNotificationPrefs(change)
    }

    /// A fresh TOTP secret. Reading it changes nothing — only a working code
    /// does.
    func twoFactorSetup() async throws -> TwoFactorSetup {
        try await client.twoFactorSetup()
    }

    /// Turns two-factor on. The server answers with the account; `/me` is
    /// re-read so every screen holding it repaints together.
    func twoFactorEnable(code: String) async throws {
        _ = try await client.twoFactorEnable(code: code)
        try await sessions.refreshUser(using: client)
    }

    func twoFactorDisable(code: String) async throws {
        _ = try await client.twoFactorDisable(code: code)
        try await sessions.refreshUser(using: client)
    }

    /// A fresh set of single-use codes; the previous set stops working.
    func twoFactorBackupCodes() async throws -> [String] {
        let codes = try await client.twoFactorBackupCodes()
        // Generating them is what the `needs_backup_codes` gate was waiting
        // for, so the account state is re-read rather than left on the screen
        // that sent the reader here.
        try? await sessions.refreshUser(using: client)
        return codes
    }

    /// Everyone this account has blocked, and everyone it has muted.
    func blocks() async throws -> BlockList {
        try await client.blocks()
    }

    func setBlocked(_ blocked: Bool, handle: String) async throws {
        try await client.setBlocked(blocked, handle: handle)
    }

    /// The account's own data as the server wrote it, for the share sheet.
    func accountExport() async throws -> Data {
        try await client.accountExport()
    }

    /// Hides the account and signs this device out. The server revokes every
    /// session, so staying signed in here would be a screen full of requests
    /// that all answer 401.
    func deactivateAccount(reason: String) async throws {
        try await client.deactivateAccount(reason: reason)
        await signOut()
    }

    /// Asks for deletion, then signs out for the same reason.
    func deleteAccount(reason: String) async throws {
        try await client.deleteAccount(reason: reason)
        await signOut()
    }

    func followers(handle: String, cursor: String?) async throws -> UserPage {
        try await client.followers(handle: handle, cursor: cursor)
    }

    func following(handle: String, cursor: String?) async throws -> UserPage {
        try await client.following(handle: handle, cursor: cursor)
    }

    /// Uploads one image and answers the path the media origin serves it at.
    func uploadImage(_ data: Data, filename: String, mimeType: String) async throws -> String {
        try await client.uploadImage(data, filename: filename, mimeType: mimeType)
    }

    // MARK: - Feeds

    /// Live feed stores, keyed by what they are a feed *of*. Cached so a store a
    /// `.task` started loading is the one still on screen when it finishes, and
    /// so a tab switch finds the list — and the scroll position — already there.
    @ObservationIgnored private var feeds: [String: WorkFeed] = [:]
    @ObservationIgnored private var notifications: NotificationsFeed?
    @ObservationIgnored private var conversations: ConversationsFeed?
    /// What opens a sealed message on this device. Nil until the account has
    /// unlocked its messaging key, which happens at sign-in and nowhere else —
    /// the key is wrapped under something derived from the password, and the
    /// server holds only the wrapped form.
    @ObservationIgnored private var vault: GnosisVault?
    /// Said once, when an unlock could not happen, so Messages can explain
    /// itself rather than showing every sealed thread as simply shut.
    private(set) var vaultProblem: String?
    /// One store per open conversation, so a thread the reader came back to
    /// still has its messages and its scroll position.
    @ObservationIgnored private var threads: [String: MessageThreadStore] = [:]
    /// The account's own Numbers and the knocks waiting on them. One store,
    /// because both are read by one screen and refreshed together.
    @ObservationIgnored private var numbers: NumbersStore?

    func homeFeed(surface: FeedSurface) -> WorkFeed {
        cached("home:\(surface.rawValue)") {
            #if DEBUG
            if isSampleMode {
                return WorkFeed { _ in SampleData.page(lane: surface, from: self.sampleOffset) }
            }
            #endif
            return .home(client: client, surface: surface)
        }
    }

    /// The feed behind one lane of the strip, whichever kind it is.
    ///
    /// A topic lane is a tag read as a feed. The server already answers that
    /// shape, so this is only a matter of asking for the right one and keeping
    /// it cached under its own key like every other feed.
    func homeFeed(lane: FeedLane) -> WorkFeed {
        switch lane {
        case .surface(let surface):
            return homeFeed(surface: surface)
        case .topic(let tag):
            return cached("topic:\(tag)") {
                #if DEBUG
                if isSampleMode {
                    return WorkFeed { _ in SampleData.page(lane: .forYou, from: self.sampleOffset) }
                }
                #endif
                return .tag(client: self.client, tag: tag)
            }
        }
    }

    func exploreFeed(lane: ExploreLane) -> WorkFeed {
        cached("explore:\(lane.rawValue)") {
            #if DEBUG
            if isSampleMode {
                return WorkFeed { _ in SampleData.page(explore: lane, from: self.sampleOffset) }
            }
            #endif
            return .explore(client: client, lane: lane)
        }
    }

    func musicFeed() -> WorkFeed {
        cached("music") {
            #if DEBUG
            if isSampleMode {
                return WorkFeed { _ in SampleData.page(lane: .music, from: self.sampleOffset) }
            }
            #endif
            return .music(client: client)
        }
    }

    func profileFeed(handle: String, tab: Profile.Tab) -> WorkFeed {
        cached("profile:\(handle):\(tab.rawValue)") {
            #if DEBUG
            if isSampleMode {
                let works = SampleData.works.filter { $0.author.handle == handle }
                return WorkFeed { _ in WorkPage(works: works) }
            }
            #endif
            return .profile(client: client, handle: handle, tab: tab)
        }
    }

    func tagFeed(tag: String) -> WorkFeed {
        cached("tag:\(tag.lowercased())") {
            .tag(client: client, tag: tag)
        }
    }

    /// Works mentioning one ticker, behind the quote card.
    ///
    /// Search over the bare symbol, because that is what the web's /stocks page
    /// runs — `SearchWorks(db, ticker)`. Search answers one page and carries no
    /// cursor, so this is a feed of one page rather than a list pretending
    /// there is more below it.
    func stocksFeed(ticker: String) -> WorkFeed {
        let client = self.client
        let symbol = ticker.uppercased()
        return cached("stocks:\(symbol)") {
            WorkFeed { _ in
                WorkPage(works: try await client.search(symbol).works)
            }
        }
    }

    @ObservationIgnored private var search: SearchStore?

    /// Search, one store so the query and results survive a tab switch.
    func searchStore() -> SearchStore {
        if let search { return search }
        let created = SearchStore(client: client)
        search = created
        return created
    }

    @ObservationIgnored private var tray: VisionTrayStore?
    @ObservationIgnored private var liveRooms: [String: LiveRoomStore] = [:]

    /// The vision tray. One store, so the viewer and the row agree on what
    /// has been seen.
    func visionTray() -> VisionTrayStore {
        if let tray { return tray }
        let created = VisionTrayStore(client: client)
        tray = created
        return created
    }

    @ObservationIgnored private var liveLaneStore: LiveLaneStore?

    /// The Live lane's list of rooms. One store, so the lane, its badge and the
    /// stream's `live_start` frame all read and refresh the same list.
    func liveLane() -> LiveLaneStore {
        if let liveLaneStore { return liveLaneStore }
        let created = LiveLaneStore(client: client)
        liveLaneStore = created
        return created
    }

    /// How many rooms are live, when the Live lane has actually looked. Nil
    /// otherwise, so the strip falls back to what the loaded feed's page said
    /// rather than claiming nobody is on air.
    var liveRoomCount: Int? {
        guard let liveLaneStore, liveLaneStore.hasLoaded else { return nil }
        return liveLaneStore.rooms.count
    }

    /// One live room's store, kept while the app runs so leaving and coming
    /// back to a room does not replay its backlog.
    func liveRoom(id: String) -> LiveRoomStore {
        if let existing = liveRooms[id] { return existing }
        let created = LiveRoomStore(streamID: id, client: client)
        liveRooms[id] = created
        return created
    }

    @ObservationIgnored private var music: MusicPlayer?

    /// Stops the music if any is playing. A voice note about to play calls
    /// this: one audio output, one thing on it. Nothing is created to be
    /// paused.
    func pauseMusic() {
        music?.pause()
    }

    /// The one music player. A phone has one audio output, so the lane, the
    /// now-playing bar on every tab and the lock screen all read this instance.
    /// Media paths resolve against the server the client talks to.
    func musicPlayer() -> MusicPlayer {
        if let music { return music }
        let created = MusicPlayer(mediaOrigin: client.baseURL, client: client, loader: media)
        music = created
        return created
    }

    /// The reader's notifications. One store for the tab and the badge.
    func notificationsFeed() -> NotificationsFeed {
        if let notifications { return notifications }
        let created = NotificationsFeed(client: client)
        notifications = created
        return created
    }

    /// The unread badge. The stream's count is the freshest — the server
    /// pushes it on every notification and every read — then the feed's own
    /// page, then what `/me` said at sign-in.
    var unreadCount: Int {
        signals?.unread ?? notifications?.unreadCount ?? state.user?.unreadCount ?? 0
    }

    /// The conversation list. One store for the tab and its badge.
    func conversationsFeed() -> ConversationsFeed {
        if let conversations { return conversations }
        let created = ConversationsFeed(client: client)
        conversations = created
        return created
    }

    /// What opens sealed messages, rebuilt after a relaunch.
    ///
    /// A relaunch has no password, but it does not need one: the key was put in
    /// the Keychain when it was last unwrapped, and the vault finds it there.
    /// Only a device that has never signed in on this build has nothing to find.
    private func messagingVault() -> GnosisVault? {
        if let vault { return vault }
        guard let handle = state.user?.user.handle, !handle.isEmpty else { return nil }
        let opener = GnosisVault(client: client, account: handle.lowercased())
        vault = opener
        return opener
    }

    /// One conversation's messages.
    func threadStore(_ id: String) -> MessageThreadStore {
        if let existing = threads[id] { return existing }
        let created = MessageThreadStore(conversationID: id, client: client, opener: messagingVault())
        threads[id] = created
        return created
    }

    /// The account's own Numbers, its contact policy, and the contact requests
    /// waiting on it.
    func numbersStore() -> NumbersStore {
        if let numbers { return numbers }
        let created = NumbersStore(client: client)
        numbers = created
        return created
    }

    /// Unread messages, for the Messages tab. Nil until a list has arrived, so
    /// a badge is only drawn once there is something true to draw.
    var unreadMessageCount: Int {
        conversations?.unreadTotal ?? 0
    }

    func workThread(id: String) async throws -> WorkThread {
        #if DEBUG
        if isSampleMode { return SampleData.thread(id: id) }
        #endif
        let thread = try await client.work(id: id)
        remember(thread.work)
        return thread
    }

    func profile(handle: String) async throws -> Profile {
        #if DEBUG
        if isSampleMode { return SampleData.profile }
        #endif
        return try await client.profile(handle: handle)
    }

    func wallet() async throws -> WalletSnapshot {
        #if DEBUG
        if isSampleMode { return SampleData.wallet }
        #endif
        return try await client.wallet()
    }

    private func cached(_ key: String, _ make: () -> WorkFeed) -> WorkFeed {
        if let existing = feeds[key] { return existing }
        let created = make()
        feeds[key] = created
        return created
    }

    func discardFeeds() {
        feeds.removeAll()
        notifications = nil
        conversations = nil
        threads.removeAll()
        numbers = nil
        tray = nil
        search = nil
        for room in liveRooms.values { room.disconnect() }
        liveRooms.removeAll()
        liveLaneStore = nil
    }

    // MARK: - Works: the server's latest row

    /// The freshest version of a work this session has seen.
    func current(_ work: Work) -> Work {
        latestWorks[work.id] ?? work
    }

    func isDeleted(_ id: String) -> Bool { deletedWorkIDs.contains(id) }

    private func remember(_ work: Work) {
        latestWorks[work.id] = work
        for feed in feeds.values { feed.replace(work) }
    }

    /// Fetches a work back from the server and hands the row to every list.
    @discardableResult
    func refreshWork(id: String) async throws -> Work {
        let work = try await client.work(id: id).work
        remember(work)
        return work
    }

    // MARK: - Watching works

    /// The works on screen. Cards report themselves as they come and go; this
    /// is what the server *should* be pushing counts for.
    @ObservationIgnored private var visibleWorkIDs: Set<String> = []
    /// The works the server has actually been told about. The two are brought
    /// together by ``reconcileWatches()``, so a burst of scrolling costs one
    /// pass rather than one request per row that flickered past.
    @ObservationIgnored private var watchedWorkIDs: Set<String> = []
    @ObservationIgnored private var watchReconcileTask: Task<Void, Never>?

    /// A card came on screen. Watching is a registration on the server — it
    /// decides who gets `work_engagement` — not a note kept here.
    func noteWorkVisible(_ id: String) {
        guard visibleWorkIDs.insert(id).inserted else { return }
        scheduleWatchReconcile()
    }

    /// A card left the screen.
    func noteWorkHidden(_ id: String) {
        guard visibleWorkIDs.remove(id) != nil else { return }
        scheduleWatchReconcile()
    }

    /// Coalesces a scroll's worth of appearances into one pass.
    private func scheduleWatchReconcile() {
        watchReconcileTask?.cancel()
        watchReconcileTask = Task { [weak self] in
            try? await Task.sleep(for: .milliseconds(300))
            guard !Task.isCancelled else { return }
            await self?.reconcileWatches()
        }
    }

    /// Tells the server the difference between what is on screen and what it
    /// already knows about. A request that fails leaves the set unchanged, so
    /// the next pass tries it again.
    private func reconcileWatches() async {
        #if DEBUG
        if isSampleMode { return }
        #endif
        guard signals?.isConnected == true else { return }
        for id in visibleWorkIDs.subtracting(watchedWorkIDs) {
            guard (try? await client.watchWork(id: id)) != nil else { continue }
            watchedWorkIDs.insert(id)
        }
        for id in watchedWorkIDs.subtracting(visibleWorkIDs) {
            guard (try? await client.unwatchWork(id: id)) != nil else { continue }
            watchedWorkIDs.remove(id)
        }
    }

    /// The stream came up or went down.
    ///
    /// Registrations belong to a connection: a new one knows nothing, so
    /// everything on screen is registered again; a dropped one should not be
    /// left with a list of works it will never push for.
    private func streamConnectionChanged(_ connected: Bool) {
        if connected {
            watchedWorkIDs.removeAll()
            scheduleWatchReconcile()
        } else {
            let stale = watchedWorkIDs
            watchedWorkIDs.removeAll()
            guard !stale.isEmpty else { return }
            let client = self.client
            Task { for id in stale { try? await client.unwatchWork(id: id) } }
        }
    }

    // MARK: - App lifecycle

    /// The app left the screen. The stream is closed rather than left holding a
    /// socket the system will tear down anyway; the watches on it are released
    /// with it, and the id of the last frame seen is kept so the server can
    /// replay the gap when the reader comes back.
    func enterBackground() {
        signals?.disconnect()
    }

    /// The app is on screen again. A fresh connection, from a one-second
    /// backoff — the previous one's ladder died with its task — and everything
    /// still on screen is registered again as soon as it is up.
    func enterForeground() {
        signals?.connect()
    }

    /// Drops every watch without telling the server, for when the session
    /// itself is going away and the registrations go with it.
    private func forgetWatches() {
        watchReconcileTask?.cancel()
        watchReconcileTask = nil
        visibleWorkIDs.removeAll()
        watchedWorkIDs.removeAll()
    }

    // MARK: - Mutations

    /// Applies a reaction and reflects the server's answer.
    ///
    /// The write answers with the whole row, so one round trip does it. A
    /// server that still answers `204` — the Linux box until it is rebuilt —
    /// gives back nothing, and only then is the work fetched again.
    func react(_ reaction: APIClient.WorkReaction, on work: Work) async throws {
        #if DEBUG
        if isSampleMode { return }
        #endif
        if let updated = try await client.react(reaction, workID: work.id) {
            remember(updated)
        } else {
            try await refreshWork(id: work.id)
        }
    }

    func toggleLike(_ work: Work) async throws {
        try await react(current(work).likedByViewer ? .unlike : .like, on: work)
        refreshOwnTab(.likes)
    }

    func toggleRepost(_ work: Work) async throws {
        try await react(current(work).repostedByViewer ? .unrepost : .repost, on: work)
    }

    func toggleBookmark(_ work: Work) async throws {
        try await react(current(work).bookmarkedByViewer ? .unbookmark : .bookmark, on: work)
        refreshOwnTab(.saves)
    }

    /// The owner's Likes and Saves tabs are lists the server assembles from
    /// reactions; after one changes, the tab that is loaded asks again.
    private func refreshOwnTab(_ tab: Profile.Tab) {
        guard let me = state.user?.user.handle, let feed = feeds["profile:\(me):\(tab.rawValue)"] else { return }
        Task { await feed.reload() }
    }

    func deleteWork(_ work: Work) async throws {
        #if DEBUG
        if isSampleMode { return }
        #endif
        try await client.deleteWork(id: work.id)
        deletedWorkIDs.insert(work.id)
        latestWorks[work.id] = nil
        for feed in feeds.values { feed.remove(id: work.id) }
        if let parentCID = work.parentCID, let parent = latestWorks.values.first(where: { $0.cid == parentCID }) {
            try? await refreshWork(id: parent.id)
        }
    }

    /// Rewrites the body of the reader's own work and shows the server's row.
    func editWork(_ work: Work, body: String) async throws {
        try await client.editWork(id: work.id, body: body)
        try await refreshWork(id: work.id)
    }

    func setPinned(_ pinned: Bool, work: Work) async throws {
        try await client.setPinned(pinned, workID: work.id)
        try await refreshWork(id: work.id)
    }

    func setReplyRestriction(_ gating: CommentGating, work: Work) async throws {
        try await client.setReplyRestriction(gating, workID: work.id)
        try await refreshWork(id: work.id)
    }

    func votePoll(work: Work, option: Int) async throws {
        #if DEBUG
        if isSampleMode { return }
        #endif
        try await client.votePoll(workID: work.id, optionIndex: option)
        try await refreshWork(id: work.id)
    }

    /// Tips a work's author. `amountHundredths` is in hundredths of an AET.
    func tip(work: Work, amountHundredths: Int) async throws {
        try await client.tip(handle: work.author.handle, amountAETHundredths: amountHundredths, workID: work.id)
        try await refreshWork(id: work.id)
    }

    func tip(handle: String, amountHundredths: Int) async throws {
        try await client.tip(handle: handle, amountAETHundredths: amountHundredths)
    }

    func setFollowing(_ following: Bool, handle: String) async throws {
        try await client.setFollowing(following, handle: handle)
    }

    func setMuted(_ muted: Bool, handle: String) async throws {
        try await client.setMuted(muted, handle: handle)
        if muted {
            // A muted creator's works leave the lists the reader is looking at;
            // the server already omits them from the next page.
            for feed in feeds.values {
                for work in feed.works where work.author.handle == handle {
                    feed.remove(id: work.id)
                }
            }
        }
    }

    func report(work: Work, reason: APIClient.ReportReason) async throws {
        try await client.report(workID: work.id, reason: reason)
    }

    func notInterested(_ work: Work) async throws {
        #if DEBUG
        if isSampleMode { return }
        #endif
        try await client.notInterested(workID: work.id)
        for feed in feeds.values where feed.works.contains(where: { $0.id == work.id }) {
            feed.remove(id: work.id)
        }
    }

    // MARK: - Compose

    /// What the next work opened from the shell starts out saying.
    ///
    /// Starting a work from somebody's profile should start it addressed to
    /// them — the web does the same thing, hanging the handle on the profile
    /// page and reading it when the composer opens. The compose button belongs
    /// to the shell and not to any screen, so the screen cannot hand the sheet
    /// a value; it leaves one here instead, and the sheet takes it.
    ///
    /// Taken rather than read: a seed left by a screen the reader has walked
    /// away from would put a stranger's handle in the next work they write.
    private(set) var composerSeed = ""

    /// Says what a work started from the current screen should open with.
    /// The empty string is the ordinary case and means a blank draft.
    func seedComposer(_ body: String) {
        composerSeed = body
    }

    /// The seed, once. Reading it clears it.
    func takeComposerSeed() -> String {
        defer { composerSeed = "" }
        return composerSeed
    }

    /// The `@`, `#` and `$` dropdowns for one compose sheet. Knows the reader's
    /// own handle so it never offers them a mention of themselves.
    func makeComposerSuggestions() -> ComposeSuggestions {
        ComposeSuggestions(client: client, ownHandle: state.user?.user.handle)
    }

    /// A draft in the given mode, or nil while the device does not yet know
    /// the identity it would sign as.
    func makeComposer(_ mode: WorkComposer.Mode) -> WorkComposer? {
        guard let identity, identity.canSign else { return nil }
        return WorkComposer(mode: mode, client: client, signer: signer, identity: identity)
    }

    /// The saved drafts for one account. One store per handle for the life of
    /// the app, so every compose sheet sees the same list and a draft saved in
    /// one is in the header of the next.
    ///
    /// Throws when the drafts folder cannot be made — the sheet shows why and
    /// offers no Save, rather than offering a save that would fail.
    func draftStore(forHandle handle: String) throws -> ComposeDraftStore {
        if let store = draftStores[handle] { return store }
        let store = ComposeDraftStore(directory: try ComposeDraftStore.directory(forHandle: handle))
        draftStores[handle] = store
        return store
    }
    private var draftStores: [String: ComposeDraftStore] = [:]

    /// After a work is accepted: fetch it back and put the server's row where
    /// the reader will look for it.
    ///
    /// A scheduled work is fetched but put nowhere. The server's feeds leave
    /// it out until its time comes — `blockedFilter` asks for
    /// `scheduled_at <= NOW()` — and a local feed that showed it would be
    /// showing something the server's does not.
    func didPublish(_ accepted: WorkAccepted, mode: WorkComposer.Mode, scheduled: Bool = false) async {
        guard let work = try? await refreshWork(id: accepted.workID) else { return }
        if scheduled { return }
        let me = state.user?.user.handle
        switch mode {
        case .post, .quote:
            feeds["home:following"]?.insert(work)
            feeds["home:foryou"]?.insert(work)
            if let me { feeds["profile:\(me):works"]?.insert(work) }
            if work.hasMedia, let visions = feeds["home:visions"] { visions.insert(work) }
            if let quoted = mode.quoted { try? await refreshWork(id: quoted.id) }
        case .reply(let parent):
            if let me { feeds["profile:\(me):replies"]?.insert(work) }
            try? await refreshWork(id: parent.id)
        }
    }

    #if DEBUG
    /// Renders the UI against `SampleData` instead of a server. Opt-in per run
    /// via `F33D3R_SAMPLE=1`; never a fallback.
    let isSampleMode = ProcessInfo.processInfo.environment["F33D3R_SAMPLE"] == "1"

    let sampleOffset = Int(ProcessInfo.processInfo.environment["F33D3R_SAMPLE_FROM"] ?? "") ?? 0

    enum DebugPage: String, Sendable {
        case home, explore, music, visions, notifications, wallet, profile, work, live
        case search, settings, followers, following, tag, editprofile
        /// `F33D3R_PAGE=workquotes F33D3R_WORK=<id>`: the works quoting one
        /// work, otherwise reachable only by tapping "View quotes".
        case workquotes
        case messages, conversation, stocks
    }

    /// `F33D3R_QUERY=…` fills the search field; `F33D3R_TAG=…` roots at a tag;
    /// `F33D3R_MEDIA_VIEWER=1` opens the first image of a `work` page.
    let debugQuery = ProcessInfo.processInfo.environment["F33D3R_QUERY"]
    let debugTag = ProcessInfo.processInfo.environment["F33D3R_TAG"]
    let debugMediaViewer = ProcessInfo.processInfo.environment["F33D3R_MEDIA_VIEWER"] == "1"

    let debugPage = ProcessInfo.processInfo.environment["F33D3R_PAGE"]
        .flatMap { DebugPage(rawValue: $0.lowercased()) }

    /// `F33D3R_PAGE=profile F33D3R_PROFILE=miiyazuko` roots at a profile;
    /// `F33D3R_PAGE=work F33D3R_WORK=<id>` at a work. Pushed screens cannot be
    /// reached by a launch argument otherwise.
    let debugProfile = ProcessInfo.processInfo.environment["F33D3R_PROFILE"]
    let debugWork = ProcessInfo.processInfo.environment["F33D3R_WORK"]

    /// `F33D3R_PAGE=stocks F33D3R_TICKER=AAPL` roots at one symbol's page. It
    /// is otherwise reachable only by tapping a `$TICKER` inside a work, which
    /// is the one thing a headless simulator cannot do — and it is the screen
    /// that says out loud whether this deployment's quotes provider answers.
    let debugTicker = ProcessInfo.processInfo.environment["F33D3R_TICKER"]

    /// `F33D3R_PAGE=settings F33D3R_SETTINGS_SHEET=twofactor|blocked|danger`
    /// opens one of Settings' subscreens on launch. They are all a tap deep
    /// from a list, which on a headless Simulator is a tap nobody can make —
    /// and a screen that ships having been compiled and never looked at is
    /// exactly how the layout bugs before this one got through.
    let debugSettingsSheet = ProcessInfo.processInfo.environment["F33D3R_SETTINGS_SHEET"]?.lowercased()

    /// `F33D3R_COMPOSE=1` opens the compose sheet over Home on launch.
    let debugCompose = ProcessInfo.processInfo.environment["F33D3R_COMPOSE"] == "1"

    /// What the compose sheet opens showing, for looking at a state a headless
    /// Simulator cannot type its way into: `thread` (three parts written),
    /// `long` (a draft past the long-post threshold), `suggest` (two
    /// paragraphs, offered as a thread), `scheduled` (a time set), `scheduler`
    /// (the date picker open), `emoji` (the picker open), `drafts` (a draft
    /// saved and the list open). Nothing is sent.
    let debugComposeScene = ProcessInfo.processInfo.environment["F33D3R_COMPOSE_SCENE"]

    /// `F33D3R_VISION=1` opens the first ring in the vision viewer on launch;
    /// `F33D3R_VISION_COMPOSE=1` opens the vision composer; `F33D3R_GO_LIVE=1`
    /// opens the go-live sheet; `F33D3R_BROADCAST=1` opens the reader's own
    /// room as broadcaster. Simulator-only ways into screens nobody can tap
    /// to.
    let debugVision = ProcessInfo.processInfo.environment["F33D3R_VISION"] == "1"
    let debugVisionCompose = ProcessInfo.processInfo.environment["F33D3R_VISION_COMPOSE"] == "1"
    let debugGoLive = ProcessInfo.processInfo.environment["F33D3R_GO_LIVE"] == "1"
    let debugBroadcast = ProcessInfo.processInfo.environment["F33D3R_BROADCAST"] == "1"

    let debugExploreLane = ProcessInfo.processInfo.environment["F33D3R_EXPLORE_LANE"]
        .flatMap { ExploreLane(rawValue: $0.lowercased()) }

    /// `F33D3R_SIGN_IN=handle:password`, for a run that has to reach a
    /// signed-in screen on a device nobody can type into.
    static var debugCredentials: (handle: String, password: String)? {
        guard let raw = ProcessInfo.processInfo.environment["F33D3R_SIGN_IN"] else { return nil }
        let parts = raw.split(separator: ":", maxSplits: 1, omittingEmptySubsequences: false)
        guard parts.count == 2, !parts[0].isEmpty, !parts[1].isEmpty else { return nil }
        return (String(parts[0]), String(parts[1]))
    }
    #endif

    /// What the user will see in their active-sessions list.
    private static var deviceName: String {
        #if canImport(UIKit)
        return UIDevice.current.name
        #else
        return "F33D3R for iOS"
        #endif
    }
}

#if canImport(UIKit)
import UIKit
#endif
