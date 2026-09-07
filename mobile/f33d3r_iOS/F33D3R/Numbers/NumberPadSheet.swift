import SwiftUI
import F33D3RKit

/// Reaching somebody by their F33D3R Number.
///
/// A Number is twelve digits somebody gives out so they can be reached without
/// handing over their handle. It is typed, not pasted — usually read off a card,
/// a screen or somebody's voice — which is why this is a keypad and not a text
/// field. Big keys, grouped digits, and no keyboard covering half the screen.
///
/// The twelfth digit is a check digit, and this screen uses it. Enter stays
/// dark until the twelve digits check out, so a misheard digit costs nothing.
/// That is the *only* thing the check is used for: the moment the server has
/// answered about a well-formed Number, this app knows nothing about why, and
/// it says nothing — an unminted Number, a retired one and somebody who does
/// not want to hear from you are one outcome by design, and a screen that
/// distinguished them would undo that at the last possible layer.
///
/// Then it asks for the message. That order is the server's: what is written is
/// stored before the question of whether it may be delivered is asked, so a
/// message is never lost to a refusal and can be released later if the person
/// allows it.
struct NumberPadSheet: View {
    /// Who the caller was looking at, when they were looking at anybody. Passed
    /// to the server as a hint and shown in the line at the top; the answer does
    /// not change if it is wrong, and there is no handle at all when a Number is
    /// all somebody has.
    var handle: String?
    /// Called when the Number opened a conversation, so the thread behind this
    /// sheet can stop saying it could not reach anyone.
    var onOpened: (Conversation) -> Void = { _ in }

    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    @State private var digits = ""
    @State private var number: F33Number?
    @State private var draft = ""
    @State private var isSending = false
    @State private var outcome: ConversationStart?
    @State private var problem: String?
    @FocusState private var writing: Bool

