import SwiftUI
import F33D3RKit

/// The works kept for later, newest first.
///
/// Each row says when the draft was put down and how it starts, which is
/// enough to tell one from another and nothing the reader has to study. A tap
/// loads it into the composer behind this sheet; a swipe deletes it. Nothing
/// here is fetched — drafts are the device's, see ``ComposeDraft``.
struct ComposeDraftsSheet: View {
    let store: ComposeDraftStore
    /// Called with the draft to open. The sheet closes itself afterwards.
    let choose: (ComposeDraft) -> Void

    @Environment(\.dismiss) private var dismiss

    @State private var problem: String?

    var body: some View {
        NavigationStack {
            Group {
                if store.drafts.isEmpty {
                    VStack(spacing: F33Spacing.sm) {
                        Image(systemName: "doc.text")
                            .font(.system(size: 30))
                            .foregroundStyle(F33Color.ink5)
                        Text("No drafts")
                            .font(.headline)
                            .foregroundStyle(F33Color.ink2)
                    }
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                } else {
                    List {
                        ForEach(store.drafts) { draft in
                            Button {
                                choose(draft)
                                dismiss()
                            } label: {
                                DraftRow(draft: draft)
                            }
                            .buttonStyle(.plain)
                            .listRowBackground(Color.clear)
                            .listRowSeparatorTint(F33Color.hairline)
                            .accessibilityLabel("\(draft.title), saved \(RelativeTime.label(for: draft.savedAt))")
                            .accessibilityHint("Opens this draft")
                        }
                        .onDelete(perform: delete)

                        if let problem {
                            Text(problem)
                                .font(.footnote)
                                .foregroundStyle(F33Color.danger)
                                .listRowBackground(Color.clear)
                        }
                    }
                    .listStyle(.plain)
                    .scrollContentBackground(.hidden)
                }
            }
            .background(F33Color.bg)
            .navigationTitle("Drafts")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("Done") { dismiss() }
                        .foregroundStyle(F33Color.accent)
                }
            }
        }
        .presentationDetents([.medium, .large])
        .presentationDragIndicator(.visible)
    }

    private func delete(_ offsets: IndexSet) {
        let doomed = offsets.map { store.drafts[$0] }
        for draft in doomed {
            do {
                try store.delete(id: draft.id)
            } catch {
                // The row stays, because the file did. Saying so beats a row
                // that vanishes and comes back on the next open.
                problem = "Couldn't delete that draft: \(error.localizedDescription)"
            }
        }
    }
}

private struct DraftRow: View {
    let draft: ComposeDraft

    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            Text(draft.title)
                .font(.system(size: 15, weight: .semibold))
                .foregroundStyle(F33Color.ink)
                .lineLimit(2)
            Text(detail)
                .font(.system(size: 13))
                .foregroundStyle(F33Color.ink4)
                .lineLimit(1)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .frame(minHeight: F33Layout.minTouchTarget)
        .padding(.vertical, F33Spacing.xs)
        .contentShape(Rectangle())
    }

    /// When it was saved, then what else is in it.
    private var detail: String {
        var pieces = [RelativeTime.label(for: draft.savedAt)]
        if !draft.segments.isEmpty { pieces.append("\(draft.segments.count + 1) parts") }
        if !draft.attachments.isEmpty, !draft.body.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
            pieces.append(draft.attachments.count == 1 ? "1 photo" : "\(draft.attachments.count) photos")
        }
        if draft.poll != nil, !draft.body.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty { pieces.append("Poll") }
        if let at = draft.scheduledAt { pieces.append("Scheduled \(ScheduleClock.label(at))") }
        if draft.continuesThreadFrom != nil { pieces.append("Continues a thread") }
        return pieces.joined(separator: " · ")
    }
}
