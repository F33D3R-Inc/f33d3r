import SwiftUI
import F33D3RKit

/// The account's settings.
///
/// Every switch here draws a value from `/me` and sends one event when it is
/// flipped; the screen re-reads `/me` after the server answers, so what it
/// shows is what the server holds. Density is the one exception — it is a
/// reading preference kept on the device, and the row says so.
struct SettingsView: View {
    @Environment(AppModel.self) private var model

    @State private var isChangingPassword = false
    @State private var isConfirmingSignOut = false
    @State private var isSettingUpTwoFactor = false
    @State private var isDeactivating = false
    @State private var isDeleting = false
    @State private var isShowingBlocked = false
    @State private var busy: Set<String> = []
    @State private var error: String?

    private var me: CurrentUser? { model.state.user }

    var body: some View {
        List {
            if let me {
                readingSection
                contentSection(me)
                privacySection(me)
                NotificationSettingsSection(error: $error)
                numbersSection
                securitySection(me)
                SessionsSection(busy: $busy, error: $error)
                verificationSection(me)
                accountSection(me)
                dangerSection(me)
                aboutSection(me)
            }
        }
        .listStyle(.insetGrouped)
        .scrollContentBackground(.hidden)
        .background(F33Color.bg)
        // No compose button here — a settings form is not somewhere you post from.
        .hidesComposeFAB()
        .navigationTitle("Settings")
        .navigationBarTitleDisplayMode(.inline)
        .tint(F33Color.accent)
        .sheet(isPresented: $isChangingPassword) {
            ChangePasswordSheet()
        }
        .sheet(isPresented: $isSettingUpTwoFactor) {
            TwoFactorSetupSheet()
        }
        .sheet(isPresented: $isDeactivating) {
            DeactivateAccountSheet()
        }
        .sheet(isPresented: $isDeleting) {
            if let me {
                DeleteAccountSheet(handle: me.user.handle)
            }
        }
        .confirmationDialog("Sign out of F33D3R on this device?", isPresented: $isConfirmingSignOut, titleVisibility: .visible) {
            Button("Sign out", role: .destructive) {
                Task { await model.signOut() }
            }
        }
        .alert("That didn't go through", isPresented: Binding(get: { error != nil }, set: { if !$0 { error = nil } })) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(error ?? "")
        }
        // Blocked & muted is Settings' own subscreen and nothing else pushes
        // it, so it is a destination here rather than a case in the app-wide
        // route enum — one more shared file two bots would both be editing.
        .navigationDestination(isPresented: $isShowingBlocked) {
            BlockedMutedView()
        }
        .task { await model.refreshMe() }
        #if DEBUG
        // A headless Simulator cannot tap a row, so a run can name the screen
        // it wants to look at.
        .task {
            switch model.debugSettingsSheet {
            case "twofactor": isSettingUpTwoFactor = true
            case "danger": isDeleting = true
            case "blocked": isShowingBlocked = true
            default: break
            }
        }
        #endif
    }

    // MARK: Sections

    private var readingSection: some View {
        Section {
            Picker("Appearance", selection: appearanceBinding) {
                ForEach(Appearance.allCases) { appearance in
                    Text(appearance.title).tag(appearance)
                }
            }
            .pickerStyle(.segmented)
        } header: {
            Text("Reading")
        } footer: {
            Text("Kept on this device, like the web's light and dark switch. Your accent theme is part of your profile — change it under Edit profile.")
        }
    }

    private var appearanceBinding: Binding<Appearance> {
        Binding(
            get: { model.preferences.appearance },
            set: { model.preferences.appearance = $0 }
        )
    }

    private func contentSection(_ me: CurrentUser) -> some View {
        Section {
            Picker("Content", selection: contentSettingBinding(me)) {
                Text("Safe mode").tag("safe_mode")
                Text("Default").tag("default")
                // The rule is the server's, and it is not `is_adult`: a minor
                // never qualifies, and above that it takes either a verified
                // age or a verified account. Offering the option to anyone
                // else is a picker that springs back with no explanation.
                if me.canEnableAdultContent {
                    Text("Adult enabled").tag("adult_enabled")
                }
            }
            .disabled(busy.contains(AccountSetting.contentSetting.rawValue))

            settingToggle("Show sensitive media", setting: .showSensitive, isOn: me.showSensitive)
            settingToggle("Celebrations", setting: .celebrations, isOn: me.celebrationsEnabled)
        } header: {
            Text("Content")
        } footer: {
            Text(me.canEnableAdultContent
                 ? "Safe mode hides anything marked sensitive. Celebrations are the confetti on a realm milestone."
                 : "Safe mode hides anything marked sensitive. Adult content needs a verified age or a verified account.")
        }
    }

    private func privacySection(_ me: CurrentUser) -> some View {
        Section {
            settingToggle("Private account", setting: .accountPrivate, isOn: me.user.isPrivate)

            Button {
                isShowingBlocked = true
            } label: {
                HStack {
                    Label("Blocked & muted", systemImage: "hand.raised")
                    Spacer(minLength: 0)
                    Image(systemName: "chevron.right")
                        .font(.system(size: 13, weight: .semibold))
                        .foregroundStyle(F33Color.ink5)
                }
            }
            .foregroundStyle(F33Color.ink)
        } header: {
            Text("Privacy")
        } footer: {
            Text("A private account's works are shown to followers only. Likes and saves are always yours alone.")
        }
    }

    /// A Number is a contact address, not a security setting and not a privacy
    /// switch, so it gets its own row rather than being buried under either.
    private var numbersSection: some View {
        Section {
            NavigationLink(value: Route.numbers) {
                Label("F33D3R Numbers", systemImage: "number")
            }
        } header: {
            Text("Reaching you")
        } footer: {
            Text("Twelve digits somebody can write to you at without knowing your handle. Mint as many as you like and retire any of them.")
        }
    }

    private func securitySection(_ me: CurrentUser) -> some View {
        Section("Security") {
            Button {
                isChangingPassword = true
            } label: {
                Label(me.hasPassword ? "Change password" : "Set a password", systemImage: "key.horizontal")
            }
            .foregroundStyle(F33Color.ink)

            Button {
                isSettingUpTwoFactor = true
            } label: {
                HStack {
                    Label("Two-factor sign-in", systemImage: "lock.shield")
                    Spacer(minLength: 0)
                    Text(me.twoFAEnabled ? "On" : "Off")
                        .foregroundStyle(F33Color.ink4)
                    Image(systemName: "chevron.right")
                        .font(.system(size: 13, weight: .semibold))
                        .foregroundStyle(F33Color.ink5)
                }
            }
            .foregroundStyle(F33Color.ink)
        }
    }

    /// What identity verification says, and nothing more.
    ///
    /// Read-only on purpose. Documents go through the web flow, and a row that
    /// opened Safari would land on a page with no session — the bug this
    /// screen used to have in the backup-codes gate. The state is worth showing
    /// here; the submission is not this screen's to run.
    @ViewBuilder
    private func verificationSection(_ me: CurrentUser) -> some View {
        if let status = me.kycStatus, !status.isEmpty {
            Section {
                LabeledContent {
                    Text(Self.kycLabel(status))
                        .foregroundStyle(status == "approved" ? F33Color.accent : F33Color.ink4)
                } label: {
                    Label("Identity verification", systemImage: "checkmark.seal")
                }

                if let submitted = me.kycSubmittedAt {
                    LabeledContent("Submitted", value: RelativeTime.label(for: submitted))
                }

                if me.payoutEnabled {
                    LabeledContent("Payouts", value: "Enabled")
                }
            } header: {
                Text("Verification")
            } footer: {
                Text("Verify on the web — f33d3r.com handles the documents. This screen only reports where it stands.")
            }
        }
    }

    private static func kycLabel(_ status: String) -> String {
        switch status {
        case "approved", "verified": return "Verified"
        case "pending", "submitted", "in_review": return "In review"
        case "rejected", "denied": return "Not accepted"
        case "none", "unstarted": return "Not started"
        default: return status.replacingOccurrences(of: "_", with: " ").capitalized
        }
    }

    private func accountSection(_ me: CurrentUser) -> some View {
        Section {
            AccountExportRow(error: $error)

            Button(role: .destructive) {
                isConfirmingSignOut = true
            } label: {
                Label("Sign out", systemImage: "rectangle.portrait.and.arrow.right")
            }
        } header: {
            Text("Account")
        } footer: {
            Text("Signed in as @\(me.user.handle). Signing out revokes this device's session on the server.")
        }
    }

    /// The two irreversible things, kept away from Sign out so a thumb aiming
    /// for one cannot land on the other.
    private func dangerSection(_ me: CurrentUser) -> some View {
        Section {
            Button(role: .destructive) {
                isDeactivating = true
            } label: {
                Label("Deactivate account", systemImage: "moon.zzz")
            }

            Button(role: .destructive) {
                isDeleting = true
            } label: {
                Label("Delete account", systemImage: "trash")
            }
        } header: {
            Text("Danger zone")
        } footer: {
            Text("Deactivating hides everything until you sign in again. Deleting is permanent, and @\(me.user.handle) does not come back to you.")
        }
    }

    private func aboutSection(_ me: CurrentUser) -> some View {
        Section("About") {
            LabeledContent("Version", value: Self.versionLabel)
            LabeledContent("Tier", value: me.tier.capitalized)
            LabeledContent("Realm", value: "\(me.user.realmName) · \(me.user.xp) XP")
            #if DEBUG
            LabeledContent("Server", value: model.client.baseURL.absoluteString)
            #endif
        }
    }

    // MARK: Bindings

    /// A toggle whose value is the server's. The knob moves with the finger,
    /// as any switch does; the position it settles in is the one `/me`
    /// reports after the write, and a refused write settles it back.
    private func settingToggle(_ title: String, setting: AccountSetting, isOn: Bool) -> some View {
        Toggle(title, isOn: Binding(
            get: { isOn },
            set: { value in
                Task { await apply(setting.rawValue) { try await model.setSetting(setting, on: value) } }
            }
        ))
        .disabled(busy.contains(setting.rawValue))
    }

    private func contentSettingBinding(_ me: CurrentUser) -> Binding<String> {
        Binding(
            get: { me.contentSetting },
            set: { value in
                Task { await apply(AccountSetting.contentSetting.rawValue) { try await model.setContentSetting(value) } }
            }
        )
    }


    private func apply(_ key: String, _ operation: @escaping () async throws -> Void) async {
        busy.insert(key)
        defer { busy.remove(key) }
        do {
            try await operation()
        } catch let error as APIError {
            self.error = error.userMessage
        } catch {
            self.error = error.localizedDescription
        }
    }

    private static var versionLabel: String {
        let info = Bundle.main.infoDictionary
        let version = info?["CFBundleShortVersionString"] as? String ?? "—"
        let build = info?["CFBundleVersion"] as? String ?? "—"
        return "\(version) (\(build))"
    }
}