    var body: some View {
        NavigationStack {
            Group {
                if let outcome {
                    result(outcome)
                } else if number != nil {
                    message
                } else {
                    keypad
                }
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .top)
            .background(F33Color.bg)
            .navigationTitle("Enter F33D3R Number")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button(outcome == nil ? "Cancel" : "Done") { dismiss() }
                }
            }
        }
        .presentationDetents([.large])
    }

    // MARK: The keypad

    private var keypad: some View {
        // Scrolls, because at the largest accessibility sizes four rows of keys
        // and a display are taller than a phone, and a keypad whose bottom row
        // is off screen is not a keypad.
        ScrollView {
            VStack(spacing: F33Spacing.lg) {
                VStack(spacing: F33Spacing.xs) {
                    Text(prompt)
                        .font(.subheadline)
                        .foregroundStyle(F33Color.ink3)
                    if let caveat {
                        Text(caveat)
                            .font(.footnote)
                            .foregroundStyle(F33Color.ink4)
                    }
                }
                .multilineTextAlignment(.center)
                .fixedSize(horizontal: false, vertical: true)

                NumberDisplay(digits: digits)

                NumberKeypad(
                    onDigit: { append($0) },
                    onBackspace: { remove() },
                    canBackspace: !digits.isEmpty
                )

                Button("Enter") { enter() }
                    .buttonStyle(F33PrimaryButtonStyle())
                    .disabled(F33Number(digits) == nil)
                    .accessibilityHint("Available once all twelve digits have been entered")
            }
            .padding(.horizontal, F33Spacing.xl)
            .padding(.vertical, F33Spacing.lg)
        }
    }

    /// The line under the title, in the two shapes this sheet has.
    ///
    /// With a handle it is the thread's own gate: the reader was writing to
    /// somebody named, it did not get through, and this is the other door.
    /// Without one it is a Number and nothing else — which is the ordinary
    /// case, because a Number exists precisely so it can be handed to somebody
    /// who does not know whose it is.
    private var prompt: String {
        if let handle, !handle.isEmpty {
            return "Enter @\(handle)'s F33D3R Number to contact them."
        }
        return "Enter the F33D3R Number you were given."
    }

    /// Said only when there is nobody named, and said before anything is sent.
    ///
    /// There is no reverse lookup on this surface and there is not going to be
    /// one: a screen that turned twelve digits into a person would give away
    /// more than the server's fixed delay is there to protect. So the app says
    /// plainly that it cannot, rather than leaving somebody to conclude it
    /// simply would not.
    private var caveat: String? {
        guard handle?.isEmpty ?? true else { return nil }
        return "Nothing here can tell you whose it is. If they let you through, the conversation will."
    }

    // MARK: The message

    private var message: some View {
        VStack(alignment: .leading, spacing: F33Spacing.lg) {
            VStack(alignment: .leading, spacing: F33Spacing.xs) {
                Text(number?.spoken ?? "")
                    .font(.system(.title3, design: .monospaced).weight(.semibold))
                    .foregroundStyle(F33Color.ink)
                Text("Write something. It is stored either way — if they have to accept a request first, this is what they will see.")
                    .font(.footnote)
                    .foregroundStyle(F33Color.ink4)
                    .fixedSize(horizontal: false, vertical: true)
            }

            TextField("Write something", text: $draft, axis: .vertical)
                .lineLimit(4...10)
                .font(.system(size: 16))
                .foregroundStyle(F33Color.ink)
                .focused($writing)
                .f33Field()

            if let problem {
                Text(problem)
                    .font(.footnote)
                    .foregroundStyle(F33Color.danger)
                    .fixedSize(horizontal: false, vertical: true)
            }

            Button {
                send()
            } label: {
                if isSending {
                    ProgressView().tint(F33Color.accentInk)
                } else {
                    Text("Send")
                }
            }
            .buttonStyle(F33PrimaryButtonStyle())
            .disabled(draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || isSending)

            Button("Change the Number") {
                number = nil
                problem = nil
            }
            .font(.subheadline.weight(.medium))
            .foregroundStyle(F33Color.accent)
            .frame(maxWidth: .infinity, minHeight: F33Layout.minTouchTarget)

            Spacer(minLength: 0)
        }
        .padding(.horizontal, F33Spacing.xl)
        .padding(.top, F33Spacing.lg)
        .onAppear { writing = true }
    }

    // MARK: The answer

    @ViewBuilder
    private func result(_ outcome: ConversationStart) -> some View {
        VStack(alignment: .leading, spacing: F33Spacing.md) {
            Label(outcome.userMessage, systemImage: icon(for: outcome.state))
                .font(.system(size: 15))
                .foregroundStyle(outcome.state == .opened ? F33Color.ink : F33Color.ink2)
                .fixedSize(horizontal: false, vertical: true)

            if outcome.conversation != nil {
                // Where the conversation now is depends on where this sheet was
                // raised from, so the screen underneath is told and this one
                // gets out of the way rather than pushing anything itself.
                Button("Close") { dismiss() }
                    .font(.system(size: 14, weight: .semibold))
                    .foregroundStyle(F33Color.accent)
                    .frame(minHeight: F33Layout.minTouchTarget)
            }

            Spacer(minLength: 0)
        }
        .padding(.horizontal, F33Spacing.xl)
        .padding(.top, F33Spacing.xl)
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func icon(for state: ConversationStart.State) -> String {
        switch state {
        case .opened: return "checkmark.circle"
        case .held: return "clock"
        case .undelivered: return "exclamationmark.circle"
        }
    }

    // MARK: Behaviour

    private func append(_ digit: Character) {
        guard digits.count < F33Number.digitCount else { return }
        digits.append(digit)
    }

    private func remove() {
        guard !digits.isEmpty else { return }
        digits.removeLast()
    }

    private func enter() {
        guard let parsed = F33Number(digits) else { return }
        number = parsed
    }

    private func send() {
        guard let number else { return }
        let text = draft
        isSending = true
        problem = nil
        Task {
            defer { isSending = false }
            do {
                let answer = try await model.client.startConversation(
                    number: number, body: text, handle: handle
                )
                outcome = answer
                if let conversation = answer.conversation {
                    onOpened(conversation)
                    await model.conversationsFeed().reload()
                }
            } catch let error as MessagingError {
                problem = error.userMessage
            } catch {
                problem = "That could not be sent."
            }
        }
    }
}

/// The digits so far, grouped as they fill.
///
/// Grouped rather than run together because the person at the keypad is
/// checking what they typed against something they are reading or hearing, and
/// twelve unbroken digits cannot be checked against anything.
private struct NumberDisplay: View {
    let digits: String

