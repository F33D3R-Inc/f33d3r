import Foundation

/// Argon2 (RFC 9106), written out because there is nothing to reach for.
///
/// CryptoKit has no password hash, and this package deliberately has no
/// third-party dependencies — see `Package.swift`. So the one primitive the
/// sealed-mode vault depends on that Apple does not ship is here, in full.
///
/// ## What it has to agree with
///
/// `sealcore`'s `derive_backup_key` calls `Argon2::default()` from the Rust
/// `argon2` crate, pinned at 0.5.3 in `sealcore/Cargo.lock`. That default is not
/// a guess: `Algorithm::default()` is `Argon2id` (the `#[default]` attribute is
/// on that variant), `Version::default()` is `V0x13`, and `Params::DEFAULT` is
/// `m_cost: 19 * 1024`, `t_cost: 2`, `p_cost: 1`, no secret, no associated
/// data. `hash_password_into` takes the tag length from the caller's buffer,
/// which in `derive_backup_key_impl` is `[0u8; 32]`. Those are
/// ``Parameters/sealcore``.
///
/// Getting any one of them wrong produces a key that is a perfectly good
/// 32 bytes and unwraps nothing, with no error that says why — which is why the
/// tests pin the crate's own known-answer vectors rather than only a round trip
/// through this file.
///
/// ## Why the whole algorithm and not just the one parameter set
///
/// The RFC 9106 §5 vectors use a secret and associated data, and the reference
/// vectors use `p = 2` and Argon2i and version 0x10. None of those are on the
/// path this app takes, and all of them are the only published answers anyone
/// else has computed. An implementation that supports only the shape it needs
/// can only be tested against itself.
///
/// ## Cost
///
/// A ``Parameters/sealcore`` derivation touches 19 MiB and runs 38,912 block
/// compressions. Measured timings are in `Argon2Tests`. It is far too slow for
/// anything but a login-shaped moment, and it is meant to be: that is the
/// property being bought.
public enum Argon2 {

    // MARK: Parameters

    /// Which of the three Argon2 flavours. The raw value is the `y` that goes
    /// into the initial hash, so it is part of the wire contract, not a label.
    public enum Variant: UInt32, Sendable, Equatable {
        case d = 0
        case i = 1
        case id = 2
    }

    /// Version 0x13 is what everything current produces. 0x10 differs in one
    /// place — it overwrites a block where 0x13 XORs into it — and exists here
    /// only because the reference test vectors cover it, and a vector this code
    /// cannot express is a vector that cannot check it.
    public enum Version: UInt32, Sendable, Equatable {
        case v0x10 = 0x10
        case v0x13 = 0x13
    }

    public struct Parameters: Sendable, Equatable {
        public var variant: Variant
        public var version: Version
        /// `t`: passes over memory.
        public var timeCost: UInt32
        /// `m`: memory in KiB, one KiB per block.
        public var memoryKiB: UInt32
        /// `p`: lanes.
        public var parallelism: UInt32
        /// `T`: tag length in bytes.
        public var tagLength: Int

        public init(
            variant: Variant = .id,
            version: Version = .v0x13,
            timeCost: UInt32,
            memoryKiB: UInt32,
            parallelism: UInt32,
            tagLength: Int
        ) {
            self.variant = variant
            self.version = version
            self.timeCost = timeCost
            self.memoryKiB = memoryKiB
            self.parallelism = parallelism
            self.tagLength = tagLength
        }

        /// `Argon2::default()` in argon2 0.5.3, which is what the browser
        /// derives the vault key with. Changing any field here silently locks
        /// every existing account out of its own private key.
        public static let sealcore = Parameters(
            variant: .id,
            version: .v0x13,
            timeCost: 2,
            memoryKiB: 19 * 1024,
            parallelism: 1,
            tagLength: 32
        )
    }

    // MARK: Entry point

