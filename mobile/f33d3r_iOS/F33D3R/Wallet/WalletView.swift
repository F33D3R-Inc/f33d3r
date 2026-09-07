import SwiftUI
import F33D3RKit

/// Wallet: what the reader has, what moved, and where it went.
///
/// Every figure here is the server's, fetched when the screen appears and on
/// pull. Settled and pending are shown as two numbers and never added: a tip
/// inside its reversal window is not the reader's money yet. While the answer
/// is on its way the screen shows no number at all — not a zero, not a
/// shimmer in the shape of one — because a figure on this screen is a claim
/// about somebody's money.
///
/// It also carries sign-out. The account has to be reachable from somewhere
/// and the profile is a push from the feed header rather than a tab of its own.
struct WalletView: View {
    @Environment(AppModel.self) private var model

    @State private var wallet: WalletSnapshot?
    @State private var error: APIError?

    var body: some View {
        ScrollView {
            LazyVStack(spacing: 0) {
                #if DEBUG
                if model.isSampleMode {
                    SampleDataBanner()
                    content(SampleData.wallet)
                } else {
                    live
                }
                #else
                live
                #endif
            }
        }
        .background(F33Color.bg)
        .navigationTitle("Wallet")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                Button("Sign out") {
                    Task { await model.signOut() }
                }
                .foregroundStyle(F33Color.ink3)
                .frame(minHeight: F33Layout.minTouchTarget)
            }
        }
        .refreshable { await load() }
        .task { await load() }
        // The server pushes a balance frame after every tip in or out; the
        // snapshot is fetched again so the entries below it match.
        .onChange(of: model.signals?.balanceUAET) { _, _ in
            Task { await load() }
        }
    }

    @ViewBuilder
    private var live: some View {
        if let wallet {
            content(wallet)
        } else if let error {
            ErrorStateView(error: error) {
                Task { await load() }
            }
        } else {
            // Deliberately no placeholder figure.
            VStack(spacing: F33Spacing.md) {
                ProgressView()
                Text("Reading your ledger")
                    .font(.footnote)
                    .foregroundStyle(F33Color.ink4)
            }
            .frame(maxWidth: .infinity)
            .padding(.top, F33Spacing.xxl * 2)
        }
    }

    @ViewBuilder
    private func content(_ wallet: WalletSnapshot) -> some View {
        WalletBalanceCard(wallet: wallet)

        if let user = model.state.user?.user {
            HStack(spacing: F33Spacing.md) {
                Text("@\(user.handle)")
                    .font(.system(size: 13, weight: .medium))
                    .foregroundStyle(F33Color.ink3)
                Spacer(minLength: 0)
                Text("Tips settle straight to you. F33D3R holds none of it.")
                    .font(.system(size: 12))
                    .foregroundStyle(F33Color.ink4)
                    .multilineTextAlignment(.trailing)
            }
            .padding(.horizontal, F33Card.paddingHorizontal)
            .padding(.bottom, F33Spacing.lg)
        }

        SectionHeading(title: "Activity")

        if wallet.entries.isEmpty {
            Text("Nothing has moved yet.")
                .font(.footnote)
                .foregroundStyle(F33Color.ink5)
                .frame(maxWidth: .infinity)
                .padding(.vertical, F33Spacing.xl)
        } else {
            ForEach(wallet.entries) { entry in
                WalletEntryRow(entry: entry)
                CardDivider()
            }
        }
    }

    private func load() async {
        error = nil
        do {
            wallet = try await model.wallet()
        } catch let apiError as APIError {
            if wallet == nil { error = apiError }
        } catch {
            if wallet == nil { self.error = .transport(error.localizedDescription) }
        }
    }
}

/// The balance block: what is settled, and what has arrived but is not spendable.
struct WalletBalanceCard: View {
    let wallet: WalletSnapshot

