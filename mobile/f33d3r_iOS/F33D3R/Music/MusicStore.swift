import SwiftUI
import F33D3RKit

// MARK: - The mutation

extension AppModel {
    /// Buys a priced work outright and shows the server's row.
    ///
    /// Nothing is flipped ahead of the answer: the row comes back with
    /// `work_purchased_by_viewer` set by the server, and that is when "Owned"
    /// is drawn. The player holding the same work is handed the row too, so
    /// the full player and the store agree.
    func purchase(_ work: Work) async throws {
        #if DEBUG
        if isSampleMode { return }
        #endif
        try await client.purchase(workID: work.id)
        let fresh = try await refreshWork(id: work.id)
        musicPlayer().refresh(fresh)
    }
}

// MARK: - Price and ownership

/// The price on a track — "2 AET" in the currency colour — or "Owned" once the
/// server says the viewer has bought it.
///
/// Reads the row it is given and nothing else. There is no in-between state
/// drawn here: a purchase in flight dims the button that started it, and the
/// pill changes when the server's row does.
struct PricePill: View {
    let work: Work

    var body: some View {
        Group {
            if work.purchasedByViewer {
                Label("Owned", systemImage: "checkmark")
                    .foregroundStyle(F33Color.ok)
            } else if let price = work.priceUAET, price > 0 {
                Text(AET.label(uAET: price))
                    .foregroundStyle(F33Color.aet)
            }
        }
        .font(.system(size: 13, weight: .semibold).monospacedDigit())
        .labelStyle(.titleAndIcon)
        .padding(.horizontal, F33Spacing.sm + 2)
        .frame(height: 26)
        .background(
            Capsule().fill(work.purchasedByViewer ? F33Color.ok.opacity(0.12) : F33Color.aet.opacity(0.12))
        )
        .lineLimit(1)
        .fixedSize()
    }
}

/// The buy control. A tap asks first — money moves on the second tap — then
/// sends `work_purchase` and waits for the server's row.
///
/// The button carries the confirmation and the error because it is the thing
/// that was tapped; a store row and the full player both use it and neither
/// has to know how buying works.
struct BuyButton: View {
    let work: Work

    @Environment(AppModel.self) private var model

    @State private var isConfirming = false
    @State private var isBuying = false
    @State private var error: String?

    private var price: Int64 { work.priceUAET ?? 0 }
    private var isOwn: Bool { model.state.user?.user.handle == work.author.handle }

    var body: some View {
        Button {
            isConfirming = true
        } label: {
            if isBuying {
                ProgressView().tint(F33Color.aet)
                    .frame(width: 52)
            } else {
                Text("Buy")
                    .font(.system(size: 14, weight: .semibold))
                    .foregroundStyle(F33Color.aet)
                    .frame(minWidth: 52)
            }
        }
        .buttonStyle(.plain)
        .frame(height: 32)
        .background(Capsule().fill(F33Color.aet.opacity(0.14)))
        .disabled(isBuying || isOwn || price <= 0)
        .opacity(isOwn ? 0.4 : 1)
        .accessibilityLabel("Buy for \(AET.label(uAET: price))")
        .confirmationDialog(
            "Buy \u{201C}\(work.trackTitle)\u{201D} for \(AET.label(uAET: price))?",
            isPresented: $isConfirming,
            titleVisibility: .visible
        ) {
            Button("Buy for \(AET.label(uAET: price))") { Task { await buy() } }
        } message: {
            Text("Paid straight to @\(work.author.handle)'s wallet. F33D3R never holds it.")
        }
        .alert("That didn't go through", isPresented: Binding(get: { error != nil }, set: { if !$0 { error = nil } })) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(error ?? "")
        }
    }

    private func buy() async {
        isBuying = true
        defer { isBuying = false }
        do {
            try await model.purchase(work)
        } catch let malkuth as MalkuthError {
            if case .rejected(let status, _) = malkuth, status == 402 {
                error = "Not enough in your wallet for \(AET.label(uAET: price))."
            } else {
                error = malkuth.description
            }
        } catch let apiError as APIError {
            error = apiError.userMessage
        } catch {
            self.error = error.localizedDescription
        }
    }
}

// MARK: - Rows

/// One track in the Store: artwork, title, artist, and the price or "Owned"
/// with the buy control beside it. Tapping the row plays the track; buying is
/// the button, so a reader cannot spend by mistake on the way to listening.
struct StoreRow: View {
    let work: Work
    let isCurrent: Bool
    let isPlaying: Bool
    let play: () -> Void