    /// Derives a tag from `password` and `salt`.
    ///
    /// `secret` and `associatedData` are empty on every path this app uses;
    /// they are here because RFC 9106's own vectors use them, and a test vector
    /// that cannot be expressed is a test vector that proves nothing.
    public static func hash(
        password: Data,
        salt: Data,
        secret: Data = Data(),
        associatedData: Data = Data(),
        parameters: Parameters = .sealcore
    ) throws(Argon2Error) -> Data {
        // The same bounds the Rust crate enforces, so that an input it would
        // reject does not quietly produce a tag here.
        guard salt.count >= 8 else { throw .saltTooShort(salt.count) }
        guard parameters.tagLength >= 4 else { throw .tagTooShort(parameters.tagLength) }
        guard parameters.parallelism >= 1 else { throw .invalidParallelism }
        guard parameters.timeCost >= 1 else { throw .invalidTimeCost }
        let lanes = Int(parameters.parallelism)
        guard parameters.memoryKiB >= 8 * parameters.parallelism else {
            throw .memoryTooSmall(parameters.memoryKiB)
        }

        // m' = 4 * p * floor(m / 4p): memory rounded down to a whole number of
        // segments, four per lane per pass.
        let blockCount = 4 * lanes * (Int(parameters.memoryKiB) / (4 * lanes))
        let laneLength = blockCount / lanes
        let segmentLength = laneLength / 4

        let h0 = initialHash(
            password: password, salt: salt, secret: secret,
            associatedData: associatedData, parameters: parameters
        )

        // One flat allocation of 128-bit-aligned words rather than an array of
        // arrays: this is 19 MiB on the sealcore parameters, and every access is
        // a random index into it.
        let memory = UnsafeMutablePointer<UInt64>.allocate(capacity: blockCount * 128)
        memory.initialize(repeating: 0, count: blockCount * 128)
        defer {
            // Wipe before returning it. The blocks are password-derived and this
            // is the only copy of them that outlives the call.
            memory.update(repeating: 0, count: blockCount * 128)
            memory.deallocate()
        }

        fillFirstBlocks(memory, h0: h0, lanes: lanes, laneLength: laneLength)
        fillMemory(
            memory, parameters: parameters, lanes: lanes,
            blockCount: blockCount, laneLength: laneLength, segmentLength: segmentLength
        )

        // C = XOR of the last block of every lane; the tag is H'(C).
        var final = [UInt64](repeating: 0, count: 128)
        for lane in 0..<lanes {
            let block = memory + (lane * laneLength + laneLength - 1) * 128
            for word in 0..<128 { final[word] ^= block[word] }
        }
        var finalBytes = [UInt8](repeating: 0, count: 1024)
        for word in 0..<128 { storeLE64(final[word], into: &finalBytes, at: word * 8) }
        let tag = variableLengthHash(outputLength: parameters.tagLength, finalBytes)
        return Data(tag)
    }

    // MARK: H0

    /// The pre-hashing digest, in exactly the field order `initial_hash` in the
    /// Rust crate feeds BLAKE2b-512. Every length is a little-endian u32, and
    /// an absent secret contributes a zero length and no bytes — not nothing.
    static func initialHash(
        password: Data, salt: Data, secret: Data, associatedData: Data, parameters: Parameters
    ) -> [UInt8] {
        var h = Blake2b(outputLength: 64)
        h.update(le32(parameters.parallelism))
        h.update(le32(UInt32(parameters.tagLength)))
        h.update(le32(parameters.memoryKiB))
        h.update(le32(parameters.timeCost))
        h.update(le32(parameters.version.rawValue))
        h.update(le32(parameters.variant.rawValue))
        h.update(le32(UInt32(password.count)))
        h.update(password)
        h.update(le32(UInt32(salt.count)))
        h.update(salt)
        h.update(le32(UInt32(secret.count)))
        h.update(secret)
        h.update(le32(UInt32(associatedData.count)))
        h.update(associatedData)
        return h.finalize()
    }

    /// H': BLAKE2b when the output fits in one digest, and a chain of 32-byte
    /// halves when it does not — which is every 1024-byte block.
    static func variableLengthHash(outputLength: Int, _ input: [UInt8]) -> [UInt8] {
        let lengthPrefix = le32(UInt32(outputLength))
        if outputLength <= 64 {
            var h = Blake2b(outputLength: outputLength)
            h.update(lengthPrefix)
            h.update(input)
            return h.finalize()
        }

        var out = [UInt8]()
        out.reserveCapacity(outputLength)
        var h = Blake2b(outputLength: 64)
        h.update(lengthPrefix)
        h.update(input)
        var last = h.finalize()
        out.append(contentsOf: last[0..<32])

        // r = ceil(T/32) - 2 full 32-byte halves, then one final digest of
        // whatever is left, which is always between 1 and 64 bytes.
        let r = (outputLength + 31) / 32 - 2
        if r > 1 {
            for _ in 1..<r {
                var next = Blake2b(outputLength: 64)
                next.update(last)
                last = next.finalize()
                out.append(contentsOf: last[0..<32])
            }
        }
        var tail = Blake2b(outputLength: outputLength - 32 * r)
        tail.update(last)
        out.append(contentsOf: tail.finalize())
        return out
    }

