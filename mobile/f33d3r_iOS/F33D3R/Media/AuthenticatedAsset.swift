import AVFoundation
import F33D3RKit

/// Builds an `AVURLAsset` that carries the reader's session.
///
/// F33D3R's post media is walled: `/static/media/posts/…` is checked against the
/// work that owns it, and a request naming nobody is refused with `403`. That is
/// every frame of a work's video, because HLS fetches the playlist and then each
/// segment as ordinary HTTP requests, and `AVPlayer(url:)` sends no credential
/// on any of them.
///
/// The session travels as a cookie rather than a header for one reason:
/// `AVURLAssetHTTPCookiesKey` is public API and the header equivalent is not.
/// The server reads the same session either way — `f33d3r_session` is what the
/// web's own `<video>` sends — so this is the web's mechanism, not a workaround.
///
/// The cookie is handed to the asset and to nothing else. It is never put in
/// `HTTPCookieStorage`, which would write a thirty-day bearer credential to a
/// plist in the app container.
enum AuthenticatedAsset {
    static func make(url: URL, credential: MediaCredential?) -> AVURLAsset {
        var options: [String: Any] = [:]
        if let cookie = credential?.sessionCookie() {
            options[AVURLAssetHTTPCookiesKey] = [cookie]
        }
        return AVURLAsset(url: url, options: options)
    }

    /// The same asset, wrapped for a player.
    static func playerItem(url: URL, credential: MediaCredential?) -> AVPlayerItem {
        AVPlayerItem(asset: make(url: url, credential: credential))
    }
}