/// Everything F33D3R holds about this account, as a file the reader can put
/// anywhere they like.
///
/// The bytes are fetched first and written to a temporary file, because
/// `ShareLink` hands over a URL and a share sheet that has to wait on a network
/// request is a share sheet that looks broken. Once the file exists the row
/// becomes the share button itself rather than a button that opens one — the
/// fetch happened, and there is nothing left to confirm.
private struct AccountExportRow: View {
    @Binding var error: String?

    @Environment(AppModel.self) private var model

    @State private var file: URL?
    @State private var isWorking = false

    var body: some View {
        Group {
            if let file {
                ShareLink(item: file) {
                    Label("Share your data", systemImage: "square.and.arrow.up")
                }
                .foregroundStyle(F33Color.ink)
            } else {
                Button {
                    Task { await prepare() }
                } label: {
                    HStack {
                        Label("Export your data", systemImage: "arrow.down.doc")
                        Spacer(minLength: 0)
                        if isWorking { ProgressView() }
                    }
                }
                .foregroundStyle(F33Color.ink)
                .disabled(isWorking)
            }
        }
    }

    private func prepare() async {
        isWorking = true
        defer { isWorking = false }
        do {
            let data = try await model.accountExport()
            let handle = model.state.user?.user.handle ?? "account"
            let url = FileManager.default.temporaryDirectory
                .appendingPathComponent("f33d3r-\(handle)-export.json")
            try data.write(to: url, options: .atomic)
            file = url
        } catch let error as APIError {
            self.error = error.isUnservedSurface
                ? "This F33D3R doesn't serve data exports yet."
                : error.userMessage
        } catch {
            self.error = error.localizedDescription
        }
    }
}