    // MARK: Memory

    private static func fillFirstBlocks(
        _ memory: UnsafeMutablePointer<UInt64>, h0: [UInt8], lanes: Int, laneLength: Int
    ) {
        for lane in 0..<lanes {
            for column in 0..<2 {
                var input = h0
                input.append(contentsOf: le32(UInt32(column)))
                input.append(contentsOf: le32(UInt32(lane)))
                let bytes = variableLengthHash(outputLength: 1024, input)
                let block = memory + (lane * laneLength + column) * 128
                for word in 0..<128 { block[word] = loadLE64(bytes, at: word * 8) }
            }
        }
    }

    private static func fillMemory(
        _ memory: UnsafeMutablePointer<UInt64>,
        parameters: Parameters, lanes: Int,
        blockCount: Int, laneLength: Int, segmentLength: Int
    ) {
        // Scratch reused across every compression: R, Z, and the 16 words a
        // column-wise permutation has to be gathered into. Allocating these per
        // block would dominate the run time in a debug build.
        let scratch = UnsafeMutablePointer<UInt64>.allocate(capacity: 128 + 128 + 16)
        scratch.initialize(repeating: 0, count: 128 + 128 + 16)
        defer { scratch.deallocate() }
        let r = scratch, z = scratch + 128, gather = scratch + 256

        // Data-independent addressing needs three more blocks of its own.
        let addressing = UnsafeMutablePointer<UInt64>.allocate(capacity: 128 * 3)
        addressing.initialize(repeating: 0, count: 128 * 3)
        defer { addressing.deallocate() }
        let addressBlock = addressing, inputBlock = addressing + 128, zeroBlock = addressing + 256

        for pass in 0..<Int(parameters.timeCost) {
            for slice in 0..<4 {
                for lane in 0..<lanes {
                    // Argon2id is Argon2i for the first half of the first pass
                    // and Argon2d after that. This line is the whole hybrid.
                    let dataIndependent: Bool
                    switch parameters.variant {
                    case .i: dataIndependent = true
                    case .d: dataIndependent = false
                    case .id: dataIndependent = (pass == 0 && slice < 2)
                    }

                    if dataIndependent {
                        for word in 0..<128 { inputBlock[word] = 0 }
                        inputBlock[0] = UInt64(pass)
                        inputBlock[1] = UInt64(lane)
                        inputBlock[2] = UInt64(slice)
                        inputBlock[3] = UInt64(blockCount)
                        inputBlock[4] = UInt64(parameters.timeCost)
                        inputBlock[5] = UInt64(parameters.variant.rawValue)
                    }

                    // The first two blocks of every lane are H0-derived, so the
                    // very first segment starts at column 2 — and still burns
                    // the first address block, because the address stream is
                    // indexed by position in the segment, not by how many
                    // blocks have been written.
                    var startingIndex = 0
                    if pass == 0 && slice == 0 {
                        startingIndex = 2
                        if dataIndependent {
                            nextAddresses(
                                addressBlock: addressBlock, inputBlock: inputBlock,
                                zeroBlock: zeroBlock, r: r, z: z, gather: gather
                            )
                        }
                    }

                    var current = lane * laneLength + slice * segmentLength + startingIndex
                    var previous = current % laneLength == 0 ? current + laneLength - 1 : current - 1

                    for index in startingIndex..<segmentLength {
                        // Wrapped around to the top of a lane: the previous
                        // block is the one before this in the lane, not the one
                        // before it in memory.
                        if current % laneLength == 1 { previous = current - 1 }

                        let pseudoRandom: UInt64
                        if dataIndependent {
                            if index % 128 == 0 {
                                nextAddresses(
                                    addressBlock: addressBlock, inputBlock: inputBlock,
                                    zeroBlock: zeroBlock, r: r, z: z, gather: gather
                                )
                            }
                            pseudoRandom = addressBlock[index % 128]
                        } else {
                            pseudoRandom = memory[previous * 128]
                        }

                        // Nothing but the current lane exists yet in the first
                        // segment of the first pass.
                        let referenceLane: Int
                        if pass == 0 && slice == 0 {
                            referenceLane = lane
                        } else {
                            referenceLane = Int((pseudoRandom >> 32) % UInt64(lanes))
                        }
                        let referenceIndex = alphaIndex(
                            pass: pass, slice: slice, index: index,
                            pseudoRandom: UInt32(truncatingIfNeeded: pseudoRandom),
                            sameLane: referenceLane == lane,
                            laneLength: laneLength, segmentLength: segmentLength
                        )

                        compress(
                            previous: memory + previous * 128,
                            reference: memory + (referenceLane * laneLength + referenceIndex) * 128,
                            into: memory + current * 128,
                            // Version 0x13 XORs the new block into the old one;
                            // 0x10 overwrites it. Only from the second pass on,
                            // when there is an old one.
                            xorIntoDestination: pass != 0 && parameters.version == .v0x13,
                            r: r, z: z, gather: gather
                        )

                        current += 1
                        previous += 1
                    }
                }
            }
        }
    }