    var body: some View {
        HStack(spacing: F33Spacing.md) {
            Button(action: play) {
                HStack(spacing: F33Spacing.md) {
                    MusicArtwork(work: work, size: 56, cornerRadius: F33Radius.sm)
                        .overlay {
                            if isCurrent {
                                RoundedRectangle(cornerRadius: F33Radius.sm, style: .continuous)
                                    .fill(.black.opacity(0.35))
                                NowPlayingGlyph(isPlaying: isPlaying)
                            }
                        }
                    VStack(alignment: .leading, spacing: 3) {
                        Text(work.trackTitle)
                            .font(.system(size: 15, weight: .semibold))
                            .foregroundStyle(isCurrent ? F33Color.accent : F33Color.ink)
                            .lineLimit(1)
                        Text("@\(work.author.handle)")
                            .font(.system(size: 13))
                            .foregroundStyle(F33Color.ink3)
                            .lineLimit(1)
                        PricePill(work: work)
                    }
                    Spacer(minLength: 0)
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("\(work.trackTitle), @\(work.author.handle), \(work.purchasedByViewer ? "owned" : AET.label(uAET: work.priceUAET ?? 0))")
            .accessibilityHint(isCurrent && isPlaying ? "Pauses" : "Plays")

            if !work.purchasedByViewer {
                BuyButton(work: work)
            }
        }
        .padding(.horizontal, F33Card.paddingHorizontal)
        .padding(.vertical, F33Spacing.sm + 2)
        .frame(minHeight: F33Layout.minTouchTarget + 16)
    }
}

/// One track in the Tracks list: artwork, title, artist, length. The row
/// that is loaded shows the moving bars in place of its length.
struct TrackRow: View {
    let work: Work
    let isCurrent: Bool
    let isPlaying: Bool
    let play: () -> Void

    var body: some View {
        Button(action: play) {
            HStack(spacing: F33Spacing.md) {
                MusicArtwork(work: work, size: 48, cornerRadius: F33Radius.xs)
                VStack(alignment: .leading, spacing: 2) {
                    Text(work.trackTitle)
                        .font(.system(size: 15, weight: .medium))
                        .foregroundStyle(isCurrent ? F33Color.accent : F33Color.ink)
                        .lineLimit(1)
                    HStack(spacing: F33Spacing.xs) {
                        Text("@\(work.author.handle)")
                        if work.isForSale {
                            Text("·")
                            Text(work.purchasedByViewer ? "Owned" : AET.label(uAET: work.priceUAET ?? 0))
                                .foregroundStyle(work.purchasedByViewer ? F33Color.ok : F33Color.aet)
                        }
                    }
                    .font(.system(size: 13))
                    .foregroundStyle(F33Color.ink3)
                    .lineLimit(1)
                }
                Spacer(minLength: F33Spacing.sm)
                if isCurrent {
                    NowPlayingGlyph(isPlaying: isPlaying)
                } else {
                    Text(MusicTime.label(work.playbackDurationSecs))
                        .font(.system(size: 13).monospacedDigit())
                        .foregroundStyle(F33Color.ink4)
                }
            }
            .padding(.horizontal, F33Card.paddingHorizontal)
            .padding(.vertical, F33Spacing.sm)
            .frame(minHeight: F33Layout.minTouchTarget + 12)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel("\(work.trackTitle), @\(work.author.handle), \(MusicTime.label(work.playbackDurationSecs))")
        .accessibilityHint(isCurrent && isPlaying ? "Pauses" : "Plays")
    }
}

/// A square tile in the New row: artwork with the title and artist beneath.
struct NewTrackTile: View {
    let work: Work
    let isCurrent: Bool
    let isPlaying: Bool
    let play: () -> Void

    static let size: CGFloat = 148

    var body: some View {
        Button(action: play) {
            VStack(alignment: .leading, spacing: F33Spacing.sm) {
                MusicArtwork(work: work, size: Self.size, cornerRadius: F33Radius.md)
                    .overlay(alignment: .bottomTrailing) {
                        playBadge
                            .padding(F33Spacing.sm)
                    }
                    .overlay(alignment: .topLeading) {
                        if work.isForSale {
                            PricePill(work: work)
                                .padding(F33Spacing.sm)
                        }
                    }
                VStack(alignment: .leading, spacing: 2) {
                    Text(work.trackTitle)
                        .font(.system(size: 14, weight: .semibold))
                        .foregroundStyle(isCurrent ? F33Color.accent : F33Color.ink)
                    Text("@\(work.author.handle)")
                        .font(.system(size: 12))
                        .foregroundStyle(F33Color.ink3)
                }
                .lineLimit(1)
            }
            .frame(width: Self.size, alignment: .leading)
        }
        .buttonStyle(.plain)
        .accessibilityLabel("\(work.trackTitle), @\(work.author.handle)")
        .accessibilityHint(isCurrent && isPlaying ? "Pauses" : "Plays")
    }

    /// The play badge on the tile — a small floating control, so it takes the
    /// glass the tile itself does not.
    private var playBadge: some View {
        Image(systemName: isCurrent && isPlaying ? "pause.fill" : "play.fill")
            .font(.system(size: 13, weight: .bold))
            .foregroundStyle(F33Color.ink)
            .frame(width: 32, height: 32)
            .f33Glass(in: Circle(), elevation: .floating)
    }
}
