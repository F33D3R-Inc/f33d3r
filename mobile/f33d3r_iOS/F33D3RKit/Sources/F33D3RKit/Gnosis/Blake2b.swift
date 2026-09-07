import Foundation

/// BLAKE2b (RFC 7693), unkeyed, with a variable digest length.
///
/// It exists only because ``Argon2`` needs it: Argon2 hashes with BLAKE2b-512 in
/// two places and with a variable-length digest — 1 byte to 1 KiB — in two more,
/// and CryptoKit has no BLAKE2 at all. Nothing else in this package should reach
/// for it; SHA-256 through CryptoKit is the right hash everywhere else, because
/// the platform's implementation is the one that gets the constant-time and
/// side-channel attention.
///
/// Deliberately not a public type. The correctness argument for this file is
/// entirely "Argon2's known-answer vectors pass", and those exercise it at
/// exactly two output lengths and two input shapes. It is not general-purpose
/// crypto, it is a subroutine.
struct Blake2b {

    private static let iv: [UInt64] = [
        0x6a09_e667_f3bc_c908, 0xbb67_ae85_84ca_a73b,
        0x3c6e_f372_fe94_f82b, 0xa54f_f53a_5f1d_36f1,
        0x510e_527f_ade6_82d1, 0x9b05_688c_2b3e_6c1f,
        0x1f83_d9ab_fb41_bd6b, 0x5be0_cd19_137e_2179,
    ]

    /// The message-word schedule. Twelve rounds; rounds 10 and 11 repeat the
    /// first two permutations, which is a property of the spec and not a
    /// copy-paste slip.
    private static let sigma: [[Int]] = [
        [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15],
        [14, 10, 4, 8, 9, 15, 13, 6, 1, 12, 0, 2, 11, 7, 5, 3],
        [11, 8, 12, 0, 5, 2, 15, 13, 10, 14, 3, 6, 7, 1, 9, 4],
        [7, 9, 3, 1, 13, 12, 11, 14, 2, 6, 5, 10, 4, 0, 15, 8],
        [9, 0, 5, 7, 2, 4, 10, 15, 14, 1, 11, 12, 6, 8, 3, 13],
        [2, 12, 6, 10, 0, 11, 8, 3, 4, 13, 7, 5, 15, 14, 1, 9],
        [12, 5, 1, 15, 14, 13, 4, 10, 0, 7, 6, 3, 9, 2, 8, 11],
        [13, 11, 7, 14, 12, 1, 3, 9, 5, 0, 15, 4, 8, 6, 2, 10],
        [6, 15, 14, 9, 11, 3, 0, 8, 12, 2, 13, 7, 1, 4, 10, 5],
        [10, 2, 8, 4, 7, 6, 1, 5, 15, 11, 9, 14, 3, 12, 13, 0],
        [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15],
        [14, 10, 4, 8, 9, 15, 13, 6, 1, 12, 0, 2, 11, 7, 5, 3],
    ]

    private var h: [UInt64]
    private var buffer = [UInt8](repeating: 0, count: 128)
    private var buffered = 0
    private var counterLow: UInt64 = 0
    private var counterHigh: UInt64 = 0
    private let outputLength: Int

    /// - Parameter outputLength: 1...64 bytes. It is mixed into the state, so
    ///   BLAKE2b-32 is not BLAKE2b-64 truncated — which is exactly why Argon2's
    ///   `H'` can chain digests without them colliding.
    init(outputLength: Int) {
        precondition((1...64).contains(outputLength), "BLAKE2b digest must be 1...64 bytes")
        self.outputLength = outputLength
        self.h = Self.iv
        // Parameter block: digest length, key length 0, fanout 1, depth 1.
        self.h[0] ^= 0x0101_0000 ^ UInt64(outputLength)
    }

    mutating func update(_ data: Data) {
        data.withUnsafeBytes { absorb($0) }
    }

    mutating func update(_ bytes: [UInt8]) {
        bytes.withUnsafeBytes { absorb($0) }
    }

    private mutating func absorb(_ input: UnsafeRawBufferPointer) {
        var offset = 0
        while offset < input.count {
            // A full buffer is only compressed once more input is known to
            // exist: the last block is the one that carries the final flag, and
            // it must not be compressed early.
            if buffered == 128 {
                incrementCounter(by: 128)
                compress(final: false)
                buffered = 0
            }
            let take = min(128 - buffered, input.count - offset)
            for i in 0..<take { buffer[buffered + i] = input[offset + i] }
            buffered += take
            offset += take
        }
    }

    mutating func finalize() -> [UInt8] {
        incrementCounter(by: UInt64(buffered))
        for i in buffered..<128 { buffer[i] = 0 }
        compress(final: true)

        var out = [UInt8](repeating: 0, count: outputLength)
        for i in 0..<outputLength {
            out[i] = UInt8(truncatingIfNeeded: h[i / 8] >> (8 * UInt64(i % 8)))
        }
        return out
    }

    private mutating func incrementCounter(by amount: UInt64) {
        let (sum, overflow) = counterLow.addingReportingOverflow(amount)
        counterLow = sum
        if overflow { counterHigh &+= 1 }
    }

    private mutating func compress(final: Bool) {
        var m = [UInt64](repeating: 0, count: 16)
        for i in 0..<16 { m[i] = loadLE64(buffer, at: i * 8) }

        var v = [UInt64](repeating: 0, count: 16)
        for i in 0..<8 { v[i] = h[i] }
        for i in 0..<8 { v[8 + i] = Self.iv[i] }
        v[12] ^= counterLow
        v[13] ^= counterHigh
        if final { v[14] = ~v[14] }

        for round in 0..<12 {
            let s = Self.sigma[round]
            mix(&v, 0, 4, 8, 12, m[s[0]], m[s[1]])
            mix(&v, 1, 5, 9, 13, m[s[2]], m[s[3]])
            mix(&v, 2, 6, 10, 14, m[s[4]], m[s[5]])
            mix(&v, 3, 7, 11, 15, m[s[6]], m[s[7]])
            mix(&v, 0, 5, 10, 15, m[s[8]], m[s[9]])
            mix(&v, 1, 6, 11, 12, m[s[10]], m[s[11]])
            mix(&v, 2, 7, 8, 13, m[s[12]], m[s[13]])
            mix(&v, 3, 4, 9, 14, m[s[14]], m[s[15]])
        }

        for i in 0..<8 { h[i] ^= v[i] ^ v[i + 8] }
    }

    @inline(__always)
    private func mix(
        _ v: inout [UInt64], _ a: Int, _ b: Int, _ c: Int, _ d: Int, _ x: UInt64, _ y: UInt64
    ) {
        v[a] = v[a] &+ v[b] &+ x
        v[d] = (v[d] ^ v[a]).rotated(right: 32)
        v[c] = v[c] &+ v[d]
        v[b] = (v[b] ^ v[c]).rotated(right: 24)
        v[a] = v[a] &+ v[b] &+ y
        v[d] = (v[d] ^ v[a]).rotated(right: 16)
        v[c] = v[c] &+ v[d]
        v[b] = (v[b] ^ v[c]).rotated(right: 63)
    }

    /// One-shot, for the places that hash a single buffer.
    static func hash(_ input: [UInt8], outputLength: Int) -> [UInt8] {
        var h = Blake2b(outputLength: outputLength)
        h.update(input)
        return h.finalize()
    }
}