    /// The address block for the next 128 data-independent positions:
    /// `G(zero, G(zero, input))`, with a counter that advances first.
    private static func nextAddresses(
        addressBlock: UnsafeMutablePointer<UInt64>,
        inputBlock: UnsafeMutablePointer<UInt64>,
        zeroBlock: UnsafeMutablePointer<UInt64>,
        r: UnsafeMutablePointer<UInt64>,
        z: UnsafeMutablePointer<UInt64>,
        gather: UnsafeMutablePointer<UInt64>
    ) {
        inputBlock[6] &+= 1
        compress(previous: zeroBlock, reference: inputBlock, into: addressBlock,
                 xorIntoDestination: false, r: r, z: z, gather: gather)
        compress(previous: zeroBlock, reference: addressBlock, into: addressBlock,
                 xorIntoDestination: false, r: r, z: z, gather: gather)
    }

    /// Which already-written block this one references.
    ///
    /// Transcribed from `index_alpha` in the reference implementation, including
    /// its unsigned wrap-around: the squaring in the middle is what biases the
    /// choice towards recent blocks, and doing it in signed arithmetic would
    /// pick different blocks and produce a different, self-consistent, wrong
    /// answer.
    static func alphaIndex(
        pass: Int, slice: Int, index: Int, pseudoRandom: UInt32, sameLane: Bool,
        laneLength: Int, segmentLength: Int
    ) -> Int {
        var referenceAreaSize: Int
        if pass == 0 {
            if slice == 0 {
                referenceAreaSize = index - 1
            } else if sameLane {
                referenceAreaSize = slice * segmentLength + index - 1
            } else {
                referenceAreaSize = slice * segmentLength + (index == 0 ? -1 : 0)
            }
        } else {
            if sameLane {
                referenceAreaSize = laneLength - segmentLength + index - 1
            } else {
                referenceAreaSize = laneLength - segmentLength + (index == 0 ? -1 : 0)
            }
        }

        let area = UInt64(UInt32(truncatingIfNeeded: referenceAreaSize))
        var relative = UInt64(pseudoRandom)
        relative = (relative &* relative) >> 32
        relative = area &- 1 &- ((area &* relative) >> 32)

        var start = 0
        if pass != 0 { start = slice == 3 ? 0 : (slice + 1) * segmentLength }
        return Int((UInt64(start) &+ relative) % UInt64(laneLength))
    }

    // MARK: The compression function G

    /// `G(X, Y) = Z ⊕ R` where `R = X ⊕ Y` and `Z` is `R` put through the
    /// permutation row-wise and then column-wise.
    ///
    /// The column pass is the part worth reading twice: it takes 16 words that
    /// are *not* adjacent — two from each of the eight rows — which is what
    /// diffuses a change across the whole 1 KiB block rather than along one row.
    @inline(__always)
    private static func compress(
        previous: UnsafePointer<UInt64>,
        reference: UnsafePointer<UInt64>,
        into destination: UnsafeMutablePointer<UInt64>,
        xorIntoDestination: Bool,
        r: UnsafeMutablePointer<UInt64>,
        z: UnsafeMutablePointer<UInt64>,
        gather: UnsafeMutablePointer<UInt64>
    ) {
        for i in 0..<128 {
            let value = previous[i] ^ reference[i]
            r[i] = value
            z[i] = value
        }

        for row in 0..<8 { permute(z + row * 16) }

        for column in 0..<8 {
            let base = column * 2
            for j in 0..<8 {
                gather[j * 2] = z[base + j * 16]
                gather[j * 2 + 1] = z[base + j * 16 + 1]
            }
            permute(gather)
            for j in 0..<8 {
                z[base + j * 16] = gather[j * 2]
                z[base + j * 16 + 1] = gather[j * 2 + 1]
            }
        }

        if xorIntoDestination {
            for i in 0..<128 { destination[i] ^= z[i] ^ r[i] }
        } else {
            for i in 0..<128 { destination[i] = z[i] ^ r[i] }
        }
    }

