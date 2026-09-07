import SwiftUI
import CoreImage
import CoreImage.CIFilterBuiltins
import F33D3RKit

/// Turning two-factor sign-in on, and off again.
///
/// The QR code is drawn here from the `otpauth://` string rather than fetched
/// as an image. A server-rendered QR would be a second copy of the secret
/// crossing the wire and a second thing that can fail to load, and there is
/// nothing in the string a phone cannot draw itself.
///
/// Nothing is stored. The secret lives in this view for as long as the sheet is
/// open; enabling proves the authenticator got it, and after that the server
/// holds it and the device holds nothing.
struct TwoFactorSetupSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    @State private var setup: TwoFactorSetup?
    @State private var code = ""
    @State private var isLoading = true
    @State private var isSubmitting = false
    @State private var error: String?
    @State private var showingBackupCodes = false
    @FocusState private var codeFocused: Bool

    private var isOn: Bool { model.state.user?.twoFAEnabled ?? false }

    private var canSubmit: Bool {
        code.count == 6 && !isSubmitting && (isOn || setup != nil)
    }

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: F33Spacing.lg) {
                    if isOn {
                        enabledBody
                    } else {
                        enrolmentBody
                    }

                    if let error {
                        Text(error)
                            .font(.footnote)
                            .foregroundStyle(F33Color.danger)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
                .padding(.horizontal, F33Spacing.xl)
                .padding(.vertical, F33Spacing.lg)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            .background(F33Color.bg)
            .scrollDismissesKeyboard(.interactively)
            .navigationTitle("Two-factor sign-in")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Done") { dismiss() }
                        .disabled(isSubmitting)
                }
            }
        }
        .tint(F33Color.accent)
        .sheet(isPresented: $showingBackupCodes) {
            BackupCodesSheet()
        }
        .task { await load() }
    }

    // MARK: Off — enrolment

    @ViewBuilder
    private var enrolmentBody: some View {
        Text("Scan this with an authenticator app, then type the six digits it shows.")
            .font(.callout)
            .foregroundStyle(F33Color.ink3)
            .fixedSize(horizontal: false, vertical: true)

        if isLoading {
            HStack(spacing: F33Spacing.md) {
                ProgressView()
                Text("Asking the server for a secret…")
                    .font(.callout)
                    .foregroundStyle(F33Color.ink4)
            }
            .frame(maxWidth: .infinity, minHeight: 200)
        } else if let setup {
            qr(setup.uri)
            secretRow(setup)
            codeField
            submitButton("Turn on") { try await model.twoFactorEnable(code: code) }
        } else {
            EmptyStateView(
                icon: "lock.shield",
                title: "Two-factor isn't available here",
                message: "This F33D3R doesn't offer authenticator sign-in yet.",
                actionTitle: "Try again",
                action: { Task { await load() } }
            )
        }
    }

    private func qr(_ uri: String) -> some View {
        Group {
            if let image = Self.qrImage(uri) {
                Image(uiImage: image)
                    .interpolation(.none)
                    .resizable()
                    .scaledToFit()
                    .frame(width: 208, height: 208)
                    .padding(F33Spacing.md)
                    // White behind the code whatever the theme: a scanner
                    // needs the contrast, and an inverted QR is one many
                    // cameras will not read.
                    .background(Color.white, in: RoundedRectangle(cornerRadius: F33Radius.md))
            } else {
                Text("Couldn't draw the code. Type the secret below instead.")
                    .font(.footnote)
                    .foregroundStyle(F33Color.ink4)
            }
        }
        .frame(maxWidth: .infinity)
        .accessibilityLabel("Enrolment QR code")
    }

    private func secretRow(_ setup: TwoFactorSetup) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Or type this in")
                .font(.system(size: 13, weight: .semibold))
                .foregroundStyle(F33Color.ink3)

            HStack(spacing: F33Spacing.md) {
                Text(setup.groupedSecret)
                    .font(.system(size: 15, weight: .medium, design: .monospaced))
                    .foregroundStyle(F33Color.ink)
                    .textSelection(.enabled)
                    .fixedSize(horizontal: false, vertical: true)

                Spacer(minLength: 0)

                Button {
                    UIPasteboard.general.string = setup.secret
                } label: {
                    Image(systemName: "doc.on.doc")
                        .font(.system(size: 15, weight: .semibold))
                        .foregroundStyle(F33Color.accent)
                        .frame(minWidth: F33Layout.minTouchTarget, minHeight: F33Layout.minTouchTarget)
                        .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .accessibilityLabel("Copy the secret")
            }
            .padding(.horizontal, F33Spacing.lg)
            .padding(.vertical, F33Spacing.sm)
            .background(F33Color.bgElevated, in: RoundedRectangle(cornerRadius: F33Radius.md))
            .overlay(RoundedRectangle(cornerRadius: F33Radius.md).strokeBorder(F33Color.hairlineStrong, lineWidth: 1))
        }
    }

    // MARK: On

    @ViewBuilder
    private var enabledBody: some View {
        HStack(spacing: F33Spacing.md) {
            Image(systemName: "checkmark.shield.fill")
                .font(.system(size: 22))
                .foregroundStyle(F33Color.accent)
            Text("Two-factor sign-in is on for this account.")
                .font(.callout)
                .foregroundStyle(F33Color.ink2)
                .fixedSize(horizontal: false, vertical: true)
        }

        Button {
            showingBackupCodes = true
        } label: {
            Label("Backup codes", systemImage: "key.horizontal")
                .font(.callout.weight(.semibold))
                .foregroundStyle(F33Color.accent)
                .frame(maxWidth: .infinity, minHeight: F33Layout.minTouchTarget, alignment: .leading)
        }
        .buttonStyle(.plain)

        Divider().overlay(F33Color.hairline)

        Text("Turning it off takes a current code — an unlocked phone on a table isn't permission to remove the second factor.")
            .font(.footnote)
            .foregroundStyle(F33Color.ink4)
            .fixedSize(horizontal: false, vertical: true)

        codeField
        submitButton("Turn off", role: .destructive) { try await model.twoFactorDisable(code: code) }
    }

    // MARK: Shared

    private var codeField: some View {
        TextField("000000", text: $code)
            .keyboardType(.numberPad)
            .textContentType(.oneTimeCode)
            .font(.system(size: 22, weight: .semibold, design: .monospaced))
            .multilineTextAlignment(.center)
            .focused($codeFocused)
            .f33Field()
            .onChange(of: code) { _, new in
                let digits = new.filter(\.isNumber)
                code = String(digits.prefix(6))
            }
            .accessibilityLabel("Six-digit code")
    }

    private func submitButton(
        _ title: String,
        role: ButtonRole? = nil,
        _ work: @escaping () async throws -> Void
    ) -> some View {
        Button {
            Task { await submit(work) }
        } label: {
            if isSubmitting {
                ProgressView().tint(F33Color.accentInk)
            } else {
                Text(title)
            }
        }
        .buttonStyle(F33PrimaryButtonStyle())
        .disabled(!canSubmit)
        .tint(role == .destructive ? F33Color.danger : F33Color.accent)
    }

    // MARK: Work

    private func load() async {
        guard !isOn else {
            isLoading = false
            return
        }
        isLoading = true
        defer { isLoading = false }
        do {
            setup = try await model.twoFactorSetup()
        } catch let error as APIError {
            // A deployment without the route says so; the sheet shows the
            // server's words rather than a spinner that never stops.
            setup = nil
            self.error = error.isUnservedSurface ? nil : error.userMessage
        } catch {
            setup = nil
            self.error = error.localizedDescription
        }
    }

    private func submit(_ work: @escaping () async throws -> Void) async {
        guard canSubmit else { return }
        isSubmitting = true
        error = nil
        codeFocused = false
        defer { isSubmitting = false }
        do {
            try await work()
            code = ""
            dismiss()
        } catch let error as MalkuthError {
            self.error = error.description
        } catch let error as APIError {
            self.error = error.userMessage
        } catch {
            self.error = error.localizedDescription
        }
    }

    /// The `otpauth://` string as a QR bitmap, scaled up from the tiny image
    /// CoreImage produces so it is not a blur on a retina screen.
    private static func qrImage(_ text: String) -> UIImage? {
        let filter = CIFilter.qrCodeGenerator()
        filter.message = Data(text.utf8)
        // M: the level every authenticator's own docs assume.
        filter.correctionLevel = "M"
        guard let output = filter.outputImage else { return nil }
        let scaled = output.transformed(by: CGAffineTransform(scaleX: 10, y: 10))
        let context = CIContext()
        guard let cg = context.createCGImage(scaled, from: scaled.extent) else { return nil }
        return UIImage(cgImage: cg)
    }
}
