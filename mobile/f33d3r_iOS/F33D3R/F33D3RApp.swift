import SwiftUI
import F33D3RKit

@main
struct F33D3RApp: App {
    @State private var model = AppModel()
    @Environment(\.scenePhase) private var scenePhase

    var body: some Scene {
        WindowGroup {
            RootView()
                .environment(model)
                // Media arrives as server-relative paths. Publishing the origin
                // once here is what lets every image view resolve one without
                // reaching for the client or hardcoding a host.
                .environment(\.mediaOrigin, model.client.baseURL)
                // Post media is walled, so it is fetched with the session
                // attached rather than by AsyncImage, which cannot carry one.
                .environment(\.mediaLoader, model.media)
                .environment(\.mediaClient, model.client)
                .tint(F33Color.accent)
                .task { await model.start() }
                // The user stream is a socket, and a backgrounded app is not
                // allowed to keep one. Closing it deliberately — rather than
                // letting the system tear it down and the reader come back to
                // a connection that died quietly — is what makes the badge,
                // the pill and the live counts right on the first frame after
                // the app returns.
                .onChange(of: scenePhase) { _, phase in
                    switch phase {
                    case .background: model.enterBackground()
                    case .active: model.enterForeground()
                    default: break
                    }
                }
        }
    }
}