    /// The BLAKE2b round applied to 16 contiguous words.
    @inline(__always)
    private static func permute(_ v: UnsafeMutablePointer<UInt64>) {
        mix(v, 0, 4, 8, 12)
        mix(v, 1, 5, 9, 13)
        mix(v, 2, 6, 10, 14)
        mix(v, 3, 7, 11, 15)
        mix(v, 0, 5, 10, 15)
        mix(v, 1, 6, 11, 12)
        mix(v, 2, 7, 8, 13)
        mix(v, 3, 4, 9, 14)
    }

    /// Argon2's `GB`, which is BLAKE2b's `G` plus `2 * lo32(a) * lo32(b)` on
    /// each addition. That multiplication is the point of the whole function:
    /// it is what makes a cheap ASIC pass over memory expensive.
    @inline(__always)
    private static func mix(
        _ v: UnsafeMutablePointer<UInt64>, _ a: Int, _ b: Int, _ c: Int, _ d: Int
    ) {
        v[a] = v[a] &+ v[b] &+ 2 &* UInt64(UInt32(truncatingIfNeeded: v[a]))
            &* UInt64(UInt32(truncatingIfNeeded: v[b]))
        v[d] = (v[d] ^ v[a]).rotated(right: 32)
        v[c] = v[c] &+ v[d] &+ 2 &* UInt64(UInt32(truncatingIfNeeded: v[c]))
            &* UInt64(UInt32(truncatingIfNeeded: v[d]))
        v[b] = (v[b] ^ v[c]).rotated(right: 24)
        v[a] = v[a] &+ v[b] &+ 2 &* UInt64(UInt32(truncatingIfNeeded: v[a]))
            &* UInt64(UInt32(truncatingIfNeeded: v[b]))
        v[d] = (v[d] ^ v[a]).rotated(right: 16)
        v[c] = v[c] &+ v[d] &+ 2 &* UInt64(UInt32(truncatingIfNeeded: v[c]))
            &* UInt64(UInt32(truncatingIfNeeded: v[d]))
        v[b] = (v[b] ^ v[c]).rotated(right: 63)
    }
}

// MARK: - Errors

public enum Argon2Error: Error, Equatable, Sendable {
    /// The Rust crate refuses a salt under 8 bytes, so this does too rather
    /// than producing a tag the browser never would.
    case saltTooShort(Int)
    case tagTooShort(Int)
    case memoryTooSmall(UInt32)
    case invalidParallelism
    case invalidTimeCost
}

extension Argon2Error: CustomStringConvertible {
    public var description: String {
        switch self {
        case .saltTooShort(let count):
            return "Argon2 salt must be at least 8 bytes, got \(count)."
        case .tagTooShort(let count):
            return "Argon2 tag must be at least 4 bytes, got \(count)."
        case .memoryTooSmall(let kib):
            return "Argon2 memory must be at least 8 KiB per lane, got \(kib) KiB."
        case .invalidParallelism:
            return "Argon2 needs at least one lane."
        case .invalidTimeCost:
            return "Argon2 needs at least one pass."
        }
    }
}

// MARK: - Little-endian helpers

@inline(__always)
func le32(_ value: UInt32) -> [UInt8] {
    [
        UInt8(truncatingIfNeeded: value),
        UInt8(truncatingIfNeeded: value >> 8),
        UInt8(truncatingIfNeeded: value >> 16),
        UInt8(truncatingIfNeeded: value >> 24),
    ]
}

@inline(__always)
func loadLE64(_ bytes: [UInt8], at offset: Int) -> UInt64 {
    var value: UInt64 = 0
    for i in (0..<8).reversed() { value = (value << 8) | UInt64(bytes[offset + i]) }
    return value
}

@inline(__always)
func storeLE64(_ value: UInt64, into bytes: inout [UInt8], at offset: Int) {
    for i in 0..<8 { bytes[offset + i] = UInt8(truncatingIfNeeded: value >> (8 * i)) }
}

extension UInt64 {
    @inline(__always)
    func rotated(right count: UInt64) -> UInt64 {
        (self >> count) | (self << (64 - count))
    }
}