/// The devices signed in to this account, with the means to sign any of them
/// out. The current device is marked and cannot revoke itself from here —
/// that is Sign out.
private struct SessionsSection: View {
    @Binding var busy: Set<String>
    @Binding var error: String?

    @Environment(AppModel.self) private var model

    @State private var sessions: [SessionInfo]?
    @State private var isConfirmingRevokeOthers = false

    var body: some View {
        Section {
            if let sessions {
                ForEach(sessions) { session in
                    row(session)
                        .swipeActions(edge: .trailing, allowsFullSwipe: false) {
                            if !session.isCurrent {
                                Button(role: .destructive) {
                                    Task { await revoke(session) }
                                } label: {
                                    Label("Sign out", systemImage: "xmark.circle")
                                }
                            }
                        }
                }
                if sessions.count > 1 {
                    Button(role: .destructive) {
                        isConfirmingRevokeOthers = true
                    } label: {
                        Label("Sign out other devices", systemImage: "iphone.slash")
                    }
                    .disabled(busy.contains("sessions"))
                }
            } else {
                HStack {
                    ProgressView()
                    Text("Loading devices…")
                        .foregroundStyle(F33Color.ink4)
                }
            }
        } header: {
            Text("Devices")
        } footer: {
            Text("Swipe a device to sign it out.")
        }
        .task { await load() }
        .confirmationDialog("Sign out every other device?", isPresented: $isConfirmingRevokeOthers, titleVisibility: .visible) {
            Button("Sign out others", role: .destructive) {
                Task { await revokeOthers() }
            }
        }
    }

