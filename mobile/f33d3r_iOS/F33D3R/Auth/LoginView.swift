import SwiftUI
import F33D3RKit

/// Sign-in. Handle and password only — F33D3R signup takes no email or phone.
///
/// An account with two-factor on needs a third thing, and the screen learns
/// that from the refusal rather than up front: the first attempt goes without
/// a code, the server answers `two_fa_required`, and the code field appears.
/// Asking every account for a code before knowing whether it has one would
/// make every sign-in look like it does.
struct LoginView: View {
    @Environment(AppModel.self) private var model

    @State private var handle = ""
    @State private var password = ""
    @State private var displayName = ""
    @State private var code = ""
    /// Set once the server has asked for the second factor for this handle.
    @State private var needsCode = false
    @State private var isCreatingAccount = false
    @State private var errorMessage: String?
    @State private var isSubmitting = false
    @FocusState private var focused: Field?

    private enum Field { case handle, displayName, password, code }

    /// Matches the server's handle rule, so an obviously invalid handle is caught
    /// before it costs a round trip and a rate-limit slot.
    private var canSubmit: Bool {
        !isSubmitting
            && !password.isEmpty
            && (!isCreatingAccount || password.count >= 8)
            && (!needsCode || code.count == 6)
            && handle.count <= 30
            && !handle.isEmpty
            && handle.allSatisfy { $0.isLetter || $0.isNumber || $0 == "_" || $0 == "-" }
    }

    var body: some View {
        ZStack {
            F33Color.bg.ignoresSafeArea()
            brandWash.ignoresSafeArea()

            ScrollView {
                VStack(spacing: F33Spacing.lg) {
                    Spacer(minLength: F33Spacing.xxl)

                    VStack(spacing: F33Spacing.md) {
                        BrandMark(size: 72)
                        BrandWordmark(markSize: 0, textSize: 28)
                    }
                    .padding(.bottom, F33Spacing.sm)

                    signInPanel
                }
                .frame(maxWidth: F33Layout.feedMaxWidth)
                .padding(.horizontal, F33Spacing.xl)
            }
            .scrollDismissesKeyboard(.interactively)
        }
        .animation(F33Motion.easeOut, value: errorMessage)
        .onAppear { focused = .handle }
    }

    /// The form on a single glass panel.
    ///
    /// This is the one screen in the app with nothing behind it to blur, which is
    /// normally the argument against glass. It earns it here because the wash
    /// below gives the panel something to refract, and because sign-in is the
    /// first thing anybody sees — the surface that says what kind of product
    /// this is before a single work has loaded.
    private var signInPanel: some View {
        VStack(spacing: F33Spacing.lg) {
            TextField("Handle", text: $handle)
                .textContentType(.username)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
                .submitLabel(.next)
                .focused($focused, equals: .handle)
                .onSubmit { focused = isCreatingAccount ? .displayName : .password }
                .f33Field()

            if isCreatingAccount {
                TextField("Display name", text: $displayName)
                    .textContentType(.name)
                    .submitLabel(.next)
                    .focused($focused, equals: .displayName)
                    .onSubmit { focused = .password }
                    .f33Field()
                    .transition(.opacity.combined(with: .move(edge: .top)))
            }

            SecureField(isCreatingAccount ? "Password (8+ characters)" : "Password", text: $password)
                .textContentType(isCreatingAccount ? .newPassword : .password)
                .submitLabel(needsCode ? .next : .go)
                .focused($focused, equals: .password)
                .onSubmit {
                    if needsCode { focused = .code } else if canSubmit { submit() }
                }
                .f33Field()

            if needsCode {
                VStack(alignment: .leading, spacing: F33Spacing.xs) {
                    Text("Enter the six digits from your authenticator app.")
                        .font(.footnote)
                        .foregroundStyle(F33Color.ink3)
                        .frame(maxWidth: .infinity, alignment: .leading)

                    TextField("000000", text: $code)
                        .keyboardType(.numberPad)
                        .textContentType(.oneTimeCode)
                        .font(.system(size: 20, weight: .semibold, design: .monospaced))
                        .multilineTextAlignment(.center)
                        .submitLabel(.go)
                        .focused($focused, equals: .code)
                        .onChange(of: code) { _, new in
                            code = String(new.filter(\.isNumber).prefix(6))
                        }
                        .f33Field()
                        .accessibilityLabel("Six-digit code")
                }
                .transition(.opacity.combined(with: .move(edge: .top)))
            }

            if let errorMessage {
                Text(errorMessage)
                    .font(.footnote)
                    .foregroundStyle(F33Color.danger)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .transition(.opacity)
            }

            Button {
                submit()
            } label: {
                if isSubmitting {
                    ProgressView().tint(F33Color.accentInk)
                } else {
                    Text(isCreatingAccount ? "Create account" : "Sign in")
                }
            }
            .buttonStyle(F33PrimaryButtonStyle())
            .disabled(!canSubmit)
            .padding(.top, F33Spacing.xs)

            // No email, no phone: a handle and a password make an account.
            Button {
                withAnimation(F33Motion.easeOut) {
                    isCreatingAccount.toggle()
                    errorMessage = nil
                    // A code field belongs to one account's sign-in attempt.
                    // Switching to signup, or back, starts that over.
                    needsCode = false
                    code = ""
                }
            } label: {
                Text(isCreatingAccount ? "Have an account? Sign in" : "New here? Create an account")
                    .font(.footnote.weight(.medium))
                    .foregroundStyle(F33Color.ink3)
                    .frame(maxWidth: .infinity)
                    .frame(minHeight: F33Layout.minTouchTarget)
            }
            .buttonStyle(.plain)
        }
        .padding(F33Spacing.lg)
        .f33Glass(in: RoundedRectangle(cornerRadius: F33Radius.xl), elevation: .floating)
    }

