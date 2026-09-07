import SwiftUI
import F33D3RKit

/// Reporting a work. One tap on a reason sends it; the author never learns.
struct ReportSheet: View {
    let work: Work

    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    @State private var sending: APIClient.ReportReason?
    @State private var sent = false
    @State private var error: String?

    var body: some View {
        VStack(alignment: .leading, spacing: F33Spacing.lg) {
            Capsule()
                .fill(F33Color.ink5)
                .frame(width: 36, height: 5)
                .frame(maxWidth: .infinity)
                .padding(.top, F33Spacing.sm)

            HStack(spacing: F33Spacing.md) {
                Image(systemName: "flag.fill")
                    .font(.system(size: 20, weight: .medium))
                    .foregroundStyle(F33Color.danger)
                    .frame(width: 40, height: 40)
                    .background(F33Color.danger.opacity(0.12), in: Circle())

                VStack(alignment: .leading, spacing: 2) {
                    Text(sent ? "Reported" : "Report this work")
                        .font(.title3.weight(.semibold))
                        .foregroundStyle(F33Color.ink)
                    Text(sent
                         ? "Moderation has it. @\(work.author.handle) won't be told who reported."
                         : "Goes to moderation with the reason. Never to @\(work.author.handle).")
                        .font(.footnote)
                        .foregroundStyle(F33Color.ink4)
                        .fixedSize(horizontal: false, vertical: true)
                }
                Spacer(minLength: 0)
            }

            if !sent {
                VStack(spacing: F33Spacing.xs) {
                    ForEach(APIClient.ReportReason.allCases) { reason in
                        Button {
                            Task { await send(reason) }
                        } label: {
                            HStack {
                                Text(reason.title)
                                    .font(.system(size: 15, weight: .medium))
                                    .foregroundStyle(F33Color.ink)
                                Spacer(minLength: 0)
                                if sending == reason {
                                    ProgressView()
                                } else {
                                    Image(systemName: "chevron.right")
                                        .font(.system(size: 12, weight: .semibold))
                                        .foregroundStyle(F33Color.ink5)
                                }
                            }
                            .padding(.horizontal, F33Spacing.lg)
                            .frame(height: F33Layout.minTouchTarget + 4)
                            .background(F33Color.bgElevated, in: RoundedRectangle(cornerRadius: F33Radius.md))
                            .overlay(RoundedRectangle(cornerRadius: F33Radius.md).strokeBorder(F33Color.hairline, lineWidth: 1))
                        }
                        .buttonStyle(.plain)
                        .disabled(sending != nil)
                    }
                }
            }

            if let error {
                Text(error)
                    .font(.footnote)
                    .foregroundStyle(F33Color.danger)
            }

            Spacer(minLength: 0)

            if sent {
                Button("Done") { dismiss() }
                    .buttonStyle(F33PrimaryButtonStyle())
                    .padding(.bottom, F33Spacing.lg)
            }
        }
        .padding(.horizontal, F33Spacing.xl)
        .frame(maxWidth: .infinity, alignment: .leading)
        .f33GlassSheet()
        .presentationDetents([.height(520)])
        .presentationDragIndicator(.hidden)
        .animation(F33Motion.easeOut, value: sent)
    }

    private func send(_ reason: APIClient.ReportReason) async {
        sending = reason
        error = nil
        defer { sending = nil }
        do {
            try await model.report(work: work, reason: reason)
            sent = true
        } catch let malkuth as MalkuthError {
            error = malkuth.description
        } catch let apiError as APIError {
            error = apiError.userMessage
        } catch {
            self.error = error.localizedDescription
        }
    }
}
