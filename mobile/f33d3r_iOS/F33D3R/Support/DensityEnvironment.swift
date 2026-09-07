import SwiftUI
import F33D3RKit

/// The card density in force for whatever is being drawn.
///
/// In the environment rather than a parameter because density has to reach the
/// card, its media, its body and its action row, and threading it through every
/// initialiser would put a reading preference in the signature of half the app.
/// It is also exactly what the environment is for: an ambient value a subtree
/// reads and nobody owns.
private struct FeedDensityKey: EnvironmentKey {
    static let defaultValue: FeedDensity = .default
}

extension EnvironmentValues {
    var feedDensity: FeedDensity {
        get { self[FeedDensityKey.self] }
        set { self[FeedDensityKey.self] = newValue }
    }
}

extension View {
    /// The resolved geometry for the density in force, so a view asks for
    /// numbers rather than branching on the enum itself.
    func feedDensity(_ density: FeedDensity) -> some View {
        environment(\.feedDensity, density)
    }
}
