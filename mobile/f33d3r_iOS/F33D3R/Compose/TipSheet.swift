import SwiftUI
import F33D3RKit

/// Sending a tip to a creator, on a work or on their profile.
///
/// The balance shown is the server's, fetched when the sheet opens. The
/// amount goes as hundredths of an AET, which is how the write lane counts.
/// Nothing on this screen is a figure the app invented.
struct TipSheet: View {
    let handle: String
    /// The work being tipped, when there is one. Attributes the tip so the card
    /// can show what the work has earned.
    var work: Work?

    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    @State private var wallet: WalletSnapshot?
    @State private var walletError: APIError?
    @State private var choice: Int? = 200   // hundredths: 2 AET
    @State private var custom = ""
    @State private var isSending = false
    @State private var error: String?
    @State private var didSend = false

    private static let presets: [Int] = [100, 200, 500, 1000, 2500]

    private var amountHundredths: Int? {
        if let choice { return choice }
        guard let value = Double(custom.replacingOccurrences(of: ",", with: ".")), value > 0 else { return nil }
        return Int((value * 100).rounded())
    }

    private var canSend: Bool {
        guard let amount = amountHundredths, amount > 0, !isSending, !didSend else { return false }
        if let wallet { return Int64(amount) * (AET.microsPerAET / 100) <= wallet.balanceUAET }
        return true
    }

    var body: some View {
        VStack(alignment: .leading, spacing: F33Spacing.lg) {
            Capsule()
                .fill(F33Color.ink5)
                .frame(width: 36, height: 5)
                .frame(maxWidth: .infinity)
                .padding(.top, F33Spacing.sm)

            HStack(spacing: F33Spacing.md) {
                Image(systemName: "bolt.fill")
                    .font(.system(size: 20, weight: .medium))
                    .foregroundStyle(F33Action.tip)
                    .frame(width: 40, height: 40)
                    .background(F33Action.tip.opacity(0.14), in: Circle())

                VStack(alignment: .leading, spacing: 2) {
                    Text("Tip @\(handle)")
                        .font(.title3.weight(.semibold))
                        .foregroundStyle(F33Color.ink)
                    Text("Goes straight to their wallet. F33D3R never holds it.")
                        .font(.footnote)
                        .foregroundStyle(F33Color.ink4)
                }
                Spacer(minLength: 0)
            }

            if didSend {
                sent
            } else {
                amounts
                customField
                balanceLine
            }

            if let error {
                Text(error)
                    .font(.footnote)
                    .foregroundStyle(F33Color.danger)
                    .fixedSize(horizontal: false, vertical: true)
            }

            Spacer(minLength: 0)

            Button {
                if didSend { dismiss() } else { Task { await send() } }
            } label: {
                if isSending {
                    ProgressView().tint(F33Color.accentInk)
                } else if didSend {
                    Text("Done")
                } else if let amount = amountHundredths {
                    Text("Send \(AET.label(uAET: Int64(amount) * (AET.microsPerAET / 100)))")
                } else {
                    Text("Send a tip")
                }
            }
            .buttonStyle(F33PrimaryButtonStyle())
            .disabled(!canSend && !didSend)
            .padding(.bottom, F33Spacing.lg)
        }
        .padding(.horizontal, F33Spacing.xl)
        .frame(maxWidth: .infinity, alignment: .leading)
        .f33GlassSheet()
        .presentationDetents([.height(440)])
        .presentationDragIndicator(.hidden)
        .task { await loadWallet() }
        .animation(F33Motion.easeOut, value: didSend)
    }

    private var amounts: some View {
        HStack(spacing: F33Spacing.sm) {
            ForEach(Self.presets, id: \.self) { preset in
                let isOn = choice == preset
                Button {
                    choice = preset
                    custom = ""
                } label: {
                    Text(AET.amount(uAET: Int64(preset) * (AET.microsPerAET / 100)))
                        .font(.system(size: 15, weight: .semibold).monospacedDigit())
                        .foregroundStyle(isOn ? F33Color.accentInk : F33Color.ink2)
                        .frame(maxWidth: .infinity)
                        .frame(height: F33Layout.minTouchTarget)
                        .f33Glass(in: Capsule(), tint: isOn ? F33Action.tip : nil, interactive: true)
                }
                .buttonStyle(.plain)
                .accessibilityLabel("\(AET.label(uAET: Int64(preset) * (AET.microsPerAET / 100)))")
                .accessibilityAddTraits(isOn ? [.isSelected, .isButton] : .isButton)
            }
        }
    }

    private var customField: some View {
        HStack(spacing: F33Spacing.sm) {
            TextField("Other amount", text: $custom)
                .keyboardType(.decimalPad)
                .onChange(of: custom) { _, new in
                    if !new.isEmpty { choice = nil }
                }
                .f33Field()
            Text("AET")
                .font(.system(size: 14, weight: .semibold))
                .foregroundStyle(F33Color.ink3)
        }
    }

    @ViewBuilder
    private var balanceLine: some View {
        if let wallet {
            Text("Balance: \(AET.label(uAET: wallet.balanceUAET))")
                .font(.footnote.monospacedDigit())
                .foregroundStyle(canSend || amountHundredths == nil ? F33Color.ink4 : F33Color.danger)
        } else if let walletError {
            Text("Couldn't read your balance: \(walletError.userMessage)")
                .font(.footnote)
                .foregroundStyle(F33Color.ink4)
        } else {
            Text("Reading your balance…")
                .font(.footnote)
                .foregroundStyle(F33Color.ink5)
        }
    }

    private var sent: some View {
        HStack(spacing: F33Spacing.sm) {
            Image(systemName: "checkmark.circle.fill")
                .foregroundStyle(F33Color.ok)
            Text("Sent. @\(handle) has it now.")
                .font(.callout)
                .foregroundStyle(F33Color.ink2)
        }
        .frame(minHeight: F33Layout.minTouchTarget)
    }

    private func loadWallet() async {
        do {
            wallet = try await model.wallet()
        } catch let apiError as APIError {
            walletError = apiError
        } catch {
            walletError = .transport(error.localizedDescription)
        }
    }

    private func send() async {
        guard let amount = amountHundredths else { return }
        isSending = true
        error = nil
        defer { isSending = false }
        do {
            if let work {
                try await model.tip(work: work, amountHundredths: amount)
            } else {
                try await model.tip(handle: handle, amountHundredths: amount)
            }
            didSend = true
        } catch let malkuth as MalkuthError {
            if case .rejected(let status, let reason) = malkuth, status == 402 {
                error = "Not enough in your wallet. \(reason.capitalizedFirst)"
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

private extension String {
    var capitalizedFirst: String {
        guard let first else { return self }
        return String(first).uppercased() + dropFirst()
    }
}