    var body: some View {
        VStack(alignment: .leading, spacing: F33Spacing.sm) {
            Text("Balance")
                .font(.system(size: 12, weight: .semibold))
                .tracking(0.4)
                .textCase(.uppercase)
                .foregroundStyle(F33Color.ink4)

            Text(AET.label(uAET: wallet.balanceUAET))
                .font(.system(size: 34, weight: .semibold).monospacedDigit())
                .foregroundStyle(F33Color.ink)
                .lineLimit(1)
                .minimumScaleFactor(0.6)
                .contentTransition(.numericText())

            if wallet.pendingUAET > 0 {
                Label(
                    "\(AET.label(uAET: wallet.pendingUAET)) pending",
                    systemImage: "clock"
                )
                .font(.system(size: 13))
                .foregroundStyle(F33Color.ink4)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(F33Spacing.lg)
        // The one card in the app that takes glass: it floats over the brand
        // wash below it and is chrome about money rather than content.
        .f33Glass(in: RoundedRectangle(cornerRadius: F33Radius.lg), elevation: .floating)
        .padding(.horizontal, F33Card.paddingHorizontal)
        .padding(.vertical, F33Spacing.lg)
        .background(alignment: .top) {
            RadialGradient(
                colors: [F33Color.aet.opacity(0.22), .clear],
                center: UnitPoint(x: 0.15, y: 0.1),
                startRadius: 0,
                endRadius: 260
            )
            .accessibilityHidden(true)
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel(accessibilityLabel)
    }

    private var accessibilityLabel: String {
        var label = "Balance, \(AET.label(uAET: wallet.balanceUAET))"
        if wallet.pendingUAET > 0 {
            label += ", plus \(AET.label(uAET: wallet.pendingUAET)) pending"
        }
        return label
    }
}

/// One row of the ledger: what it was, who it was with, and which way it went.
struct WalletEntryRow: View {
    let entry: WalletEntry

    var body: some View {
        HStack(spacing: F33Spacing.md) {
            Image(systemName: icon)
                .font(.system(size: 14, weight: .semibold))
                .foregroundStyle(entry.isCredit ? F33Color.ok : F33Color.ink3)
                .frame(width: 34, height: 34)
                .background(
                    (entry.isCredit ? F33Color.ok : F33Color.ink4).opacity(0.12),
                    in: Circle()
                )

            VStack(alignment: .leading, spacing: 2) {
                Text(entry.title)
                    .font(.system(size: 14, weight: .medium))
                    .foregroundStyle(F33Color.ink)

                Text(subtitle)
                    .font(.system(size: 12))
                    .foregroundStyle(F33Color.ink4)
            }

            Spacer(minLength: F33Spacing.sm)

            Text(entry.signedLabel)
                .font(.system(size: 14, weight: .semibold).monospacedDigit())
                .foregroundStyle(entry.isCredit ? F33Color.ok : F33Color.ink2)
                .lineLimit(1)
                .fixedSize()
        }
        .padding(.horizontal, F33Card.paddingHorizontal)
        .padding(.vertical, F33Spacing.md)
        .frame(minHeight: F33Layout.minTouchTarget)
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("\(entry.title), \(subtitle), \(spokenAmount)")
    }

    private var subtitle: String {
        let time = RelativeTime.label(for: entry.createdAt)
        guard let handle = entry.counterpartyHandle else { return time }
        return "@\(handle) · \(time)"
    }

    private var spokenAmount: String {
        let magnitude = AET.label(uAET: abs(entry.amountUAET))
        return entry.isCredit ? "received \(magnitude)" : "sent \(magnitude)"
    }

    private var icon: String {
        switch entry.kind {
        case "tip_received", "tip_sent": return "bolt.fill"
        case "subscription": return "star.fill"
        case "unlock": return "lock.open.fill"
        case "sale": return "music.note"
        case "payout": return "arrow.up.right"
        case "airdrop": return "gift.fill"
        default: return "arrow.left.arrow.right"
        }
    }
}

/// A small heading above a group of rows.
struct SectionHeading: View {
    let title: String

    var body: some View {
        Text(title)
            .font(.system(size: 12, weight: .semibold))
            .tracking(0.4)
            .textCase(.uppercase)
            .foregroundStyle(F33Color.ink4)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, F33Card.paddingHorizontal)
            .padding(.bottom, F33Spacing.sm)
            .accessibilityAddTraits(.isHeader)
    }
}

#Preview("Wallet") {
    NavigationStack {
        WalletView()
    }
    .environment(AppModel())
}
