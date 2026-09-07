import Foundation
import Testing
@testable import F33D3RKit

/// Intercepts requests so the client can be exercised against scripted responses
/// without a server.
final class MockURLProtocol: URLProtocol, @unchecked Sendable {
    /// Set before each test. Receives the request, returns status/body.
    nonisolated(unsafe) static var handler: (@Sendable (URLRequest) -> (Int, Data))?
    /// Every request that reached the transport, so tests can assert on headers.
    nonisolated(unsafe) static var recorded: [URLRequest] = []

    static func reset() {
        handler = nil
        recorded = []
    }

    static func makeSession() -> URLSession {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [MockURLProtocol.self]
        return URLSession(configuration: config)
    }

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        Self.recorded.append(request)
        let (status, body) = Self.handler?(request) ?? (500, Data())
        let response = HTTPURLResponse(
            url: request.url!, statusCode: status,
            httpVersion: "HTTP/1.1", headerFields: ["Content-Type": "application/json"]
        )!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: body)
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}
}

/// A token provider with no Keychain, so tests can drive auth state directly.
actor StubTokens: TokenProviding {
    private var token: String?
    private(set) var rejectionCount = 0

    init(token: String?) { self.token = token }

    func currentToken() async -> String? { token }
    func tokenRejected() async {
        rejectionCount += 1
        token = nil
    }
}

@Suite(.serialized)
struct APIClientTests {

    static func makeClient(token: String? = "tok") -> (APIClient, StubTokens) {
        MockURLProtocol.reset()
        let tokens = StubTokens(token: token)
        let client = APIClient(
            baseURL: URL(string: "https://f33d3r.com")!,
            tokens: tokens,
            session: MockURLProtocol.makeSession()
        )
        return (client, tokens)
    }

    @Test("Requests resolve under /api/v1 and carry the bearer token")
    func buildsAuthenticatedRequest() async throws {
        let (client, _) = Self.makeClient()
        MockURLProtocol.handler = { _ in
            (200, try! JSONEncoder().encode(["ok": true]))
        }

        _ = try? await client.send(.me, body: Optional<Never>.none, as: [String: Bool].self)

        let request = try #require(MockURLProtocol.recorded.first)
        #expect(request.url?.path == "/api/v1/me")
        #expect(request.value(forHTTPHeaderField: "Authorization") == "Bearer tok")
        #expect(request.value(forHTTPHeaderField: "Accept") == "application/json")
    }

    /// Login is the one endpoint that must work without a token; sending an
    /// Authorization header there would be meaningless at best.
    @Test("Unauthenticated endpoints send no Authorization header")
    func loginSendsNoAuthHeader() async throws {
        let (client, _) = Self.makeClient(token: nil)
        MockURLProtocol.handler = { _ in (401, Data()) }

        _ = try? await client.login(.init(handle: "dev", password: "pw", deviceName: "Test"))

        let request = try #require(MockURLProtocol.recorded.first)
        #expect(request.url?.path == "/api/v1/auth/login")
        #expect(request.httpMethod == "POST")
        #expect(request.value(forHTTPHeaderField: "Authorization") == nil)
    }

    /// The server wraps errors as {"error": {...}}. Decoding that into a typed
    /// code is what lets the UI distinguish "wrong password" from "suspended".
    @Test("The server's error envelope becomes a typed APIError")
    func decodesErrorEnvelope() async throws {
        let (client, _) = Self.makeClient(token: nil)
        MockURLProtocol.handler = { _ in
            (401, Data(#"{"error":{"code":"invalid_credentials","message":"Invalid handle or password."}}"#.utf8))
        }

        await #expect(throws: APIError.self) {
            _ = try await client.login(.init(handle: "dev", password: "wrong", deviceName: "Test"))
        }

        do {
            _ = try await client.login(.init(handle: "dev", password: "wrong", deviceName: "Test"))
            Issue.record("expected a throw")
        } catch let error as APIError {
            guard case .api(let status, let body) = error else {
                Issue.record("wrong case: \(error)"); return
            }
            #expect(status == 401)
            #expect(body.code == "invalid_credentials")
            #expect(error.userMessage == "Invalid handle or password.")
            #expect(error.isUnauthenticated)
        }
    }

    /// A 401 must tell the token's owner, so a revoked session clears itself
    /// once rather than failing every subsequent request in the same way.
    @Test("A 401 notifies the token provider exactly once")
    func notifiesOnUnauthorized() async throws {
        let (client, tokens) = Self.makeClient()
        MockURLProtocol.handler = { _ in (401, Data()) }

        _ = try? await client.me()

        #expect(await tokens.rejectionCount == 1)
        #expect(await tokens.currentToken() == nil)
    }

    /// A 403 is a real answer about this account, not a broken session — it must
    /// not clear the token.
    @Test("Non-401 failures leave the session alone")
    func doesNotClearOnForbidden() async throws {
        let (client, tokens) = Self.makeClient()
        MockURLProtocol.handler = { _ in
            (403, Data(#"{"error":{"code":"account_suspended","message":"Suspended."}}"#.utf8))
        }

        _ = try? await client.me()

        #expect(await tokens.rejectionCount == 0)
        #expect(await tokens.currentToken() == "tok")
    }

    @Test("Rate limiting is recognisable so the UI can back off rather than retry")
    func recognisesRateLimit() async throws {
        let (client, _) = Self.makeClient()
        MockURLProtocol.handler = { _ in (429, Data()) }

        do {
            _ = try await client.me()
            Issue.record("expected a throw")
        } catch let error as APIError {
            #expect(error.isRateLimited)
            #expect(!error.isUnauthenticated)
        }
    }

    /// Logout answers 204 with no body; decoding that as a value would fail.
    @Test("A 204 with no body is a success, not a decode failure")
    func handlesNoContent() async throws {
        let (client, _) = Self.makeClient()
        MockURLProtocol.handler = { _ in (204, Data()) }

        try await client.logout()
    }

    /// An HTML error page from a proxy must not surface as a confusing decode
    /// error — the status is the useful signal.
    @Test("A non-JSON error body still yields a usable error")
    func handlesNonJSONErrorBody() async throws {
        let (client, _) = Self.makeClient()
        MockURLProtocol.handler = { _ in (502, Data("<html>bad gateway</html>".utf8)) }

        do {
            _ = try await client.me()
            Issue.record("expected a throw")
        } catch let error as APIError {
            #expect(error == .unexpectedStatus(502))
            #expect(error.userMessage.contains("trouble"))
        }
    }

    /// Calling an authenticated endpoint with no token must fail locally rather
    /// than spending a request and a rate-limit slot to be told 401.
    @Test("An authenticated call with no token fails without hitting the network")
    func failsFastWithoutToken() async throws {
        let (client, _) = Self.makeClient(token: nil)
        MockURLProtocol.handler = { _ in (200, Data("{}".utf8)) }

        do {
            _ = try await client.me()
            Issue.record("expected a throw")
        } catch let error as APIError {
            #expect(error.isUnauthenticated)
        }
        #expect(MockURLProtocol.recorded.isEmpty)
    }
}