    /// Two soft pools of colour behind the panel — the product accent above, the
    /// mark's own gold below.
    ///
    /// Drawn rather than left flat because glass over a single uniform colour is
    /// a rectangle of that colour: the treatment only reads when there is
    /// something underneath varying for it to bend. Pooled into the far corners
    /// rather than spread across the screen: a wash that reaches the middle
    /// stops being a ground for the panel and becomes a coloured background with
    /// a panel on it. Hidden from VoiceOver, which has nothing to say about a
    /// gradient.
    private var brandWash: some View {
        ZStack {
            RadialGradient(
                colors: [F33Color.accent.opacity(0.22), .clear],
                center: UnitPoint(x: 0.04, y: 0.02),
                startRadius: 0,
                endRadius: 330
            )
            RadialGradient(
                colors: [F33Color.brandGold.opacity(0.16), .clear],
                center: UnitPoint(x: 0.98, y: 0.95),
                startRadius: 0,
                endRadius: 330
            )
        }
        .accessibilityHidden(true)
    }

    private func submit() {
        guard canSubmit else { return }
        isSubmitting = true
        errorMessage = nil
        focused = nil

        Task {
            defer { isSubmitting = false }
            do {
                // The server lowercases handles; do it here too so the field is
                // forgiving about capitalisation.
                let normalised = handle.trimmingCharacters(in: .whitespaces).lowercased()
                if isCreatingAccount {
                    try await model.signUp(
                        handle: normalised,
                        password: password,
                        displayName: displayName.trimmingCharacters(in: .whitespaces)
                    )
                } else {
                    try await model.signIn(
                        handle: normalised,
                        password: password,
                        code: needsCode ? code : nil
                    )
                }
                password = ""
                code = ""
            } catch let error as APIError {
                switch error.twoFactor {
                case .required:
                    // The password was right; this account has a second
                    // factor. Open the field and keep the password, so the
                    // reader types the code and nothing else.
                    withAnimation(F33Motion.easeOut) { needsCode = true }
                    errorMessage = nil
                    focused = .code
                case .invalid:
                    code = ""
                    errorMessage = error.userMessage
                    focused = .code
                case nil:
                    errorMessage = error.userMessage
                }
            } catch {
                errorMessage = "Something went wrong. Try again."
            }
        }
    }
}
