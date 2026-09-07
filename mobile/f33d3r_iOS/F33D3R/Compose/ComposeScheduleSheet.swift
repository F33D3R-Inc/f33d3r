import SwiftUI
import F33D3RKit

/// Choosing when a work goes public.
///
/// A native date picker rather than a port of the web's `datetime-local`
/// input, because the phone already has the control the web input is an
/// imitation of. The earliest time it offers is the composer's floor — see
/// ``WorkComposer/scheduleFloor`` for why there is one at all — and the
/// composer checks the choice again on the way in, because the floor is
/// measured from *now* and the sheet may have been open a while.
///
/// Nothing here is sent. The time becomes `scheduled_at` inside the signed
/// payload when the author taps Schedule on the composer; the server stores
/// the work hidden and a sweep publishes it on the minute.
struct ComposeScheduleSheet: View {
    let composer: WorkComposer

    @Environment(\.dismiss) private var dismiss

    @State private var date: Date
    @State private var problem: String?
    private let earliest: Date

    init(composer: WorkComposer) {
        self.composer = composer
        let earliest = WorkComposer.earliestSchedule()
        self.earliest = earliest
        // A time already chosen is shown as chosen, unless it has since
        // fallen under the floor, in which case the floor is the honest place
        // to start — the composer would refuse the old time anyway.
        _date = State(initialValue: max(composer.scheduledAt ?? earliest, earliest))
    }

    var body: some View {
        NavigationStack {
            VStack(alignment: .leading, spacing: F33Spacing.lg) {
                DatePicker(
                    "Publish at",
                    selection: $date,
                    in: earliest...,
                    displayedComponents: [.date, .hourAndMinute]
                )
                .datePickerStyle(.graphical)
                .tint(F33Color.accent)

                Text("Goes public at \(ScheduleClock.label(date)), your time. The soonest is \(Int(WorkComposer.scheduleFloor / 60)) minutes from now.")
                    .font(.footnote)
                    .foregroundStyle(F33Color.ink3)
                    .fixedSize(horizontal: false, vertical: true)

                if let problem {
                    Text(problem)
                        .font(.footnote)
                        .foregroundStyle(F33Color.danger)
                        .fixedSize(horizontal: false, vertical: true)
                }

                Spacer(minLength: 0)

                Button("Schedule") { set() }
                    .buttonStyle(F33PrimaryButtonStyle())

                if composer.scheduledAt != nil {
                    Button("Remove schedule") {
                        composer.clearSchedule()
                        dismiss()
                    }
                    .font(.subheadline.weight(.semibold))
                    .foregroundStyle(F33Color.danger)
                    .frame(maxWidth: .infinity)
                    .frame(minHeight: F33Layout.minTouchTarget)
                }
            }
            .padding(.horizontal, F33Spacing.xl)
            .padding(.top, F33Spacing.md)
            .padding(.bottom, F33Spacing.lg)
            .background(F33Color.bg)
            .navigationTitle("Schedule")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                        .foregroundStyle(F33Color.ink2)
                }
            }
        }
        .presentationDetents([.large])
        .presentationDragIndicator(.visible)
    }

    private func set() {
        // Refused, not rounded up: the clock moved while this was open and the
        // author should pick again knowing that.
        if composer.schedule(for: date) {
            dismiss()
        } else {
            problem = "That time is under \(Int(WorkComposer.scheduleFloor / 60)) minutes from now. Pick a later one."
        }
    }
}

/// The wording of a scheduled time, wherever the composer shows one.
///
/// Day and short month the way the reader's locale orders them, then a
/// 24-hour clock — the web's chip is `{month:'short', day:'numeric',
/// hour:'2-digit', minute:'2-digit', hour12:false}` and the feed's timestamps
/// pin 24-hour too, so the composer does not introduce a third clock. The
/// year appears only when it is not this one.
enum ScheduleClock {
    static func label(_ date: Date, now: Date = Date()) -> String {
        let sameYear = Calendar.current.isDate(date, equalTo: now, toGranularity: .year)
        return "\((sameYear ? dayMonth : dayMonthYear).string(from: date)) \(clock.string(from: date))"
    }

    private static let dayMonth = fixed("d MMM")
    private static let dayMonthYear = fixed("d MMM yyyy")
    private static let clock: DateFormatter = {
        let f = DateFormatter()
        f.locale = Locale.current
        f.dateFormat = "HH:mm"
        return f
    }()

    private static func fixed(_ template: String) -> DateFormatter {
        let f = DateFormatter()
        f.locale = Locale.current
        f.setLocalizedDateFormatFromTemplate(template)
        return f
    }
}