    private func row(_ session: SessionInfo) -> some View {
        HStack(spacing: F33Spacing.md) {
            Image(systemName: session.deviceName.localizedCaseInsensitiveContains("ipad") ? "ipad" : "iphone")
                .font(.system(size: 20))
                .foregroundStyle(session.isCurrent ? F33Color.accent : F33Color.ink3)
                .frame(width: 28)
            VStack(alignment: .leading, spacing: 2) {
                HStack(spacing: 6) {
                    Text(session.deviceName)
                        .font(.system(size: 15, weight: .medium))
                        .foregroundStyle(F33Color.ink)
                    if session.isCurrent {
                        Text("This device")
                            .font(.system(size: 11, weight: .semibold))
                            .foregroundStyle(F33Color.accent)
                            .padding(.horizontal, 6)
                            .padding(.vertical, 2)
                            .background(F33Color.accentSoft, in: Capsule())
                    }
                }
                Text("Last seen \(RelativeTime.label(for: session.lastSeenAt))" + (session.ipAddress.map { " · \($0)" } ?? ""))
                    .font(.system(size: 12))
                    .foregroundStyle(F33Color.ink4)
            }
            Spacer(minLength: 0)
            if busy.contains(session.id) {
                ProgressView()
            }
        }
    }

    private func load() async {
        do {
            sessions = try await model.activeSessions()
        } catch let error as APIError {
            self.error = error.userMessage
            sessions = []
        } catch {
            self.error = error.localizedDescription
            sessions = []
        }
    }

