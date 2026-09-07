import SwiftUI
import PhotosUI
import F33D3RKit

/// Editing the reader's own profile.
///
/// A new avatar or header is uploaded first, so the profile event carries a
/// URL the media origin already serves. Save sends only what changed, then
/// re-reads `/me` — the screen that opened this one draws the server's row,
/// not the form's.
struct EditProfileView: View {
    let user: User
    /// Called after the server has confirmed the change.
    var onSaved: () async -> Void

    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    @State private var displayName: String
    @State private var bio: String
    @State private var pronouns: String
    @State private var location: String
    @State private var website: String
    @State private var accent: Color
    @State private var theme: F33Theme

    @State private var avatarPick: PhotosPickerItem?
    @State private var headerPick: PhotosPickerItem?
    @State private var avatarData: Data?
    @State private var headerData: Data?

    // Everything below comes from `/me` rather than from the `User` this view
    // was handed: the public profile DTO does not carry an account's own
    // settings, and it should not — these are the reader's business and
    // nobody else's. They are hydrated once, when the screen appears.
    @State private var isAdultCreator = false
    @State private var birthdayMd: BirthdayVisibility = .everyone
    @State private var birthdayYear: BirthdayVisibility = .everyone
    @State private var countryCode = ""
    @State private var social: [String: String] = [:]
    @State private var tips: [String: String] = [:]
    @State private var original: CurrentUser?

    @State private var isSaving = false
    @State private var error: String?

    init(user: User, onSaved: @escaping () async -> Void) {
        self.user = user
        self.onSaved = onSaved
        _displayName = State(initialValue: user.displayName)
        _bio = State(initialValue: user.bio ?? "")
        _pronouns = State(initialValue: user.pronouns ?? "")
        _location = State(initialValue: user.location ?? "")
        _website = State(initialValue: user.website ?? "")
        _accent = State(initialValue: Color(hexString: user.accentHex) ?? F33Color.accent)
        _theme = State(initialValue: F33Theme.named(user.themeID))
    }

    private var accentHex: String {
        accent.hexString ?? (user.accentHex ?? "")
    }

    private var hasChanges: Bool {
        avatarData != nil || headerData != nil || theme.id != F33Theme.named(user.themeID).id
            || accountChanges != nil
            || !ProfileUpdate.diff(
                from: user, displayName: displayName, bio: bio, pronouns: pronouns,
                location: location, website: website, accentHex: accentHex, avatarURL: nil, headerURL: nil
            ).isEmpty
    }

    /// The `/me`-sourced half of the form, as a partial update — nil when the
    /// reader has touched none of it. Both link maps go whole or not at all:
    /// the server replaces the map it is sent, so posting one changed row
    /// would clear every row the editor did not touch.
    private var accountChanges: ProfileUpdate? {
        guard let original else { return nil }
        var update = ProfileUpdate()
        if isAdultCreator != original.isAdultCreator { update.isAdultCreator = isAdultCreator }
        if birthdayMd.rawValue != original.birthdayMdVisibility {
            update.birthdayMdVisibility = birthdayMd.rawValue
        }
        if birthdayYear.rawValue != original.birthdayYearVisibility {
            update.birthdayYearVisibility = birthdayYear.rawValue
        }
        if countryCode != (original.countryCode ?? "") {
            update.countryCode = countryCode.uppercased()
        }
        if Self.trimmed(social) != Self.trimmed(original.socialLinks) {
            update.socialLinks = Self.trimmed(social)
        }
        if Self.trimmed(tips) != Self.trimmed(original.externalTipLinks) {
            update.externalTipLinks = Self.trimmed(tips)
        }
        return update.isEmpty ? nil : update
    }

    /// Empty is absent. A field cleared out and a field never filled in are the
    /// same thing to the server, and comparing them as different is a Save
    /// button that stays lit over no change at all.
    private static func trimmed(_ links: [String: String]) -> [String: String] {
        links.compactMapValues { value in
            let clean = value.trimmingCharacters(in: .whitespacesAndNewlines)
            return clean.isEmpty ? nil : clean
        }
    }

