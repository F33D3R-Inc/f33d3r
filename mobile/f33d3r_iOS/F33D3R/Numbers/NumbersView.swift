import SwiftUI
import UIKit
import F33D3RKit

/// The account's own F33D3R Numbers.
///
/// A Number is a contact address the owner hands out: twelve digits somebody
/// can reach them at without being given a handle, and without learning
/// anything by trying. An account holds as many as it likes, and that is the
/// point — a Number given to a venue and a Number given to a friend are retired
/// separately, so losing control of one costs one.
///
/// Three things live here and nowhere else: the Numbers with their policies,
/// the account-wide policy under them, and the people currently asking to be
/// let through.
///
/// A Number is meant to be read aloud, so it is set in monospaced digits at a
/// size that survives being held up, and a tap copies it. Everything on this
/// screen is the owner's own; nothing on it describes anybody else's, and no
/// policy shown here is ever told to somebody it turned away.
struct NumbersView: View {
    @Environment(AppModel.self) private var model

    @State private var justCopied: String?
    @State private var confirmingRevoke: ContactNumber?

    var body: some View {
        let store = model.numbersStore()

        List {
            switch store.phase {
            case .idle, .loading:
                Section {
                    HStack(spacing: F33Spacing.md) {
                        ProgressView()
                        Text("Loading your Numbers…")
                            .foregroundStyle(F33Color.ink4)
                    }
                }

            case .failed(let message):
                Section {
                    VStack(alignment: .leading, spacing: F33Spacing.sm) {
                        Text(message)
                            .font(.subheadline)
                            .foregroundStyle(F33Color.ink3)
                        Button("Try again") { Task { await store.reload() } }
                            .font(.subheadline.weight(.semibold))
                            .foregroundStyle(F33Color.accent)
                            .frame(minHeight: F33Layout.minTouchTarget)
                    }
                }

            case .loaded:
                requestsSection(store)
                numbersSection(store)
                policySection(store)
                retiredSection(store)
            }
        }
        .listStyle(.insetGrouped)
        .scrollContentBackground(.hidden)
        .background(F33Color.bg)
        // No compose button here — a settings screen.
        .hidesComposeFAB()
        .navigationTitle("Numbers")
        .navigationBarTitleDisplayMode(.inline)
        .tint(F33Color.accent)
        .refreshable { await store.reload() }
        .task { await store.loadIfNeeded() }
        .confirmationDialog(
            "Retire this Number?",
            isPresented: Binding(get: { confirmingRevoke != nil }, set: { if !$0 { confirmingRevoke = nil } }),
            titleVisibility: .visible,
            presenting: confirmingRevoke
        ) { number in
            Button("Retire \(number.display)", role: .destructive) {
                Task { await store.revoke(number) }
            }
        } message: { _ in
            Text("Anyone who writes to it afterwards is told the same thing as somebody writing to a Number that never existed. Your other Numbers are unaffected.")
        }
        .alert(
            "That didn't go through",
            isPresented: Binding(get: { store.problem != nil }, set: { if !$0 { store.problem = nil } })
        ) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(store.problem ?? "")
        }
    }

    // MARK: The Numbers

    private func numbersSection(_ store: NumbersStore) -> some View {
        Section {
            ForEach(store.page.active) { number in
                row(number, store: store)
            }

            Button {
                Task { await store.mint() }
            } label: {
                HStack(spacing: F33Spacing.sm) {
                    Label("Mint a Number", systemImage: "plus.circle")
                    Spacer(minLength: 0)
                    if store.isBusy(NumbersStore.mintKey) { ProgressView() }
                }
            }
            .disabled(store.isBusy(NumbersStore.mintKey))
        } header: {
            Text("Your Numbers")
        } footer: {
            Text(store.page.active.isEmpty
                 ? "Mint one and give it out. Somebody who has it can write to you without knowing your handle."
                 : "Give one out instead of your handle. Retire it whenever you like — the others keep working.")
        }
    }

    private func row(_ number: ContactNumber, store: NumbersStore) -> some View {
        VStack(alignment: .leading, spacing: F33Spacing.xs) {
            HStack(spacing: F33Spacing.sm) {
                Text(number.display)
                    .font(.system(.title3, design: .monospaced).weight(.semibold))
                    .foregroundStyle(F33Color.ink)
                    .lineLimit(1)
                    .minimumScaleFactor(0.6)

                Spacer(minLength: 0)

                if store.isBusy(number.id) {
                    ProgressView()
                } else {
                    Menu {
                        Picker("Who can use this Number", selection: policyBinding(number, store: store)) {
                            ForEach(store.page.policies, id: \.self) { policy in
                                Text(ContactPolicy.title(policy)).tag(policy)
                            }
                        }
                        Button {
                            copy(number)
                        } label: {
                            Label("Copy", systemImage: "doc.on.doc")
                        }
                        Button(role: .destructive) {
                            confirmingRevoke = number
                        } label: {
                            Label("Retire", systemImage: "xmark.circle")
                        }
                    } label: {
                        Image(systemName: "ellipsis.circle")
                            .font(.system(size: 18))
                            .foregroundStyle(F33Color.ink3)
                            .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget, alignment: .trailing)
                    }
                    .accessibilityLabel("Options for this Number")
                }
            }

            HStack(spacing: F33Spacing.sm) {
                Text(ContactPolicy.title(number.policy))
                    .font(.system(size: 12, weight: .medium))
                    .foregroundStyle(F33Color.accent)
                    .padding(.horizontal, 8)
                    .padding(.vertical, 2)
                    .background(F33Color.accentSoft, in: Capsule())

                if justCopied == number.id {
                    Text("Copied")
                        .font(.system(size: 12))
                        .foregroundStyle(F33Color.ok)
                } else if number.isLegacy {
                    // Older Numbers keep working for ever. This is an offer, not
                    // a warning: nothing about it has stopped being valid.
                    Text("An older format")
                        .font(.system(size: 12))
                        .foregroundStyle(F33Color.ink4)
                }

                Spacer(minLength: 0)
            }
        }
        .padding(.vertical, F33Spacing.xs)
        .contentShape(Rectangle())
        .onTapGesture { copy(number) }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(spoken(number))
        .accessibilityHint("Double tap to copy")
        .accessibilityAddTraits(.isButton)
    }

    /// Read one digit at a time, because that is how a Number is said out loud
    /// and how somebody writing it down needs to hear it.
    private func spoken(_ number: ContactNumber) -> String {
        let digits = number.display.filter { !$0.isWhitespace }.map(String.init).joined(separator: " ")
        return "\(digits). \(ContactPolicy.title(number.policy))."
    }

    // MARK: The account-wide policy

    private func policySection(_ store: NumbersStore) -> some View {
        Section {
            Picker("Who can reach you", selection: contactPolicyBinding(store)) {
                // Built from what the server offers, not from a list in this
                // build: a policy added on the far side appears without a new
                // app, and one removed stops being offered.
                ForEach(store.page.policies, id: \.self) { policy in
                    Text(ContactPolicy.title(policy)).tag(policy)
                }
            }
            .disabled(store.isBusy(NumbersStore.contactPolicyKey))
        } header: {
            Text("Who can reach you")
        } footer: {
            Text(ContactPolicy.explanation(store.page.contactPolicy)
                 + " Anyone with one of your Numbers comes through the policy on that Number instead.")
        }
    }

    // MARK: The knocks

    @ViewBuilder
    private func requestsSection(_ store: NumbersStore) -> some View {
        if !store.requests.isEmpty {
            Section {
                ForEach(store.requests) { request in
                    requestRow(request, store: store)
                }
            } header: {
                Text("Asking to reach you")
            } footer: {
                // True under every policy, which is why it does not name the one
                // that produced the knock — and declining tells them nothing.
                Text("Accepting opens the conversation and releases what they already wrote. Declining tells them nothing.")
            }
        }
    }

    private func requestRow(_ request: ContactRequest, store: NumbersStore) -> some View {
        VStack(alignment: .leading, spacing: F33Spacing.sm) {
            HStack(spacing: F33Spacing.sm) {
                F33Avatar(
                    handle: request.handle,
                    displayName: request.name,
                    avatarURL: request.avatar,
                    size: 36
                )
                VStack(alignment: .leading, spacing: 1) {
                    Text(request.name)
                        .font(.system(size: 15, weight: .semibold))
                        .foregroundStyle(F33Color.ink)
                    Text(RelativeTime.label(for: request.createdAt))
                        .font(.system(size: 12))
                        .foregroundStyle(F33Color.ink4)
                }
                Spacer(minLength: 0)
                if store.isBusy(request.id) { ProgressView() }
            }

            if !request.note.isEmpty {
                // The note is what the decision rests on, so it is shown in
                // full rather than trimmed to a line.
                Text(request.note)
                    .font(.system(size: 14))
                    .foregroundStyle(F33Color.ink2)
                    .fixedSize(horizontal: false, vertical: true)
            }

            HStack(spacing: F33Spacing.sm) {
                Button("Accept") {
                    Task { await store.decide(request, accept: true) }
                }
                .font(.system(size: 14, weight: .semibold))
                .foregroundStyle(F33Color.accentInk)
                .padding(.horizontal, F33Spacing.lg)
                .frame(minHeight: F33Layout.minTouchTarget)
                .background(F33Color.accent, in: Capsule())

                Button("Decline") {
                    Task { await store.decide(request, accept: false) }
                }
                .font(.system(size: 14, weight: .semibold))
                .foregroundStyle(F33Color.ink2)
                .padding(.horizontal, F33Spacing.lg)
                .frame(minHeight: F33Layout.minTouchTarget)
                .background(F33Color.bgSunken, in: Capsule())

                Spacer(minLength: 0)
            }
            .buttonStyle(.plain)
            .disabled(store.isBusy(request.id))
        }
        .padding(.vertical, F33Spacing.xs)
    }

    // MARK: Retired

    @ViewBuilder
    private func retiredSection(_ store: NumbersStore) -> some View {
        if !store.page.revoked.isEmpty {
            Section {
                ForEach(store.page.revoked) { number in
                    HStack(spacing: F33Spacing.sm) {
                        Text(number.display)
                            .font(.system(.body, design: .monospaced))
                            .foregroundStyle(F33Color.ink4)
                            .strikethrough()
                        Spacer(minLength: 0)
                        Text(RelativeTime.label(for: number.createdAt))
                            .font(.system(size: 12))
                            .foregroundStyle(F33Color.ink5)
                    }
                }
            } header: {
                Text("Retired")
            } footer: {
                Text("Kept here so you know where a Number went. Nobody can reach you through these.")
            }
        }
    }

    // MARK: Behaviour

    private func policyBinding(_ number: ContactNumber, store: NumbersStore) -> Binding<String> {
        Binding(
            get: { number.policy },
            set: { policy in Task { await store.setPolicy(policy, on: number) } }
        )
    }

    private func contactPolicyBinding(_ store: NumbersStore) -> Binding<String> {
        Binding(
            get: { store.page.contactPolicy },
            set: { policy in Task { await store.setContactPolicy(policy) } }
        )
    }

    /// The canonical hyphenated form goes on the pasteboard: that is the form
    /// that survives being pasted into a field somewhere else.
    private func copy(_ number: ContactNumber) {
        UIPasteboard.general.string = number.copyable
        withAnimation(F33Motion.easeOut) { justCopied = number.id }
        Task {
            // Long enough to be read, short enough that the row goes back to
            // saying what its policy is, which is the fact that stays true.
            try? await Task.sleep(for: .seconds(2))
            if justCopied == number.id {
                withAnimation(F33Motion.easeOut) { justCopied = nil }
            }
        }
    }
}

#Preview("Numbers") {
    NavigationStack {
        NumbersView()
    }
    .environment(AppModel())
}
