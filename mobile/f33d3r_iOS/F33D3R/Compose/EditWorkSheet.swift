import SwiftUI
import F33D3RKit

/// Rewriting the body of the reader's own work.
///
/// The server allows an edit for an hour after posting, and the menu offers
/// this only inside that hour; the server still decides, and a 409 is shown
/// as the rule rather than as a failure. Saving sends `work_edit`, then the
/// work is fetched back so the card shows the row the server now holds,
/// "Edited" label included.
struct EditWorkSheet: View {
    let work: Work

    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    @State private var body_: String
    @State private var isSaving = false
    @State private var error: String?
    @FocusState private var isFocused: Bool

    /// The web's body limit.
    private static let limit = 2000

    init(work: Work) {
        self.work = work
        _body_ = State(initialValue: work.body)
    }

    private var trimmed: String { body_.trimmingCharacters(in: .whitespacesAndNewlines) }

    private var canSave: Bool {
        !trimmed.isEmpty && trimmed != work.body && body_.count <= Self.limit && !isSaving
    }

    var body: some View {
        NavigationStack {
            VStack(alignment: .leading, spacing: F33Spacing.md) {
                windowLine

                TextEditor(text: $body_)
                    .focused($isFocused)
                    .font(.system(size: 17))
                    .foregroundStyle(F33Color.ink)
                    .scrollContentBackground(.hidden)
                    .padding(F33Spacing.sm)
                    .background(F33Color.bgElevated, in: RoundedRectangle(cornerRadius: F33Radius.md))
                    .overlay(RoundedRectangle(cornerRadius: F33Radius.md).strokeBorder(F33Color.hairlineStrong, lineWidth: 1))
                    .frame(minHeight: 160)

                HStack {
                    if let error {
                        Text(error)
                            .font(.footnote)
                            .foregroundStyle(F33Color.danger)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    Spacer()
                    Text("\(body_.count)/\(Self.limit)")
                        .font(.system(size: 12).monospacedDigit())
                        .foregroundStyle(body_.count > Self.limit ? F33Color.danger : F33Color.ink4)
                }

                Spacer(minLength: 0)
            }
            .padding(.horizontal, F33Spacing.xl)
            .padding(.top, F33Spacing.md)
            .background(F33Color.bg)
            .navigationTitle("Edit work")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }.disabled(isSaving)
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button {
                        Task { await save() }
                    } label: {
                        if isSaving { ProgressView() } else { Text("Save").fontWeight(.semibold) }
                    }
                    .disabled(!canSave)
                }
            }
        }
        .tint(F33Color.accent)
        .interactiveDismissDisabled(isSaving)
        .onAppear { isFocused = true }
    }

    private var windowLine: some View {
        HStack(spacing: 6) {
            Image(systemName: "clock")
                .font(.system(size: 11, weight: .semibold))
            Text(work.editWindowLabel)
                .font(.system(size: 12))
        }
        .foregroundStyle(F33Color.ink4)
    }

    private func save() async {
        guard canSave else { return }
        isSaving = true
        error = nil
        defer { isSaving = false }
        do {
            try await model.editWork(work, body: trimmed)
            dismiss()
        } catch let error as APIError {
            self.error = error.code == "edit_window_closed"
                ? "The hour to edit this work has passed. It stays as it was posted."
                : error.userMessage
        } catch {
            self.error = error.localizedDescription
        }
    }
}

extension Work {
    /// The server's edit window, mirrored so the menu offers only what the
    /// server will accept. The server is still the one that decides.
    static let editWindow: TimeInterval = 60 * 60

    var editWindowEndsAt: Date { createdAt.addingTimeInterval(Self.editWindow) }

    func isEditable(now: Date = Date()) -> Bool { now < editWindowEndsAt }

    var editWindowLabel: String {
        let remaining = editWindowEndsAt.timeIntervalSinceNow
        guard remaining > 0 else { return "The edit window has closed." }
        let minutes = Int(remaining / 60)
        return minutes < 1 ? "Editable for under a minute more." : "Editable for another \(minutes) min."
    }
}