    private var canSave: Bool {
        hasChanges && !isSaving && !displayName.trimmingCharacters(in: .whitespaces).isEmpty
    }

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: F33Spacing.lg) {
                    images

                    field("Name", text: $displayName, limit: 50)
                    bioField
                    field("Pronouns", text: $pronouns, limit: 30)
                    locationRow
                    field("Website", text: $website, limit: 200, keyboard: .URL)

                    adultToggle
                    birthdayVisibility
                    linksSection(
                        "Social links",
                        note: nil,
                        rows: ProfileLinks.social.map { ($0.key, $0.label, "", $0.placeholder) },
                        values: $social
                    )
                    linksSection(
                        "Payment links",
                        note: "Shown in the tip dialog when somebody sends you a tip. Never on your public profile.",
                        rows: ProfileLinks.payment.map { ($0.key, $0.label, $0.prefix, $0.placeholder) },
                        values: $tips
                    )

                    themeRow
                    accentRow

                    if let error {
                        Text(error)
                            .font(.footnote)
                            .foregroundStyle(F33Color.danger)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
                .padding(.horizontal, F33Spacing.xl)
                .padding(.bottom, F33Spacing.xxl)
            }
            .background(F33Color.bg)
            .scrollDismissesKeyboard(.interactively)
            .navigationTitle("Edit profile")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                        .disabled(isSaving)
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button {
                        Task { await save() }
                    } label: {
                        if isSaving {
                            ProgressView()
                        } else {
                            Text("Save").fontWeight(.semibold)
                        }
                    }
                    .disabled(!canSave)
                }
            }
        }
        .tint(F33Color.accent)
        .interactiveDismissDisabled(isSaving)
        .onChange(of: avatarPick) { _, item in Task { avatarData = await load(item) } }
        .onChange(of: headerPick) { _, item in Task { headerData = await load(item) } }
        .task { hydrate() }
    }

    /// Fills the `/me`-sourced fields once. Guarded on `original` so a
    /// re-entrant `.task` — a scene coming back to the foreground — cannot
    /// throw away what the reader has typed.
    private func hydrate() {
        guard original == nil, let me = model.state.user else { return }
        isAdultCreator = me.isAdultCreator
        birthdayMd = BirthdayVisibility.named(me.birthdayMdVisibility)
        birthdayYear = BirthdayVisibility.named(me.birthdayYearVisibility)
        countryCode = me.countryCode ?? ""
        social = me.socialLinks
        tips = me.externalTipLinks
        original = me
    }

    // MARK: Images

    private var images: some View {
        ZStack(alignment: .bottomLeading) {
            PhotosPicker(selection: $headerPick, matching: .images) {
                Group {
                    if let headerData, let image = UIImage(data: headerData) {
                        Image(uiImage: image).resizable().aspectRatio(contentMode: .fill)
                    } else {
                        RemoteImage(path: user.headerURL, seed: user.handle)
                    }
                }
                .frame(height: 120)
                .frame(maxWidth: .infinity)
                .clipShape(RoundedRectangle(cornerRadius: F33Radius.md))
                .overlay(alignment: .topTrailing) {
                    cameraBadge.padding(F33Spacing.sm)
                }
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Change header image")

            PhotosPicker(selection: $avatarPick, matching: .images) {
                Group {
                    if let avatarData, let image = UIImage(data: avatarData) {
                        Image(uiImage: image).resizable().aspectRatio(contentMode: .fill)
                            .frame(width: 76, height: 76)
                            .clipShape(Circle())
                    } else {
                        F33Avatar(user: user, size: 76)
                    }
                }
                .overlay(Circle().strokeBorder(F33Color.bg, lineWidth: 4))
                .overlay(alignment: .bottomTrailing) { cameraBadge }
            }
            .buttonStyle(.plain)
            .offset(x: F33Spacing.lg, y: 28)
            .accessibilityLabel("Change avatar")
        }
        .padding(.bottom, 28)
        .padding(.top, F33Spacing.sm)
    }

    private var cameraBadge: some View {
        Image(systemName: "camera.fill")
            .font(.system(size: 11, weight: .semibold))
            .foregroundStyle(.white)
            .frame(width: 26, height: 26)
            .background(Color.black.opacity(0.55), in: Circle())
    }

    // MARK: Fields

    private func field(_ title: String, text: Binding<String>, limit: Int, keyboard: UIKeyboardType = .default) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Text(title)
                    .font(.system(size: 13, weight: .semibold))
                    .foregroundStyle(F33Color.ink3)
                Spacer()
                Text("\(text.wrappedValue.count)/\(limit)")
                    .font(.system(size: 11).monospacedDigit())
                    .foregroundStyle(text.wrappedValue.count > limit ? F33Color.danger : F33Color.ink5)
            }
            TextField(title, text: text)
                .keyboardType(keyboard)
                .textInputAutocapitalization(keyboard == .URL ? .never : .words)
                .autocorrectionDisabled(keyboard == .URL)
                .f33Field()
        }
    }

    /// The place and the country code that goes with it. Two fields on one
    /// line because they are one answer: the web's form fills the code in from
    /// the place picker, and here it is typed, so it sits where it belongs
    /// rather than three sections away under a heading of its own.
    private var locationRow: some View {
        HStack(alignment: .bottom, spacing: F33Spacing.md) {
            field("Location", text: $location, limit: 60)
            VStack(alignment: .leading, spacing: 6) {
                Text("Country")
                    .font(.system(size: 13, weight: .semibold))
                    .foregroundStyle(F33Color.ink3)
                TextField("US", text: $countryCode)
                    .textInputAutocapitalization(.characters)
                    .autocorrectionDisabled()
                    .onChange(of: countryCode) { _, new in
                        countryCode = String(new.uppercased().filter(\.isLetter).prefix(2))
                    }
                    .f33Field()
                    .frame(width: 78)
                    .accessibilityLabel("Two-letter country code")
            }
        }
    }

    /// Marks the whole profile adult. The consequence is spelled out because
    /// it applies to everything already posted, not only to what comes next.
    private var adultToggle: some View {
        Toggle(isOn: $isAdultCreator) {
            VStack(alignment: .leading, spacing: 2) {
                Text("Adult Content Profile")
                    .font(.system(size: 15, weight: .medium))
                    .foregroundStyle(F33Color.ink)
                Text("Every work you post needs age verification to see.")
                    .font(.system(size: 12))
                    .foregroundStyle(F33Color.ink4)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .frame(minHeight: F33Layout.minTouchTarget)
    }

    /// The birth date itself is PIAL's and cannot be changed here; who sees it
    /// can be, in the two halves the server keeps separately.
    private var birthdayVisibility: some View {
        VStack(alignment: .leading, spacing: F33Spacing.sm) {
            VStack(alignment: .leading, spacing: 2) {
                Text("Who sees your birthday")
                    .font(.system(size: 13, weight: .semibold))
                    .foregroundStyle(F33Color.ink3)
                Text("Set at account creation — contact support to change the date itself.")
                    .font(.system(size: 12))
                    .foregroundStyle(F33Color.ink4)
                    .fixedSize(horizontal: false, vertical: true)
            }

            Picker("Month and day", selection: $birthdayMd) {
                ForEach(BirthdayVisibility.allCases) { option in
                    Text(option.title).tag(option)
                }
            }
            .frame(minHeight: F33Layout.minTouchTarget)

            Picker("Year", selection: $birthdayYear) {
                ForEach(BirthdayVisibility.allCases) { option in
                    Text(option.title).tag(option)
                }
            }
            .frame(minHeight: F33Layout.minTouchTarget)
        }
    }

    /// One block of link fields. Both blocks are the same shape, so they are
    /// the same function: the rows and their prefixes come from the server's
    /// own list in ``ProfileLinks``, and nothing about a platform is written
    /// out twice.
    private func linksSection(
        _ title: String,
        note: String?,
        rows: [(key: String, label: String, prefix: String, placeholder: String)],
        values: Binding<[String: String]>
    ) -> some View {
        VStack(alignment: .leading, spacing: F33Spacing.sm) {
            VStack(alignment: .leading, spacing: 2) {
                Text(title)
                    .font(.system(size: 13, weight: .semibold))
                    .foregroundStyle(F33Color.ink3)
                if let note {
                    Text(note)
                        .font(.system(size: 12))
                        .foregroundStyle(F33Color.ink4)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }

            ForEach(rows, id: \.key) { row in
                HStack(spacing: F33Spacing.sm) {
                    Text(row.label)
                        .font(.system(size: 13))
                        .foregroundStyle(F33Color.ink3)
                        .frame(width: 116, alignment: .leading)

                    HStack(spacing: 2) {
                        if !row.prefix.isEmpty {
                            Text(row.prefix)
                                .font(.system(size: 13))
                                .foregroundStyle(F33Color.ink5)
                        }
                        TextField(row.placeholder, text: Binding(
                            get: { values.wrappedValue[row.key] ?? "" },
                            set: { values.wrappedValue[row.key] = $0 }
                        ))
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .keyboardType(.URL)
                    }
                    .f33Field()
                }
                .frame(minHeight: F33Layout.minTouchTarget)
                .accessibilityElement(children: .combine)
                .accessibilityLabel(row.label)
            }
        }
    }

    private var bioField: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Text("Bio")
                    .font(.system(size: 13, weight: .semibold))
                    .foregroundStyle(F33Color.ink3)
                Spacer()
                Text("\(bio.count)/280")
                    .font(.system(size: 11).monospacedDigit())
                    .foregroundStyle(bio.count > 280 ? F33Color.danger : F33Color.ink5)
            }
            TextField("Say something about yourself", text: $bio, axis: .vertical)
                .lineLimit(3...6)
                .padding(.horizontal, F33Spacing.lg)
                .padding(.vertical, F33Spacing.md)
                .background(F33Color.bgElevated, in: RoundedRectangle(cornerRadius: F33Radius.md))
                .overlay(RoundedRectangle(cornerRadius: F33Radius.md).strokeBorder(F33Color.hairlineStrong, lineWidth: 1))
                .foregroundStyle(F33Color.ink)
        }
    }

    /// The six accent themes the web offers, as swatches. The one chosen
    /// colours the whole app once the server has it.
    private var themeRow: some View {
        VStack(alignment: .leading, spacing: F33Spacing.sm) {
            VStack(alignment: .leading, spacing: 2) {
                Text("Theme")
                    .font(.system(size: 13, weight: .semibold))
                    .foregroundStyle(F33Color.ink3)
                Text("The accent F33D3R uses for you, here and on the web.")
                    .font(.system(size: 12))
                    .foregroundStyle(F33Color.ink4)
            }
            HStack(spacing: F33Spacing.md) {
                ForEach(F33Theme.all) { option in
                    let isOn = option.id == theme.id
                    Button {
                        theme = option
                    } label: {
                        Circle()
                            .fill(option.swatch)
                            .frame(width: 34, height: 34)
                            .overlay {
                                if isOn {
                                    Image(systemName: "checkmark")
                                        .font(.system(size: 14, weight: .bold))
                                        .foregroundStyle(.white)
                                }
                            }
                            .overlay(Circle().strokeBorder(isOn ? F33Color.ink : .clear, lineWidth: 2).padding(-4))
                            .frame(minWidth: F33Layout.minTouchTarget, minHeight: F33Layout.minTouchTarget)
                            .contentShape(Circle())
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel(option.name)
                    .accessibilityAddTraits(isOn ? [.isSelected, .isButton] : .isButton)
                }
            }
        }
    }

    private var accentRow: some View {
        HStack {
            VStack(alignment: .leading, spacing: 2) {
                Text("Accent")
                    .font(.system(size: 13, weight: .semibold))
                    .foregroundStyle(F33Color.ink3)
                Text("Colours your profile on the web and in the app.")
                    .font(.system(size: 12))
                    .foregroundStyle(F33Color.ink4)
            }
            Spacer()
            ColorPicker("Accent colour", selection: $accent, supportsOpacity: false)
                .labelsHidden()
        }
        .frame(minHeight: F33Layout.minTouchTarget)
    }

    // MARK: Work

    private func load(_ item: PhotosPickerItem?) async -> Data? {
        guard let item else { return nil }
        return try? await item.loadTransferable(type: Data.self)
    }

    private func save() async {
        guard canSave else { return }
        isSaving = true
        error = nil
        defer { isSaving = false }
        do {
            var avatarURL: String?
            var headerURL: String?
            if let avatarData {
                let kind = ImageSniff.type(of: avatarData)
                avatarURL = try await model.uploadImage(avatarData, filename: "avatar.\(kind.ext)", mimeType: kind.mime)
            }
            if let headerData {
                let kind = ImageSniff.type(of: headerData)
                headerURL = try await model.uploadImage(headerData, filename: "header.\(kind.ext)", mimeType: kind.mime)
            }
            var update = ProfileUpdate.diff(
                from: user,
                displayName: displayName.trimmingCharacters(in: .whitespaces),
                bio: bio, pronouns: pronouns, location: location, website: website,
                accentHex: accentHex, avatarURL: avatarURL, headerURL: headerURL
            )
            if theme.id != F33Theme.named(user.themeID).id { update.themeID = theme.id }
            if let account = accountChanges {
                update.isAdultCreator = account.isAdultCreator
                update.birthdayMdVisibility = account.birthdayMdVisibility
                update.birthdayYearVisibility = account.birthdayYearVisibility
                update.countryCode = account.countryCode
                update.socialLinks = account.socialLinks
                update.externalTipLinks = account.externalTipLinks
            }
            try await model.updateProfile(update)
            await onSaved()
            dismiss()
        } catch let error as APIError {
            self.error = error.userMessage
        } catch {
            self.error = error.localizedDescription
        }
    }
}

extension Color {
    /// `#RRGGBB` → colour. Nil for anything else.
    init?(hexString: String?) {
        guard let hexString, hexString.count == 7, hexString.hasPrefix("#"),
              let value = UInt32(hexString.dropFirst(), radix: 16) else { return nil }
        self.init(
            red: Double((value >> 16) & 0xFF) / 255,
            green: Double((value >> 8) & 0xFF) / 255,
            blue: Double(value & 0xFF) / 255
        )
    }

    /// Colour → `#RRGGBB`, in sRGB. Nil when the colour has no RGB form.
    var hexString: String? {
        guard let components = UIColor(self).cgColor.converted(to: CGColorSpace(name: CGColorSpace.sRGB)!, intent: .defaultIntent, options: nil)?.components,
              components.count >= 3 else { return nil }
        let r = Int((components[0] * 255).rounded())
        let g = Int((components[1] * 255).rounded())
        let b = Int((components[2] * 255).rounded())
        return String(format: "#%02X%02X%02X", r, g, b)
    }
}
