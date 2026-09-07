import SwiftUI
import F33D3RKit

/// Turning the account off, and asking for it to be erased.
///
/// Both are two-step on purpose, and the two steps are not the same kind of
/// friction: deactivation is reversible, so it asks for a confirmation;
/// deletion is not, so it asks the reader to type their own handle. A
/// destructive button behind a single tap is a destructive button somebody
/// hits with their thumb on a train.
///
/// The server revokes every session for both, so the app signs itself out
/// after either — staying signed in would be a screen full of requests that
/// all answer 401.

/// Step one is a warning, step two is the deed.
struct DeactivateAccountSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    @State private var reason = ""
    @State private var isConfirming = false
    @State private var isWorking = false
    @State private var error: String?

    var body: some View {
        DangerSheetLayout(
            title: "Deactivate account",
            icon: "moon.zzz",
            error: error
        ) {
            Text("""
                Your profile and works stop being visible to anyone else. Nothing is \
                deleted — signing in again brings it all back.
                """)
                .font(.callout)
                .foregroundStyle(F33Color.ink3)
                .fixedSize(horizontal: false, vertical: true)

            VStack(alignment: .leading, spacing: 6) {
                Text("Why, if you'd like to say")
                    .font(.system(size: 13, weight: .semibold))
                    .foregroundStyle(F33Color.ink3)
                TextField("Optional", text: $reason, axis: .vertical)
                    .lineLimit(2...4)
                    .padding(.horizontal, F33Spacing.lg)
                    .padding(.vertical, F33Spacing.md)
                    .background(F33Color.bgElevated, in: RoundedRectangle(cornerRadius: F33Radius.md))
                    .overlay(RoundedRectangle(cornerRadius: F33Radius.md).strokeBorder(F33Color.hairlineStrong, lineWidth: 1))
                    .foregroundStyle(F33Color.ink)
            }

            if isConfirming {
                Text("This signs you out on every device. Tap again to confirm.")
                    .font(.footnote.weight(.medium))
                    .foregroundStyle(F33Color.warn)
                    .fixedSize(horizontal: false, vertical: true)
            }
        } action: {
            DangerButton(
                title: isConfirming ? "Yes, deactivate" : "Deactivate",
                isWorking: isWorking,
                isEnabled: !isWorking
            ) {
                if isConfirming {
                    Task { await deactivate() }
                } else {
                    withAnimation(F33Motion.easeOut) { isConfirming = true }
                }
            }
        }
    }

    private func deactivate() async {
        isWorking = true
        error = nil
        defer { isWorking = false }
        do {
            try await model.deactivateAccount(reason: reason.trimmingCharacters(in: .whitespacesAndNewlines))
            dismiss()
        } catch let error as MalkuthError {
            self.error = error.description
        } catch let error as APIError {
            self.error = error.userMessage
        } catch {
            self.error = error.localizedDescription
        }
    }
}

/// Step two is typing the handle. Nothing else unlocks the button.
struct DeleteAccountSheet: View {
    let handle: String

    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    @State private var typed = ""
    @State private var reason = ""
    @State private var isConfirming = false
    @State private var isWorking = false
    @State private var error: String?

    private var matches: Bool {
        typed.trimmingCharacters(in: .whitespaces)
            .trimmingCharacters(in: CharacterSet(charactersIn: "@"))
            .caseInsensitiveCompare(handle) == .orderedSame
    }

    var body: some View {
        DangerSheetLayout(
            title: "Delete account",
            icon: "trash",
            error: error
        ) {
            Text("""
                Everything you've posted, your handle and your F33D3R Numbers go. \
                This can't be undone, and the handle doesn't come back to you.
                """)
                .font(.callout)
                .foregroundStyle(F33Color.ink3)
                .fixedSize(horizontal: false, vertical: true)

            if isConfirming {
                VStack(alignment: .leading, spacing: 6) {
                    Text("Type @\(handle) to confirm")
                        .font(.system(size: 13, weight: .semibold))
                        .foregroundStyle(F33Color.ink3)
                    TextField("@\(handle)", text: $typed)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .f33Field()
                }

                VStack(alignment: .leading, spacing: 6) {
                    Text("Why, if you'd like to say")
                        .font(.system(size: 13, weight: .semibold))
                        .foregroundStyle(F33Color.ink3)
                    TextField("Optional", text: $reason, axis: .vertical)
                        .lineLimit(2...4)
                        .padding(.horizontal, F33Spacing.lg)
                        .padding(.vertical, F33Spacing.md)
                        .background(F33Color.bgElevated, in: RoundedRectangle(cornerRadius: F33Radius.md))
                        .overlay(RoundedRectangle(cornerRadius: F33Radius.md).strokeBorder(F33Color.hairlineStrong, lineWidth: 1))
                        .foregroundStyle(F33Color.ink)
                }
            }
        } action: {
            DangerButton(
                title: isConfirming ? "Delete my account" : "I want to delete my account",
                isWorking: isWorking,
                isEnabled: !isWorking && (!isConfirming || matches)
            ) {
                if isConfirming {
                    Task { await delete() }
                } else {
                    withAnimation(F33Motion.easeOut) { isConfirming = true }
                }
            }
        }
    }

    private func delete() async {
        guard matches else { return }
        isWorking = true
        error = nil
        defer { isWorking = false }
        do {
            try await model.deleteAccount(reason: reason.trimmingCharacters(in: .whitespacesAndNewlines))
            dismiss()
        } catch let error as MalkuthError {
            self.error = error.description
        } catch let error as APIError {
            self.error = error.userMessage
        } catch {
            self.error = error.localizedDescription
        }
    }
}

/// The frame both sheets sit in, so the two irreversible screens in the app
/// look like each other and like nothing else.
private struct DangerSheetLayout<Content: View, Action: View>: View {
    let title: String
    let icon: String
    let error: String?
    @ViewBuilder var content: Content
    @ViewBuilder var action: Action

    @Environment(\.dismiss) private var dismiss

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: F33Spacing.lg) {
                    Image(systemName: icon)
                        .font(.system(size: 34))
                        .foregroundStyle(F33Color.danger)
                        .frame(maxWidth: .infinity)
                        .padding(.top, F33Spacing.md)

                    content

                    if let error {
                        Text(error)
                            .font(.footnote)
                            .foregroundStyle(F33Color.danger)
                            .fixedSize(horizontal: false, vertical: true)
                    }

                    action
                        .padding(.top, F33Spacing.sm)
                }
                .padding(.horizontal, F33Spacing.xl)
                .padding(.bottom, F33Spacing.xl)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            .background(F33Color.bg)
            .scrollDismissesKeyboard(.interactively)
            .navigationTitle(title)
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
            }
        }
        .tint(F33Color.accent)
    }
}

private struct DangerButton: View {
    let title: String
    let isWorking: Bool
    let isEnabled: Bool
    let tapped: () -> Void

    var body: some View {
        Button(action: tapped) {
            Group {
                if isWorking {
                    ProgressView().tint(.white)
                } else {
                    Text(title)
                }
            }
            .font(.system(size: 16, weight: .semibold))
            .foregroundStyle(.white)
            .frame(maxWidth: .infinity, minHeight: F33Layout.minTouchTarget)
            .background(F33Color.danger.opacity(isEnabled ? 1 : 0.4), in: Capsule())
        }
        .buttonStyle(.plain)
        .disabled(!isEnabled)
    }
}
