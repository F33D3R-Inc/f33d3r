import SwiftUI
import F33D3RKit

/// The origin server-relative media paths resolve against.
///
/// Media arrives from the API as `/media/...` because the web app is served from
/// the same origin as Caeor's Caddy route and has a document base to resolve
/// against. A native client has none, so the origin is put in the environment
/// once at the root and every image view resolves against it — rather than each
/// call site reaching for the client, or worse, hardcoding a host.
private struct MediaOriginKey: EnvironmentKey {
    static let defaultValue: URL? = nil
}

/// The loader every media view fetches through.
///
/// It exists because F33D3R's media is walled: a post's images and its HLS
/// playlist are checked against the work that owns them, so a request that
/// carries no session is refused. `AsyncImage` has no way to carry one. The
/// default is the anonymous loader, which is right for previews and for the
/// avatars that no work owns.
private struct MediaLoaderKey: EnvironmentKey {
    static let defaultValue: MediaLoader = .unauthenticated
}

/// The client, for the one case a loader cannot serve: `AVPlayer` fetches its
/// own playlist and segments, so it needs the credential itself rather than
/// bytes somebody else fetched.
private struct MediaClientKey: EnvironmentKey {
    static let defaultValue: APIClient? = nil
}

extension EnvironmentValues {
    var mediaOrigin: URL? {
        get { self[MediaOriginKey.self] }
        set { self[MediaOriginKey.self] = newValue }
    }

    var mediaLoader: MediaLoader {
        get { self[MediaLoaderKey.self] }
        set { self[MediaLoaderKey.self] = newValue }
    }

    var mediaClient: APIClient? {
        get { self[MediaClientKey.self] }
        set { self[MediaClientKey.self] = newValue }
    }
}

extension URL {
    /// Resolves a server-supplied media path. Absolute URLs pass through
    /// untouched, which is what happens once media moves to its own hostname.
    static func media(_ path: String?, origin: URL?) -> URL? {
        guard let path, !path.isEmpty else { return nil }
        if let absolute = URL(string: path), absolute.scheme != nil { return absolute }
        guard let origin else { return nil }
        return URL(string: path, relativeTo: origin)?.absoluteURL
    }
}