    private func revoke(_ session: SessionInfo) async {
        busy.insert(session.id)
        defer { busy.remove(session.id) }
        do {
            try await model.revokeSession(id: session.id)
            await load()
        } catch let error as APIError {
            self.error = error.userMessage
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func revokeOthers() async {
        busy.insert("sessions")
        defer { busy.remove("sessions") }
        do {
            try await model.revokeOtherSessions()
            await load()
        } catch let error as APIError {
            self.error = error.userMessage
        } catch {
            self.error = error.localizedDescription
        }
    }
}

/// Changing the password: the current one, the new one twice.
///
/// An account created through a channel that set no password has none to prove,
/// and `/me` says so. Asking for a current password there is a field nobody can
/// fill and a form nobody can submit, so the field is not drawn at all.
struct ChangePasswordSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    @State private var current = ""
    @State private var new = ""
    @State private var confirm = ""
    @State private var isSaving = false
    @State private var error: String?
    @State private var didSave = false

    private var hasPassword: Bool { model.state.user?.hasPassword ?? true }

    private var canSave: Bool {
        (!hasPassword || !current.isEmpty) && new.count >= 8 && new == confirm && !isSaving
    }

    var body: some View {
        VStack(alignment: .leading, spacing: F33Spacing.lg) {
            Capsule()
                .fill(F33Color.ink5)
                .frame(width: 36, height: 5)
                .frame(maxWidth: .infinity)
                .padding(.top, F33Spacing.sm)

            Text(didSave ? "Password changed" : (hasPassword ? "Change password" : "Set a password"))
                .font(.title3.weight(.semibold))
                .foregroundStyle(F33Color.ink)

            if didSave {
                Text("Other devices stay signed in until you sign them out from Settings.")
                    .font(.callout)
                    .foregroundStyle(F33Color.ink3)
                    .fixedSize(horizontal: false, vertical: true)
            } else {
                if hasPassword {
                    SecureField("Current password", text: $current)
                        .textContentType(.password)
                        .f33Field()
                }
                SecureField("New password (8 or more characters)", text: $new)
                    .textContentType(.newPassword)
                    .f33Field()
                SecureField("New password again", text: $confirm)
                    .textContentType(.newPassword)
                    .f33Field()

                if !confirm.isEmpty, new != confirm {
                    Text("The two new passwords don't match.")
                        .font(.footnote)
                        .foregroundStyle(F33Color.warn)
                }
            }

            if let error {
                Text(error)
                    .font(.footnote)
                    .foregroundStyle(F33Color.danger)
                    .fixedSize(horizontal: false, vertical: true)
            }

            Spacer(minLength: 0)

            Button {
                if didSave { dismiss() } else { Task { await save() } }
            } label: {
                if isSaving {
                    ProgressView().tint(F33Color.accentInk)
                } else {
                    Text(didSave ? "Done" : (hasPassword ? "Change password" : "Set password"))
                }
            }
            .buttonStyle(F33PrimaryButtonStyle())
            .disabled(!canSave && !didSave)
            .padding(.bottom, F33Spacing.lg)
        }
        .padding(.horizontal, F33Spacing.xl)
        .frame(maxWidth: .infinity, alignment: .leading)
        .f33GlassSheet()
        .presentationDetents([.height(440)])
        .presentationDragIndicator(.hidden)
        .animation(F33Motion.easeOut, value: didSave)
    }

    private func save() async {
        isSaving = true
        error = nil
        defer { isSaving = false }
        do {
            try await model.changePassword(current: current, new: new)
            didSave = true
        } catch let error as APIError {
            self.error = error.userMessage
        } catch {
            self.error = error.localizedDescription
        }
    }
}
