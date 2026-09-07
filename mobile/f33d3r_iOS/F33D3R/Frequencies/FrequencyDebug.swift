#if DEBUG
import SwiftUI
import F33D3RKit

/// The two launch pages Frequencies adds, parsed here rather than in
/// ``AppModel``.
///
/// `F33D3R_PAGE=frequencies` roots at the list; `F33D3R_PAGE=frequency` with
/// `F33D3R_FREQUENCY=<id>` roots at one room. The simulator this app is looked
/// at on has no way to tap a pill, so a screen with no launch page is a screen
/// that ships having been compiled and never seen.
///
/// It reads the same environment variable ``AppModel/debugPage`` does and
/// answers first when the value is one of these two, so nothing about the
/// existing table changes.
enum FrequencyDebug: String {
    case frequencies
    case frequency

    static var page: FrequencyDebug? {
        ProcessInfo.processInfo.environment["F33D3R_PAGE"]
            .flatMap { FrequencyDebug(rawValue: $0.lowercased()) }
    }

    static var frequencyID: String? {
        ProcessInfo.processInfo.environment["F33D3R_FREQUENCY"]
    }

    /// `F33D3R_FREQUENCY_START=1` opens the start sheet over the list;
    /// `F33D3R_FREQUENCY_CONTROLS=1` opens the host controls over the room.
    static var opensStartSheet: Bool {
        ProcessInfo.processInfo.environment["F33D3R_FREQUENCY_START"] == "1"
    }

    static var opensHostControls: Bool {
        ProcessInfo.processInfo.environment["F33D3R_FREQUENCY_CONTROLS"] == "1"
    }

    /// `F33D3R_FREQUENCY=<id>` on any page but `frequency` reconnects that room
    /// in the background, which is the only way a headless run can be
    /// photographed with the listening bar on screen.
    static var backgroundRoomID: String? {
        guard page != .frequency else { return nil }
        return frequencyID
    }
}

/// Roots the app at one Frequencies surface, inside the same routed stack the
/// shell would have put around it.
struct FrequencyDebugHost: View {
    let page: FrequencyDebug

    @Environment(AppModel.self) private var model

    var body: some View {
        RoutedStack {
            surface
        }
        .feedDensity(model.preferences.density)
    }

    @ViewBuilder
    private var surface: some View {
        switch page {
        case .frequencies:
            FrequencyListView()
        case .frequency:
            if let id = FrequencyDebug.frequencyID {
                FrequencyRoomView(frequencyID: id)
            } else {
                FrequencyListView()
            }
        }
    }
}
#endif