    var body: some View {
        Text(rendered)
            .font(.system(.largeTitle, design: .monospaced).weight(.semibold))
            .lineLimit(1)
            .minimumScaleFactor(0.4)
            .frame(maxWidth: .infinity)
            .padding(.vertical, F33Spacing.lg)
            .background(F33Color.bgSunken, in: RoundedRectangle(cornerRadius: F33Radius.md, style: .continuous))
            .accessibilityElement(children: .ignore)
            .accessibilityLabel(spoken)
            .accessibilityValue("\(digits.count) of \(F33Number.digitCount) digits")
    }

    /// Typed digits in ink, the rest as the shape still to be filled — so the
    /// display says how long a Number is without anyone having to be told.
    private var rendered: AttributedString {
        let typed = Array(digits)
        var out = AttributedString()
        for slot in 0..<F33Number.digitCount {
            if slot == 4 || slot == 8 {
                out += AttributedString(" ")
            }
            var glyph = AttributedString(slot < typed.count ? String(typed[slot]) : "–")
            glyph.foregroundColor = slot < typed.count ? F33Color.ink : F33Color.ink5
            out += glyph
        }
        return out
    }

    /// Read one digit at a time. "Zero zero nine eight" is checkable against a
    /// Number somebody is saying out loud; "ninety-eight" is not.
    private var spoken: String {
        digits.isEmpty ? "No digits entered" : digits.map(String.init).joined(separator: " ")
    }
}

/// Ten keys and a backspace.
///
/// Rows of an `HStack` rather than a grid, and every key carries both of its
/// dimensions: a shape given only one is greedy in the other and inflates
/// whatever holds it, which on a four-row keypad means a sheet taller than the
/// screen. The keys grow with Dynamic Type from a base well over the 44pt
/// minimum, because this is a keypad and a thumb lands on it twelve times.
private struct NumberKeypad: View {
    let onDigit: (Character) -> Void
    let onBackspace: () -> Void
    let canBackspace: Bool

    @ScaledMetric(relativeTo: .title) private var keyHeight: CGFloat = 62

    private static let rows: [[Character]] = [
        ["1", "2", "3"],
        ["4", "5", "6"],
        ["7", "8", "9"],
    ]

    var body: some View {
        VStack(spacing: F33Spacing.sm) {
            ForEach(Self.rows, id: \.self) { row in
                HStack(spacing: F33Spacing.sm) {
                    ForEach(row, id: \.self) { digit in
                        key(digit)
                    }
                }
            }
            HStack(spacing: F33Spacing.sm) {
                // The blank keeps 0 in the middle column, where it is on every
                // keypad anybody has used.
                Color.clear
                    .frame(maxWidth: .infinity)
                    .frame(height: keyHeight)
                    .accessibilityHidden(true)
                key("0")
                backspace
            }
        }
    }

    private func key(_ digit: Character) -> some View {
        Button {
            onDigit(digit)
        } label: {
            Text(String(digit))
                .font(.system(.title, design: .rounded).weight(.medium))
                .foregroundStyle(F33Color.ink)
                .frame(maxWidth: .infinity)
                .frame(height: keyHeight)
                .background(F33Color.bgElevated, in: RoundedRectangle(cornerRadius: F33Radius.md, style: .continuous))
                .overlay(
                    RoundedRectangle(cornerRadius: F33Radius.md, style: .continuous)
                        .strokeBorder(F33Color.hairline, lineWidth: 1)
                )
        }
        .buttonStyle(KeyPressStyle())
        .accessibilityLabel(String(digit))
    }

    private var backspace: some View {
        Button {
            onBackspace()
        } label: {
            Image(systemName: "delete.left")
                .font(.system(size: 22, weight: .medium))
                .foregroundStyle(canBackspace ? F33Color.ink2 : F33Color.ink5)
                .frame(maxWidth: .infinity)
                .frame(height: keyHeight)
                .contentShape(Rectangle())
        }
        .buttonStyle(KeyPressStyle())
        .disabled(!canBackspace)
        .accessibilityLabel("Delete")
    }
}

/// A key presses in, rather than dimming. A dimmed key at the moment of contact
/// reads as one that did not take.
private struct KeyPressStyle: ButtonStyle {
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .scaleEffect(configuration.isPressed && !reduceMotion ? 0.94 : 1)
            .animation(F33Motion.spring, value: configuration.isPressed)
    }
}

#Preview("Number pad") {
    NumberPadSheet(handle: "miiyazuko")
        .environment(AppModel())
}
