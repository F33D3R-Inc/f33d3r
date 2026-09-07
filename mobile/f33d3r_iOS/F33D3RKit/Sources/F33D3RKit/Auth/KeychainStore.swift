import Foundation
import Security

/// Minimal Keychain wrapper for the session token.
///
/// The token is a bearer credential — anyone holding it is the user for 30 days
/// — so it belongs in the Keychain and nowhere else. Not UserDefaults, not a
/// file, not a log line.
///
/// Accessibility is `afterFirstUnlock`: the app needs to refresh a feed from a
/// background push before the user has unlocked the device that session, but the
/// token should still be unreadable while the device has never been unlocked.
/// `ThisDeviceOnly` keeps it out of iCloud Keychain backups, so a restored
/// backup cannot resurrect a session on another device.
public struct KeychainStore: Sendable {
    public enum Failure: Error, Equatable {
        case unexpectedStatus(OSStatus)
        case malformedData
    }

    private let service: String
    private let account: String

    public init(service: String = "com.f33d3r.ios", account: String = "session-token") {
        self.service = service
        self.account = account
    }

    private var baseQuery: [String: Any] {
        [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
        ]
    }

    /// Stores `value`, replacing any existing entry.
    public func set(_ value: String) throws {
        guard let data = value.data(using: .utf8) else { throw Failure.malformedData }

        // Delete-then-add rather than SecItemUpdate: it is one code path for both
        // "first login" and "token replaced", and it guarantees the accessibility
        // attribute is reapplied rather than inherited from the old item.
        SecItemDelete(baseQuery as CFDictionary)

        var query = baseQuery
        query[kSecValueData as String] = data
        query[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly

        let status = SecItemAdd(query as CFDictionary, nil)
        guard status == errSecSuccess else { throw Failure.unexpectedStatus(status) }
    }

    /// Returns the stored value, or nil when there is none.
    public func get() throws -> String? {
        var query = baseQuery
        query[kSecReturnData as String] = true
        query[kSecMatchLimit as String] = kSecMatchLimitOne

        var item: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &item)
        if status == errSecItemNotFound { return nil }
        guard status == errSecSuccess else { throw Failure.unexpectedStatus(status) }
        guard let data = item as? Data, let value = String(data: data, encoding: .utf8) else {
            throw Failure.malformedData
        }
        return value
    }

    /// Removes the stored value. Succeeds when there was nothing to remove.
    public func delete() throws {
        let status = SecItemDelete(baseQuery as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else {
            throw Failure.unexpectedStatus(status)
        }
    }
}
