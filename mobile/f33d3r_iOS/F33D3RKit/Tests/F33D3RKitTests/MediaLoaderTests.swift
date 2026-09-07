import Foundation
import Testing
@testable import F33D3RKit

/// F33D3R's post media is walled: `/static/media/posts/…` is authorised against
/// the work that owns it, so a request carrying no session is refused. The app
/// therefore attaches one — and the rule that matters is where it does not.
@Suite("Walled media carries the session, and only to us")
struct MediaLoaderTests {
    static let origin = URL(string: "https://192.168.0.228:8443")!

    static func client(token: String? = "tok", base: URL = origin) -> APIClient {
        APIClient(baseURL: base, tokens: StubTokens(token: token), session: MalkuthMockURLProtocol.makeSession())
    }

    /// A media URL may be absolute, because media moves to its own hostname one
    /// day. Until it does, an absolute URL naming somebody else is the one case
    /// that must never receive the token: a bearer credential is the account for
    /// thirty days, and handing it to a host the server merely named would give
    /// it away.
    @Test("The token is never sent to another origin")
    func refusesToCredentialForeignOrigins() async {
        let client = Self.client()
        for foreign in [
            "https://evil.example.com/static/media/posts/x.webp",
            "http://192.168.0.228:8443/static/media/posts/x.webp",   // scheme differs
            "https://192.168.0.228:9000/static/media/posts/x.webp",  // port differs
            "https://192.168.0.229:8443/static/media/posts/x.webp",  // host differs
        ] {
            let credential = await client.mediaCredential(for: URL(string: foreign)!)
            #expect(credential == nil, "credential leaked to \(foreign)")
        }
    }

    @Test("The token is sent to this instance, including its implicit port")
    func credentialsOurOwnOrigin() async {
        let client = Self.client()
        let ours = await client.mediaCredential(for: URL(string: "https://192.168.0.228:8443/static/media/posts/x.webp")!)
        #expect(ours?.token == "tok")
        #expect(ours?.authorizationHeader == "Bearer tok")

        // https://host and https://host:443 are the same origin.
        let implicit = Self.client(base: URL(string: "https://f33d3r.com")!)
        let credential = await implicit.mediaCredential(for: URL(string: "https://f33d3r.com:443/static/media/posts/x.webp")!)
        #expect(credential?.token == "tok")
    }

    @Test("No session, no credential")
    func noTokenNoCredential() async {
        let client = Self.client(token: nil)
        let credential = await client.mediaCredential(for: URL(string: "https://192.168.0.228:8443/static/media/posts/x.webp")!)
        #expect(credential == nil)
    }

    /// `AVPlayer` fetches an HLS playlist and every segment itself and cannot be
    /// given a header through public API, so the session travels as the cookie
    /// the server already reads — the same one the web's `<video>` sends. The
    /// name and path have to match `handler.SessionCookieName` and `/`, or the
    /// segments are forbidden even though the playlist was not.
    @Test("The player's cookie is the session the server reads")
    func buildsTheSessionCookie() throws {
        let cookie = try #require(MediaCredential(token: "tok", origin: Self.origin).sessionCookie())
        #expect(cookie.name == "f33d3r_session")
        #expect(cookie.value == "tok")
        #expect(cookie.path == "/")
        #expect(cookie.domain.contains("192.168.0.228"))
        #expect(cookie.isSecure)
    }

    /// A development instance is reached over plain HTTP by address. A cookie
    /// marked secure would simply not be sent there, and the symptom would be
    /// "still forbidden" rather than anything naming the cause.
    @Test("Over plain HTTP the cookie is not marked secure")
    func plainOriginCookieIsNotSecure() throws {
        let cookie = try #require(
            MediaCredential(token: "tok", origin: URL(string: "http://127.0.0.1:8081")!).sessionCookie()
        )
        #expect(!cookie.isSecure)
        #expect(cookie.name == "f33d3r_session")
    }
}

/// A connection that never happened is diagnosed by where it was aimed, more
/// often than by why it failed.
@Suite("An unreachable server says which one")
struct UnreachableOriginTests {
    @Test("The message names the origin")
    func namesTheOrigin() {
        let error = APIError.unreachable(origin: "http://127.0.0.1:8081", reason: "Could not connect to the server.")
        #expect(error.userMessage.contains("http://127.0.0.1:8081"))
    }

    /// It is still a transport failure, so nothing that branches on the session
    /// or on rate limiting starts treating it as one.
    @Test("It is not mistaken for an expired session")
    func isNotAnAuthFailure() {
        let error = APIError.unreachable(origin: "https://f33d3r.com", reason: "offline")
        #expect(!error.isUnauthenticated)
        #expect(!error.isRateLimited)
        #expect(error.code == nil)
    }
}
