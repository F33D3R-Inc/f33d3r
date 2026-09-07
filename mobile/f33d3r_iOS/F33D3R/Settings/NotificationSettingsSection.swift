import SwiftUI
import F33D3RKit

/// What Herald is allowed to send, and when it has to stay quiet.
///
/// Every switch posts the one key it changed and then draws the whole set the
/// server answers with. Posting all twelve back would let a stale read undo a
/// change made on another device between the read and the write; drawing the
/// answer rather than the finger's position means a preference the server
/// refused settles back where the server put it.
struct NotificationSettingsSection: View {
    @Binding var error: String?

    @Environment(AppModel.self) private var model

    @State private var prefs: NotificationPrefs?
    @State private var unavailable = false
    @State private var busy: Set<String> = []

    var body: some View {
        Section {
            if let prefs {
                ForEach(NotificationPrefs.Toggle.allCases) { toggle in
                    Toggle(toggle.title, isOn: Binding(
                        get: { prefs[toggle] },
                        set: { on in
                            Task { await apply(toggle.rawValue, .toggle(toggle, on)) }
                        }
                    ))
                    .disabled(busy.contains(toggle.rawValue))
                }

                Toggle("Quiet hours", isOn: Binding(
                    get: { prefs.quietHoursEnabled },
                    set: { on in
                        Task { await apply(NotificationPrefs.quietHoursKey, .quietHours(enabled: on)) }
                    }
                ))
                .disabled(busy.contains(NotificationPrefs.quietHoursKey))

                if prefs.quietHoursEnabled {
                    timePicker("From", time: prefs.quietHoursStart ?? "22:00", isStart: true)
                    timePicker("Until", time: prefs.quietHoursEnd ?? "07:00", isStart: false)
                }
            } else if unavailable {
                Text("This F33D3R doesn't serve notification preferences yet.")
                    .font(.subheadline)
                    .foregroundStyle(F33Color.ink4)
            } else {
                HStack(spacing: F33Spacing.md) {
                    ProgressView()
                    Text("Loading preferences…")
                        .foregroundStyle(F33Color.ink4)
                }
            }
        } header: {
            Text("Notifications")
        } footer: {
            Text("Quiet hours hold everything back until the window closes; nothing is lost, it just waits.")
        }
        .task { await load() }
    }

    /// A wheel over `HH:MM`. The value on the wire is a wall-clock time in the
    /// account's own day, not an instant, so it is carried as those five
    /// characters and only turned into a `Date` for as long as the picker needs
    /// one — a timestamp here would drift by a timezone every time it crossed
    /// the wire.
    private func timePicker(_ title: String, time: String, isStart: Bool) -> some View {
        DatePicker(
            title,
            selection: Binding(
                get: { Self.date(from: time) },
                set: { newDate in
                    guard let prefs else { return }
                    let stamp = Self.string(from: newDate)
                    let start = isStart ? stamp : (prefs.quietHoursStart ?? "22:00")
                    let end = isStart ? (prefs.quietHoursEnd ?? "07:00") : stamp
                    Task { await apply("quiet_hours_window", .quietHours(start: start, end: end)) }
                }
            ),
            displayedComponents: .hourAndMinute
        )
        .disabled(busy.contains("quiet_hours_window"))
    }

    private func load() async {
        do {
            prefs = try await model.notificationPreferences()
            unavailable = false
        } catch let error as APIError {
            if error.isUnservedSurface {
                unavailable = true
            } else {
                self.error = error.userMessage
                unavailable = true
            }
        } catch {
            self.error = error.localizedDescription
            unavailable = true
        }
    }

    private func apply(_ key: String, _ change: NotificationPrefsChange) async {
        busy.insert(key)
        defer { busy.remove(key) }
        do {
            prefs = try await model.setNotificationPrefs(change)
        } catch let error as MalkuthError {
            self.error = error.description
            await load()
        } catch let error as APIError {
            self.error = error.userMessage
            await load()
        } catch {
            self.error = error.localizedDescription
            await load()
        }
    }

    // MARK: HH:MM

    private static func date(from time: String) -> Date {
        let parts = time.split(separator: ":").compactMap { Int($0) }
        var components = DateComponents()
        components.hour = parts.count == 2 ? parts[0] : 22
        components.minute = parts.count == 2 ? parts[1] : 0
        return Calendar.current.date(from: components) ?? Date()
    }

    private static func string(from date: Date) -> String {
        let c = Calendar.current.dateComponents([.hour, .minute], from: date)
        return String(format: "%02d:%02d", c.hour ?? 0, c.minute ?? 0)
    }
}
