import SwiftUI
import F33D3RKit

/// A poll on a card: a ballot until the reader votes or it closes, results
/// after.
///
/// Everything drawn — percentages, the winner, the time left, whether the
/// ballot is open — arrived projected from the server. Tapping an option sends
/// one event; the results appear when the server's re-read of the work does.
struct PollCard: View {
    let poll: Poll
    /// Casts a ballot for the option at this index. Nil draws the ballot
    /// without a vote path — a preview, or a work whose poll is closed.
    var onVote: ((Int) -> Void)?

    var body: some View {
        VStack(alignment: .leading, spacing: F33Spacing.sm) {
            ForEach(poll.results) { result in
                if poll.showsResults {
                    resultRow(result)
                } else {
                    ballotRow(result)
                }
            }

            HStack(spacing: F33Spacing.xs) {
                Text(votesLabel)
                if !poll.timeLeft.isEmpty {
                    Text("·")
                    Text(poll.timeLeft)
                }
            }
            .font(.system(size: 12))
            .foregroundStyle(F33Color.ink4)
            .padding(.top, 2)
        }
        .padding(.top, F33Card.mediaTopInset)
    }

    private func ballotRow(_ result: PollResult) -> some View {
        Button {
            onVote?(result.index)
        } label: {
            Text(result.label)
                .font(.system(size: 14, weight: .medium))
                .foregroundStyle(F33Color.accent)
                .frame(maxWidth: .infinity)
                .frame(minHeight: F33Layout.minTouchTarget)
                .overlay(
                    RoundedRectangle(cornerRadius: F33Radius.full)
                        .strokeBorder(F33Color.accent.opacity(0.5), lineWidth: 1.5)
                )
                .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .disabled(onVote == nil)
        .accessibilityLabel("Vote \(result.label)")
    }

    private func resultRow(_ result: PollResult) -> some View {
        GeometryReader { geo in
            ZStack(alignment: .leading) {
                RoundedRectangle(cornerRadius: F33Radius.xs)
                    .fill(F33Color.bgSunken)

                RoundedRectangle(cornerRadius: F33Radius.xs)
                    .fill(fill(for: result))
                    .frame(width: geo.size.width * CGFloat(result.pct) / 100)

                HStack(spacing: F33Spacing.xs) {
                    Text(result.label)
                        .font(.system(size: 14, weight: result.isWinner ? .semibold : .regular))
                        .foregroundStyle(F33Color.ink)
                        .lineLimit(1)

                    if result.voted {
                        Image(systemName: "checkmark.circle.fill")
                            .font(.system(size: 12))
                            .foregroundStyle(F33Color.accent)
                    }

                    Spacer(minLength: F33Spacing.sm)

                    Text("\(result.pct)%")
                        .font(.system(size: 13, weight: .semibold).monospacedDigit())
                        .foregroundStyle(F33Color.ink2)
                }
                .padding(.horizontal, F33Spacing.md)
            }
        }
        .frame(height: 36)
        .accessibilityElement(children: .combine)
        .accessibilityLabel(
            "\(result.label), \(result.pct) percent, \(Counts.exact(result.votes)) votes"
                + (result.voted ? ", your vote" : "")
        )
    }

    private func fill(for result: PollResult) -> Color {
        if result.voted { return F33Color.accent.opacity(0.30) }
        if result.isWinner { return F33Color.accent.opacity(0.16) }
        return F33Color.ink5.opacity(0.28)
    }

    private var votesLabel: String {
        poll.totalVotes == 1 ? "1 vote" : "\(Counts.exact(poll.totalVotes)) votes"
    }
}
